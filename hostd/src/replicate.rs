//! Snapshot replication between hosts (architecture: "Image and storage
//! layer", Distribution).
//!
//! The control plane brokers every transfer: it sends the source host a
//! `ServeSnapshot` grant carrying a single-use token and the receiving host a
//! `PullSnapshot` with the same token. The receiver dials the source's
//! `PeerSnapshotService` and presents the token and its host id; the source
//! streams the snapshot only if a matching, unexpired grant exists.
//!
//! Prebuilds and sticky volumes move as `zfs send | zfs recv` streams,
//! incremental from the newest snapshot the receiver already holds that the
//! sender can use as a base (an earlier snapshot of the same dataset, or the
//! dataset's clone origin). Base images are plain files and move whole.

use anyhow::{bail, Context, Result};
use std::collections::HashMap;
use std::fs::{self, File};
use std::io::{self, Read, Write};
use std::path::PathBuf;
use std::process::{Child, ChildStdin, ChildStdout, Command, Stdio};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tonic::{Request, Response, Status};

use crate::pb::{
    self, peer_snapshot_service_client::PeerSnapshotServiceClient,
    peer_snapshot_service_server::PeerSnapshotService,
};
use crate::storage::{check_id, CommandRunner};

/// Bytes per stream message.
const CHUNK: usize = 1 << 20;
/// Grant lifetime when the control plane sent no expiry.
const DEFAULT_GRANT_TTL: Duration = Duration::from_secs(600);

/// The receiving end of a transfer. Dropping it without `finish` discards
/// whatever was written.
pub trait Sink: Write + Send {
    fn finish(self: Box<Self>) -> Result<()>;
}

/// One kind of snapshot store that can stream snapshots out and in.
pub trait Replicator: Send + Sync {
    /// Snapshot ids held here.
    fn local(&self) -> Result<Vec<String>>;
    /// Opens a stream of `id`, incremental from the newest usable snapshot
    /// in `have`; returns the stream and the base it is incremental from.
    fn send(&self, id: &str, have: &[String]) -> Result<(Box<dyn Read + Send>, Option<String>)>;
    /// Starts receiving `id`.
    fn receive(&self, id: &str) -> Result<Box<dyn Sink>>;
}

/// Volume replicator (prebuilds, sticky heads) and base image replicator.
#[derive(Clone)]
pub struct Replicators {
    pub volumes: Arc<dyn Replicator>,
    pub images: Arc<dyn Replicator>,
}

impl Replicators {
    pub fn for_kind(&self, kind: i32) -> Result<Arc<dyn Replicator>> {
        use pb::snapshot_ref::Kind;
        match Kind::try_from(kind) {
            Ok(Kind::BaseImage) => Ok(self.images.clone()),
            Ok(Kind::Prebuild | Kind::Sticky) => Ok(self.volumes.clone()),
            _ => bail!("unsupported snapshot kind {kind}"),
        }
    }
}

// ---- grants ----

struct Grant {
    snapshot_id: String,
    to_host_id: String,
    expires: SystemTime,
}

/// Pull grants received from the control plane, keyed by token.
#[derive(Default)]
pub struct Grants {
    m: Mutex<HashMap<String, Grant>>,
}

impl Grants {
    pub fn allow(&self, g: &pb::ServeSnapshot, now: SystemTime) {
        if g.token.is_empty() {
            return;
        }
        let expires = g
            .expires_at
            .and_then(|t| SystemTime::try_from(t).ok())
            .unwrap_or(now + DEFAULT_GRANT_TTL)
            .min(now + DEFAULT_GRANT_TTL);
        let mut m = self.m.lock().unwrap();
        m.retain(|_, g| g.expires > now);
        m.insert(
            g.token.clone(),
            Grant {
                snapshot_id: g.snapshot_id.clone(),
                to_host_id: g.to_host_id.clone(),
                expires,
            },
        );
    }

    /// Consumes the grant for `token` if it covers this pull.
    pub fn take(
        &self,
        token: &str,
        snapshot_id: &str,
        host_id: &str,
        now: SystemTime,
    ) -> std::result::Result<(), &'static str> {
        let mut m = self.m.lock().unwrap();
        let Some(g) = m.get(token) else {
            return Err("no grant for this token");
        };
        if g.expires <= now {
            m.remove(token);
            return Err("grant expired");
        }
        if g.snapshot_id != snapshot_id || g.to_host_id != host_id {
            return Err("grant does not cover this pull");
        }
        m.remove(token);
        Ok(())
    }
}

