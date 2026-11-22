//! The guest's root filesystem. No job ever copies an image:
//!
//! - The **lower layer** is the base image, an ext4 file on host NVMe mapped
//!   into every VM through virtio-pmem. The guest mounts it with DAX, so its
//!   reads go straight to the host page cache and every VM on the host shares
//!   the same pages. The device is mapped with `discard_writes=on`
//!   (MAP_PRIVATE), and the file itself has no write permission, so no VM can
//!   change the shared base.
//! - The **upper layer** is the job's writable disk (serial `job`), a
//!   copy-on-write clone on local NVMe.
//! - `/var/lib/docker` is never on the overlay: Docker's storage driver needs
//!   a real filesystem, so it is always a sticky volume.
//!
//! The pmem device is part of the paused base snapshot (the guest boots from
//! it before the snapshot is taken), so hostd does not hot-plug it per job.
//! Instead it checks every snapshot it restores maps the base read-only, and
//! tells the guest agent how to assemble the overlay.

use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};
use std::collections::HashSet;
use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};

use crate::pb;

/// Device id of the base image in the VM config.
pub const BASE_PMEM_ID: &str = "base";
/// Block device the guest sees for the first (and only) pmem region.
pub const LOWER_DEVICE: &str = "/dev/pmem0";
/// Virtio serial of the job's writable disk, the overlay's upper layer.
pub const UPPER_SERIAL: &str = "job";
/// Docker's data directory: always a sticky volume.
pub const DOCKER_MOUNT: &str = "/var/lib/docker";
pub const DOCKER_KEY: &str = "docker";
/// Cloud Hypervisor requires a pmem region to be a multiple of 2 MiB.
pub const PMEM_ALIGN: u64 = 2 << 20;

/// How the guest agent assembles its root. Sent in the vsock payload.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct GuestRoot {
    /// name@version of the base image, for the guest's own logs.
    pub base_image: String,
    pub lower_device: String,
    pub lower_fstype: String,
    /// Mount options for the lower layer: read-only, DAX.
    pub lower_options: Vec<String>,
    /// Serial of the disk holding the overlay's upperdir and workdir.
    pub upper_serial: String,
}

/// The overlay root for a job on `base_image`.
pub fn guest_root(base_image: &str) -> GuestRoot {
    GuestRoot {
        base_image: base_image.to_string(),
        lower_device: LOWER_DEVICE.into(),
        lower_fstype: "ext4".into(),
        lower_options: vec!["ro".into(), "dax".into()],
        upper_serial: UPPER_SERIAL.into(),
    }
}

/// Cloud Hypervisor `--pmem` argument mapping the base image read-only.
pub fn pmem_arg(file: &Path) -> String {
    format!(
        "file={},discard_writes=on,id={BASE_PMEM_ID}",
        file.display()
    )
}

/// Checks a base image file can be shared by every VM: a non-empty regular
/// file, 2 MiB aligned, with no write permission for anyone. Returns its size.
pub fn check_base_image(file: &Path) -> Result<u64> {
    let md = fs::metadata(file).with_context(|| format!("base image {}", file.display()))?;
    if !md.is_file() {
        bail!("base image {} is not a regular file", file.display());
    }
    let size = md.len();
    if size == 0 || size % PMEM_ALIGN != 0 {
        bail!(
            "base image {} is {size} bytes; virtio-pmem needs a non-zero multiple of 2 MiB",
            file.display()
        );
    }
    if md.permissions().mode() & 0o222 != 0 {
        bail!(
            "base image {} is writable (mode {:o}); it is shared by every VM and must be read-only",
            file.display(),
            md.permissions().mode() & 0o7777
        );
    }
    Ok(size)
}

/// The parts of Cloud Hypervisor's snapshot `config.json` checked here.
#[derive(Debug, Deserialize)]
struct SnapshotConfig {
    #[serde(default)]
    pmem: Option<Vec<PmemConfig>>,
}

#[derive(Debug, Deserialize)]
struct PmemConfig {
    file: PathBuf,
    #[serde(default)]
    discard_writes: bool,
}

/// Checks a paused base snapshot maps its base image through virtio-pmem,
/// with writes discarded, from a file that passes `check_base_image`.
/// Returns the base image file.
pub fn check_snapshot(snapshot_dir: &Path) -> Result<PathBuf> {
    let path = snapshot_dir.join("config.json");
    let raw = fs::read_to_string(&path).with_context(|| format!("read {}", path.display()))?;
    let file = check_snapshot_config(&raw).with_context(|| format!("{}", path.display()))?;
    check_base_image(&file)?;
    Ok(file)
}

