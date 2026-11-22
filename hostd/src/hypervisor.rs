//! MicroVM control. Each job runs in its own Cloud Hypervisor VM, restored
//! from a paused base snapshot (no kernel boot) and destroyed afterwards.
//! Cloud Hypervisor is chosen over Firecracker for virtio-pmem (sharing the
//! base image through the host page cache), snapshot/restore and VFIO.

use anyhow::{anyhow, bail, Context, Result};
use base64::Engine as _;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use std::fs;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::Shutdown;
use std::os::unix::net::UnixStream;
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use crate::baseimage::SnapshotSource;
use crate::jailer::{slot_ids, JailSpec};
use crate::network::{EgressPolicy, HostNetwork, VmLink};
use crate::rootfs::{self, GuestRoot};

/// What the guest agent receives over vsock before the runner starts.
/// Debug and Display both omit the runner config.
#[derive(Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct GuestPayload {
    pub job_id: String,
    /// Single-use runner registration, base64. Secret: never logged, never
    /// written to host disk.
    pub runner_config_b64: String,
    pub env: BTreeMap<String, String>,
    /// File path -> base64 contents, written before the runner starts.
    pub files: BTreeMap<String, String>,
    /// Hot-plugged disks the guest mounts, by virtio serial.
    pub disks: Vec<GuestDisk>,
    /// Set for prebuild builds: run these, keep paths, scrub credentials,
    /// then shut down without starting a runner.
    pub prebuild: Option<GuestPrebuild>,
    /// Overlay root: base image (virtio-pmem, DAX) under the job disk.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub root: Option<GuestRoot>,
}

impl std::fmt::Display for GuestPayload {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Deliberately omits the runner config.
        write!(
            f,
            "GuestPayload{{job_id: {}, disks: {}}}",
            self.job_id,
            self.disks.len()
        )
    }
}

impl std::fmt::Debug for GuestPayload {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        std::fmt::Display::fmt(self, f)
    }
}

impl GuestPayload {
    pub fn set_runner_config(&mut self, raw: &[u8]) {
        self.runner_config_b64 = base64::engine::general_purpose::STANDARD.encode(raw);
    }
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct GuestDisk {
    pub serial: String,
    /// "/" for the overlay upper layer, else a mount point.
    pub mount: String,
}

#[derive(Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct GuestPrebuild {
    pub warm_commands: Vec<String>,
    pub keep_paths: Vec<String>,
    pub scrub_paths: Vec<String>,
    /// Run in the workspace before the warm steps, with the clone token in
    /// `$TACHYON_CLONE_TOKEN` for these commands only.
    #[serde(default)]
    pub clone_commands: Vec<String>,
    /// Short-lived read-only clone token, base64. Secret: never logged,
    /// never written to host disk, never in the warm steps' environment.
    #[serde(default)]
    pub clone_token_b64: String,
    /// Key unchanged: the disk is a fork of the previous prebuild and only
    /// new git objects are fetched.
    #[serde(default)]
    pub refresh: bool,
}

impl std::fmt::Debug for GuestPrebuild {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Deliberately omits the clone token.
        f.debug_struct("GuestPrebuild")
            .field("warm_commands", &self.warm_commands)
            .field("keep_paths", &self.keep_paths)
            .field("scrub_paths", &self.scrub_paths)
            .field("clone_commands", &self.clone_commands.len())
            .field("refresh", &self.refresh)
            .finish_non_exhaustive()
    }
}

/// The guest agent's final report, sent before it powers the VM off.
#[derive(Debug, Clone, Default, Serialize, Deserialize, PartialEq)]
pub struct GuestResult {
    pub exit_code: i32,
    /// Phase timestamps the guest saw (runner_online, job_started), RFC 3339.
    #[serde(default)]
    pub phases: BTreeMap<String, String>,
    /// For prebuild builds: did the secret scan pass?
    #[serde(default)]
    pub secret_scan_clean: Option<bool>,
}