// ---- streaming processes ----

/// Spawns commands whose stdout or stdin is streamed; replaced in tests.
pub trait Spawner: Send + Sync {
    fn reader(&self, program: &str, args: &[String]) -> Result<Box<dyn Read + Send>>;
    fn writer(&self, program: &str, args: &[String]) -> Result<Box<dyn Sink>>;
}

pub struct ExecSpawner;

impl Spawner for ExecSpawner {
    fn reader(&self, program: &str, args: &[String]) -> Result<Box<dyn Read + Send>> {
        let mut child = Command::new(program)
            .args(args)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .spawn()
            .with_context(|| format!("spawn {program}"))?;
        let stdout = child.stdout.take().context("child stdout")?;
        Ok(Box::new(ChildReader {
            child: Some(child),
            stdout,
            what: format!("{program} {}", args.join(" ")),
        }))
    }

    fn writer(&self, program: &str, args: &[String]) -> Result<Box<dyn Sink>> {
        let mut child = Command::new(program)
            .args(args)
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
            .spawn()
            .with_context(|| format!("spawn {program}"))?;
        let stdin = child.stdin.take().context("child stdin")?;
        Ok(Box::new(ChildWriter {
            child: Some(child),
            stdin: Some(stdin),
            what: format!("{program} {}", args.join(" ")),
        }))
    }
}

/// A child's stdout; EOF is reported only if the child exited cleanly.
struct ChildReader {
    child: Option<Child>,
    stdout: ChildStdout,
    what: String,
}

impl Read for ChildReader {
    fn read(&mut self, buf: &mut [u8]) -> io::Result<usize> {
        let n = self.stdout.read(buf)?;
        if n == 0 && !buf.is_empty() {
            if let Some(mut child) = self.child.take() {
                let status = child.wait()?;
                if !status.success() {
                    return Err(io::Error::other(format!("{}: {status}", self.what)));
                }
            }
        }
        Ok(n)
    }
}

impl Drop for ChildReader {
    fn drop(&mut self) {
        if let Some(mut c) = self.child.take() {
            let _ = c.kill();
            let _ = c.wait();
        }
    }
}

/// A child's stdin; `finish` closes it and checks the exit status, drop
/// kills the child (an interrupted `zfs recv` leaves nothing behind).
struct ChildWriter {
    child: Option<Child>,
    stdin: Option<ChildStdin>,
    what: String,
}

impl Write for ChildWriter {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        self.stdin
            .as_mut()
            .ok_or_else(|| io::Error::other("closed"))?
            .write(buf)
    }
    fn flush(&mut self) -> io::Result<()> {
        match self.stdin.as_mut() {
            Some(s) => s.flush(),
            None => Ok(()),
        }
    }
}

impl Sink for ChildWriter {
    fn finish(mut self: Box<Self>) -> Result<()> {
        drop(self.stdin.take());
        let mut child = self.child.take().context("already finished")?;
        let status = child.wait()?;
        if !status.success() {
            bail!("{}: {status}", self.what);
        }
        Ok(())
    }
}

impl Drop for ChildWriter {
    fn drop(&mut self) {
        drop(self.stdin.take());
        if let Some(mut c) = self.child.take() {
            let _ = c.kill();
            let _ = c.wait();
        }
    }
}

// ---- ZFS ----

/// Streams ZFS snapshots under `pool` with `zfs send` / `zfs recv`.
pub struct ZfsReplicator<R: CommandRunner, P: Spawner> {
    pub pool: String,
    pub runner: R,
    pub spawner: P,
}

impl<R: CommandRunner, P: Spawner> ZfsReplicator<R, P> {
    fn full(&self, id: &str) -> String {
        format!("{}/{}", self.pool, id)
    }

