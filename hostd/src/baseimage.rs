//! Base images on a host. Each version lives in its own directory:
//!
//! ```text
//! <store>/<name>@<version>/
//!   manifest.signed.json   ed25519-signed manifest from the image pipeline
//!   rootfs.ext4            read-only lower layer, mapped via virtio-pmem
//!   vmlinux                guest kernel (if the image ships one)
//!   snapshots/<vcpu>/      paused Cloud Hypervisor snapshot per VM size
//! ```
//!
//! `prepare` verifies the signature and every file digest, then boots the
//! image once per VM size (2/4/8/16 vCPU, 4 GB per vCPU), waits for the
//! guest agent, pauses and snapshots it. Snapshots are made on the host,
//! not shipped, because a memory snapshot is tied to the host's Cloud
//! Hypervisor build. Old versions stay while jobs use them (`gc`).

use anyhow::{anyhow, bail, Context, Result};
use base64::Engine as _;
use ed25519_dalek::{Signature, VerifyingKey};
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::collections::HashSet;
use std::fs;
use std::io::{BufRead, BufReader, Read, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use crate::hypervisor::IN_JAIL_DIR;
use crate::jailer::JailSpec;
use crate::network::TAP;
use crate::pb;

/// VM sizes in vCPU.
pub const SIZES: [u32; 4] = [2, 4, 8, 16];
pub const GB_PER_VCPU: u32 = 4;
/// vsock port the guest agent opens once the guest has booted and is ready
/// to be snapshotted. Separate from the job port so a probe never consumes
/// the job connection.
pub const GUEST_READY_PORT: u32 = 1025;
/// Must match internal/images/sign.go.
const SIGNING_CONTEXT: &[u8] = b"tachyon-base-image-manifest-v1\x00";
/// Written into a snapshot directory last; its absence means incomplete.
const READY_MARKER: &str = ".tachyon-ready";

/// The snapshot's virtio-net MAC. Every restored VM reuses it: each sits
/// alone on its own tap in its own namespace.
pub const GUEST_MAC: &str = "52:54:00:7a:c0:02";

#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct Blob {
    pub path: String,
    pub sha256: String,
    pub size: u64,
}

/// The fields of internal/images.Manifest hostd needs. Others are ignored.
#[derive(Debug, Clone, Deserialize, PartialEq)]
pub struct Manifest {
    pub name: String,
    pub version: String,
    pub arch: String,
    pub rootfs: Blob,
    #[serde(default)]
    pub kernel: Option<Blob>,
    pub sizes: Vec<u32>,
}

impl Manifest {
    pub fn image_ref(&self) -> String {
        format!("{}@{}", self.name, self.version)
    }
}

/// internal/images.Signed. Go encodes []byte as standard base64.
#[derive(Debug, Clone, Deserialize)]
pub struct Signed {
    pub manifest: String,
    pub key_id: String,
    pub signature: String,
}

/// First 16 hex digits of the key's SHA-256, as in Go's images.KeyID.
pub fn key_id(key: &VerifyingKey) -> String {
    hex::encode(&Sha256::digest(key.as_bytes())[..8])
}

/// DER prefix of an ed25519 SubjectPublicKeyInfo (RFC 8410).
const ED25519_SPKI_PREFIX: [u8; 12] = [
    0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00,
];

/// Parses one or more "PUBLIC KEY" PEM blocks holding ed25519 keys.
pub fn parse_public_keys(pem: &str) -> Result<Vec<VerifyingKey>> {
    let mut keys = Vec::new();
    let mut body: Option<String> = None;
    for line in pem.lines().map(str::trim) {
        match (line, body.as_mut()) {
            ("-----BEGIN PUBLIC KEY-----", None) => body = Some(String::new()),
            ("-----END PUBLIC KEY-----", Some(b)) => {
                let der = base64::engine::general_purpose::STANDARD
                    .decode(b.as_bytes())
                    .context("public key base64")?;
                let raw = der
                    .strip_prefix(&ED25519_SPKI_PREFIX[..])
                    .filter(|r| r.len() == 32)
                    .ok_or_else(|| anyhow!("public key is not ed25519"))?;
                let raw: [u8; 32] = raw.try_into()?;
                keys.push(VerifyingKey::from_bytes(&raw)?);
                body = None;
            }
            (l, Some(b)) => b.push_str(l),
            _ => {}
        }
    }
    if keys.is_empty() {
        bail!("no ed25519 public keys found");
    }
    Ok(keys)
}

