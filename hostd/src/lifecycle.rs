//! The microVM lifecycle on a host:
//!
//! 1. Keep a small pool of VMs restored from the base snapshot and paused,
//!    so no job waits for a kernel boot.
//! 2. On assignment, fork the job's writable disk (copy-on-write from the
//!    repo's prebuild, or empty) and its sticky volumes, resume the VM,
//!    attach the disks and pass the job config over vsock.
//! 3. The guest starts the runner with its single-use config and runs the job.
//! 4. Destroy the VM and discard its disks, unless the run built a prebuild
//!    or is a trusted, successful run whose sticky volumes get promoted.
//!
//! One job per VM, never reused. A failure here should make a job slower,
//! never fail it silently: every error path still tears the VM down.
//!
//! Cancellation: `Agent::cancel` asks the guest agent to stop the runner
//! and, if the guest has not reported within `cancel_grace`, unblocks the
//! wait so the VM is torn down anyway. A job cancelled before its VM runs
//! never starts one. Either way the result is OUTCOME_CANCELLED and nothing
//! is promoted.

use anyhow::{Context, Result};
use std::collections::HashMap;
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime};

use crate::hypervisor::{GuestDisk, GuestPayload, GuestPrebuild, GuestStop, Hypervisor};
use crate::network::EgressPolicy;
use crate::pb;
use crate::rootfs;
use crate::prefork::PreForks;
use crate::storage::{self, Storage};

/// Max job wall time, matching GitHub-hosted runners.
pub const JOB_DEADLINE: Duration = Duration::from_secs(6 * 3600);
/// Size of an empty job disk when there is no prebuild.
pub const EMPTY_DISK_GB: u32 = 64;
/// How long a cancelled job's guest gets to stop its runner and report
/// before hostd tears the VM down regardless.
pub const CANCEL_GRACE: Duration = Duration::from_secs(30);

pub type Events<'a> = &'a (dyn Fn(pb::Phase) + Sync);

/// Per-job cancellation state, shared between `run_job` and `cancel`.
#[derive(Default)]
struct CancelState {
    requested: bool,
    /// Set once the guest has its payload and can be told to stop.
    stop: Option<Arc<dyn GuestStop>>,
}

type CancelSlot = Arc<Mutex<CancelState>>;

fn cancel_requested(slot: &CancelSlot) -> bool {
    slot.lock().unwrap().requested
}

pub struct Agent<H: Hypervisor, S: Storage> {
    pub hv: H,
    pub storage: S,
    /// Paused VMs kept ready per vCPU size.
    pub warm_target: HashMap<u32, usize>,
    pub cancel_grace: Duration,
    pool: Mutex<HashMap<u32, Vec<H::Vm>>>,
    jobs: Mutex<HashMap<String, CancelSlot>>,
    /// Job disks forked ahead of assignment from push/PR hints.
    pub(crate) pre_forks: PreForks,
}

impl<H: Hypervisor, S: Storage> Agent<H, S> {
    pub fn new(hv: H, storage: S, warm_target: HashMap<u32, usize>) -> Self {
        Self {
            hv,
            storage,
            warm_target,
            cancel_grace: CANCEL_GRACE,
            pool: Mutex::new(HashMap::new()),
            jobs: Mutex::new(HashMap::new()),
            pre_forks: PreForks::default(),
        }
    }

    /// Makes a job cancellable before `run_job` starts it, so a CancelJob
    /// that arrives right behind its AssignJob is not lost. `run_job` calls
    /// this too; calling it first is optional.
    pub fn admit(&self, job_id: &str) {
        self.slot(job_id);
    }

    fn slot(&self, job_id: &str) -> CancelSlot {
        self.jobs
            .lock()
            .unwrap()
            .entry(job_id.to_string())
            .or_default()
            .clone()
    }

    /// Cancels a job on this host. Returns false if the job is unknown
    /// (never assigned here, or already finished).
    pub fn cancel(&self, job_id: &str) -> bool {
        let Some(slot) = self.jobs.lock().unwrap().get(job_id).cloned() else {
            return false;
        };
        let stop = {
            let mut c = slot.lock().unwrap();
            if c.requested {
                return true;
            }
            c.requested = true;
            c.stop.clone()
        };
        if let Some(stop) = stop {
            self.stop_guest(job_id, stop);
        }
        true
    }