/// Control messages hostd sends the guest agent after the payload, on the
/// same vsock connection and with the same framing (u32 big-endian length,
/// then JSON).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum GuestControl {
    /// Stop the runner (the job was cancelled). The guest agent stops it,
    /// then sends its GuestResult as usual.
    StopRunner,
}

/// Reaches a running guest from another thread while `Hypervisor::wait`
/// blocks on it. Used to cancel jobs.
pub trait GuestStop: Send + Sync {
    /// Asks the guest agent to stop the runner.
    fn stop_runner(&self) -> Result<()>;
    /// Makes a blocked `wait` return an error, for when the guest does not
    /// report in time after `stop_runner`.
    fn abort_wait(&self);
}

/// Measured resource use, read from the VM's cgroup.
#[derive(Debug, Clone, Copy, Default, PartialEq)]
pub struct Usage {
    pub avg_busy_vcpu: f64,
    pub peak_memory_gb: f64,
}

pub trait Hypervisor: Send + Sync {
    type Vm: Send;

    /// Checks this host can run jobs on base image `image` (name@version)
    /// and returns the canonical key warm VMs are pooled under. Fails if
    /// the image is not prepared here; a job must never run on another
    /// version.
    fn check_image(&self, image: &str) -> Result<String>;
    /// Canonical keys of the images to keep warm VMs for.
    fn warm_images(&self) -> Vec<String>;
    /// Restores a paused VM of the given size from the base snapshot of
    /// `image`, a key returned by `check_image`.
    fn restore_paused(&self, image: &str, vcpu: u32, memory_gb: u32) -> Result<Self::Vm>;
    /// Applies the job's CPU weight to a VM taken from the warm pool.
    fn set_cpu_weight(&self, vm: &mut Self::Vm, weight: u32) -> Result<()>;
    /// Replaces the VM's egress policy. Called before resume.
    fn set_egress(&self, vm: &mut Self::Vm, policy: &EgressPolicy) -> Result<()>;
    fn resume(&self, vm: &mut Self::Vm) -> Result<()>;
    /// Hot-plugs a block device.
    fn attach_disk(&self, vm: &mut Self::Vm, serial: &str, device: &str) -> Result<()>;
    fn send_payload(&self, vm: &mut Self::Vm, payload: &GuestPayload) -> Result<()>;
    /// Blocks until the guest reports its result (or the deadline passes).
    fn wait(&self, vm: &mut Self::Vm, deadline: Duration) -> Result<GuestResult>;
    /// A handle for stopping the guest while `wait` blocks. Valid after
    /// `send_payload`.
    fn guest_stop(&self, vm: &Self::Vm) -> Result<Arc<dyn GuestStop>>;
    fn usage(&self, vm: &Self::Vm) -> Usage;
    fn destroy(&self, vm: Self::Vm) -> Result<()>;
}

/// Fixed in-jail directory every VM sees (see JailSpec::bind).
pub const IN_JAIL_DIR: &str = "/run/tachyon/vm";
/// vsock port the guest agent listens on.
pub const GUEST_AGENT_PORT: u32 = 1024;

pub struct CloudHypervisor {
    pub binary: PathBuf,
    pub ch_remote: PathBuf,
    /// Per-VM working directories are created here.
    pub run_dir: PathBuf,
    /// Where base snapshots come from: per image version and VM size, each a
    /// directory with config.json, state.json and memory-ranges.
    pub snapshots: SnapshotSource,
    pub cgroup_parent: PathBuf,
    pub base_uid: u32,
    pub next_slot: AtomicU32,
    /// Per-VM network namespaces, taps and egress rules.
    pub net: HostNetwork,
}

pub struct ChVm {
    pub id: String,
    dir: PathBuf,
    child: Child,
    jail: JailSpec,
    link: VmLink,
    agent: Option<UnixStream>,
    started: Instant,
}

impl CloudHypervisor {
    fn api_socket(&self, vm: &ChVm) -> PathBuf {
        vm.dir.join("api.sock")
    }

