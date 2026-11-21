//! Startup recovery. hostd may restart (crash, upgrade, reboot) while jobs
//! are running; that should make a job slower, never fail it silently or
//! leak the host's capacity. Before linking to the control plane hostd:
//!
//! 1. reports jobs a previous hostd was running as infrastructure errors
//!    (their VMs died with it, so the job is eligible for a re-run);
//! 2. kills orphaned VMs and removes their cgroups and run directories;
//! 3. destroys orphaned job disks (anything under `jobs/` with no snapshot);
//! 4. rebuilds the snapshot inventory from the labels hostd puts on every
//!    snapshot it keeps (`zfs list -t snapshot` on ZFS).
//!
//! Nothing runs before recovery finishes, so everything found is an orphan.

use anyhow::Result;
use std::fs;
use std::path::Path;
use std::time::{Duration, Instant};

use crate::link::Inventory;
use crate::outbox::Outbox;
use crate::pb::{self, snapshot_ref::Kind};
use crate::storage::Storage;

/// Label value for each snapshot kind.
pub fn kind_label(kind: Kind) -> Option<&'static str> {
    match kind {
        Kind::BaseImage => Some("base_image"),
        Kind::Prebuild => Some("prebuild"),
        Kind::Sticky => Some("sticky"),
        Kind::Unspecified => None,
    }
}

fn parse_kind(s: &str) -> Option<Kind> {
    [Kind::BaseImage, Kind::Prebuild, Kind::Sticky]
        .into_iter()
        .find(|k| kind_label(*k) == Some(s))
}

/// Labels a snapshot hostd keeps so `rebuild_inventory` finds it again.
/// Failures are logged: the snapshot still works until the next restart.
pub fn label_snapshot<S: Storage>(storage: &S, r: &pb::SnapshotRef) {
    let Some(kind) = kind_label(r.kind()) else {
        return;
    };
    let mut labels = vec![("kind", kind)];
    if !r.repo.is_empty() {
        labels.push(("repo", r.repo.as_str()));
    }
    if !r.key.is_empty() {
        labels.push(("key", r.key.as_str()));
    }
    if let Err(e) = storage.set_labels(&r.id, &labels) {
        tracing::warn!(snapshot = %r.id, error = %format!("{e:#}"), "label snapshot");
    }
}

/// Rebuilds the inventory from labeled snapshots on the host. Snapshots are
/// listed oldest first, so the newest prebuild per repo (and sticky head per
/// repo and key) wins. Returns how many snapshots are now in the inventory.
pub fn rebuild_inventory<S: Storage>(storage: &S, inv: &Inventory) -> Result<usize> {
    for s in storage.list_snapshots()? {
        let Some(kind) = s.labels.get("kind").and_then(|k| parse_kind(k)) else {
            continue;
        };
        let label = |k: &str| s.labels.get(k).cloned().unwrap_or_default();
        inv.put(pb::SnapshotRef {
            kind: kind as i32,
            repo: label("repo"),
            key: label("key"),
            id: s.id.clone(),
        });
    }
    Ok(inv.list().len())
}

/// Destroys job disks a crash left behind: every volume under `jobs/` that
/// has no snapshot. Volumes with snapshots back a prebuild or a sticky head
/// and are kept. Returns the volumes destroyed.
pub fn clean_job_disks<S: Storage>(storage: &S) -> Result<Vec<String>> {
    let snapshotted: std::collections::HashSet<String> = storage
        .list_snapshots()?
        .into_iter()
        .filter_map(|s| s.id.split_once('@').map(|(v, _)| v.to_string()))
        .collect();
    let mut destroyed = Vec::new();
    for vol in storage.list_volumes("jobs")? {
        if snapshotted.contains(&vol) {
            continue;
        }
        match storage.destroy(&vol) {
            Ok(()) => destroyed.push(vol),
            Err(e) => {
                tracing::warn!(disk = %vol, error = %format!("{e:#}"), "destroy orphaned job disk")
            }
        }
    }
    Ok(destroyed)
}