    /// Asks the guest to stop its runner, and unblocks the wait after the
    /// grace period in case it never reports.
    fn stop_guest(&self, job_id: &str, stop: Arc<dyn GuestStop>) {
        if let Err(e) = stop.stop_runner() {
            tracing::warn!(job = %job_id, error = %format!("{e:#}"), "signal guest to stop runner");
            stop.abort_wait();
            return;
        }
        let grace = self.cancel_grace;
        std::thread::spawn(move || {
            std::thread::sleep(grace);
            stop.abort_wait();
        });
    }

    /// Restores paused VMs until every size reaches its target.
    pub fn refill_pool(&self) -> Result<()> {
        for (&vcpu, &target) in &self.warm_target {
            loop {
                let have = self.pool.lock().unwrap().get(&vcpu).map_or(0, Vec::len);
                if have >= target {
                    break;
                }
                let vm = self.hv.restore_paused(vcpu, vcpu * 4)?;
                self.pool.lock().unwrap().entry(vcpu).or_default().push(vm);
            }
        }
        Ok(())
    }

    /// Warm VM counts per size, for heartbeats.
    pub fn warm_counts(&self) -> HashMap<u32, u32> {
        self.pool
            .lock()
            .unwrap()
            .iter()
            .map(|(k, v)| (*k, v.len() as u32))
            .collect()
    }

    fn take_vm(&self, vcpu: u32, memory_gb: u32) -> Result<H::Vm> {
        if let Some(vm) = self.pool.lock().unwrap().get_mut(&vcpu).and_then(Vec::pop) {
            return Ok(vm);
        }
        self.hv.restore_paused(vcpu, memory_gb)
    }

    /// Runs one job to completion and reports what happened. Never panics on
    /// job or infrastructure errors; they become the result's outcome.
    pub fn run_job(&self, job: &pb::AssignJob, events: Events) -> pb::JobResult {
        let mut result = pb::JobResult {
            job_id: job.job_id.clone(),
            base_image: job.base_image.clone(),
            commit: job.commit.clone(),
            ..Default::default()
        };
        let slot = self.slot(&job.job_id);
        let mut disks: Vec<String> = Vec::new(); // volumes to discard
        let outcome = self.run_inner(job, events, &slot, &mut disks, &mut result);
        if let Err(e) = outcome {
            // An error after a cancel (e.g. tearing the VM down) is still
            // reported as a cancel, so it is not retried as an infra error.
            result.set_outcome(if cancel_requested(&slot) {
                pb::job_result::Outcome::Cancelled
            } else {
                pb::job_result::Outcome::InfraError
            });
            result.error = format!("{e:#}");
        }
        self.jobs.lock().unwrap().remove(&job.job_id);
        // Discard whatever wasn't kept. Errors here are logged, not
        // surfaced: the job's result is already decided.
        for d in disks.into_iter().rev() {
            if let Err(e) = self.storage.destroy(&d) {
                tracing::warn!(job = %job.job_id, disk = %d, error = %e, "discard disk");
            }
        }
        result
    }