impl Signed {
    /// Checks the signature over the manifest's exact bytes, then decodes it.
    pub fn verify(&self, trusted: &[VerifyingKey]) -> Result<Manifest> {
        let key = trusted
            .iter()
            .find(|k| key_id(k) == self.key_id)
            .ok_or_else(|| anyhow!("image signed by untrusted key {:?}", self.key_id))?;
        let b64 = base64::engine::general_purpose::STANDARD;
        let body = b64.decode(&self.manifest).context("manifest base64")?;
        let sig = Signature::from_slice(&b64.decode(&self.signature).context("signature base64")?)?;
        let mut msg = SIGNING_CONTEXT.to_vec();
        msg.extend_from_slice(&body);
        key.verify_strict(&msg, &sig)
            .map_err(|_| anyhow!("image manifest signature does not verify"))?;
        let m: Manifest = serde_json::from_slice(&body)?;
        check_ref(&m.image_ref())?;
        for b in std::iter::once(&m.rootfs).chain(m.kernel.as_ref()) {
            check_rel(&b.path)?;
        }
        Ok(m)
    }
}

/// name@version with no path tricks.
pub fn check_ref(r: &str) -> Result<()> {
    let ok = r.split_once('@').is_some_and(|(n, v)| {
        !n.is_empty()
            && !v.is_empty()
            && !r.contains("..")
            && r.chars()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || "@.-".contains(c))
    });
    if !ok {
        bail!("invalid image ref {r:?}");
    }
    Ok(())
}

fn check_rel(p: &str) -> Result<()> {
    if p.is_empty() || p.starts_with('/') || p.split('/').any(|c| c == ".." || c.is_empty()) {
        bail!("invalid file path {p:?} in manifest");
    }
    Ok(())
}

/// SHA-256 (hex) and size of a file, streamed.
pub fn sha256_file(path: &Path) -> Result<(String, u64)> {
    let mut f = fs::File::open(path).with_context(|| format!("open {}", path.display()))?;
    let mut h = Sha256::new();
    let mut buf = vec![0u8; 1 << 20];
    let mut n = 0u64;
    loop {
        let k = f.read(&mut buf)?;
        if k == 0 {
            break;
        }
        h.update(&buf[..k]);
        n += k as u64;
    }
    Ok((hex::encode(h.finalize()), n))
}

/// What to boot for one size's base snapshot.
#[derive(Debug, Clone, PartialEq)]
pub struct BootSpec {
    pub vcpu: u32,
    pub memory_gb: u32,
    pub kernel: PathBuf,
    pub rootfs: PathBuf,
    pub cmdline: String,
}

/// Boots an image and leaves a paused snapshot in `out`.
pub trait SnapshotBuilder: Send + Sync {
    fn build(&self, spec: &BootSpec, out: &Path) -> Result<()>;
}

pub struct ImageStore {
    pub root: PathBuf,
    /// Kernel to boot when the image doesn't ship one.
    pub default_kernel: Option<PathBuf>,
    pub cmdline: String,
}

/// Default guest kernel command line: the ext4 image is the read-only
/// root, read through DAX straight from the host page cache.
pub const DEFAULT_CMDLINE: &str =
    "console=hvc0 root=/dev/pmem0 rootfstype=ext4 ro rootflags=dax quiet";

impl ImageStore {
    pub fn new(root: PathBuf) -> Self {
        Self {
            root,
            default_kernel: None,
            cmdline: DEFAULT_CMDLINE.into(),
        }
    }

    pub fn dir(&self, image: &str) -> Result<PathBuf> {
        check_ref(image)?;
        Ok(self.root.join(image))
    }

    pub fn snapshot_dir(&self, image: &str, vcpu: u32) -> Result<PathBuf> {
        Ok(self.dir(image)?.join("snapshots").join(vcpu.to_string()))
    }

    /// Verifies an image's signature and every file it names.
    pub fn verify(&self, image: &str, trusted: &[VerifyingKey]) -> Result<Manifest> {
        let dir = self.dir(image)?;
        let raw = fs::read(dir.join("manifest.signed.json"))
            .with_context(|| format!("{image}: read signed manifest"))?;
        let signed: Signed = serde_json::from_slice(&raw)?;
        let m = signed.verify(trusted).with_context(|| image.to_string())?;
        if m.image_ref() != image {
            bail!("{image}: manifest is for {}", m.image_ref());
        }
        for b in std::iter::once(&m.rootfs).chain(m.kernel.as_ref()) {
            let (sum, size) = sha256_file(&dir.join(&b.path))?;
            if sum != b.sha256 || size != b.size {
                bail!(
                    "{image}: {} is {sum} ({size} bytes), manifest says {} ({} bytes)",
                    b.path,
                    b.sha256,
                    b.size
                );
            }
        }
        Ok(m)
    }