    fn strip<'a>(&self, name: &'a str) -> Option<&'a str> {
        name.strip_prefix(&self.pool)?.strip_prefix('/')
    }

    /// The newest snapshot in `have` that `zfs send -i` can use for `id`:
    /// an earlier snapshot of the same dataset, else the dataset's origin.
    pub fn incremental_base(&self, id: &str, have: &[String]) -> Result<Option<String>> {
        check_id(id)?;
        let (ds, _) = id
            .split_once('@')
            .with_context(|| format!("{id:?} is not a snapshot id"))?;
        let out = self.runner.run(
            "zfs",
            &[
                "list".into(),
                "-H".into(),
                "-o".into(),
                "name".into(),
                "-t".into(),
                "snapshot".into(),
                "-s".into(),
                "createtxg".into(),
                "-d".into(),
                "1".into(),
                self.full(ds),
            ],
        )?;
        let snaps: Vec<&str> = out.lines().filter_map(|l| self.strip(l.trim())).collect();
        let pos = snaps
            .iter()
            .position(|s| *s == id)
            .with_context(|| format!("no snapshot {id}"))?;
        if let Some(b) = snaps[..pos]
            .iter()
            .rev()
            .find(|s| have.iter().any(|h| h == *s))
        {
            return Ok(Some((*b).to_string()));
        }
        let origin = self.runner.run(
            "zfs",
            &[
                "get".into(),
                "-H".into(),
                "-o".into(),
                "value".into(),
                "origin".into(),
                self.full(ds),
            ],
        )?;
        Ok(self
            .strip(origin.trim())
            .filter(|o| have.iter().any(|h| h == o))
            .map(str::to_string))
    }
}

impl<R: CommandRunner, P: Spawner> Replicator for ZfsReplicator<R, P> {
    fn local(&self) -> Result<Vec<String>> {
        let out = self.runner.run(
            "zfs",
            &[
                "list".into(),
                "-H".into(),
                "-o".into(),
                "name".into(),
                "-t".into(),
                "snapshot".into(),
                "-r".into(),
                self.pool.clone(),
            ],
        )?;
        Ok(out
            .lines()
            .filter_map(|l| self.strip(l.trim()))
            .map(str::to_string)
            .collect())
    }

    fn send(&self, id: &str, have: &[String]) -> Result<(Box<dyn Read + Send>, Option<String>)> {
        let base = self.incremental_base(id, have)?;
        let mut args = vec!["send".to_string()];
        if let Some(b) = &base {
            check_id(b)?;
            args.push("-i".into());
            args.push(self.full(b));
        }
        args.push(self.full(id));
        Ok((self.spawner.reader("zfs", &args)?, base))
    }

    fn receive(&self, id: &str) -> Result<Box<dyn Sink>> {
        check_id(id)?;
        if !id.contains('@') {
            bail!("{id:?} is not a snapshot id");
        }
        // -u: do not mount; an incremental stream from a clone origin is
        // received as a clone of that origin.
        self.spawner
            .writer("zfs", &["recv".into(), "-u".into(), self.full(id)])
    }
}

// ---- base images ----

/// Base images as files `<dir>/<name@version>.img`.
pub struct FileImages {
    pub dir: PathBuf,
}

fn check_image(id: &str) -> Result<()> {
    let ok = !id.is_empty()
        && !id.starts_with('.')
        && !id.contains("..")
        && id
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || "_.@-".contains(c));
    if !ok {
        bail!("invalid image id {id:?}");
    }
    Ok(())
}

impl FileImages {
    fn path(&self, id: &str) -> Result<PathBuf> {
        check_image(id)?;
        Ok(self.dir.join(format!("{id}.img")))
    }
}

impl Replicator for FileImages {
    fn local(&self) -> Result<Vec<String>> {
        let mut out = Vec::new();
        let entries = match fs::read_dir(&self.dir) {
            Ok(e) => e,
            Err(e) if e.kind() == io::ErrorKind::NotFound => return Ok(out),
            Err(e) => return Err(e).with_context(|| format!("read {}", self.dir.display())),
        };
        for e in entries {
            let name = e?.file_name();
            if let Some(id) = name.to_str().and_then(|n| n.strip_suffix(".img")) {
                if check_image(id).is_ok() {
                    out.push(id.to_string());
                }
            }
        }
        out.sort();
        Ok(out)
    }

    fn send(&self, id: &str, _have: &[String]) -> Result<(Box<dyn Read + Send>, Option<String>)> {
        let p = self.path(id)?;
        let f = File::open(&p).with_context(|| format!("open {}", p.display()))?;
        Ok((Box::new(f), None))
    }

    fn receive(&self, id: &str) -> Result<Box<dyn Sink>> {
        let dst = self.path(id)?;
        fs::create_dir_all(&self.dir)?;
        let tmp = self.dir.join(format!(".{id}.img.partial"));
        let file = File::create(&tmp).with_context(|| format!("create {}", tmp.display()))?;
        Ok(Box::new(FileSink {
            file,
            tmp,
            dst,
            done: false,
        }))
    }
}