    fn run_inner(
        &self,
        job: &pb::AssignJob,
        events: Events,
        slot: &CancelSlot,
        disks: &mut Vec<String>,
        result: &mut pb::JobResult,
    ) -> Result<()> {
        let trusted = job.trust() == pb::Trust::Trusted;
        let building = job.prebuild_build.as_ref();
        if building.is_some() && !trusted {
            anyhow::bail!("prebuilds may only be built from trusted refs");
        }

        // Sticky volumes to attach; /var/lib/docker is always among them,
        // never on the overlay.
        let volumes = rootfs::plan_volumes(&job.volumes)?;

        // Job disk: a disk pre-forked for this repo/commit if one is held,
        // else a copy-on-write fork of the prebuild, or an empty disk.
        let job_disk = if let Some(d) = self.take_pre_fork(job, SystemTime::now()) {
            tracing::info!(job = %job.job_id, disk = %d, "using pre-forked disk");
            d
        } else {
            let d = storage::job_disk(&job.job_id);
            if job.prebuild_snapshot_id.is_empty() {
                self.storage.create_empty(&d, EMPTY_DISK_GB)?;
            } else {
                self.storage
                    .clone_snapshot(&job.prebuild_snapshot_id, &d)
                    .context("fork prebuild")?;
            }
            d
        };
        disks.push(job_disk.clone());

        // Sticky volumes: clone-then-promote, so two jobs never write the
        // same volume and an untrusted job can never poison it.
        let mut sticky = Vec::new();
        for v in &volumes {
            let clone = storage::sticky_clone(&job.job_id, &v.key);
            if v.source_snapshot_id.is_empty() {
                self.storage.create_empty(&clone, EMPTY_DISK_GB)?;
            } else {
                self.storage.clone_snapshot(&v.source_snapshot_id, &clone)?;
            }
            disks.push(clone.clone());
            sticky.push((v, clone));
        }

        // Cancelled while forking disks: don't start a VM at all.
        if cancel_requested(slot) {
            result.set_outcome(pb::job_result::Outcome::Cancelled);
            return Ok(());
        }

        let mut vm = self.take_vm(job.vcpu, job.memory_gb)?;
        let run = (|| -> Result<pb::job_result::Outcome> {
            self.hv.set_cpu_weight(&mut vm, job.cpu_weight.max(1))?;
            self.hv
                .set_egress(&mut vm, &EgressPolicy::for_job(job))
                .context("apply egress policy")?;
            self.hv.resume(&mut vm)?;
            events(pb::Phase::VmResumed);

            let mut payload = GuestPayload {
                job_id: job.job_id.clone(),
                root: Some(rootfs::guest_root(&job.base_image)),
                ..Default::default()
            };
            self.hv.attach_disk(
                &mut vm,
                rootfs::UPPER_SERIAL,
                &self.storage.device_path(&job_disk),
            )?;
            payload.disks.push(GuestDisk {
                serial: rootfs::UPPER_SERIAL.into(),
                mount: "/".into(),
            });
            for (v, clone) in &sticky {
                let serial = format!("sticky-{}", storage::sanitize(&v.key));
                self.hv
                    .attach_disk(&mut vm, &serial, &self.storage.device_path(clone))?;
                payload.disks.push(GuestDisk {
                    serial,
                    mount: v.mount.clone(),
                });
            }
            if let Some(g) = &job.guest {
                payload.set_runner_config(&g.runner_config);
                payload.env = g.env.clone().into_iter().collect();
                payload.files = g
                    .files
                    .iter()
                    .map(|(k, v)| (k.clone(), base64_encode(v)))
                    .collect();
            }
            if let Some(b) = building {
                payload.prebuild = Some(GuestPrebuild {
                    warm_commands: b.warm_commands.clone(),
                    keep_paths: b.keep_paths.clone(),
                    scrub_paths: b.scrub_paths.clone(),
                });
            }
            // Don't hand the runner its config if the job is already
            // cancelled.
            if cancel_requested(slot) {
                return Ok(pb::job_result::Outcome::Cancelled);
            }
            self.hv.send_payload(&mut vm, &payload)?;

            // From here a cancel reaches the guest. Registering and checking
            // under one lock means a cancel racing with this is never lost.
            let stop = self.hv.guest_stop(&vm)?;
            let cancelled = {
                let mut c = slot.lock().unwrap();
                c.stop = Some(stop.clone());
                c.requested
            };
            if cancelled {
                self.stop_guest(&job.job_id, stop);
            }

            let waited = self.hv.wait(&mut vm, JOB_DEADLINE);
            // The guest stopped (or was cut off) because of a cancel: the
            // job did not fail on its own.
            if cancel_requested(slot) {
                return Ok(pb::job_result::Outcome::Cancelled);
            }
            let guest = waited?;
            events(pb::Phase::JobFinished);
            if building.is_some() && guest.secret_scan_clean != Some(true) {
                anyhow::bail!("prebuild secret scan failed or did not run");
            }
            Ok(if guest.exit_code == 0 {
                pb::job_result::Outcome::Succeeded
            } else {
                pb::job_result::Outcome::Failed
            })
        })();

        let usage = self.hv.usage(&vm);
        result.avg_busy_vcpu = usage.avg_busy_vcpu;
        result.peak_memory_gb = usage.peak_memory_gb;
        self.hv.destroy(vm).context("destroy VM")?;
        events(pb::Phase::VmDestroyed);

        let outcome = run?;
        result.set_outcome(outcome);
        if outcome != pb::job_result::Outcome::Succeeded {
            return Ok(());
        }

        if let Some(b) = building {
            // Keep the job disk as the repo's new prebuild.
            let snap = self
                .storage
                .snapshot(&job_disk, &storage::sanitize(&b.key))?;
            disks.retain(|d| d != &job_disk);
            let r = pb::SnapshotRef {
                kind: pb::snapshot_ref::Kind::Prebuild as i32,
                repo: job.repo.clone(),
                key: b.key.clone(),
                id: snap,
            };
            crate::recovery::label_snapshot(&self.storage, &r);
            result.prebuild = Some(r);
        }
        if trusted {
            for (v, clone) in &sticky {
                let snap = self
                    .storage
                    .snapshot(clone, &format!("head-{}", storage::sanitize(&job.job_id)))?;
                disks.retain(|d| d != clone);
                let r = pb::SnapshotRef {
                    kind: pb::snapshot_ref::Kind::Sticky as i32,
                    repo: job.repo.clone(),
                    key: v.key.clone(),
                    id: snap,
                };
                crate::recovery::label_snapshot(&self.storage, &r);
                result.promoted.push(r);
            }
        }
        Ok(())
    }
}