    fn ch_remote(&self, vm: &ChVm, args: &[&str]) -> Result<()> {
        let out = Command::new(&self.ch_remote)
            .arg("--api-socket")
            .arg(self.api_socket(vm))
            .args(args)
            .output()
            .context("spawn ch-remote")?;
        if !out.status.success() {
            bail!(
                "ch-remote {}: {}",
                args.join(" "),
                String::from_utf8_lossy(&out.stderr).trim()
            );
        }
        Ok(())
    }
}

impl Hypervisor for CloudHypervisor {
    type Vm = ChVm;

    fn check_image(&self, image: &str) -> Result<String> {
        self.snapshots.check(image)
    }

    fn warm_images(&self) -> Vec<String> {
        self.snapshots.warm_images()
    }

    fn restore_paused(&self, image: &str, vcpu: u32, memory_gb: u32) -> Result<ChVm> {
        let snapshot = self.snapshots.dir(image, vcpu)?;
        // The base image must be shared read-only through virtio-pmem.
        rootfs::check_snapshot(&snapshot).context("base snapshot")?;
        let slot = self.next_slot.fetch_add(1, Ordering::Relaxed);
        let id = format!("vm{slot}");
        let dir = self.run_dir.join(&id);
        fs::create_dir_all(&dir)?;
        let (uid, gid) = slot_ids(self.base_uid, slot % 10_000);
        std::os::unix::fs::chown(&dir, Some(uid), Some(gid))?;
        // The tap must exist, owned by the VM's user, before the restored
        // VM opens it; egress stays denied until a job's policy is set.
        let link = self.net.setup(slot, uid, gid)?;
        let jail = JailSpec {
            job_id: id.clone(),
            uid,
            gid,
            cgroup_parent: self.cgroup_parent.clone(),
            cpu_weight: 100 * vcpu,
            // VM RAM plus 512 MiB for the VMM.
            memory_max_bytes: (u64::from(memory_gb) << 30) + (512 << 20),
            bind: Some((dir.clone(), PathBuf::from(IN_JAIL_DIR))),
            netns: Some(link.netns_path()),
        };
        if let Err(e) = jail.create_cgroup() {
            let _ = self.net.teardown(&link);
            return Err(e);
        }
        let mut cmd = Command::new(&self.binary);
        cmd.arg("--api-socket")
            .arg(format!("path={IN_JAIL_DIR}/api.sock"))
            .arg("--restore")
            .arg(format!("source_url=file://{}", snapshot.display()))
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::piped());
        let child = match jail
            .apply(&mut cmd)
            .and_then(|()| cmd.spawn().context("spawn cloud-hypervisor"))
        {
            Ok(c) => c,
            Err(e) => {
                let _ = jail.remove_cgroup();
                let _ = self.net.teardown(&link);
                return Err(e);
            }
        };
        // Restored VMs stay paused until resume.
        let vm = ChVm {
            id,
            dir,
            child,
            jail,
            link,
            agent: None,
            started: Instant::now(),
        };
        wait_for_socket(&self.api_socket(&vm), Duration::from_secs(5))?;
        Ok(vm)
    }

    fn set_cpu_weight(&self, vm: &mut ChVm, weight: u32) -> Result<()> {
        vm.jail.cpu_weight = weight;
        fs::write(
            vm.jail.cgroup_dir().join("cpu.weight"),
            weight.clamp(1, 10_000).to_string(),
        )?;
        Ok(())
    }

    fn set_egress(&self, vm: &mut ChVm, policy: &EgressPolicy) -> Result<()> {
        self.net.apply(&vm.link, policy)
    }

    fn resume(&self, vm: &mut ChVm) -> Result<()> {
        vm.started = Instant::now();
        self.ch_remote(vm, &["resume"])
    }

    fn attach_disk(&self, vm: &mut ChVm, serial: &str, device: &str) -> Result<()> {
        // The device node must be reachable by the jailed VMM user; hostd
        // links it into the VM dir and hands ownership to that user.
        let link = vm.dir.join(format!("disk-{serial}"));
        let _ = fs::remove_file(&link);
        std::os::unix::fs::symlink(device, &link)?;
        std::os::unix::fs::chown(device, Some(vm.jail.uid), Some(vm.jail.gid))?;
        self.ch_remote(
            vm,
            &[
                "add-disk",
                &format!("path={IN_JAIL_DIR}/disk-{serial},serial={serial},id={serial}"),
            ],
        )
    }

    fn send_payload(&self, vm: &mut ChVm, payload: &GuestPayload) -> Result<()> {
        let mut s = UnixStream::connect(vm.dir.join("vsock.sock")).context("connect vsock")?;
        s.set_read_timeout(Some(Duration::from_secs(10)))?;
        // Cloud Hypervisor's host-initiated vsock handshake.
        writeln!(s, "CONNECT {GUEST_AGENT_PORT}")?;
        let mut line = String::new();
        BufReader::new(&mut s).read_line(&mut line)?;
        if !line.starts_with("OK ") {
            bail!("vsock handshake: {line:?}");
        }
        let body = serde_json::to_vec(payload)?;
        s.write_all(&(body.len() as u32).to_be_bytes())?;
        s.write_all(&body)?;
        vm.agent = Some(s);
        Ok(())
    }

    fn wait(&self, vm: &mut ChVm, deadline: Duration) -> Result<GuestResult> {
        let s = vm
            .agent
            .as_mut()
            .ok_or_else(|| anyhow!("no guest agent connection"))?;
        s.set_read_timeout(Some(deadline))?;
        let mut len = [0u8; 4];
        s.read_exact(&mut len)
            .context("guest result (VM died or deadline passed)")?;
        let n = u32::from_be_bytes(len) as usize;
        if n > 1 << 20 {
            bail!("guest result too large: {n} bytes");
        }
        let mut body = vec![0u8; n];
        s.read_exact(&mut body)?;
        Ok(serde_json::from_slice(&body)?)
    }

    fn guest_stop(&self, vm: &ChVm) -> Result<Arc<dyn GuestStop>> {
        let s = vm
            .agent
            .as_ref()
            .ok_or_else(|| anyhow!("no guest agent connection"))?;
        Ok(Arc::new(VsockGuestStop(s.try_clone()?)))
    }

    fn usage(&self, vm: &ChVm) -> Usage {
        read_cgroup_usage(&vm.jail.cgroup_dir(), vm.started.elapsed())
    }

    fn destroy(&self, mut vm: ChVm) -> Result<()> {
        let _ = self.ch_remote(&vm, &["shutdown-vmm"]);
        let _ = vm.child.kill();
        let _ = vm.child.wait();
        let net = self.net.teardown(&vm.link);
        vm.jail.remove_cgroup()?;
        fs::remove_dir_all(&vm.dir).ok();
        net
    }
}

