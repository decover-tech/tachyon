//! Per-VM jail: every hypervisor process runs as its own unprivileged user,
//! in its own cgroup, its own network namespace (see `network`) and its own
//! mount/IPC/UTS namespaces. Cloud Hypervisor applies
//! its own seccomp filters (`--seccomp true`, the default) on top.
//!
//! Doing this in Go would fight the multithreaded runtime (runc carries a C
//! shim for exactly this); in Rust it is a `pre_exec` hook.

use anyhow::{Context, Result};
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;

#[derive(Debug, Clone)]
pub struct JailSpec {
    pub job_id: String,
    pub uid: u32,
    pub gid: u32,
    /// cgroup v2 parent, e.g. /sys/fs/cgroup/tachyon.
    pub cgroup_parent: PathBuf,
    /// cgroup cpu.weight (1..=10000), proportional to reserved vCPUs.
    pub cpu_weight: u32,
    /// Hard memory limit: VM RAM plus hypervisor overhead.
    pub memory_max_bytes: u64,
    /// Bind-mounts `.0` onto `.1` inside the jail's mount namespace. Used to
    /// give every VM the same in-jail path for its sockets and disks, since
    /// a restored snapshot reuses the paths it was created with.
    pub bind: Option<(PathBuf, PathBuf)>,
    /// Network namespace to join (e.g. /run/netns/tachyon-vm3), so the VMM
    /// opens its tap there and never sees the host's interfaces.
    pub netns: Option<PathBuf>,
}

impl JailSpec {
    pub fn cgroup_dir(&self) -> PathBuf {
        self.cgroup_parent
            .join(format!("vm-{}", crate::storage::sanitize(&self.job_id)))
    }

    /// Creates the VM's cgroup and writes its limits.
    pub fn create_cgroup(&self) -> Result<PathBuf> {
        let dir = self.cgroup_dir();
        fs::create_dir_all(&dir).with_context(|| format!("create cgroup {}", dir.display()))?;
        write(
            &dir,
            "cpu.weight",
            &self.cpu_weight.clamp(1, 10_000).to_string(),
        )?;
        write(&dir, "memory.max", &self.memory_max_bytes.to_string())?;
        // Memory is never oversubscribed: no swap for VMs.
        write(&dir, "memory.swap.max", "0")?;
        Ok(dir)
    }

    pub fn remove_cgroup(&self) -> Result<()> {
        let dir = self.cgroup_dir();
        match fs::remove_dir(&dir) {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(e).with_context(|| format!("remove cgroup {}", dir.display())),
        }
    }

    /// Configures `cmd` to start inside the jail.
    pub fn apply(&self, cmd: &mut Command) -> Result<()> {
        // The bind mount in the child needs an existing mount point.
        if let Some((_, dst)) = &self.bind {
            fs::create_dir_all(dst).with_context(|| format!("create {}", dst.display()))?;
        }
        let procs = self.cgroup_dir().join("cgroup.procs");
        apply_os(self, cmd, procs)
    }
}

fn write(dir: &Path, file: &str, value: &str) -> Result<()> {
    fs::write(dir.join(file), value).with_context(|| format!("write {}/{file}", dir.display()))
}