struct FileSink {
    file: File,
    tmp: PathBuf,
    dst: PathBuf,
    done: bool,
}

impl Write for FileSink {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        self.file.write(buf)
    }
    fn flush(&mut self) -> io::Result<()> {
        self.file.flush()
    }
}

impl Sink for FileSink {
    fn finish(mut self: Box<Self>) -> Result<()> {
        self.file.sync_all()?;
        fs::rename(&self.tmp, &self.dst)?;
        self.done = true;
        Ok(())
    }
}

impl Drop for FileSink {
    fn drop(&mut self) {
        if !self.done {
            let _ = fs::remove_file(&self.tmp);
        }
    }
}

/// Base images present on this host, as inventory entries.
pub fn image_refs(images: &dyn Replicator) -> Result<Vec<pb::SnapshotRef>> {
    Ok(images
        .local()?
        .into_iter()
        .map(|id| pb::SnapshotRef {
            kind: pb::snapshot_ref::Kind::BaseImage as i32,
            repo: String::new(),
            key: id.clone(),
            id,
        })
        .collect())
}

// ---- peer service ----

/// Serves snapshot pulls to peers holding a grant.
pub struct PeerServer {
    pub grants: Arc<Grants>,
    pub repl: Replicators,
}

type FetchStream = ReceiverStream<std::result::Result<pb::FetchSnapshotResponse, Status>>;

#[tonic::async_trait]
impl PeerSnapshotService for PeerServer {
    type FetchSnapshotStream = FetchStream;

    async fn fetch_snapshot(
        &self,
        req: Request<pb::FetchSnapshotRequest>,
    ) -> std::result::Result<Response<FetchStream>, Status> {
        let r = req.into_inner();
        let snap = r
            .snapshot
            .ok_or_else(|| Status::invalid_argument("snapshot is required"))?;
        self.grants
            .take(&r.token, &snap.id, &r.host_id, SystemTime::now())
            .map_err(Status::permission_denied)?;
        let repl = self
            .repl
            .for_kind(snap.kind)
            .map_err(|e| Status::invalid_argument(e.to_string()))?;
        let (id, have) = (snap.id.clone(), r.have);
        let (reader, base) = tokio::task::spawn_blocking(move || repl.send(&id, &have))
            .await
            .map_err(|e| Status::internal(e.to_string()))?
            .map_err(|e| Status::failed_precondition(format!("{e:#}")))?;
        tracing::info!(snapshot = %snap.id, to = %r.host_id, base = ?base, "serving snapshot");
        let (tx, rx) = mpsc::channel(4);
        tokio::task::spawn_blocking(move || pump(reader, base, &tx));
        Ok(Response::new(ReceiverStream::new(rx)))
    }
}

fn pump(
    mut reader: Box<dyn Read + Send>,
    base: Option<String>,
    tx: &mpsc::Sender<std::result::Result<pb::FetchSnapshotResponse, Status>>,
) {
    let mut first = Some(base.unwrap_or_default());
    let mut buf = vec![0u8; CHUNK];
    loop {
        let msg = match reader.read(&mut buf) {
            Ok(0) => return,
            Ok(n) => Ok(pb::FetchSnapshotResponse {
                data: buf[..n].to_vec(),
                incremental_from: first.take().unwrap_or_default(),
            }),
            Err(e) if e.kind() == io::ErrorKind::Interrupted => continue,
            Err(e) => Err(Status::internal(e.to_string())),
        };
        let failed = msg.is_err();
        // A closed channel means the peer went away; dropping the reader
        // stops the sender process.
        if tx.blocking_send(msg).is_err() || failed {
            return;
        }
    }
}

/// How a pull retries: the grant may reach the source a moment after the
/// pull reaches us.
#[derive(Clone, Debug)]
pub struct PullOptions {
    pub attempts: u32,
    pub backoff: Duration,
}

impl Default for PullOptions {
    fn default() -> Self {
        Self {
            attempts: 5,
            backoff: Duration::from_secs(1),
        }
    }
}