    /// Verifies the image and builds any missing per-size base snapshots.
    /// Idempotent: finished sizes are skipped, half-built ones redone.
    /// Returns the sizes built by this call.
    pub fn prepare(
        &self,
        image: &str,
        trusted: &[VerifyingKey],
        builder: &dyn SnapshotBuilder,
    ) -> Result<Vec<u32>> {
        let m = self.verify(image, trusted)?;
        let dir = self.dir(image)?;
        let kernel = match (&m.kernel, &self.default_kernel) {
            (Some(k), _) => dir.join(&k.path),
            (None, Some(k)) => k.clone(),
            (None, None) => bail!("{image} ships no kernel and no default kernel is set"),
        };
        let mut built = Vec::new();
        for &vcpu in &m.sizes {
            let out = self.snapshot_dir(image, vcpu)?;
            if out.join(READY_MARKER).exists() {
                continue;
            }
            let tmp = out.with_file_name(format!(".{vcpu}.tmp"));
            let _ = fs::remove_dir_all(&tmp);
            let _ = fs::remove_dir_all(&out);
            fs::create_dir_all(&tmp)?;
            let spec = BootSpec {
                vcpu,
                memory_gb: vcpu * GB_PER_VCPU,
                kernel: kernel.clone(),
                rootfs: dir.join(&m.rootfs.path),
                cmdline: self.cmdline.clone(),
            };
            builder
                .build(&spec, &tmp)
                .with_context(|| format!("{image}: snapshot {vcpu} vCPU"))?;
            fs::write(tmp.join(READY_MARKER), b"")?;
            fs::rename(&tmp, &out)?;
            built.push(vcpu);
        }
        // Every size the manifest lists is done: the image is usable.
        fs::write(dir.join("snapshots").join(READY_MARKER), b"")?;
        Ok(built)
    }

    /// Images whose every snapshot is ready, sorted.
    pub fn ready(&self) -> Vec<String> {
        let mut out = Vec::new();
        let Ok(entries) = fs::read_dir(&self.root) else {
            return out;
        };
        for e in entries.flatten() {
            let name = e.file_name().to_string_lossy().into_owned();
            if check_ref(&name).is_ok() && e.path().join("snapshots").join(READY_MARKER).exists() {
                out.push(name);
            }
        }
        out.sort();
        out
    }

    /// Inventory entries for heartbeats: one base-image ref per ready image.
    pub fn snapshot_refs(&self) -> Vec<pb::SnapshotRef> {
        self.ready()
            .into_iter()
            .map(|r| pb::SnapshotRef {
                kind: pb::snapshot_ref::Kind::BaseImage as i32,
                repo: String::new(),
                key: r.clone(),
                id: r,
            })
            .collect()
    }

    /// Deletes image versions that no job uses, keeping the newest
    /// `keep_newest` of each image name for new jobs and rollback.
    pub fn gc(&self, in_use: &HashSet<String>, keep_newest: usize) -> Result<Vec<String>> {
        let mut by_name: std::collections::BTreeMap<String, Vec<String>> = Default::default();
        for e in fs::read_dir(&self.root)?.flatten() {
            let r = e.file_name().to_string_lossy().into_owned();
            if let (Ok(()), Some((n, v))) = (check_ref(&r), r.split_once('@')) {
                by_name
                    .entry(n.to_string())
                    .or_default()
                    .push(v.to_string());
            }
        }
        let mut removed = Vec::new();
        for (name, mut versions) in by_name {
            versions.sort_by_key(|v| std::cmp::Reverse(version_key(v))); // newest first
            for v in versions.into_iter().skip(keep_newest) {
                let r = format!("{name}@{v}");
                if in_use.contains(&r) {
                    continue;
                }
                fs::remove_dir_all(self.root.join(&r))?;
                removed.push(r);
            }
        }
        Ok(removed)
    }
}

/// Where VMs restore their base snapshots from.
#[derive(Debug, Clone, PartialEq)]
pub enum SnapshotSource {
    /// Versioned: `<root>/<name>@<version>/snapshots/<vcpu>/`, as written by
    /// `prepare-image`. Only images whose snapshots are all ready are used.
    Store(PathBuf),
    /// One fixed directory `<dir>/<vcpu>/` (TACHYON_BASE_SNAPSHOTS), for
    /// single-image setups. With `image` set, only jobs pinned to that
    /// image run; without it, every job runs on whatever the directory
    /// holds, as before versions existed.
    Static { dir: PathBuf, image: Option<String> },
}