#[cfg(target_os = "linux")]
fn apply_os(spec: &JailSpec, cmd: &mut Command, procs: PathBuf) -> Result<()> {
    use std::ffi::CString;
    use std::os::unix::ffi::OsStrExt;
    use std::os::unix::process::CommandExt;

    let procs = CString::new(procs.as_os_str().as_bytes())?;
    let (uid, gid) = (spec.uid, spec.gid);
    let bind = match &spec.bind {
        Some((src, dst)) => Some((
            CString::new(src.as_os_str().as_bytes())?,
            CString::new(dst.as_os_str().as_bytes())?,
        )),
        None => None,
    };
    // setgroups below drops the caller's groups; keep the groups that own
    // the devices the VMM opens (/dev/kvm and /dev/vhost-vsock are 0660 root:kvm).
    let mut groups: Vec<libc::gid_t> = Vec::new();
    for dev in ["/dev/kvm", "/dev/vhost-vsock"] {
        if let Ok(m) = fs::metadata(dev) {
            use std::os::unix::fs::MetadataExt;
            if m.gid() != 0 && !groups.contains(&m.gid()) {
                groups.push(m.gid());
            }
        }
    }
    let netns = match &spec.netns {
        Some(p) => Some(CString::new(p.as_os_str().as_bytes())?),
        None => None,
    };
    // SAFETY: only async-signal-safe libc calls between fork and exec.
    unsafe {
        cmd.pre_exec(move || {
            // Join the cgroup before anything else runs.
            let fd = libc::open(procs.as_ptr(), libc::O_WRONLY | libc::O_CLOEXEC);
            if fd < 0 {
                return Err(std::io::Error::last_os_error());
            }
            let pid = b"0\n";
            let n = libc::write(fd, pid.as_ptr().cast(), pid.len());
            libc::close(fd);
            if n < 0 {
                return Err(std::io::Error::last_os_error());
            }
            if let Some(ns) = &netns {
                let fd = libc::open(ns.as_ptr(), libc::O_RDONLY | libc::O_CLOEXEC);
                if fd < 0 {
                    return Err(std::io::Error::last_os_error());
                }
                let rc = libc::setns(fd, libc::CLONE_NEWNET);
                libc::close(fd);
                if rc != 0 {
                    return Err(std::io::Error::last_os_error());
                }
            }
            if libc::unshare(libc::CLONE_NEWNS | libc::CLONE_NEWIPC | libc::CLONE_NEWUTS) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            // Keep mounts made in the jail from propagating to the host.
            let root = b"/\0";
            if libc::mount(
                std::ptr::null(),
                root.as_ptr().cast(),
                std::ptr::null(),
                libc::MS_REC | libc::MS_PRIVATE,
                std::ptr::null(),
            ) != 0
            {
                return Err(std::io::Error::last_os_error());
            }
            if let Some((src, dst)) = &bind {
                if libc::mount(
                    src.as_ptr(),
                    dst.as_ptr(),
                    std::ptr::null(),
                    libc::MS_BIND,
                    std::ptr::null(),
                ) != 0
                {
                    return Err(std::io::Error::last_os_error());
                }
            }
            if libc::setgroups(groups.len(), groups.as_ptr()) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            if libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            // Drop to the VM's own user last. Not Command::uid/gid: std
            // applies those before pre_exec runs, which would leave the
            // cgroup join, unshare and mount above unprivileged.
            if libc::setgid(gid) != 0 || libc::setuid(uid) != 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
    }
    Ok(())
}

#[cfg(not(target_os = "linux"))]
fn apply_os(_spec: &JailSpec, _cmd: &mut Command, _procs: PathBuf) -> Result<()> {
    anyhow::bail!("the jailer requires Linux")
}

/// Allocates a distinct unprivileged uid/gid per VM slot.
pub fn slot_ids(base_uid: u32, slot: u32) -> (u32, u32) {
    (base_uid + slot, base_uid + slot)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cgroup_limits() {
        let tmp = tempfile::tempdir().unwrap();
        let spec = JailSpec {
            job_id: "gh/1".into(),
            uid: 100_001,
            gid: 100_001,
            cgroup_parent: tmp.path().to_path_buf(),
            cpu_weight: 0,
            memory_max_bytes: 8 << 30,
            bind: None,
            netns: None,
        };
        let dir = spec.create_cgroup().unwrap();
        assert_eq!(dir, tmp.path().join("vm-gh_1"));
        assert_eq!(fs::read_to_string(dir.join("cpu.weight")).unwrap(), "1");
        assert_eq!(
            fs::read_to_string(dir.join("memory.max")).unwrap(),
            (8u64 << 30).to_string()
        );
        assert_eq!(
            fs::read_to_string(dir.join("memory.swap.max")).unwrap(),
            "0"
        );
        // remove_dir fails on a non-empty dir in a tempdir (real cgroupfs
        // allows it); clean the files to exercise the happy path.
        for f in ["cpu.weight", "memory.max", "memory.swap.max"] {
            fs::remove_file(dir.join(f)).unwrap();
        }
        spec.remove_cgroup().unwrap();
        spec.remove_cgroup().unwrap(); // idempotent
    }

    #[test]
    fn slots_are_distinct() {
        assert_ne!(slot_ids(100_000, 1), slot_ids(100_000, 2));
    }
}