/// Pulls the snapshot named in `p` from its peer. Returns the pulled ref,
/// ready for the inventory. A no-op if the snapshot is already here.
pub async fn pull(
    host_id: &str,
    p: &pb::PullSnapshot,
    repl: &Replicators,
    opts: &PullOptions,
) -> Result<pb::SnapshotRef> {
    let snap = p
        .snapshot
        .clone()
        .context("PullSnapshot without snapshot")?;
    if p.from_address.is_empty() {
        bail!("PullSnapshot without from_address");
    }
    let r = repl.for_kind(snap.kind)?;
    let have = {
        let r = r.clone();
        tokio::task::spawn_blocking(move || r.local()).await??
    };
    if have.contains(&snap.id) {
        return Ok(snap);
    }
    let req = pb::FetchSnapshotRequest {
        snapshot: Some(snap.clone()),
        token: p.token.clone(),
        host_id: host_id.to_string(),
        have,
    };
    let mut attempt = 0;
    loop {
        attempt += 1;
        match fetch_once(&p.from_address, req.clone(), r.clone()).await {
            Ok(()) => return Ok(snap),
            Err(e) if attempt < opts.attempts => {
                tracing::warn!(snapshot = %snap.id, attempt, error = %format!("{e:#}"), "snapshot pull failed; retrying");
                tokio::time::sleep(opts.backoff).await;
            }
            Err(e) => return Err(e.context(format!("pull {} from {}", snap.id, p.from_host_id))),
        }
    }
}

enum Chunk {
    Data(Vec<u8>),
    End,
}

async fn fetch_once(
    addr: &str,
    req: pb::FetchSnapshotRequest,
    r: Arc<dyn Replicator>,
) -> Result<()> {
    let id = req
        .snapshot
        .as_ref()
        .map(|s| s.id.clone())
        .unwrap_or_default();
    let mut client = PeerSnapshotServiceClient::connect(addr.to_string()).await?;
    let mut stream = client.fetch_snapshot(req).await?.into_inner();

    let (tx, mut rx) = mpsc::channel::<Chunk>(4);
    let writer = tokio::task::spawn_blocking(move || -> Result<()> {
        let mut sink = r.receive(&id)?;
        while let Some(c) = rx.blocking_recv() {
            match c {
                Chunk::Data(d) => sink.write_all(&d)?,
                Chunk::End => return sink.finish(),
            }
        }
        // The stream broke before its end: dropping the sink discards it.
        bail!("transfer interrupted")
    });

    let mut res = Ok(());
    loop {
        match stream.message().await {
            Ok(Some(m)) => {
                if !m.incremental_from.is_empty() {
                    tracing::info!(base = %m.incremental_from, "incremental snapshot stream");
                }
                if tx.send(Chunk::Data(m.data)).await.is_err() {
                    break; // the writer failed; its error is returned below
                }
            }
            Ok(None) => {
                let _ = tx.send(Chunk::End).await;
                break;
            }
            Err(e) => {
                res = Err(anyhow::Error::from(e));
                break;
            }
        }
    }
    drop(tx);
    let written = writer.await?;
    res?;
    written
}

#[cfg(test)]
pub mod fake {
    use super::*;
    use std::io::Cursor;

    /// In-memory replicator: snapshots are byte strings.
    #[derive(Default)]
    pub struct FakeReplicator {
        pub snaps: Mutex<HashMap<String, Vec<u8>>>,
        /// (id, base) for every send.
        pub sent: Mutex<Vec<(String, Option<String>)>>,
    }

    impl FakeReplicator {
        pub fn with(id: &str, data: &[u8]) -> Self {
            let f = Self::default();
            f.snaps.lock().unwrap().insert(id.into(), data.to_vec());
            f
        }
    }

    struct FakeSink {
        id: String,
        buf: Vec<u8>,
        into: Arc<FakeReplicator>,
    }

    impl Write for FakeSink {
        fn write(&mut self, b: &[u8]) -> io::Result<usize> {
            self.buf.extend_from_slice(b);
            Ok(b.len())
        }
        fn flush(&mut self) -> io::Result<()> {
            Ok(())
        }
    }

    impl Sink for FakeSink {
        fn finish(self: Box<Self>) -> Result<()> {
            self.into
                .snaps
                .lock()
                .unwrap()
                .insert(self.id.clone(), self.buf.clone());
            Ok(())
        }
    }