impl SnapshotSource {
    /// Checks a host can run jobs for `image` and returns its canonical
    /// key, which warm VMs are pooled under. Fails, rather than falling
    /// back to another version, when the image is not prepared here.
    pub fn check(&self, image: &str) -> Result<String> {
        match self {
            Self::Store(root) => {
                if image.is_empty() {
                    bail!("job names no base image");
                }
                check_ref(image)?;
                if !root
                    .join(image)
                    .join("snapshots")
                    .join(READY_MARKER)
                    .exists()
                {
                    bail!("base image {image} is not prepared on this host");
                }
                Ok(image.to_string())
            }
            Self::Static { image: None, .. } => Ok(String::new()),
            Self::Static {
                image: Some(pinned),
                ..
            } => {
                if image != pinned {
                    bail!("this host serves only base image {pinned}, not {image:?}");
                }
                Ok(pinned.clone())
            }
        }
    }

    /// The snapshot directory for a canonical key (see `check`) and size.
    pub fn dir(&self, key: &str, vcpu: u32) -> Result<PathBuf> {
        match self {
            Self::Store(root) => {
                check_ref(key)?;
                Ok(root.join(key).join("snapshots").join(vcpu.to_string()))
            }
            Self::Static { dir, .. } => Ok(dir.join(vcpu.to_string())),
        }
    }

    /// Canonical keys to keep warm VMs for: the newest ready version of
    /// each image name (older versions restore on demand), or the one
    /// static image.
    pub fn warm_images(&self) -> Vec<String> {
        match self {
            Self::Store(root) => {
                let mut newest: std::collections::BTreeMap<String, String> = Default::default();
                for r in ImageStore::new(root.clone()).ready() {
                    if let Some((n, v)) = r.split_once('@') {
                        let better = newest
                            .get(n)
                            .is_none_or(|cur| version_key(v) > version_key(cur));
                        if better {
                            newest.insert(n.to_string(), v.to_string());
                        }
                    }
                }
                newest
                    .into_iter()
                    .map(|(n, v)| format!("{n}@{v}"))
                    .collect()
            }
            Self::Static { image, .. } => vec![image.clone().unwrap_or_default()],
        }
    }
}

/// Orders "2026-11-16" < "2026-11-16.1" < "2026-11-16.10" < "2026-11-23".
fn version_key(v: &str) -> (String, u64) {
    match v.split_once('.') {
        Some((d, n)) => (d.to_string(), n.parse::<u64>().map_or(u64::MAX, |n| n + 1)),
        None => (v.to_string(), 0),
    }
}

/// Builds base snapshots with Cloud Hypervisor, jailed like job VMs and
/// with the same in-jail socket paths, since a restored snapshot reuses
/// the paths it was created with.
pub struct ChSnapshotBuilder {
    pub binary: PathBuf,
    pub ch_remote: PathBuf,
    pub work_dir: PathBuf,
    pub cgroup_parent: PathBuf,
    pub uid: u32,
    pub gid: u32,
    pub ready_timeout: Duration,
}

/// Cloud Hypervisor arguments for a cold boot. Sockets are at their
/// in-jail paths, so VMs restored from the snapshot find them there.
pub fn boot_args(spec: &BootSpec) -> Vec<String> {
    vec![
        "--api-socket".into(),
        format!("path={IN_JAIL_DIR}/api.sock"),
        "--kernel".into(),
        spec.kernel.display().to_string(),
        "--cmdline".into(),
        spec.cmdline.clone(),
        "--cpus".into(),
        format!("boot={}", spec.vcpu),
        "--memory".into(),
        format!("size={}G", spec.memory_gb),
        "--pmem".into(),
        format!("file={},discard_writes=on", spec.rootfs.display()),
        "--vsock".into(),
        format!("cid=3,socket={IN_JAIL_DIR}/vsock.sock"),
        // Restore reopens the tap by name in the job VM's namespace.
        "--net".into(),
        format!("tap={TAP},mac={GUEST_MAC}"),
        "--console".into(),
        "off".into(),
        "--serial".into(),
        "off".into(),
    ]
}

