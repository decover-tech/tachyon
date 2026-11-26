//! Copy-on-write volume engine. No job ever copies an image: job disks and
//! sticky volumes are instant clones on local NVMe. ZFS volumes are the
//! primary engine; LVM thin is the fallback (decision D2, from Phase 0
//! benchmarks).

use anyhow::{bail, Context, Result};
use std::collections::BTreeMap;
use std::process::Command;

/// A snapshot found on the host, with the labels hostd attached when it
/// created it (see `Storage::set_labels`). Unlabeled snapshots have none.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct SnapshotInfo {
    /// `vol@name`, relative to the engine root.
    pub id: String,
    pub labels: BTreeMap<String, String>,
}

/// Block-volume operations on one host. Ids are relative to the engine root,
/// e.g. `jobs/gh_123` or `prebuilds/acme_api@pb1-...`.
pub trait Storage: Send + Sync {
    /// Sparse, empty writable volume.
    fn create_empty(&self, vol: &str, size_gb: u32) -> Result<()>;
    /// Freezes `vol` as `vol@name`; returns the snapshot id.
    fn snapshot(&self, vol: &str, name: &str) -> Result<String>;
    /// Writable copy-on-write clone of a snapshot.
    fn clone_snapshot(&self, snapshot: &str, vol: &str) -> Result<()>;
    /// Removes a volume or snapshot.
    fn destroy(&self, id: &str) -> Result<()>;
    /// Block device to attach to a VM.
    fn device_path(&self, vol: &str) -> String;

    /// Attaches labels (`kind`, `repo`, `key`) to a snapshot so the host's
    /// inventory can be rebuilt from storage after a restart.
    fn set_labels(&self, _id: &str, _labels: &[(&str, &str)]) -> Result<()> {
        bail!("this storage engine does not support labels")
    }
    /// Every snapshot under the engine root, oldest first.
    fn list_snapshots(&self) -> Result<Vec<SnapshotInfo>> {
        bail!("this storage engine cannot list snapshots")
    }
    /// Volumes (not snapshots) under `prefix`, e.g. `jobs`.
    fn list_volumes(&self, _prefix: &str) -> Result<Vec<String>> {
        bail!("this storage engine cannot list volumes")
    }
}

/// Label keys `set_labels` accepts; stored as `tachyon:<key>` on ZFS.
pub const LABELS: [&str; 3] = ["kind", "repo", "key"];

/// Runs host commands; replaced in tests.
pub trait CommandRunner: Send + Sync {
    fn run(&self, program: &str, args: &[String]) -> Result<String>;
}

pub struct ExecRunner;

impl CommandRunner for ExecRunner {
    fn run(&self, program: &str, args: &[String]) -> Result<String> {
        let out = crate::timeline::span(crate::timeline::command_name(program, args), || {
            Command::new(program).args(args).output()
        })
        .with_context(|| format!("spawn {program}"))?;
        if !out.status.success() {
            bail!(
                "{program} {}: {}: {}",
                args.join(" "),
                out.status,
                String::from_utf8_lossy(&out.stderr).trim()
            );
        }
        Ok(String::from_utf8_lossy(&out.stdout).into_owned())
    }
}

/// ZFS volumes (zvols) under `pool`, e.g. `tank/tachyon`.
pub struct Zfs<R: CommandRunner> {
    pub pool: String,
    pub runner: R,
}

pub(crate) fn check_id(id: &str) -> Result<()> {
    let ok = !id.is_empty()
        && !id.contains("..")
        && !id.starts_with('/')
        && id
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || "_.:/@-".contains(c));
    if !ok {
        bail!("invalid volume id {id:?}");
    }
    Ok(())
}

impl<R: CommandRunner> Zfs<R> {
    fn ds(&self, id: &str) -> Result<String> {
        check_id(id)?;
        Ok(format!("{}/{}", self.pool, id))
    }

    fn zfs(&self, args: Vec<String>) -> Result<String> {
        self.runner.run("zfs", &args)
    }

    /// Strips `<pool>/` from a dataset name; None if it is outside the pool.
    fn rel<'a>(&self, ds: &'a str) -> Option<&'a str> {
        ds.strip_prefix(self.pool.as_str())?.strip_prefix('/')
    }
}

impl<R: CommandRunner> Storage for Zfs<R> {
    fn create_empty(&self, vol: &str, size_gb: u32) -> Result<()> {
        let ds = self.ds(vol)?;
        self.zfs(vec![
            "create".into(),
            "-p".into(),
            "-s".into(),
            "-V".into(),
            format!("{size_gb}G"),
            "-o".into(),
            "compression=lz4".into(),
            ds,
        ])?;
        Ok(())
    }