    impl Replicator for Arc<FakeReplicator> {
        fn local(&self) -> Result<Vec<String>> {
            let mut v: Vec<_> = self.snaps.lock().unwrap().keys().cloned().collect();
            v.sort();
            Ok(v)
        }
        fn send(
            &self,
            id: &str,
            have: &[String],
        ) -> Result<(Box<dyn Read + Send>, Option<String>)> {
            let data = self
                .snaps
                .lock()
                .unwrap()
                .get(id)
                .cloned()
                .with_context(|| format!("no snapshot {id}"))?;
            let base = have.first().cloned();
            self.sent.lock().unwrap().push((id.into(), base.clone()));
            Ok((Box::new(Cursor::new(data)), base))
        }
        fn receive(&self, id: &str) -> Result<Box<dyn Sink>> {
            Ok(Box::new(FakeSink {
                id: id.into(),
                buf: Vec::new(),
                into: self.clone(),
            }))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::fake::FakeReplicator;
    use super::*;
    use std::collections::VecDeque;

    fn grant(token: &str, snap: &str, to: &str) -> pb::ServeSnapshot {
        pb::ServeSnapshot {
            token: token.into(),
            snapshot_id: snap.into(),
            to_host_id: to.into(),
            expires_at: None,
        }
    }

    #[test]
    fn grants_are_single_use_and_scoped() {
        let g = Grants::default();
        let now = SystemTime::now();
        g.allow(&grant("t1", "jobs/a@k", "h2"), now);
        assert!(g.take("t1", "jobs/a@k", "h3", now).is_err(), "wrong host");
        assert!(
            g.take("t1", "jobs/b@k", "h2", now).is_err(),
            "wrong snapshot"
        );
        assert!(
            g.take("nope", "jobs/a@k", "h2", now).is_err(),
            "unknown token"
        );
        assert!(g.take("t1", "jobs/a@k", "h2", now).is_ok());
        assert!(g.take("t1", "jobs/a@k", "h2", now).is_err(), "reused");

        g.allow(&grant("t2", "jobs/a@k", "h2"), now);
        let later = now + DEFAULT_GRANT_TTL + Duration::from_secs(1);
        assert!(g.take("t2", "jobs/a@k", "h2", later).is_err(), "expired");

        // An explicit expiry is honoured, but capped.
        let mut g3 = grant("t3", "jobs/a@k", "h2");
        g3.expires_at = Some(prost_types::Timestamp::from(now + Duration::from_secs(5)));
        g.allow(&g3, now);
        assert!(g
            .take("t3", "jobs/a@k", "h2", now + Duration::from_secs(6))
            .is_err());
        g.allow(&grant("", "jobs/a@k", "h2"), now);
        assert!(g.take("", "jobs/a@k", "h2", now).is_err(), "empty token");
    }

    /// Scripted `zfs` metadata commands; records every call.
    #[derive(Default)]
    struct Script {
        replies: Mutex<VecDeque<String>>,
        calls: Mutex<Vec<String>>,
    }
    impl CommandRunner for &Script {
        fn run(&self, program: &str, args: &[String]) -> Result<String> {
            self.calls
                .lock()
                .unwrap()
                .push(format!("{program} {}", args.join(" ")));
            Ok(self.replies.lock().unwrap().pop_front().unwrap_or_default())
        }
    }

    #[derive(Default)]
    struct Spawns(Mutex<Vec<String>>);
    impl Spawner for &Spawns {
        fn reader(&self, program: &str, args: &[String]) -> Result<Box<dyn Read + Send>> {
            self.0
                .lock()
                .unwrap()
                .push(format!("{program} {}", args.join(" ")));
            Ok(Box::new(io::Cursor::new(b"stream".to_vec())))
        }
        fn writer(&self, program: &str, args: &[String]) -> Result<Box<dyn Sink>> {
            self.0
                .lock()
                .unwrap()
                .push(format!("{program} {}", args.join(" ")));
            struct Null;
            impl Write for Null {
                fn write(&mut self, b: &[u8]) -> io::Result<usize> {
                    Ok(b.len())
                }
                fn flush(&mut self) -> io::Result<()> {
                    Ok(())
                }
            }
            impl Sink for Null {
                fn finish(self: Box<Self>) -> Result<()> {
                    Ok(())
                }
            }
            Ok(Box::new(Null))
        }
    }

    fn zfs<'a>(s: &'a Script, p: &'a Spawns) -> ZfsReplicator<&'a Script, &'a Spawns> {
        ZfsReplicator {
            pool: "tank/t".into(),
            runner: s,
            spawner: p,
        }
    }

    fn script(replies: &[&str]) -> Script {
        let s = Script::default();
        *s.replies.lock().unwrap() = replies.iter().map(|r| r.to_string()).collect();
        s
    }