/// GuestStop over a clone of the guest agent's vsock connection.
struct VsockGuestStop(UnixStream);

impl GuestStop for VsockGuestStop {
    fn stop_runner(&self) -> Result<()> {
        let body = serde_json::to_vec(&GuestControl::StopRunner)?;
        let mut s = &self.0;
        s.set_write_timeout(Some(Duration::from_secs(5)))?;
        s.write_all(&(body.len() as u32).to_be_bytes())?;
        s.write_all(&body)?;
        Ok(())
    }

    fn abort_wait(&self) {
        // Shutting the socket down wakes the reader blocked in `wait`.
        let _ = self.0.shutdown(Shutdown::Both);
    }
}

fn wait_for_socket(path: &std::path::Path, timeout: Duration) -> Result<()> {
    let start = Instant::now();
    while !path.exists() {
        if start.elapsed() > timeout {
            bail!("{} did not appear", path.display());
        }
        std::thread::sleep(Duration::from_millis(5));
    }
    Ok(())
}

/// Average busy vCPUs (cpu.stat usage_usec over wall time) and peak memory.
pub fn read_cgroup_usage(dir: &std::path::Path, wall: Duration) -> Usage {
    let mut u = Usage::default();
    if let Ok(stat) = fs::read_to_string(dir.join("cpu.stat")) {
        for line in stat.lines() {
            if let Some(v) = line.strip_prefix("usage_usec ") {
                if let (Ok(us), true) = (v.trim().parse::<f64>(), wall.as_secs_f64() > 0.0) {
                    u.avg_busy_vcpu = us / 1e6 / wall.as_secs_f64();
                }
            }
        }
    }
    if let Ok(peak) = fs::read_to_string(dir.join("memory.peak")) {
        if let Ok(b) = peak.trim().parse::<f64>() {
            u.peak_memory_gb = b / f64::from(1u32 << 30);
        }
    }
    u
}