impl ChSnapshotBuilder {
    fn ch_remote(&self, dir: &Path, args: &[&str]) -> Result<()> {
        let out = Command::new(&self.ch_remote)
            .arg("--api-socket")
            .arg(dir.join("api.sock"))
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

    /// Waits until the guest agent accepts a connection on the ready port.
    fn wait_ready(&self, dir: &Path) -> Result<()> {
        let start = Instant::now();
        loop {
            if let Ok(mut s) = UnixStream::connect(dir.join("vsock.sock")) {
                s.set_read_timeout(Some(Duration::from_secs(2)))?;
                writeln!(s, "CONNECT {GUEST_READY_PORT}")?;
                let mut line = String::new();
                if BufReader::new(&mut s).read_line(&mut line).is_ok() && line.starts_with("OK ") {
                    return Ok(());
                }
            }
            if start.elapsed() > self.ready_timeout {
                bail!("guest not ready after {:?}", self.ready_timeout);
            }
            std::thread::sleep(Duration::from_millis(200));
        }
    }
}

impl SnapshotBuilder for ChSnapshotBuilder {
    fn build(&self, spec: &BootSpec, out: &Path) -> Result<()> {
        let id = format!("snapshot-{}", spec.vcpu);
        let dir = self.work_dir.join(&id);
        let _ = fs::remove_dir_all(&dir);
        fs::create_dir_all(&dir)?;
        std::os::unix::fs::chown(&dir, Some(self.uid), Some(self.gid))?;
        std::os::unix::fs::chown(out, Some(self.uid), Some(self.gid))?;
        let jail = JailSpec {
            job_id: id.clone(),
            uid: self.uid,
            gid: self.gid,
            cgroup_parent: self.cgroup_parent.clone(),
            cpu_weight: 100 * spec.vcpu,
            memory_max_bytes: (u64::from(spec.memory_gb) << 30) + (512 << 20),
            bind: Some((dir.clone(), PathBuf::from(IN_JAIL_DIR))),
            // Offline: the namespace holds only an unconnected tap, so the
            // snapshot has the NIC every job VM's tap is attached to.
            netns: Some(Path::new("/run/netns").join(&id)),
        };
        prep_netns(&id, self.uid, self.gid)?;
        if let Err(e) = jail.create_cgroup() {
            let _ = ip(&["netns", "del", &id]);
            return Err(e);
        }
        let mut cmd = Command::new(&self.binary);
        cmd.args(boot_args(spec))
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        jail.apply(&mut cmd)?;
        let mut child = cmd.spawn().context("spawn cloud-hypervisor")?;
        let result = (|| {
            self.wait_ready(&dir)?;
            self.ch_remote(&dir, &["pause"])?;
            self.ch_remote(&dir, &["snapshot", &format!("file://{}", out.display())])
        })();
        let _ = self.ch_remote(&dir, &["shutdown-vmm"]);
        let _ = child.kill();
        let _ = child.wait();
        let _ = jail.remove_cgroup();
        let _ = ip(&["netns", "del", &id]);
        let _ = fs::remove_dir_all(&dir);
        result
    }
}

fn ip(args: &[&str]) -> Result<()> {
    let out = Command::new("ip").args(args).output().context("spawn ip")?;
    if !out.status.success() {
        bail!(
            "ip {}: {}",
            args.join(" "),
            String::from_utf8_lossy(&out.stderr).trim()
        );
    }
    Ok(())
}

/// A namespace holding only `tap0`, owned by the snapshot VM's user.
fn prep_netns(ns: &str, uid: u32, gid: u32) -> Result<()> {
    let _ = ip(&["netns", "del", ns]);
    ip(&["netns", "add", ns])?;
    let (uid, gid) = (uid.to_string(), gid.to_string());
    let r = ip(&[
        "-n", ns, "tuntap", "add", "dev", TAP, "mode", "tap", "user", &uid, "group", &gid,
    ])
    .and_then(|()| ip(&["-n", ns, "link", "set", TAP, "up"]));
    if r.is_err() {
        let _ = ip(&["netns", "del", ns]);
    }
    r
}

#[cfg(test)]
mod tests {
    use super::*;
    use ed25519_dalek::{Signer, SigningKey};
    use std::sync::Mutex;

    #[derive(Default)]
    struct FakeBuilder {
        built: Mutex<Vec<BootSpec>>,
        fail_vcpu: Option<u32>,
    }

    impl SnapshotBuilder for FakeBuilder {
        fn build(&self, spec: &BootSpec, out: &Path) -> Result<()> {
            if self.fail_vcpu == Some(spec.vcpu) {
                fs::write(out.join("state.json"), b"partial")?;
                bail!("boot failed");
            }
            fs::write(out.join("config.json"), b"{}")?;
            self.built.lock().unwrap().push(spec.clone());
            Ok(())
        }
    }