    fn snapshot(&self, vol: &str, name: &str) -> Result<String> {
        let id = format!("{vol}@{name}");
        let ds = self.ds(&id)?;
        self.zfs(vec!["snapshot".into(), ds])?;
        Ok(id)
    }

    fn clone_snapshot(&self, snapshot: &str, vol: &str) -> Result<()> {
        if !snapshot.contains('@') {
            bail!("{snapshot:?} is not a snapshot id");
        }
        let src = self.ds(snapshot)?;
        let dst = self.ds(vol)?;
        self.zfs(vec!["clone".into(), "-p".into(), src, dst])?;
        Ok(())
    }

    fn destroy(&self, id: &str) -> Result<()> {
        let ds = self.ds(id)?;
        // A snapshot with live clones is destroyed once its last clone goes.
        let mut args = vec!["destroy".to_string()];
        if id.contains('@') {
            args.push("-d".into());
        }
        args.push(ds);
        self.zfs(args)?;
        Ok(())
    }

    fn device_path(&self, vol: &str) -> String {
        format!("/dev/zvol/{}/{}", self.pool, vol)
    }

    fn set_labels(&self, id: &str, labels: &[(&str, &str)]) -> Result<()> {
        let ds = self.ds(id)?;
        let mut args = vec!["set".to_string()];
        for (k, v) in labels {
            if !LABELS.contains(k) {
                bail!("unknown label {k:?}");
            }
            // `zfs list -H` separates columns with tabs, and "-" means unset.
            if v.is_empty() || *v == "-" || v.contains(['\t', '\n']) {
                bail!("invalid value for label {k}: {v:?}");
            }
            args.push(format!("tachyon:{k}={v}"));
        }
        if args.len() == 1 {
            return Ok(());
        }
        args.push(ds);
        self.zfs(args)?;
        Ok(())
    }

    fn list_snapshots(&self) -> Result<Vec<SnapshotInfo>> {
        let mut cols = String::from("name");
        for k in LABELS {
            cols.push_str(&format!(",tachyon:{k}"));
        }
        let out = self.zfs(vec![
            "list".into(),
            "-H".into(),
            "-p".into(),
            "-t".into(),
            "snapshot".into(),
            "-o".into(),
            cols,
            "-s".into(),
            "creation".into(),
            "-r".into(),
            self.pool.clone(),
        ])?;
        let mut snaps = Vec::new();
        for line in out.lines().filter(|l| !l.trim().is_empty()) {
            let mut fields = line.split('\t');
            let Some(id) = fields.next().and_then(|n| self.rel(n)) else {
                continue;
            };
            let labels = LABELS
                .iter()
                .zip(fields)
                .filter(|(_, v)| !v.is_empty() && *v != "-")
                .map(|(k, v)| ((*k).to_string(), v.to_string()))
                .collect();
            snaps.push(SnapshotInfo {
                id: id.to_string(),
                labels,
            });
        }
        Ok(snaps)
    }

    fn list_volumes(&self, prefix: &str) -> Result<Vec<String>> {
        let root = self.ds(prefix)?;
        let out = match self.zfs(vec![
            "list".into(),
            "-H".into(),
            "-t".into(),
            "volume".into(),
            "-o".into(),
            "name".into(),
            "-r".into(),
            root,
        ]) {
            Ok(out) => out,
            // Nothing was ever created under the prefix.
            Err(e) if format!("{e:#}").contains("does not exist") => return Ok(Vec::new()),
            Err(e) => return Err(e),
        };
        Ok(out
            .lines()
            .filter_map(|l| self.rel(l.trim()))
            .map(str::to_string)
            .collect())
    }
}

/// Volume naming on a host.
pub fn job_disk(job_id: &str) -> String {
    format!("jobs/{}", sanitize(job_id))
}

pub fn sticky_clone(job_id: &str, key: &str) -> String {
    format!("jobs/{}-{}", sanitize(job_id), sanitize(key))
}

pub fn sticky_volume(repo: &str, key: &str) -> String {
    format!("sticky/{}/{}", sanitize(repo), sanitize(key))
}

pub fn prebuild_volume(repo: &str) -> String {
    format!("prebuilds/{}", sanitize(repo))
}

pub fn sanitize(s: &str) -> String {
    s.chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() || c == '-' || c == '_' || c == '.' {
                c
            } else {
                '_'
            }
        })
        .collect()
}

#[cfg(test)]
pub mod fake {
    use super::*;
    use std::collections::BTreeSet;
    use std::sync::Mutex;

