//! Copy-on-write volume engine. No job ever copies an image: job disks and
//! sticky volumes are instant clones on local NVMe. ZFS volumes are the
//! primary engine; LVM thin is the fallback (decision D2, from Phase 0
//! benchmarks).

use anyhow::{bail, Context, Result};
use std::process::Command;

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
}

/// Runs host commands; replaced in tests.
pub trait CommandRunner: Send + Sync {
    fn run(&self, program: &str, args: &[String]) -> Result<String>;
}

pub struct ExecRunner;

impl CommandRunner for ExecRunner {
    fn run(&self, program: &str, args: &[String]) -> Result<String> {
        let out = Command::new(program)
            .args(args)
            .output()
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