fn check_snapshot_config(raw: &str) -> Result<PathBuf> {
    let cfg: SnapshotConfig = serde_json::from_str(raw).context("parse snapshot config")?;
    let pmem = cfg.pmem.unwrap_or_default();
    match pmem.as_slice() {
        [] => bail!("snapshot has no virtio-pmem base image"),
        [p] if !p.discard_writes => bail!(
            "snapshot maps {} without discard_writes; the shared base would be writable",
            p.file.display()
        ),
        [p] => Ok(p.file.clone()),
        _ => bail!("snapshot maps {} pmem devices; want exactly 1", pmem.len()),
    }
}

/// The volumes a job actually gets: the requested ones, validated, plus an
/// empty docker sticky volume if nothing is mounted at `/var/lib/docker`.
/// Mounts must be clean absolute paths other than `/` (the overlay itself),
/// and keys and mounts must be unique.
pub fn plan_volumes(vols: &[pb::Volume]) -> Result<Vec<pb::Volume>> {
    let mut keys = HashSet::new();
    let mut mounts = HashSet::new();
    for v in vols {
        if v.key.is_empty() {
            bail!("volume at {:?} has no key", v.mount);
        }
        if !is_clean_abs(&v.mount) {
            bail!(
                "volume {:?}: mount {:?} must be a clean absolute path",
                v.key,
                v.mount
            );
        }
        if v.mount == "/" {
            bail!("volume {:?}: cannot mount over the overlay root", v.key);
        }
        if v.key == DOCKER_KEY && v.mount != DOCKER_MOUNT {
            bail!(
                "volume key {DOCKER_KEY:?} is reserved for {DOCKER_MOUNT}, not {:?}",
                v.mount
            );
        }
        if !keys.insert(v.key.as_str()) {
            bail!("duplicate volume key {:?}", v.key);
        }
        if !mounts.insert(v.mount.as_str()) {
            bail!("duplicate volume mount {:?}", v.mount);
        }
    }
    let mut out = vols.to_vec();
    if !mounts.contains(DOCKER_MOUNT) {
        out.push(pb::Volume {
            key: DOCKER_KEY.into(),
            mount: DOCKER_MOUNT.into(),
            source_snapshot_id: String::new(),
        });
    }
    Ok(out)
}