    fn key() -> SigningKey {
        SigningKey::from_bytes(&[7u8; 32])
    }

    /// Writes an image directory like the pipeline produces and signs it
    /// the way internal/images.Sign does.
    fn stage(store: &ImageStore, image: &str, signer: &SigningKey) -> PathBuf {
        let dir = store.dir(image).unwrap();
        fs::create_dir_all(&dir).unwrap();
        fs::write(dir.join("rootfs.ext4"), b"ext4").unwrap();
        fs::write(dir.join("vmlinux"), b"kernel").unwrap();
        let (rsum, rsize) = sha256_file(&dir.join("rootfs.ext4")).unwrap();
        let (ksum, ksize) = sha256_file(&dir.join("vmlinux")).unwrap();
        let (name, version) = image.split_once('@').unwrap();
        let manifest = serde_json::json!({
            "name": name, "version": version, "arch": "x86_64",
            "source": {"runner_images_commit": "x"},
            "rootfs": {"path": "rootfs.ext4", "sha256": rsum, "size": rsize},
            "kernel": {"path": "vmlinux", "sha256": ksum, "size": ksize},
            "sbom": {"path": "sbom.spdx.json", "sha256": "0", "size": 1},
            "sizes": [2, 4, 8, 16],
        })
        .to_string();
        let mut msg = SIGNING_CONTEXT.to_vec();
        msg.extend_from_slice(manifest.as_bytes());
        let b64 = base64::engine::general_purpose::STANDARD;
        let signed = serde_json::json!({
            "manifest": b64.encode(manifest.as_bytes()),
            "key_id": key_id(&signer.verifying_key()),
            "signature": b64.encode(signer.sign(&msg).to_bytes()),
        });
        fs::write(dir.join("manifest.signed.json"), signed.to_string()).unwrap();
        dir
    }

    fn store(tmp: &tempfile::TempDir) -> ImageStore {
        ImageStore::new(tmp.path().to_path_buf())
    }

    #[test]
    fn prepare_builds_every_size_once() {
        let tmp = tempfile::tempdir().unwrap();
        let s = store(&tmp);
        let k = key();
        let dir = stage(&s, "ubuntu-24.04@2026-11-16", &k);
        let b = FakeBuilder::default();
        let trusted = [k.verifying_key()];
        let built = s.prepare("ubuntu-24.04@2026-11-16", &trusted, &b).unwrap();
        assert_eq!(built, SIZES);
        let specs = b.built.lock().unwrap().clone();
        assert_eq!(specs[3].vcpu, 16);
        assert_eq!(specs[3].memory_gb, 64);
        assert_eq!(specs[0].rootfs, dir.join("rootfs.ext4"));
        assert_eq!(specs[0].kernel, dir.join("vmlinux"));
        assert!(dir.join("snapshots/8/config.json").exists());
        assert_eq!(s.ready(), vec!["ubuntu-24.04@2026-11-16".to_string()]);
        let refs = s.snapshot_refs();
        assert_eq!(refs[0].kind, pb::snapshot_ref::Kind::BaseImage as i32);
        assert_eq!(refs[0].key, "ubuntu-24.04@2026-11-16");

        // Idempotent.
        assert!(s
            .prepare("ubuntu-24.04@2026-11-16", &trusted, &b)
            .unwrap()
            .is_empty());
    }

    #[test]
    fn failed_size_is_retried_and_not_ready() {
        let tmp = tempfile::tempdir().unwrap();
        let s = store(&tmp);
        let k = key();
        stage(&s, "ubuntu-24.04@2026-11-16", &k);
        let trusted = [k.verifying_key()];
        let bad = FakeBuilder {
            fail_vcpu: Some(8),
            ..Default::default()
        };
        assert!(s
            .prepare("ubuntu-24.04@2026-11-16", &trusted, &bad)
            .is_err());
        assert!(s.ready().is_empty(), "half-built image reported ready");
        let good = FakeBuilder::default();
        let built = s
            .prepare("ubuntu-24.04@2026-11-16", &trusted, &good)
            .unwrap();
        assert_eq!(built, vec![8, 16]);
        assert_eq!(s.ready().len(), 1);
    }