#[cfg(test)]
pub mod fake {
    use super::*;
    use std::sync::{Condvar, Mutex};

    /// The fake guest's state, shared with its GuestStop handle.
    #[derive(Default)]
    pub struct FakeGuest {
        state: Mutex<FakeGuestState>,
        cv: Condvar,
    }

    #[derive(Default)]
    struct FakeGuestState {
        waiting: bool,
        stop_requests: u32,
        stopped: bool,
        aborted: bool,
    }

    impl FakeGuest {
        pub fn waiting(&self) -> bool {
            self.state.lock().unwrap().waiting
        }
        pub fn stop_requests(&self) -> u32 {
            self.state.lock().unwrap().stop_requests
        }
        pub fn aborted(&self) -> bool {
            self.state.lock().unwrap().aborted
        }
    }

    struct FakeGuestStop {
        guest: Arc<FakeGuest>,
        ignore_stop: bool,
    }

    impl GuestStop for FakeGuestStop {
        fn stop_runner(&self) -> Result<()> {
            let mut st = self.guest.state.lock().unwrap();
            st.stop_requests += 1;
            if !self.ignore_stop {
                st.stopped = true;
            }
            self.guest.cv.notify_all();
            Ok(())
        }
        fn abort_wait(&self) {
            self.guest.state.lock().unwrap().aborted = true;
            self.guest.cv.notify_all();
        }
    }

    pub struct FakeVm {
        pub id: String,
        pub image: String,
        pub vcpu: u32,
        pub disks: Vec<(String, String)>,
        pub payload: Option<GuestPayload>,
        pub resumed: bool,
        pub egress: Option<EgressPolicy>,
    }

    #[derive(Default)]
    pub struct FakeHypervisor {
        pub exit_code: i32,
        pub fail_wait: bool,
        pub restored: AtomicU32,
        /// (image, vcpu) of every restore, in order.
        pub restored_from: Mutex<Vec<(String, u32)>>,
        /// Images this host has prepared; None accepts any image.
        pub prepared: Mutex<Option<Vec<String>>>,
        /// Images `warm_images` reports.
        pub warm: Mutex<Vec<String>>,
        pub destroyed: Mutex<Vec<String>>,
        pub payloads: Mutex<Vec<GuestPayload>>,
        /// When set, `wait` blocks until the guest is stopped or aborted,
        /// like a long-running job.
        pub block_wait: bool,
        /// When set, the guest ignores stop requests.
        pub ignore_stop: bool,
        pub guest: Arc<FakeGuest>,
        /// Egress policies set, with whether the VM was already resumed.
        pub egress: Mutex<Vec<(EgressPolicy, bool)>>,
    }

