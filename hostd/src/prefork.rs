//! Pre-forked job disks. Push and pull-request webhooks usually arrive
//! before a job's `queued` event, so the control plane sends a `PreFork`
//! hint and hostd clones the job disk from the prebuild right away. The
//! first `AssignJob` for the same repo, commit and prebuild snapshot takes
//! the disk instead of cloning one, which takes the clone off the pickup
//! path. Unused pre-forks are destroyed once they expire.
//!
//! State is in memory: pre-forks left on disk by a hostd restart are not
//! reclaimed here.

use anyhow::{Context, Result};
use std::collections::HashMap;
use std::sync::Mutex;
use std::time::{Duration, SystemTime};

use crate::hypervisor::Hypervisor;
use crate::lifecycle::Agent;
use crate::pb;
use crate::storage::{self, Storage};

/// Hold time when a hint carries no expiry.
pub const DEFAULT_TTL: Duration = Duration::from_secs(10 * 60);
/// Upper bound on hold time, whatever the hint asks for.
pub const MAX_TTL: Duration = Duration::from_secs(30 * 60);
/// Most pre-forks held at once; the one expiring soonest is evicted first.
pub const MAX_HELD: usize = 32;
/// How often hostd sweeps expired pre-forks.
pub const SWEEP_INTERVAL: Duration = Duration::from_secs(30);

#[derive(Debug, Clone)]
struct Held {
    repo: String,
    commit: String,
    snapshot: String,
    vol: String,
    expires_at: SystemTime,
}

/// Pre-forked disks held on this host, keyed by pre-fork id.
#[derive(Default)]
pub struct PreForks {
    held: Mutex<HashMap<String, Held>>,
}

impl PreForks {
    /// Number of pre-forks currently held.
    pub fn len(&self) -> usize {
        self.held.lock().unwrap().len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }
}

/// Volume name for a pre-forked job disk.
pub fn prefork_disk(pre_fork_id: &str) -> String {
    format!("prefork/{}", storage::sanitize(pre_fork_id))
}

fn expiry(p: &pb::PreFork, now: SystemTime) -> SystemTime {
    let max = now + MAX_TTL;
    match p
        .expires_at
        .as_ref()
        .and_then(|t| SystemTime::try_from(*t).ok())
    {
        Some(t) => t.min(max),
        None => now + DEFAULT_TTL,
    }
}

impl<H: Hypervisor, S: Storage> Agent<H, S> {
    /// Clones a job disk from the hinted prebuild and holds it until expiry.
    /// A repeated hint for the same id only extends the hold.
    pub fn pre_fork(&self, p: &pb::PreFork, now: SystemTime) -> Result<()> {
        if p.pre_fork_id.is_empty() || p.prebuild_snapshot_id.is_empty() {
            anyhow::bail!("pre-fork needs an id and a prebuild snapshot");
        }
        let expires_at = expiry(p, now);
        if expires_at <= now {
            return Ok(()); // already stale; nothing to hold
        }
        let mut evict = Vec::new();
        let cloned = {
            // The lock is held across the clone so a concurrent hint for the
            // same id cannot clone twice; clones are copy-on-write and fast.
            let mut held = self.pre_forks.held.lock().unwrap();
            if let Some(h) = held.get_mut(&p.pre_fork_id) {
                if h.snapshot == p.prebuild_snapshot_id {
                    h.expires_at = h.expires_at.max(expires_at);
                    return Ok(());
                }
                // The prebuild moved on; replace the stale fork. It shares
                // the new fork's volume name, so it goes before the clone.
                let stale = held.remove(&p.pre_fork_id).unwrap().vol;
                self.discard(vec![stale]);
            }
            while held.len() >= MAX_HELD {
                let soonest = held
                    .iter()
                    .min_by_key(|(_, h)| h.expires_at)
                    .map(|(k, _)| k.clone())
                    .unwrap();
                evict.push(held.remove(&soonest).unwrap().vol);
            }
            let vol = prefork_disk(&p.pre_fork_id);
            let cloned = self
                .storage
                .clone_snapshot(&p.prebuild_snapshot_id, &vol)
                .context("pre-fork prebuild");
            if cloned.is_ok() {
                held.insert(
                    p.pre_fork_id.clone(),
                    Held {
                        repo: p.repo.clone(),
                        commit: p.commit.clone(),
                        snapshot: p.prebuild_snapshot_id.clone(),
                        vol,
                        expires_at,
                    },
                );
            }
            cloned
        };
        self.discard(evict);
        cloned
    }