    #[test]
    fn verify_rejects_untrusted_and_tampered() {
        let tmp = tempfile::tempdir().unwrap();
        let s = store(&tmp);
        let k = key();
        let dir = stage(&s, "ubuntu-24.04@2026-11-16", &k);
        let other = SigningKey::from_bytes(&[9u8; 32]).verifying_key();
        let b = FakeBuilder::default();

        let err = s
            .prepare("ubuntu-24.04@2026-11-16", &[other], &b)
            .unwrap_err();
        assert!(format!("{err:#}").contains("untrusted key"), "{err:#}");

        fs::write(dir.join("rootfs.ext4"), b"evil").unwrap();
        let err = s
            .prepare("ubuntu-24.04@2026-11-16", &[k.verifying_key()], &b)
            .unwrap_err();
        assert!(format!("{err:#}").contains("rootfs.ext4"), "{err:#}");
        assert!(b.built.lock().unwrap().is_empty());

        // A manifest signed for one version can't be used as another.
        let other_dir = s.dir("ubuntu-24.04@2026-11-23").unwrap();
        fs::create_dir_all(&other_dir).unwrap();
        stage(&s, "ubuntu-24.04@2026-11-16", &k);
        fs::copy(
            dir.join("manifest.signed.json"),
            other_dir.join("manifest.signed.json"),
        )
        .unwrap();
        let err = s
            .verify("ubuntu-24.04@2026-11-23", &[k.verifying_key()])
            .unwrap_err();
        assert!(format!("{err:#}").contains("manifest is for"), "{err:#}");
    }

    /// Signed by the Go pipeline (internal/images TestCrossLanguageFixture)
    /// with the all-7s seed.
    #[test]
    fn verifies_go_signed_manifest() {
        let raw = include_str!("../testdata/signed-manifest.json");
        let signed: Signed = serde_json::from_str(raw).unwrap();
        let k = key().verifying_key();
        assert_eq!(key_id(&k), signed.key_id);
        let m = signed.verify(&[k]).unwrap();
        assert_eq!(m.image_ref(), "ubuntu-24.04@2026-11-16");
        assert_eq!(m.sizes, SIZES);
        assert_eq!(m.rootfs.size, 64 << 30);
        assert_eq!(m.kernel.unwrap().path, "vmlinux");
    }

    #[test]
    fn tampered_signature_fails() {
        let k = key();
        let tmp = tempfile::tempdir().unwrap();
        let s = store(&tmp);
        let dir = stage(&s, "ubuntu-24.04@2026-11-16", &k);
        let mut signed: Signed =
            serde_json::from_slice(&fs::read(dir.join("manifest.signed.json")).unwrap()).unwrap();
        let b64 = base64::engine::general_purpose::STANDARD;
        let body = String::from_utf8(b64.decode(&signed.manifest).unwrap()).unwrap();
        signed.manifest = b64.encode(body.replace("\"arch\":\"x86_64\"", "\"arch\":\"arm64\""));
        assert!(signed.verify(&[k.verifying_key()]).is_err());
    }

    #[test]
    fn gc_keeps_newest_and_in_use() {
        let tmp = tempfile::tempdir().unwrap();
        let s = store(&tmp);
        for v in ["2026-11-02", "2026-11-09", "2026-11-16", "2026-11-16.1"] {
            fs::create_dir_all(s.dir(&format!("ubuntu-24.04@{v}")).unwrap()).unwrap();
        }
        fs::create_dir_all(tmp.path().join("not-an-image")).unwrap();
        let in_use: HashSet<String> = ["ubuntu-24.04@2026-11-02".to_string()].into();
        let removed = s.gc(&in_use, 2).unwrap();
        assert_eq!(removed, vec!["ubuntu-24.04@2026-11-09".to_string()]);
        assert!(s.dir("ubuntu-24.04@2026-11-16.1").unwrap().exists());
        assert!(s.dir("ubuntu-24.04@2026-11-16").unwrap().exists());
        assert!(s.dir("ubuntu-24.04@2026-11-02").unwrap().exists());
        assert!(tmp.path().join("not-an-image").exists());
    }

    fn prepared_store(tmp: &tempfile::TempDir, refs: &[&str]) -> SnapshotSource {
        for r in refs {
            let snaps = tmp.path().join(r).join("snapshots");
            fs::create_dir_all(&snaps).unwrap();
            fs::write(snaps.join(READY_MARKER), b"").unwrap();
        }
        SnapshotSource::Store(tmp.path().to_path_buf())
    }