    impl Hypervisor for FakeHypervisor {
        type Vm = FakeVm;
        fn check_image(&self, image: &str) -> Result<String> {
            match &*self.prepared.lock().unwrap() {
                Some(p) if !p.iter().any(|x| x == image) => {
                    bail!("base image {image} is not prepared on this host")
                }
                _ => Ok(image.to_string()),
            }
        }
        fn warm_images(&self) -> Vec<String> {
            self.warm.lock().unwrap().clone()
        }
        fn restore_paused(&self, image: &str, vcpu: u32, _memory_gb: u32) -> Result<FakeVm> {
            let n = self.restored.fetch_add(1, Ordering::Relaxed);
            self.restored_from
                .lock()
                .unwrap()
                .push((image.to_string(), vcpu));
            Ok(FakeVm {
                id: format!("vm{n}"),
                image: image.to_string(),
                vcpu,
                disks: vec![],
                payload: None,
                resumed: false,
                egress: None,
            })
        }
        fn set_cpu_weight(&self, _vm: &mut FakeVm, _w: u32) -> Result<()> {
            Ok(())
        }
        fn set_egress(&self, vm: &mut FakeVm, p: &EgressPolicy) -> Result<()> {
            vm.egress = Some(p.clone());
            self.egress.lock().unwrap().push((p.clone(), vm.resumed));
            Ok(())
        }
        fn resume(&self, vm: &mut FakeVm) -> Result<()> {
            vm.resumed = true;
            Ok(())
        }
        fn attach_disk(&self, vm: &mut FakeVm, serial: &str, device: &str) -> Result<()> {
            vm.disks.push((serial.into(), device.into()));
            Ok(())
        }
        fn send_payload(&self, vm: &mut FakeVm, p: &GuestPayload) -> Result<()> {
            vm.payload = Some(p.clone());
            self.payloads.lock().unwrap().push(p.clone());
            Ok(())
        }
        fn wait(&self, _vm: &mut FakeVm, _d: Duration) -> Result<GuestResult> {
            if self.block_wait {
                let mut st = self.guest.state.lock().unwrap();
                st.waiting = true;
                self.guest.cv.notify_all();
                while !st.stopped && !st.aborted {
                    st = self.guest.cv.wait(st).unwrap();
                }
                st.waiting = false;
                if !st.stopped {
                    bail!("guest result (VM died or deadline passed)");
                }
                // The runner was stopped: SIGTERM's exit status.
                return Ok(GuestResult {
                    exit_code: 143,
                    ..Default::default()
                });
            }
            if self.fail_wait {
                bail!("VM died");
            }
            Ok(GuestResult {
                exit_code: self.exit_code,
                secret_scan_clean: Some(true),
                ..Default::default()
            })
        }
        fn guest_stop(&self, _vm: &FakeVm) -> Result<Arc<dyn GuestStop>> {
            Ok(Arc::new(FakeGuestStop {
                guest: self.guest.clone(),
                ignore_stop: self.ignore_stop,
            }))
        }
        fn usage(&self, _vm: &FakeVm) -> Usage {
            Usage {
                avg_busy_vcpu: 1.5,
                peak_memory_gb: 3.0,
            }
        }
        fn destroy(&self, vm: FakeVm) -> Result<()> {
            self.destroyed.lock().unwrap().push(vm.id);
            Ok(())
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn payload_display_hides_secret() {
        let mut p = GuestPayload {
            job_id: "j1".into(),
            ..Default::default()
        };
        p.set_runner_config(b"super-secret");
        assert!(!format!("{p}").contains(&p.runner_config_b64));
        assert!(!format!("{p}").contains("super-secret"));
        assert!(!format!("{p:?}").contains(&p.runner_config_b64));
    }

    // Golden files shared with the guest agent (internal/guest), so a field
    // renamed on either side fails a test.
    #[test]
    fn payload_matches_guest_wire_format() {
        let mut p = GuestPayload {
            job_id: "job-1".into(),
            env: [
                ("TACHYON_JOB_ID", "job-1"),
                ("TACHYON_PREBUILD_MATCH", "full"),
                ("TACHYON_WARM_PATHS", "node_modules:vendor/cache"),
            ]
            .into_iter()
            .map(|(k, v)| (k.into(), v.into()))
            .collect(),
            files: [
                (
                    "/etc/apt/apt.conf.d/01tachyon-proxy",
                    "QWNxdWlyZTo6aHR0cDo6UHJveHkgImh0dHA6Ly9taXJyb3I6MzE0MiI7Cg==",
                ),
                ("~/.m2/settings.xml", "PHNldHRpbmdzLz4K"),
            ]
            .into_iter()
            .map(|(k, v)| (k.into(), v.into()))
            .collect(),
            disks: vec![
                GuestDisk {
                    serial: "job".into(),
                    mount: "/".into(),
                },
                GuestDisk {
                    serial: "sticky-docker".into(),
                    mount: "/var/lib/docker".into(),
                },
            ],
            prebuild: Some(GuestPrebuild {
                warm_commands: vec!["npm ci".into(), "go mod download".into()],
                keep_paths: vec!["node_modules".into(), "~/.npm".into()],
                scrub_paths: vec![".npmrc".into(), ".docker/config.json".into()],
                clone_commands: vec![
                    "git init -q".into(),
                    "git fetch --depth 1 origin abc".into(),
                ],
                clone_token_b64: "ZmFrZS1jbG9uZS10b2tlbg==".into(),
                refresh: true,
            }),
            ..Default::default()
        };
        p.set_runner_config(br#"{"jit":"not-a-real-config"}"#);
        let golden: serde_json::Value =
            serde_json::from_str(include_str!("../../internal/guest/testdata/payload.json"))
                .unwrap();
        assert_eq!(serde_json::to_value(&p).unwrap(), golden);
    }

    #[test]
    fn result_matches_guest_wire_format() {
        let r: GuestResult =
            serde_json::from_str(include_str!("../../internal/guest/testdata/result.json"))
                .unwrap();
        assert_eq!(r.exit_code, 0);
        assert_eq!(r.phases["runner_online"], "2026-11-21T10:00:00Z");
        assert_eq!(r.phases["job_started"], "2026-11-21T10:00:01Z");
        assert_eq!(r.secret_scan_clean, Some(true));
        // A job's result omits the scan.
        let r: GuestResult = serde_json::from_str(r#"{"exit_code":3,"phases":{}}"#).unwrap();
        assert_eq!(r.secret_scan_clean, None);
    }

    #[test]
    fn guest_control_wire_format() {
        let j = serde_json::to_string(&GuestControl::StopRunner).unwrap();
        assert_eq!(j, r#"{"type":"stop_runner"}"#);
    }

    #[test]
    fn vsock_stop_sends_frame_and_abort_unblocks_wait() {
        let (host, mut guest) = UnixStream::pair().unwrap();
        let stop = VsockGuestStop(host.try_clone().unwrap());
        stop.stop_runner().unwrap();
        let mut len = [0u8; 4];
        guest.read_exact(&mut len).unwrap();
        let mut body = vec![0u8; u32::from_be_bytes(len) as usize];
        guest.read_exact(&mut body).unwrap();
        let msg: GuestControl = serde_json::from_slice(&body).unwrap();
        assert_eq!(msg, GuestControl::StopRunner);

        // A reader blocked on the host side (as in `wait`) is released.
        let mut reader = host;
        let t = std::thread::spawn(move || {
            let mut b = [0u8; 4];
            reader.read_exact(&mut b)
        });
        std::thread::sleep(Duration::from_millis(20));
        stop.abort_wait();
        assert!(t.join().unwrap().is_err());
    }

    #[test]
    fn cgroup_usage() {
        let tmp = tempfile::tempdir().unwrap();
        fs::write(
            tmp.path().join("cpu.stat"),
            "usage_usec 20000000\nuser_usec 1\n",
        )
        .unwrap();
        fs::write(tmp.path().join("memory.peak"), (3u64 << 30).to_string()).unwrap();
        let u = read_cgroup_usage(tmp.path(), Duration::from_secs(10));
        assert!((u.avg_busy_vcpu - 2.0).abs() < 1e-9);
        assert!((u.peak_memory_gb - 3.0).abs() < 1e-9);
    }
}