fn is_clean_abs(p: &str) -> bool {
    match p.strip_prefix('/') {
        Some("") => true,
        Some(rest) => rest
            .split('/')
            .all(|seg| !seg.is_empty() && seg != "." && seg != ".."),
        None => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn vol(key: &str, mount: &str) -> pb::Volume {
        pb::Volume {
            key: key.into(),
            mount: mount.into(),
            source_snapshot_id: String::new(),
        }
    }

    fn image(dir: &Path, size: u64, mode: u32) -> PathBuf {
        let p = dir.join(format!("rootfs-{size}-{mode:o}.ext4"));
        let f = fs::File::create(&p).unwrap();
        f.set_len(size).unwrap();
        fs::set_permissions(&p, fs::Permissions::from_mode(mode)).unwrap();
        p
    }

    #[test]
    fn root_is_dax_overlay() {
        let r = guest_root("ubuntu-24.04@2026-11-16");
        assert_eq!(r.lower_device, "/dev/pmem0");
        assert_eq!(r.lower_options, vec!["ro", "dax"]);
        assert_eq!(r.upper_serial, "job");
        assert_eq!(
            pmem_arg(Path::new("/img/rootfs.ext4")),
            "file=/img/rootfs.ext4,discard_writes=on,id=base"
        );
    }

    #[test]
    fn base_image_checks() {
        let tmp = tempfile::tempdir().unwrap();
        let ok = image(tmp.path(), 4 * PMEM_ALIGN, 0o444);
        assert_eq!(check_base_image(&ok).unwrap(), 4 * PMEM_ALIGN);

        let writable = image(tmp.path(), PMEM_ALIGN, 0o644);
        assert!(format!("{:#}", check_base_image(&writable).unwrap_err()).contains("writable"));

        let odd = image(tmp.path(), PMEM_ALIGN + 4096, 0o444);
        assert!(format!("{:#}", check_base_image(&odd).unwrap_err()).contains("2 MiB"));

        let empty = image(tmp.path(), 0, 0o444);
        assert!(check_base_image(&empty).is_err());

        assert!(check_base_image(tmp.path()).is_err(), "directory");
        assert!(check_base_image(&tmp.path().join("missing")).is_err());
    }

    #[test]
    fn snapshot_config_checks() {
        let ok = r#"{"cpus":{"boot_vcpus":4},"pmem":[{"file":"/img/rootfs.ext4","size":null,"iommu":false,"discard_writes":true,"id":"base","pci_segment":0}]}"#;
        assert_eq!(
            check_snapshot_config(ok).unwrap(),
            PathBuf::from("/img/rootfs.ext4")
        );
        for (bad, why) in [
            (r#"{"cpus":{}}"#, "no virtio-pmem"),
            (r#"{"pmem":null}"#, "no virtio-pmem"),
            (r#"{"pmem":[]}"#, "no virtio-pmem"),
            (r#"{"pmem":[{"file":"/a"}]}"#, "discard_writes"),
            (
                r#"{"pmem":[{"file":"/a","discard_writes":false}]}"#,
                "discard_writes",
            ),
            (
                r#"{"pmem":[{"file":"/a","discard_writes":true},{"file":"/b","discard_writes":true}]}"#,
                "exactly 1",
            ),
            ("not json", "parse"),
        ] {
            let e = format!("{:#}", check_snapshot_config(bad).unwrap_err());
            assert!(e.contains(why), "{bad}: {e}");
        }
    }

    #[test]
    fn check_snapshot_reads_dir_and_image() {
        let tmp = tempfile::tempdir().unwrap();
        let img = image(tmp.path(), PMEM_ALIGN, 0o444);
        let snap = tmp.path().join("4");
        fs::create_dir(&snap).unwrap();
        fs::write(
            snap.join("config.json"),
            format!(
                r#"{{"pmem":[{{"file":"{}","discard_writes":true}}]}}"#,
                img.display()
            ),
        )
        .unwrap();
        assert_eq!(check_snapshot(&snap).unwrap(), img);

        // Same snapshot, but the shared file became writable.
        fs::set_permissions(&img, fs::Permissions::from_mode(0o644)).unwrap();
        assert!(check_snapshot(&snap).is_err());
        assert!(check_snapshot(tmp.path()).is_err(), "no config.json");
    }

    #[test]
    fn docker_always_on_a_sticky_volume() {
        let planned = plan_volumes(&[]).unwrap();
        assert_eq!(planned, vec![vol("docker", "/var/lib/docker")]);

        let cache = vol("cache", "/home/runner/.cache");
        let planned = plan_volumes(std::slice::from_ref(&cache)).unwrap();
        assert_eq!(planned, vec![cache, vol("docker", "/var/lib/docker")]);

        // A requested docker volume (with its snapshot) is kept as is.
        let mut d = vol("docker", "/var/lib/docker");
        d.source_snapshot_id = "sticky/acme_api/docker@head-0".into();
        assert_eq!(
            plan_volumes(std::slice::from_ref(&d)).unwrap(),
            vec![d.clone()]
        );
        // Another key at /var/lib/docker also counts.
        let other = vol("docker-big", "/var/lib/docker");
        assert_eq!(
            plan_volumes(std::slice::from_ref(&other)).unwrap(),
            vec![other]
        );
    }

    #[test]
    fn rejects_bad_volumes() {
        for (vols, why) in [
            (vec![vol("r", "/")], "overlay root"),
            (vec![vol("r", "var/x")], "clean absolute"),
            (vec![vol("r", "/var/../etc")], "clean absolute"),
            (vec![vol("r", "/var/./x")], "clean absolute"),
            (vec![vol("r", "/var//x")], "clean absolute"),
            (vec![vol("r", "/var/x/")], "clean absolute"),
            (vec![vol("", "/x")], "no key"),
            (vec![vol("docker", "/srv/docker")], "reserved"),
            (vec![vol("a", "/x"), vol("a", "/y")], "duplicate volume key"),
            (
                vec![vol("a", "/x"), vol("b", "/x")],
                "duplicate volume mount",
            ),
        ] {
            let e = format!("{:#}", plan_volumes(&vols).unwrap_err());
            assert!(e.contains(why), "{vols:?}: {e}");
        }
    }
}