/// Reports every job a previous hostd was running as an infrastructure
/// error, unless its result is already waiting in the outbox. Returns the
/// jobs reported.
pub fn report_interrupted(outbox: &Outbox) -> Vec<String> {
    let mut reported = Vec::new();
    for job_id in outbox.interrupted() {
        if !outbox.has_result(&job_id) {
            let mut r = pb::JobResult {
                job_id: job_id.clone(),
                error: "tachyon-hostd restarted while the job was running".into(),
                ..Default::default()
            };
            r.set_outcome(pb::job_result::Outcome::InfraError);
            outbox.push(pb::link_request::Msg::JobResult(r));
            reported.push(job_id.clone());
        }
        outbox.mark_done(&job_id);
    }
    reported
}

/// Kills VMs a previous hostd started (warm or running) and removes their
/// cgroups (`<cgroup_parent>/vm-*`) and run directories (`<run_dir>/vm*`).
/// Returns the cgroups found.
pub fn reap_vms(run_dir: &Path, cgroup_parent: &Path) -> Vec<String> {
    let mut reaped = Vec::new();
    for dir in subdirs(cgroup_parent, "vm-") {
        kill_cgroup(&dir);
        if let Err(e) = fs::remove_dir(&dir) {
            tracing::warn!(cgroup = %dir.display(), error = %e, "remove orphaned VM cgroup");
        }
        reaped.push(dir.file_name().unwrap().to_string_lossy().into_owned());
    }
    for dir in subdirs(run_dir, "vm") {
        if let Err(e) = fs::remove_dir_all(&dir) {
            tracing::warn!(dir = %dir.display(), error = %e, "remove orphaned VM dir");
        }
    }
    reaped
}

fn subdirs(parent: &Path, prefix: &str) -> Vec<std::path::PathBuf> {
    let Ok(entries) = fs::read_dir(parent) else {
        return Vec::new();
    };
    let mut dirs: Vec<_> = entries
        .filter_map(Result::ok)
        .filter(|e| e.file_type().is_ok_and(|t| t.is_dir()))
        .filter(|e| e.file_name().to_string_lossy().starts_with(prefix))
        .map(|e| e.path())
        .collect();
    dirs.sort();
    dirs
}

/// Kills every process in a cgroup v2 and waits briefly for it to empty.
fn kill_cgroup(dir: &Path) {
    // cgroup.kill (Linux 5.14+) kills the whole cgroup atomically.
    if fs::write(dir.join("cgroup.kill"), "1").is_err() {
        for pid in procs(dir) {
            // SAFETY: plain kill(2) on a pid read from the VM's cgroup.
            unsafe {
                libc::kill(pid, libc::SIGKILL);
            }
        }
    }
    let start = Instant::now();
    while !procs(dir).is_empty() && start.elapsed() < Duration::from_secs(2) {
        std::thread::sleep(Duration::from_millis(20));
    }
}