    /// In-memory storage that records every call.
    #[derive(Default)]
    pub struct FakeStorage {
        pub vols: Mutex<BTreeSet<String>>,
        pub log: Mutex<Vec<String>>,
        pub fail_clone: bool,
        /// Labels per snapshot id.
        pub labels: Mutex<BTreeMap<String, BTreeMap<String, String>>>,
        /// Snapshot ids in creation order.
        pub created: Mutex<Vec<String>>,
    }

    impl FakeStorage {
        pub fn has(&self, id: &str) -> bool {
            self.vols.lock().unwrap().contains(id)
        }
        fn note(&self, s: String) {
            self.log.lock().unwrap().push(s);
        }
    }

    impl Storage for FakeStorage {
        fn create_empty(&self, vol: &str, _size_gb: u32) -> Result<()> {
            self.note(format!("create {vol}"));
            self.vols.lock().unwrap().insert(vol.into());
            Ok(())
        }
        fn snapshot(&self, vol: &str, name: &str) -> Result<String> {
            let id = format!("{vol}@{name}");
            self.note(format!("snapshot {id}"));
            self.vols.lock().unwrap().insert(id.clone());
            self.created.lock().unwrap().push(id.clone());
            Ok(id)
        }
        fn clone_snapshot(&self, snapshot: &str, vol: &str) -> Result<()> {
            self.note(format!("clone {snapshot} {vol}"));
            if self.fail_clone {
                bail!("clone failed");
            }
            if !self.has(snapshot) {
                bail!("no snapshot {snapshot}");
            }
            self.vols.lock().unwrap().insert(vol.into());
            Ok(())
        }
        fn destroy(&self, id: &str) -> Result<()> {
            self.note(format!("destroy {id}"));
            self.vols.lock().unwrap().remove(id);
            Ok(())
        }
        fn device_path(&self, vol: &str) -> String {
            format!("/dev/fake/{vol}")
        }
        fn set_labels(&self, id: &str, labels: &[(&str, &str)]) -> Result<()> {
            if !self.has(id) {
                bail!("no snapshot {id}");
            }
            let mut all = self.labels.lock().unwrap();
            let m = all.entry(id.into()).or_default();
            for (k, v) in labels {
                m.insert((*k).into(), (*v).into());
            }
            Ok(())
        }
        fn list_snapshots(&self) -> Result<Vec<SnapshotInfo>> {
            let vols = self.vols.lock().unwrap();
            let labels = self.labels.lock().unwrap();
            Ok(self
                .created
                .lock()
                .unwrap()
                .iter()
                .filter(|id| vols.contains(*id))
                .map(|id| SnapshotInfo {
                    id: id.clone(),
                    labels: labels.get(id).cloned().unwrap_or_default(),
                })
                .collect())
        }
        fn list_volumes(&self, prefix: &str) -> Result<Vec<String>> {
            let p = format!("{prefix}/");
            Ok(self
                .vols
                .lock()
                .unwrap()
                .iter()
                .filter(|v| v.starts_with(&p) && !v.contains('@'))
                .cloned()
                .collect())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    #[derive(Default)]
    struct Recorder(Mutex<Vec<String>>);
    impl CommandRunner for &Recorder {
        fn run(&self, program: &str, args: &[String]) -> Result<String> {
            self.0
                .lock()
                .unwrap()
                .push(format!("{program} {}", args.join(" ")));
            Ok(String::new())
        }
    }

    #[test]
    fn zfs_commands() {
        let rec = Recorder::default();
        let z = Zfs {
            pool: "tank/t".into(),
            runner: &rec,
        };
        z.create_empty("jobs/j1", 64).unwrap();
        assert_eq!(z.snapshot("prebuilds/a", "k1").unwrap(), "prebuilds/a@k1");
        z.clone_snapshot("prebuilds/a@k1", "jobs/j2").unwrap();
        z.destroy("jobs/j2").unwrap();
        z.destroy("prebuilds/a@k1").unwrap();
        assert_eq!(
            *rec.0.lock().unwrap(),
            vec![
                "zfs create -p -s -V 64G -o compression=lz4 tank/t/jobs/j1",
                "zfs snapshot tank/t/prebuilds/a@k1",
                "zfs clone -p tank/t/prebuilds/a@k1 tank/t/jobs/j2",
                "zfs destroy tank/t/jobs/j2",
                "zfs destroy -d tank/t/prebuilds/a@k1",
            ]
        );
        assert_eq!(z.device_path("jobs/j1"), "/dev/zvol/tank/t/jobs/j1");
    }

    #[test]
    fn rejects_bad_ids() {
        let rec = Recorder::default();
        let z = Zfs {
            pool: "tank".into(),
            runner: &rec,
        };
        assert!(z.destroy("../etc").is_err());
        assert!(z.destroy("jobs/a b").is_err());
        assert!(z.destroy("jobs/a;rm").is_err());
        assert!(z.clone_snapshot("not-a-snapshot", "jobs/x").is_err());
        assert!(rec.0.lock().unwrap().is_empty());
    }

    /// Returns canned stdout (or an error) and records the command.
    struct Canned {
        out: Result<String, String>,
        cmds: Mutex<Vec<String>>,
    }
    impl CommandRunner for &Canned {
        fn run(&self, program: &str, args: &[String]) -> Result<String> {
            self.cmds
                .lock()
                .unwrap()
                .push(format!("{program} {}", args.join(" ")));
            self.out.clone().map_err(|e| anyhow::anyhow!(e))
        }
    }
    fn canned(out: Result<&str, &str>) -> Canned {
        Canned {
            out: out.map(str::to_string).map_err(str::to_string),
            cmds: Mutex::default(),
        }
    }

    #[test]
    fn zfs_labels() {
        let rec = Recorder::default();
        let z = Zfs {
            pool: "tank/t".into(),
            runner: &rec,
        };
        z.set_labels(
            "jobs/j1@pb1",
            &[("kind", "prebuild"), ("repo", "acme/api"), ("key", "pb1")],
        )
        .unwrap();
        assert_eq!(
            rec.0.lock().unwrap()[0],
            "zfs set tachyon:kind=prebuild tachyon:repo=acme/api tachyon:key=pb1 tank/t/jobs/j1@pb1"
        );
        assert!(z.set_labels("jobs/j1@pb1", &[("owner", "x")]).is_err());
        assert!(z.set_labels("jobs/j1@pb1", &[("key", "a\tb")]).is_err());
        assert!(z.set_labels("jobs/j1@pb1", &[("key", "-")]).is_err());
        assert_eq!(rec.0.lock().unwrap().len(), 1);
    }

    #[test]
    fn zfs_list_snapshots() {
        let c = canned(Ok("tank/t/jobs/j1@pb1\tprebuild\tacme/api\tpb1\n\
             tank/t/jobs/j2-docker@head-j2\tsticky\tacme/api\tdocker\n\
             tank/t/base@v1\t-\t-\t-\n\
             other/x@y\tprebuild\ta/b\tk\n"));
        let z = Zfs {
            pool: "tank/t".into(),
            runner: &c,
        };
        let snaps = z.list_snapshots().unwrap();
        assert_eq!(
            c.cmds.lock().unwrap()[0],
            "zfs list -H -p -t snapshot -o name,tachyon:kind,tachyon:repo,tachyon:key -s creation -r tank/t"
        );
        assert_eq!(snaps.len(), 3, "dataset outside the pool skipped");
        assert_eq!(snaps[0].id, "jobs/j1@pb1");
        assert_eq!(snaps[0].labels["kind"], "prebuild");
        assert_eq!(snaps[0].labels["repo"], "acme/api");
        assert_eq!(snaps[1].labels["key"], "docker");
        assert_eq!(snaps[2].id, "base@v1");
        assert!(snaps[2].labels.is_empty(), "\"-\" means unset");
    }

    #[test]
    fn zfs_list_volumes() {
        let c = canned(Ok("tank/t/jobs/j1\ntank/t/jobs/j2-docker\n"));
        let z = Zfs {
            pool: "tank/t".into(),
            runner: &c,
        };
        assert_eq!(
            z.list_volumes("jobs").unwrap(),
            vec!["jobs/j1", "jobs/j2-docker"]
        );
        assert_eq!(
            c.cmds.lock().unwrap()[0],
            "zfs list -H -t volume -o name -r tank/t/jobs"
        );

        let missing = canned(Err("cannot open 'tank/t/jobs': dataset does not exist"));
        let z = Zfs {
            pool: "tank/t".into(),
            runner: &missing,
        };
        assert!(z.list_volumes("jobs").unwrap().is_empty());

        let broken = canned(Err("zfs: permission denied"));
        let z = Zfs {
            pool: "tank/t".into(),
            runner: &broken,
        };
        assert!(z.list_volumes("jobs").is_err());
    }

    #[test]
    fn naming() {
        assert_eq!(
            sticky_volume("acme/api", "docker"),
            "sticky/acme_api/docker"
        );
        assert_eq!(job_disk("gh_1"), "jobs/gh_1");
        assert_eq!(prebuild_volume("acme/api"), "prebuilds/acme_api");
    }
}