    #[test]
    fn zfs_send_incremental_from_same_dataset() {
        let s = script(&[
            "tank/t/sticky/a/docker@h1\ntank/t/sticky/a/docker@h2\ntank/t/sticky/a/docker@h3\n",
        ]);
        let p = Spawns::default();
        let z = zfs(&s, &p);
        let have = vec!["sticky/a/docker@h1".to_string(), "other@x".into()];
        let (_, base) = z.send("sticky/a/docker@h3", &have).unwrap();
        assert_eq!(base.as_deref(), Some("sticky/a/docker@h1"));
        assert_eq!(
            *p.0.lock().unwrap(),
            vec!["zfs send -i tank/t/sticky/a/docker@h1 tank/t/sticky/a/docker@h3"]
        );
        assert_eq!(
            s.calls.lock().unwrap()[0],
            "zfs list -H -o name -t snapshot -s createtxg -d 1 tank/t/sticky/a/docker"
        );
    }

    #[test]
    fn zfs_send_incremental_from_clone_origin() {
        // jobs/j2 was forked from the previous prebuild jobs/j1@pb1.
        let s = script(&["tank/t/jobs/j2@pb2\n", "tank/t/jobs/j1@pb1\n"]);
        let p = Spawns::default();
        let z = zfs(&s, &p);
        let (_, base) = z.send("jobs/j2@pb2", &["jobs/j1@pb1".to_string()]).unwrap();
        assert_eq!(base.as_deref(), Some("jobs/j1@pb1"));
        assert_eq!(
            s.calls.lock().unwrap()[1],
            "zfs get -H -o value origin tank/t/jobs/j2"
        );
        assert_eq!(
            *p.0.lock().unwrap(),
            vec!["zfs send -i tank/t/jobs/j1@pb1 tank/t/jobs/j2@pb2"]
        );
    }

    #[test]
    fn zfs_send_full_without_common_base() {
        let s = script(&["tank/t/jobs/j2@pb2\n", "-\n"]);
        let p = Spawns::default();
        let z = zfs(&s, &p);
        let (_, base) = z.send("jobs/j2@pb2", &["jobs/j1@pb1".into()]).unwrap();
        assert_eq!(base, None);
        assert_eq!(*p.0.lock().unwrap(), vec!["zfs send tank/t/jobs/j2@pb2"]);
    }

    #[test]
    fn zfs_send_rejects_missing_and_bad_ids() {
        let s = script(&["tank/t/jobs/j2@other\n"]);
        let p = Spawns::default();
        let z = zfs(&s, &p);
        assert!(z.send("jobs/j2@pb2", &[]).is_err(), "not on this host");
        assert!(z.send("jobs/j2", &[]).is_err(), "not a snapshot");
        assert!(z.send("../x@y", &[]).is_err());
        assert!(z.receive("jobs/a;rm@x").is_err());
        assert!(z.receive("jobs/a").is_err());
        assert!(p.0.lock().unwrap().is_empty());
    }

    #[test]
    fn zfs_receive_and_local() {
        let s = script(&["tank/t/jobs/j1@pb1\ntank/t/sticky/a/docker@h1\n"]);
        let p = Spawns::default();
        let z = zfs(&s, &p);
        assert_eq!(
            z.local().unwrap(),
            vec!["jobs/j1@pb1", "sticky/a/docker@h1"]
        );
        z.receive("jobs/j2@pb2").unwrap().finish().unwrap();
        assert_eq!(*p.0.lock().unwrap(), vec!["zfs recv -u tank/t/jobs/j2@pb2"]);
    }

    #[test]
    fn file_images_roundtrip() {
        let dir = tempfile::tempdir().unwrap();
        let imgs = FileImages {
            dir: dir.path().join("images"),
        };
        assert!(imgs.local().unwrap().is_empty());

        // An interrupted receive leaves nothing behind.
        let mut s = imgs.receive("ubuntu-24.04@v2").unwrap();
        s.write_all(b"partial").unwrap();
        drop(s);
        assert!(imgs.local().unwrap().is_empty());
        assert_eq!(fs::read_dir(&imgs.dir).unwrap().count(), 0);

        let mut s = imgs.receive("ubuntu-24.04@v2").unwrap();
        s.write_all(b"image bytes").unwrap();
        s.finish().unwrap();
        assert_eq!(imgs.local().unwrap(), vec!["ubuntu-24.04@v2"]);

        let (mut r, base) = imgs.send("ubuntu-24.04@v2", &[]).unwrap();
        let mut got = Vec::new();
        r.read_to_end(&mut got).unwrap();
        assert_eq!(got, b"image bytes");
        assert_eq!(base, None);

        assert!(imgs.receive("../etc/passwd").is_err());
        assert!(imgs.send("a/b", &[]).is_err());
        let refs = image_refs(&imgs).unwrap();
        assert_eq!(refs[0].key, "ubuntu-24.04@v2");
        assert_eq!(refs[0].kind, pb::snapshot_ref::Kind::BaseImage as i32);
    }