fn procs(dir: &Path) -> Vec<libc::pid_t> {
    fs::read_to_string(dir.join("cgroup.procs"))
        .unwrap_or_default()
        .split_whitespace()
        .filter_map(|p| p.parse().ok())
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::storage::fake::FakeStorage;

    fn sref(kind: Kind, repo: &str, key: &str, id: &str) -> pb::SnapshotRef {
        pb::SnapshotRef {
            kind: kind as i32,
            repo: repo.into(),
            key: key.into(),
            id: id.into(),
        }
    }

    #[test]
    fn kind_labels_round_trip() {
        for k in [Kind::BaseImage, Kind::Prebuild, Kind::Sticky] {
            assert_eq!(parse_kind(kind_label(k).unwrap()), Some(k));
        }
        assert_eq!(kind_label(Kind::Unspecified), None);
        assert_eq!(parse_kind("bogus"), None);
    }

    #[test]
    fn inventory_rebuilt_from_labels_newest_wins() {
        let st = FakeStorage::default();
        for (vol, name, r) in [
            (
                "jobs/gh_1",
                "pb-old",
                sref(Kind::Prebuild, "acme/api", "pb-old", ""),
            ),
            (
                "jobs/gh_1-docker",
                "head-gh_1",
                sref(Kind::Sticky, "acme/api", "docker", ""),
            ),
            (
                "jobs/gh_2",
                "pb-new",
                sref(Kind::Prebuild, "acme/api", "pb-new", ""),
            ),
            (
                "jobs/gh_3-docker",
                "head-gh_3",
                sref(Kind::Sticky, "acme/api", "docker", ""),
            ),
        ] {
            let id = st.snapshot(vol, name).unwrap();
            label_snapshot(&st, &pb::SnapshotRef { id, ..r });
        }
        st.snapshot("scratch/x", "unlabeled").unwrap();

        let inv = Inventory::default();
        assert_eq!(rebuild_inventory(&st, &inv).unwrap(), 2);
        let refs = inv.list();
        assert!(refs.contains(&sref(
            Kind::Prebuild,
            "acme/api",
            "pb-new",
            "jobs/gh_2@pb-new"
        )));
        assert!(refs.contains(&sref(
            Kind::Sticky,
            "acme/api",
            "docker",
            "jobs/gh_3-docker@head-gh_3"
        )));
    }

    #[test]
    fn unspecified_kind_is_not_labeled() {
        let st = FakeStorage::default();
        let id = st.snapshot("jobs/a", "s").unwrap();
        label_snapshot(&st, &sref(Kind::Unspecified, "r", "k", &id));
        assert!(st.labels.lock().unwrap().is_empty());
    }

    #[test]
    fn orphaned_job_disks_destroyed_kept_ones_survive() {
        let st = FakeStorage::default();
        st.create_empty("jobs/gh_9", 64).unwrap(); // crashed mid-job
        st.create_empty("jobs/gh_9-docker", 64).unwrap(); // its sticky clone
        st.create_empty("jobs/gh_1", 64).unwrap(); // backs a prebuild
        st.snapshot("jobs/gh_1", "pb1").unwrap();
        st.create_empty("sticky/acme_api/docker", 64).unwrap(); // not a job disk

        let mut gone = clean_job_disks(&st).unwrap();
        gone.sort();
        assert_eq!(gone, vec!["jobs/gh_9", "jobs/gh_9-docker"]);
        assert!(st.has("jobs/gh_1"));
        assert!(st.has("jobs/gh_1@pb1"));
        assert!(st.has("sticky/acme_api/docker"));
    }

    #[test]
    fn interrupted_jobs_reported_once_as_infra_errors() {
        let tmp = tempfile::tempdir().unwrap();
        {
            let o = Outbox::open(tmp.path()).unwrap();
            o.mark_running("gh/1");
            o.mark_running("gh/2");
            // gh/2 finished and queued its result just before the crash.
            o.push(pb::link_request::Msg::JobResult(pb::JobResult {
                job_id: "gh/2".into(),
                outcome: pb::job_result::Outcome::Succeeded as i32,
                ..Default::default()
            }));
        }
        let o = Outbox::open(tmp.path()).unwrap();
        assert_eq!(report_interrupted(&o), vec!["gh/1"]);
        assert!(o.interrupted().is_empty());
        let pending = o.pending_after(0);
        assert_eq!(pending.len(), 2);
        match &pending[1].msg {
            Some(pb::link_request::Msg::JobResult(r)) => {
                assert_eq!(r.job_id, "gh/1");
                assert_eq!(r.outcome(), pb::job_result::Outcome::InfraError);
            }
            m => panic!("unexpected {m:?}"),
        }
        assert!(report_interrupted(&o).is_empty(), "markers cleared");
    }

    #[test]
    fn orphaned_vms_reaped() {
        let tmp = tempfile::tempdir().unwrap();
        let (run, cg) = (tmp.path().join("run"), tmp.path().join("cg"));
        fs::create_dir_all(run.join("vm0")).unwrap();
        fs::write(run.join("vm0/api.sock"), "").unwrap();
        fs::create_dir_all(run.join("vm7")).unwrap();
        fs::create_dir_all(run.join("keep")).unwrap();
        fs::create_dir_all(cg.join("vm-vm0")).unwrap();
        fs::write(cg.join("vm-vm0/cgroup.procs"), "").unwrap();
        fs::create_dir_all(cg.join("other")).unwrap();

        assert_eq!(reap_vms(&run, &cg), vec!["vm-vm0"]);
        assert_eq!(
            fs::read_to_string(cg.join("vm-vm0/cgroup.kill")).unwrap(),
            "1"
        );
        assert!(!run.join("vm0").exists());
        assert!(!run.join("vm7").exists());
        assert!(run.join("keep").exists());
        assert!(cg.join("other").exists());
        // Missing directories are not an error.
        assert!(reap_vms(&tmp.path().join("nope"), &tmp.path().join("nope")).is_empty());
    }
}