    /// Takes the pre-forked disk matching this job, if one is held and has
    /// not expired. The caller owns the returned volume from then on.
    pub fn take_pre_fork(&self, job: &pb::AssignJob, now: SystemTime) -> Option<String> {
        if job.prebuild_snapshot_id.is_empty() || job.commit.is_empty() {
            return None;
        }
        let mut held = self.pre_forks.held.lock().unwrap();
        let id = held
            .iter()
            .find(|(_, h)| {
                h.repo == job.repo
                    && h.commit == job.commit
                    && h.snapshot == job.prebuild_snapshot_id
                    && h.expires_at > now
            })
            .map(|(k, _)| k.clone())?;
        held.remove(&id).map(|h| h.vol)
    }

    /// Destroys every pre-fork whose hold has expired; returns how many.
    pub fn expire_pre_forks(&self, now: SystemTime) -> usize {
        let expired: Vec<String> = {
            let mut held = self.pre_forks.held.lock().unwrap();
            let ids: Vec<String> = held
                .iter()
                .filter(|(_, h)| h.expires_at <= now)
                .map(|(k, _)| k.clone())
                .collect();
            ids.into_iter()
                .filter_map(|k| held.remove(&k).map(|h| h.vol))
                .collect()
        };
        let n = expired.len();
        self.discard(expired);
        n
    }