    #[test]
    fn exec_spawner_reports_child_failure() {
        let sp = ExecSpawner;
        let mut out = String::new();
        sp.reader("sh", &["-c".into(), "printf hello".into()])
            .unwrap()
            .read_to_string(&mut out)
            .unwrap();
        assert_eq!(out, "hello");
        let mut r = sp.reader("sh", &["-c".into(), "exit 3".into()]).unwrap();
        assert!(r.read_to_end(&mut Vec::new()).is_err());

        let dir = tempfile::tempdir().unwrap();
        let f = dir.path().join("out");
        let mut w = sp
            .writer("sh", &["-c".into(), format!("cat > {}", f.display())])
            .unwrap();
        w.write_all(b"data").unwrap();
        w.finish().unwrap();
        assert_eq!(fs::read(&f).unwrap(), b"data");
        let w = sp.writer("sh", &["-c".into(), "exit 1".into()]).unwrap();
        assert!(w.finish().is_err());
    }

    fn repl(f: &Arc<FakeReplicator>) -> Replicators {
        Replicators {
            volumes: Arc::new(f.clone()),
            images: Arc::new(f.clone()),
        }
    }

    fn prebuild(id: &str) -> pb::SnapshotRef {
        pb::SnapshotRef {
            kind: pb::snapshot_ref::Kind::Prebuild as i32,
            repo: "acme/api".into(),
            key: "pb2".into(),
            id: id.into(),
        }
    }

    #[tokio::test]
    async fn pull_from_peer_end_to_end() {
        use crate::pb::peer_snapshot_service_server::PeerSnapshotServiceServer;

        // Source holds the old and new prebuild; the receiver only the old.
        let payload: Vec<u8> = (0..3 * CHUNK + 17).map(|i| (i % 251) as u8).collect();
        let src = Arc::new(FakeReplicator::with("jobs/j2@pb2", &payload));
        src.snaps
            .lock()
            .unwrap()
            .insert("jobs/j1@pb1".into(), b"old".to_vec());
        let dst = Arc::new(FakeReplicator::with("jobs/j1@pb1", b"old"));

        let grants = Arc::new(Grants::default());
        let server = PeerServer {
            grants: grants.clone(),
            repl: repl(&src),
        };
        let lis = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = format!("http://{}", lis.local_addr().unwrap());
        tokio::spawn(
            tonic::transport::Server::builder()
                .add_service(PeerSnapshotServiceServer::new(server))
                .serve_with_incoming(tokio_stream::wrappers::TcpListenerStream::new(lis)),
        );

        let opts = PullOptions {
            attempts: 2,
            backoff: Duration::from_millis(10),
        };
        let p = pb::PullSnapshot {
            snapshot: Some(prebuild("jobs/j2@pb2")),
            from_host_id: "h1".into(),
            from_address: addr.clone(),
            token: "tok".into(),
        };

        // No grant yet: refused, nothing received.
        assert!(pull("h2", &p, &repl(&dst), &opts).await.is_err());
        assert!(!dst.snaps.lock().unwrap().contains_key("jobs/j2@pb2"));

        grants.allow(&grant("tok", "jobs/j2@pb2", "h2"), SystemTime::now());
        let got = pull("h2", &p, &repl(&dst), &opts).await.unwrap();
        assert_eq!(got.id, "jobs/j2@pb2");
        assert_eq!(dst.snaps.lock().unwrap()["jobs/j2@pb2"], payload);
        // The receiver's snapshots were offered as incremental bases.
        assert_eq!(
            src.sent.lock().unwrap().last().unwrap(),
            &("jobs/j2@pb2".to_string(), Some("jobs/j1@pb1".to_string()))
        );

        // The token was single-use; a second host cannot reuse it.
        let other = Arc::new(FakeReplicator::default());
        assert!(pull("h2", &p, &repl(&other), &opts).await.is_err());

        // Already present: no transfer at all.
        let sends = src.sent.lock().unwrap().len();
        pull("h2", &p, &repl(&dst), &opts).await.unwrap();
        assert_eq!(src.sent.lock().unwrap().len(), sends);
    }
}