fn base64_encode(b: &[u8]) -> String {
    use base64::Engine as _;
    base64::engine::general_purpose::STANDARD.encode(b)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::hypervisor::fake::FakeHypervisor;
    use crate::storage::fake::FakeStorage;
    use pb::job_result::Outcome;

    fn agent(hv: FakeHypervisor, st: FakeStorage) -> Agent<FakeHypervisor, FakeStorage> {
        Agent::new(hv, st, HashMap::from([(4, 2)]))
    }

    fn job(trust: pb::Trust) -> pb::AssignJob {
        let mut j = pb::AssignJob {
            job_id: "gh_1".into(),
            repo: "acme/api".into(),
            commit: "abc".into(),
            vcpu: 4,
            memory_gb: 16,
            base_image: "ubuntu-24.04@2026-11-16".into(),
            prebuild_snapshot_id: "prebuilds/acme_api@k1".into(),
            prebuild_match: "full".into(),
            volumes: vec![pb::Volume {
                key: "docker".into(),
                mount: "/var/lib/docker".into(),
                source_snapshot_id: "sticky/acme_api/docker@head-0".into(),
            }],
            cpu_weight: 250,
            guest: Some(pb::GuestConfig {
                runner_config: b"jit".to_vec(),
                env: [("GOPROXY".to_string(), "http://m".to_string())].into(),
                files: Default::default(),
            }),
            ..Default::default()
        };
        j.set_trust(trust);
        j
    }

    fn seeded() -> FakeStorage {
        let st = FakeStorage::default();
        st.snapshot("prebuilds/acme_api", "k1").unwrap();
        st.snapshot("sticky/acme_api/docker", "head-0").unwrap();
        st
    }

    #[test]
    fn trusted_success_promotes_sticky_and_discards_job_disk() {
        let a = agent(FakeHypervisor::default(), seeded());
        let seen = Mutex::new(Vec::new());
        let ev = |p| seen.lock().unwrap().push(p);
        let r = a.run_job(&job(pb::Trust::Trusted), &ev);
        assert_eq!(r.outcome(), Outcome::Succeeded, "{}", r.error);
        assert_eq!(r.promoted.len(), 1);
        assert_eq!(r.promoted[0].id, "jobs/gh_1-docker@head-gh_1");
        assert!(a.storage.has("jobs/gh_1-docker"), "promoted clone kept");
        assert_eq!(
            a.storage.labels.lock().unwrap()["jobs/gh_1-docker@head-gh_1"]["kind"],
            "sticky"
        );
        assert!(!a.storage.has("jobs/gh_1"), "job disk discarded");
        assert_eq!(r.avg_busy_vcpu, 1.5);
        assert_eq!(
            *seen.lock().unwrap(),
            vec![
                pb::Phase::VmResumed,
                pb::Phase::JobFinished,
                pb::Phase::VmDestroyed
            ]
        );
        let p = &a.hv.payloads.lock().unwrap()[0];
        assert_eq!(p.disks.len(), 2);
        assert_eq!(p.env["GOPROXY"], "http://m");
        assert_eq!(a.hv.destroyed.lock().unwrap().len(), 1);
    }

    #[test]
    fn untrusted_never_promotes() {
        for t in [pb::Trust::Untrusted, pb::Trust::UntrustedFork] {
            let a = agent(FakeHypervisor::default(), seeded());
            let r = a.run_job(&job(t), &|_| {});
            assert_eq!(r.outcome(), Outcome::Succeeded);
            assert!(r.promoted.is_empty());
            assert!(!a.storage.has("jobs/gh_1-docker"));
            assert!(!a.storage.has("jobs/gh_1"));
        }
    }

    #[test]
    fn failed_job_discards_everything() {
        let a = agent(
            FakeHypervisor {
                exit_code: 1,
                ..Default::default()
            },
            seeded(),
        );
        let r = a.run_job(&job(pb::Trust::Trusted), &|_| {});
        assert_eq!(r.outcome(), Outcome::Failed);
        assert!(r.promoted.is_empty());
        assert!(!a.storage.has("jobs/gh_1-docker"));
    }

    #[test]
    fn vm_death_is_infra_error_and_still_destroys() {
        let a = agent(
            FakeHypervisor {
                fail_wait: true,
                ..Default::default()
            },
            seeded(),
        );
        let r = a.run_job(&job(pb::Trust::Trusted), &|_| {});
        assert_eq!(r.outcome(), Outcome::InfraError);
        assert!(r.error.contains("VM died"));
        assert_eq!(a.hv.destroyed.lock().unwrap().len(), 1);
        assert!(!a.storage.has("jobs/gh_1"));
    }

    #[test]
    fn clone_failure_is_infra_error_without_vm() {
        let st = FakeStorage {
            fail_clone: true,
            ..seeded()
        };
        let a = agent(FakeHypervisor::default(), st);
        let r = a.run_job(&job(pb::Trust::Trusted), &|_| {});
        assert_eq!(r.outcome(), Outcome::InfraError);
        assert_eq!(a.hv.restored.load(std::sync::atomic::Ordering::Relaxed), 0);
    }

    #[test]
    fn no_prebuild_uses_empty_disk() {
        let a = agent(FakeHypervisor::default(), seeded());
        let mut j = job(pb::Trust::Untrusted);
        j.prebuild_snapshot_id.clear();
        let r = a.run_job(&j, &|_| {});
        assert_eq!(r.outcome(), Outcome::Succeeded);
        assert!(a
            .storage
            .log
            .lock()
            .unwrap()
            .iter()
            .any(|l| l == "create jobs/gh_1"));
    }

    #[test]
    fn prebuild_build_keeps_disk_as_snapshot() {
        let a = agent(FakeHypervisor::default(), seeded());
        let mut j = job(pb::Trust::Trusted);
        j.prebuild_snapshot_id.clear();
        j.prebuild_build = Some(pb::PrebuildBuild {
            key: "pb1-abc".into(),
            ..Default::default()
        });
        let r = a.run_job(&j, &|_| {});
        assert_eq!(r.outcome(), Outcome::Succeeded, "{}", r.error);
        let pb = r.prebuild.unwrap();
        assert_eq!(pb.id, "jobs/gh_1@pb1-abc");
        assert_eq!(pb.repo, "acme/api");
        assert_eq!(r.base_image, "ubuntu-24.04@2026-11-16");
        assert!(a.storage.has("jobs/gh_1"));
        assert_eq!(
            a.storage.labels.lock().unwrap()["jobs/gh_1@pb1-abc"]["repo"],
            "acme/api",
            "kept snapshots are labeled for restart recovery"
        );
    }

    #[test]
    fn untrusted_prebuild_build_refused() {
        let a = agent(FakeHypervisor::default(), seeded());
        let mut j = job(pb::Trust::Untrusted);
        j.prebuild_build = Some(pb::PrebuildBuild {
            key: "k".into(),
            ..Default::default()
        });
        let r = a.run_job(&j, &|_| {});
        assert_eq!(r.outcome(), Outcome::InfraError);
        assert_eq!(a.hv.restored.load(std::sync::atomic::Ordering::Relaxed), 0);
    }

    /// Runs the job on another thread, waits until the guest is running it,
    /// cancels it, and returns the result.
    fn cancel_while_running(
        a: Arc<Agent<FakeHypervisor, FakeStorage>>,
        j: pb::AssignJob,
    ) -> (pb::JobResult, Vec<pb::Phase>) {
        let runner = {
            let a = a.clone();
            std::thread::spawn(move || {
                let seen = Mutex::new(Vec::new());
                let r = a.run_job(&j, &|p| seen.lock().unwrap().push(p));
                (r, seen.into_inner().unwrap())
            })
        };
        let start = std::time::Instant::now();
        while !a.hv.guest.waiting() {
            assert!(start.elapsed() < Duration::from_secs(5), "job never ran");
            std::thread::sleep(Duration::from_millis(1));
        }
        assert!(a.cancel("gh_1"));
        runner.join().unwrap()
    }

    #[test]
    fn cancel_stops_runner_then_destroys_vm() {
        let hv = FakeHypervisor {
            block_wait: true,
            ..Default::default()
        };
        let a = Arc::new(agent(hv, seeded()));
        let (r, phases) = cancel_while_running(a.clone(), job(pb::Trust::Trusted));
        assert_eq!(r.outcome(), Outcome::Cancelled, "{}", r.error);
        assert_eq!(a.hv.guest.stop_requests(), 1);
        assert!(
            !a.hv.guest.aborted(),
            "guest stopped within the grace period"
        );
        assert_eq!(a.hv.destroyed.lock().unwrap().len(), 1);
        assert_eq!(phases, vec![pb::Phase::VmResumed, pb::Phase::VmDestroyed]);
        assert!(r.promoted.is_empty(), "cancelled runs never promote");
        assert!(!a.storage.has("jobs/gh_1"));
        assert!(!a.storage.has("jobs/gh_1-docker"));
        assert!(!a.cancel("gh_1"), "finished jobs are forgotten");
    }

    #[test]
    fn cancel_tears_down_guest_that_ignores_stop() {
        let hv = FakeHypervisor {
            block_wait: true,
            ignore_stop: true,
            ..Default::default()
        };
        let mut a = agent(hv, seeded());
        a.cancel_grace = Duration::from_millis(10);
        let a = Arc::new(a);
        let (r, _) = cancel_while_running(a.clone(), job(pb::Trust::Trusted));
        assert_eq!(r.outcome(), Outcome::Cancelled, "{}", r.error);
        assert_eq!(a.hv.guest.stop_requests(), 1);
        assert!(a.hv.guest.aborted());
        assert_eq!(a.hv.destroyed.lock().unwrap().len(), 1);
    }

    #[test]
    fn cancel_before_start_never_starts_a_vm() {
        let a = agent(FakeHypervisor::default(), seeded());
        a.admit("gh_1");
        assert!(a.cancel("gh_1"));
        let r = a.run_job(&job(pb::Trust::Trusted), &|_| {});
        assert_eq!(r.outcome(), Outcome::Cancelled);
        assert_eq!(a.hv.restored.load(std::sync::atomic::Ordering::Relaxed), 0);
        assert!(a.hv.payloads.lock().unwrap().is_empty());
        assert!(!a.storage.has("jobs/gh_1"), "forked disks discarded");
        assert!(!a.storage.has("jobs/gh_1-docker"));
    }

    #[test]
    fn cancel_unknown_job_is_rejected() {
        let a = agent(FakeHypervisor::default(), seeded());
        assert!(!a.cancel("gh_1"));
        // And it does not poison a later job with that id.
        let r = a.run_job(&job(pb::Trust::Untrusted), &|_| {});
        assert_eq!(r.outcome(), Outcome::Succeeded);
    }

    #[test]
    fn payload_describes_dax_overlay_root() {
        let a = agent(FakeHypervisor::default(), seeded());
        let r = a.run_job(&job(pb::Trust::Untrusted), &|_| {});
        assert_eq!(r.outcome(), Outcome::Succeeded, "{}", r.error);
        let p = &a.hv.payloads.lock().unwrap()[0];
        let root = p.root.as_ref().expect("root");
        assert_eq!(root.base_image, "ubuntu-24.04@2026-11-16");
        assert_eq!(root.lower_device, "/dev/pmem0");
        assert!(root.lower_options.iter().any(|o| o == "dax"));
        assert!(root.lower_options.iter().any(|o| o == "ro"));
        // The upper layer is the job disk, attached under the same serial.
        assert_eq!(root.upper_serial, "job");
        assert!(p.disks.iter().any(|d| d.serial == "job" && d.mount == "/"));
    }

    #[test]
    fn docker_volume_added_when_missing() {
        let a = agent(FakeHypervisor::default(), seeded());
        let mut j = job(pb::Trust::Trusted);
        j.volumes.clear();
        let r = a.run_job(&j, &|_| {});
        assert_eq!(r.outcome(), Outcome::Succeeded, "{}", r.error);
        assert!(a
            .storage
            .log
            .lock()
            .unwrap()
            .iter()
            .any(|l| l == "create jobs/gh_1-docker"));
        let p = &a.hv.payloads.lock().unwrap()[0];
        assert!(p
            .disks
            .iter()
            .any(|d| d.serial == "sticky-docker" && d.mount == "/var/lib/docker"));
        // Trusted and successful: the new docker volume is promoted.
        assert_eq!(r.promoted.len(), 1);
        assert_eq!(r.promoted[0].key, "docker");
    }

    #[test]
    fn volume_over_root_refused_before_any_disk() {
        let a = agent(FakeHypervisor::default(), seeded());
        let mut j = job(pb::Trust::Trusted);
        j.volumes.push(pb::Volume {
            key: "evil".into(),
            mount: "/".into(),
            source_snapshot_id: String::new(),
        });
        let r = a.run_job(&j, &|_| {});
        assert_eq!(r.outcome(), Outcome::InfraError);
        assert!(r.error.contains("overlay root"), "{}", r.error);
        assert_eq!(a.hv.restored.load(std::sync::atomic::Ordering::Relaxed), 0);
        assert!(a
            .storage
            .log
            .lock()
            .unwrap()
            .iter()
            .all(|l| !l.starts_with("clone") && !l.starts_with("create")));
    }

    #[test]
    fn egress_policy_set_before_resume() {
        let a = agent(FakeHypervisor::default(), seeded());
        a.run_job(&job(pb::Trust::Untrusted), &|_| {});
        let mut j = job(pb::Trust::Trusted);
        j.prebuild_build = Some(pb::PrebuildBuild {
            key: "k".into(),
            egress_allowlist: vec!["github.com:443".into()],
            ..Default::default()
        });
        a.run_job(&j, &|_| {});
        let got = a.hv.egress.lock().unwrap().clone();
        assert_eq!(
            got,
            vec![
                (EgressPolicy::Open, false),
                (
                    EgressPolicy::Restricted {
                        allow: vec!["github.com:443".into()]
                    },
                    false
                ),
            ]
        );
    }

    #[test]
    fn pool_refills_and_is_used() {
        let a = agent(FakeHypervisor::default(), seeded());
        a.refill_pool().unwrap();
        assert_eq!(a.warm_counts()[&4], 2);
        a.run_job(&job(pb::Trust::Untrusted), &|_| {});
        assert_eq!(a.warm_counts()[&4], 1);
        assert_eq!(a.hv.restored.load(std::sync::atomic::Ordering::Relaxed), 2);
    }
}