    #[test]
    fn store_source_is_per_version_and_size() {
        let tmp = tempfile::tempdir().unwrap();
        let src = prepared_store(
            &tmp,
            &["ubuntu-24.04@2026-11-09", "ubuntu-24.04@2026-11-16"],
        );
        // Half-built version: has snapshots but no ready marker.
        fs::create_dir_all(tmp.path().join("ubuntu-24.04@2026-11-23/snapshots/2")).unwrap();

        let key = src.check("ubuntu-24.04@2026-11-09").unwrap();
        assert_eq!(
            src.dir(&key, 8).unwrap(),
            tmp.path().join("ubuntu-24.04@2026-11-09/snapshots/8")
        );
        let err = src.check("ubuntu-24.04@2026-11-23").unwrap_err();
        assert!(format!("{err:#}").contains("not prepared"), "{err:#}");
        assert!(src.check("ubuntu-24.04@2020-01-01").is_err());
        assert!(src.check("").is_err(), "no silent default image");
        assert!(src.check("../../etc@x").is_err());
        assert!(src.dir("../x@1", 2).is_err());
        // Only the newest ready version is kept warm.
        assert_eq!(
            src.warm_images(),
            vec!["ubuntu-24.04@2026-11-16".to_string()]
        );
    }

    #[test]
    fn static_source_is_backward_compatible_and_can_pin() {
        let any = SnapshotSource::Static {
            dir: "/snap".into(),
            image: None,
        };
        // Unpinned: every job maps to the one directory and one pool key.
        assert_eq!(any.check("ubuntu-24.04@2026-11-16").unwrap(), "");
        assert_eq!(any.check("").unwrap(), "");
        assert_eq!(any.dir("", 4).unwrap(), PathBuf::from("/snap/4"));
        assert_eq!(any.warm_images(), vec![String::new()]);

        let pinned = SnapshotSource::Static {
            dir: "/snap".into(),
            image: Some("ubuntu-24.04@2026-11-16".into()),
        };
        assert!(pinned.check("ubuntu-24.04@2026-11-16").is_ok());
        let err = pinned.check("ubuntu-24.04@2026-11-23").unwrap_err();
        assert!(format!("{err:#}").contains("serves only"), "{err:#}");
        assert_eq!(
            pinned.warm_images(),
            vec!["ubuntu-24.04@2026-11-16".to_string()]
        );
    }

    #[test]
    fn refs_and_paths_are_checked() {
        assert!(check_ref("ubuntu-24.04@2026-11-16").is_ok());
        for bad in ["ubuntu", "../x@1", "a@../../etc", "A@1", "a@1/2", "@1"] {
            assert!(check_ref(bad).is_err(), "{bad}");
        }
        assert!(check_rel("rootfs.ext4").is_ok());
        for bad in ["/etc/passwd", "../x", "a//b", ""] {
            assert!(check_rel(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn parses_openssl_public_key() {
        // openssl pkey -pubout for the all-7s seed, generated in-test.
        let k = key().verifying_key();
        let mut der = ED25519_SPKI_PREFIX.to_vec();
        der.extend_from_slice(k.as_bytes());
        let b64 = base64::engine::general_purpose::STANDARD.encode(&der);
        let pem = format!("-----BEGIN PUBLIC KEY-----\n{b64}\n-----END PUBLIC KEY-----\n");
        let keys = parse_public_keys(&format!("{pem}{pem}")).unwrap();
        assert_eq!(keys.len(), 2);
        assert_eq!(keys[0], k);
        assert!(parse_public_keys("nothing").is_err());
    }

    #[test]
    fn boot_args_use_in_jail_sockets() {
        let a = boot_args(&BootSpec {
            vcpu: 4,
            memory_gb: 16,
            kernel: "/img/vmlinux".into(),
            rootfs: "/img/rootfs.ext4".into(),
            cmdline: DEFAULT_CMDLINE.into(),
        });
        let s = a.join(" ");
        assert!(s.contains("--cpus boot=4"));
        assert!(s.contains("--memory size=16G"));
        assert!(s.contains("--pmem file=/img/rootfs.ext4,discard_writes=on"));
        assert!(s.contains(&format!("--vsock cid=3,socket={IN_JAIL_DIR}/vsock.sock")));
        assert!(s.contains(&format!("--api-socket path={IN_JAIL_DIR}/api.sock")));
        assert!(s.contains(&format!("--net tap=tap0,mac={GUEST_MAC}")));
    }

    #[test]
    fn version_order() {
        let mut v = vec!["2026-11-16.10", "2026-11-23", "2026-11-16", "2026-11-16.2"];
        v.sort_by_key(|x| version_key(x));
        assert_eq!(
            v,
            ["2026-11-16", "2026-11-16.2", "2026-11-16.10", "2026-11-23"]
        );
    }
}