    fn discard(&self, vols: Vec<String>) {
        for v in vols {
            if let Err(e) = self.storage.destroy(&v) {
                tracing::warn!(disk = %v, error = %format!("{e:#}"), "discard pre-fork");
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::hypervisor::fake::FakeHypervisor;
    use crate::storage::fake::FakeStorage;
    use pb::job_result::Outcome;

    const SNAP: &str = "prebuilds/acme_api@k1";

    fn agent() -> Agent<FakeHypervisor, FakeStorage> {
        let st = FakeStorage::default();
        st.snapshot("prebuilds/acme_api", "k1").unwrap();
        Agent::new(FakeHypervisor::default(), st, HashMap::new())
    }

    fn t0() -> SystemTime {
        SystemTime::UNIX_EPOCH + Duration::from_secs(1_800_000_000)
    }

    fn hint(id: &str, commit: &str, ttl: Option<Duration>) -> pb::PreFork {
        pb::PreFork {
            pre_fork_id: id.into(),
            repo: "acme/api".into(),
            commit: commit.into(),
            prebuild_snapshot_id: SNAP.into(),
            expires_at: ttl.map(|d| prost_types::Timestamp::from(t0() + d)),
        }
    }

    fn job(commit: &str) -> pb::AssignJob {
        pb::AssignJob {
            job_id: "gh_1".into(),
            repo: "acme/api".into(),
            commit: commit.into(),
            vcpu: 4,
            memory_gb: 16,
            prebuild_snapshot_id: SNAP.into(),
            ..Default::default()
        }
    }

    fn clones(a: &Agent<FakeHypervisor, FakeStorage>) -> usize {
        let log = a.storage.log.lock().unwrap();
        log.iter().filter(|l| l.starts_with("clone ")).count()
    }

    #[test]
    fn pre_fork_clones_and_holds() {
        let a = agent();
        a.pre_fork(
            &hint("acme/api@abc", "abc", Some(Duration::from_secs(60))),
            t0(),
        )
        .unwrap();
        assert!(a.storage.has("prefork/acme_api_abc"));
        assert_eq!(a.pre_forks.len(), 1);
        // Held until expiry: a sweep before then keeps it.
        assert_eq!(a.expire_pre_forks(t0() + Duration::from_secs(59)), 0);
        assert!(a.storage.has("prefork/acme_api_abc"));
    }

    #[test]
    fn repeated_hint_extends_without_recloning() {
        let a = agent();
        let h = hint("acme/api@abc", "abc", Some(Duration::from_secs(60)));
        a.pre_fork(&h, t0()).unwrap();
        let later = hint("acme/api@abc", "abc", Some(Duration::from_secs(120)));
        a.pre_fork(&later, t0()).unwrap();
        assert_eq!(clones(&a), 1);
        assert_eq!(a.expire_pre_forks(t0() + Duration::from_secs(90)), 0);
        assert_eq!(a.expire_pre_forks(t0() + Duration::from_secs(120)), 1);
    }

    #[test]
    fn expired_pre_forks_are_destroyed() {
        let a = agent();
        a.pre_fork(
            &hint("acme/api@abc", "abc", Some(Duration::from_secs(60))),
            t0(),
        )
        .unwrap();
        a.pre_fork(
            &hint("acme/api@def", "def", Some(Duration::from_secs(600))),
            t0(),
        )
        .unwrap();
        assert_eq!(a.expire_pre_forks(t0() + Duration::from_secs(60)), 1);
        assert!(!a.storage.has("prefork/acme_api_abc"));
        assert!(a.storage.has("prefork/acme_api_def"));
        assert_eq!(a.pre_forks.len(), 1);
    }

    #[test]
    fn missing_expiry_uses_default_and_long_expiry_is_capped() {
        let a = agent();
        a.pre_fork(&hint("x@1", "1", None), t0()).unwrap();
        a.pre_fork(
            &hint("x@2", "2", Some(Duration::from_secs(24 * 3600))),
            t0(),
        )
        .unwrap();
        assert_eq!(a.expire_pre_forks(t0() + DEFAULT_TTL), 1);
        assert_eq!(a.expire_pre_forks(t0() + MAX_TTL), 1);
        assert!(a.pre_forks.is_empty());
    }

    #[test]
    fn stale_hint_is_ignored() {
        let a = agent();
        let mut h = hint("acme/api@abc", "abc", None);
        h.expires_at = Some(prost_types::Timestamp::from(t0() - Duration::from_secs(1)));
        a.pre_fork(&h, t0()).unwrap();
        assert_eq!(clones(&a), 0);
        assert!(a.pre_forks.is_empty());
    }

    #[test]
    fn clone_failure_holds_nothing() {
        let a = agent();
        let mut h = hint("acme/api@abc", "abc", None);
        h.prebuild_snapshot_id = "prebuilds/acme_api@gone".into();
        assert!(a.pre_fork(&h, t0()).is_err());
        assert!(a.pre_forks.is_empty());
    }

    #[test]
    fn invalid_hint_is_rejected() {
        let a = agent();
        let mut h = hint("", "abc", None);
        assert!(a.pre_fork(&h, t0()).is_err());
        h = hint("id", "abc", None);
        h.prebuild_snapshot_id.clear();
        assert!(a.pre_fork(&h, t0()).is_err());
        assert_eq!(clones(&a), 0);
    }

    #[test]
    fn new_prebuild_replaces_stale_fork() {
        let a = agent();
        a.storage.snapshot("prebuilds/acme_api", "k2").unwrap();
        a.pre_fork(&hint("acme/api@abc", "abc", None), t0())
            .unwrap();
        let mut h = hint("acme/api@abc", "abc", None);
        h.prebuild_snapshot_id = "prebuilds/acme_api@k2".into();
        a.pre_fork(&h, t0()).unwrap();
        let log = a.storage.log.lock().unwrap();
        assert_eq!(
            log[log.len() - 2..],
            [
                "destroy prefork/acme_api_abc",
                "clone prebuilds/acme_api@k2 prefork/acme_api_abc"
            ]
        );
        drop(log);
        assert!(a.storage.has("prefork/acme_api_abc"));
        assert_eq!(a.pre_forks.len(), 1);
    }

    #[test]
    fn evicts_soonest_expiring_when_full() {
        let a = agent();
        for i in 0..MAX_HELD {
            let ttl = Duration::from_secs(100 + i as u64);
            a.pre_fork(&hint(&format!("r@{i}"), &i.to_string(), Some(ttl)), t0())
                .unwrap();
        }
        a.pre_fork(&hint("r@new", "new", None), t0()).unwrap();
        assert_eq!(a.pre_forks.len(), MAX_HELD);
        assert!(!a.storage.has("prefork/r_0"));
        assert!(a.storage.has("prefork/r_1"));
        assert!(a.storage.has("prefork/r_new"));
    }

    #[test]
    fn take_matches_repo_commit_and_snapshot_once() {
        let a = agent();
        a.pre_fork(&hint("acme/api@abc", "abc", None), t0())
            .unwrap();
        assert_eq!(a.take_pre_fork(&job("other"), t0()), None);
        let mut other_repo = job("abc");
        other_repo.repo = "acme/web".into();
        assert_eq!(a.take_pre_fork(&other_repo, t0()), None);
        let mut other_snap = job("abc");
        other_snap.prebuild_snapshot_id = "prebuilds/acme_api@k0".into();
        assert_eq!(a.take_pre_fork(&other_snap, t0()), None);
        let mut no_prebuild = job("abc");
        no_prebuild.prebuild_snapshot_id.clear();
        assert_eq!(a.take_pre_fork(&no_prebuild, t0()), None);

        assert_eq!(
            a.take_pre_fork(&job("abc"), t0()).as_deref(),
            Some("prefork/acme_api_abc")
        );
        // Taken disks belong to the job: never handed out or expired again.
        assert_eq!(a.take_pre_fork(&job("abc"), t0()), None);
        assert_eq!(a.expire_pre_forks(t0() + MAX_TTL), 0);
        assert!(a.storage.has("prefork/acme_api_abc"));
    }

    #[test]
    fn take_skips_expired() {
        let a = agent();
        a.pre_fork(
            &hint("acme/api@abc", "abc", Some(Duration::from_secs(60))),
            t0(),
        )
        .unwrap();
        assert_eq!(
            a.take_pre_fork(&job("abc"), t0() + Duration::from_secs(60)),
            None
        );
    }

    #[test]
    fn assigned_job_uses_pre_forked_disk() {
        let a = agent();
        a.pre_fork(&hint("acme/api@abc", "abc", None), SystemTime::now())
            .unwrap();
        let r = a.run_job(&job("abc"), &|_| {});
        assert_eq!(r.outcome(), Outcome::Succeeded, "{}", r.error);
        // Only the pre-fork cloned the prebuild; the job attached it.
        assert_eq!(clones(&a), 1);
        assert!(a.hv.payloads.lock().unwrap().len() == 1);
        let log = a.storage.log.lock().unwrap();
        assert!(!log.iter().any(|l| l.contains("jobs/gh_1")));
        // Discarded with the job like any job disk.
        assert_eq!(log.last().unwrap(), "destroy prefork/acme_api_abc");
        drop(log);
        assert!(a.pre_forks.is_empty());
    }

    #[test]
    fn assigned_job_without_pre_fork_clones_as_before() {
        let a = agent();
        a.pre_fork(&hint("acme/api@abc", "abc", None), SystemTime::now())
            .unwrap();
        let r = a.run_job(&job("def"), &|_| {});
        assert_eq!(r.outcome(), Outcome::Succeeded, "{}", r.error);
        assert_eq!(clones(&a), 2);
        assert!(
            a.storage.has("prefork/acme_api_abc"),
            "other commit's fork kept"
        );
    }
}
