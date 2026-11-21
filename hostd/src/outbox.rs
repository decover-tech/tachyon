//! Job events and results waiting for the control plane's ack.
//!
//! Every event and result gets a sequence number and stays here until the
//! control plane acks it (hostlink.v1 `Ack`), so a dropped stream or a hostd
//! restart delays a result instead of losing it. After a reconnect every
//! unacked message is sent again, in order; the control plane handles
//! duplicates idempotently.
//!
//! With a state directory the outbox is kept on disk (`outbox.bin`), along
//! with one marker per running job (`running/`), so that after a crash
//! hostd can report the jobs it lost as infrastructure errors.

use anyhow::{Context, Result};
use prost::Message;
use std::collections::VecDeque;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Mutex;
use tokio::sync::watch;

use crate::pb;

/// Most messages kept while the control plane is unreachable. Past this the
/// oldest job events are dropped first, results only as a last resort.
pub const DEFAULT_CAPACITY: usize = 10_000;

const FILE: &str = "outbox.bin";
const RUNNING: &str = "running";

pub struct Outbox {
    state: Mutex<State>,
    /// Latest seq pushed; wakes the session's sender.
    pushed: watch::Sender<u64>,
    dir: Option<PathBuf>,
    capacity: usize,
}

struct State {
    next_seq: u64,
    pending: VecDeque<pb::LinkRequest>,
}

impl Default for Outbox {
    fn default() -> Self {
        Self::with_capacity(None, VecDeque::new(), DEFAULT_CAPACITY)
    }
}

impl Outbox {
    fn with_capacity(dir: Option<PathBuf>, pending: VecDeque<pb::LinkRequest>, cap: usize) -> Self {
        let last = pending.back().map_or(0, |m| m.seq);
        Self {
            state: Mutex::new(State {
                next_seq: last + 1,
                pending,
            }),
            pushed: watch::Sender::new(last),
            dir,
            capacity: cap.max(1),
        }
    }

    /// Opens (or creates) a durable outbox in `dir`, reloading whatever a
    /// previous hostd left unacked.
    pub fn open(dir: &Path) -> Result<Self> {
        fs::create_dir_all(dir.join(RUNNING))
            .with_context(|| format!("create state dir {}", dir.display()))?;
        let pending = match fs::read(dir.join(FILE)) {
            Ok(b) => decode(&b).unwrap_or_else(|e| {
                tracing::warn!(error = %e, "outbox file unreadable; starting empty");
                VecDeque::new()
            }),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => VecDeque::new(),
            Err(e) => return Err(e).context("read outbox"),
        };
        if !pending.is_empty() {
            tracing::info!(
                messages = pending.len(),
                "reloaded unacked job events and results"
            );
        }
        Ok(Self::with_capacity(
            Some(dir.to_path_buf()),
            pending,
            DEFAULT_CAPACITY,
        ))
    }

    /// Queues a message for the control plane and returns its seq.
    pub fn push(&self, msg: pb::link_request::Msg) -> u64 {
        let seq = {
            let mut st = self.state.lock().unwrap();
            let seq = st.next_seq;
            st.next_seq += 1;
            if st.pending.len() >= self.capacity {
                let victim = st
                    .pending
                    .iter()
                    .position(|m| matches!(m.msg, Some(pb::link_request::Msg::JobEvent(_))))
                    .unwrap_or(0);
                let dropped = st.pending.remove(victim);
                tracing::warn!(seq = dropped.map(|m| m.seq), "outbox full; dropped oldest");
            }
            st.pending.push_back(pb::LinkRequest {
                msg: Some(msg),
                seq,
            });
            self.persist(&st);
            seq
        };
        self.pushed.send_replace(seq);
        seq
    }

    /// Drops every message with seq <= `seq`.
    pub fn ack(&self, seq: u64) {
        let mut st = self.state.lock().unwrap();
        let before = st.pending.len();
        st.pending.retain(|m| m.seq > seq);
        if st.pending.len() != before {
            self.persist(&st);
        }
    }

    /// Unacked messages with seq > `after`, oldest first.
    pub fn pending_after(&self, after: u64) -> Vec<pb::LinkRequest> {
        let st = self.state.lock().unwrap();
        st.pending
            .iter()
            .filter(|m| m.seq > after)
            .cloned()
            .collect()
    }

    pub fn len(&self) -> usize {
        self.state.lock().unwrap().pending.len()
    }

    pub fn is_empty(&self) -> bool {
        self.len() == 0
    }

    /// Changes whenever a message is pushed.
    pub fn subscribe(&self) -> watch::Receiver<u64> {
        self.pushed.subscribe()
    }

    /// Is a result for `job_id` waiting to be acked?
    pub fn has_result(&self, job_id: &str) -> bool {
        self.state.lock().unwrap().pending.iter().any(
            |m| matches!(&m.msg, Some(pb::link_request::Msg::JobResult(r)) if r.job_id == job_id),
        )
    }

    /// Records that `job_id` is running, so a crash can be reported.
    pub fn mark_running(&self, job_id: &str) {
        if let Some(dir) = &self.dir {
            let path = dir.join(RUNNING).join(hex(job_id));
            if let Err(e) = fs::write(&path, job_id) {
                tracing::warn!(job = %job_id, error = %e, "write running marker");
            }
        }
    }

    /// Clears `job_id`'s running marker. Call after its result is pushed.
    pub fn mark_done(&self, job_id: &str) {
        if let Some(dir) = &self.dir {
            match fs::remove_file(dir.join(RUNNING).join(hex(job_id))) {
                Ok(()) => {}
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
                Err(e) => tracing::warn!(job = %job_id, error = %e, "remove running marker"),
            }
        }
    }

    /// Jobs a previous hostd was running when it stopped, sorted.
    pub fn interrupted(&self) -> Vec<String> {
        let Some(dir) = &self.dir else {
            return Vec::new();
        };
        let Ok(entries) = fs::read_dir(dir.join(RUNNING)) else {
            return Vec::new();
        };
        let mut ids: Vec<String> = entries
            .filter_map(|e| fs::read_to_string(e.ok()?.path()).ok())
            .filter(|id| !id.is_empty())
            .collect();
        ids.sort();
        ids
    }

    fn persist(&self, st: &State) {
        let Some(dir) = &self.dir else { return };
        if let Err(e) = write_atomic(&dir.join(FILE), &encode(&st.pending)) {
            tracing::warn!(error = %format!("{e:#}"), "persist outbox");
        }
    }
}

fn encode(msgs: &VecDeque<pb::LinkRequest>) -> Vec<u8> {
    let mut buf = Vec::new();
    for m in msgs {
        // Writing to a Vec cannot run out of space.
        m.encode_length_delimited(&mut buf).expect("encode to Vec");
    }
    buf
}

fn decode(mut b: &[u8]) -> Result<VecDeque<pb::LinkRequest>> {
    let mut out = VecDeque::new();
    while !b.is_empty() {
        out.push_back(pb::LinkRequest::decode_length_delimited(&mut b)?);
    }
    Ok(out)
}

fn write_atomic(path: &Path, data: &[u8]) -> Result<()> {
    use std::io::Write;
    let tmp = path.with_extension("tmp");
    let mut f = fs::File::create(&tmp).with_context(|| format!("create {}", tmp.display()))?;
    f.write_all(data)?;
    f.sync_all()?;
    fs::rename(&tmp, path).with_context(|| format!("rename to {}", path.display()))?;
    Ok(())
}

/// Lossless file name for a job id (ids may contain `/`).
fn hex(s: &str) -> String {
    s.bytes().map(|b| format!("{b:02x}")).collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use pb::link_request::Msg;

    fn event(job: &str) -> Msg {
        Msg::JobEvent(pb::JobEvent {
            job_id: job.into(),
            phase: pb::Phase::VmResumed as i32,
            at: None,
        })
    }

    fn result(job: &str) -> Msg {
        Msg::JobResult(pb::JobResult {
            job_id: job.into(),
            ..Default::default()
        })
    }

    fn seqs(v: &[pb::LinkRequest]) -> Vec<u64> {
        v.iter().map(|m| m.seq).collect()
    }

    #[test]
    fn ack_is_cumulative_and_replay_resends_unacked() {
        let o = Outbox::default();
        assert_eq!(o.push(event("j1")), 1);
        assert_eq!(o.push(event("j1")), 2);
        assert_eq!(o.push(result("j1")), 3);
        assert_eq!(seqs(&o.pending_after(1)), vec![2, 3]);
        o.ack(2);
        // A new session starts from 0 and gets everything unacked.
        assert_eq!(seqs(&o.pending_after(0)), vec![3]);
        assert!(o.has_result("j1"));
        o.ack(3);
        assert!(o.is_empty());
        assert!(!o.has_result("j1"));
        assert_eq!(o.push(event("j2")), 4, "seq never reused");
    }

    #[test]
    fn full_outbox_drops_events_before_results() {
        let o = Outbox::with_capacity(None, VecDeque::new(), 3);
        o.push(result("j1"));
        o.push(event("j2"));
        o.push(result("j2"));
        o.push(result("j3")); // drops the event (seq 2)
        assert_eq!(seqs(&o.pending_after(0)), vec![1, 3, 4]);
        o.push(result("j4")); // only results left: drops the oldest
        assert_eq!(seqs(&o.pending_after(0)), vec![3, 4, 5]);
    }

    #[test]
    fn survives_restart() {
        let tmp = tempfile::tempdir().unwrap();
        {
            let o = Outbox::open(tmp.path()).unwrap();
            o.push(event("j1"));
            o.push(result("j1"));
            o.push(event("gh/2"));
            o.ack(1);
            o.mark_running("gh/2");
            o.mark_running("j3");
            o.mark_done("j3");
        }
        let o = Outbox::open(tmp.path()).unwrap();
        let p = o.pending_after(0);
        assert_eq!(seqs(&p), vec![2, 3]);
        assert!(matches!(&p[0].msg, Some(Msg::JobResult(r)) if r.job_id == "j1"));
        assert_eq!(o.interrupted(), vec!["gh/2"]);
        assert_eq!(o.push(event("j4")), 4, "seq continues after restart");
        o.mark_done("gh/2");
        assert!(o.interrupted().is_empty());
    }

    #[test]
    fn corrupt_file_starts_empty() {
        let tmp = tempfile::tempdir().unwrap();
        fs::write(tmp.path().join(FILE), b"\xff\xff\xff").unwrap();
        let o = Outbox::open(tmp.path()).unwrap();
        assert!(o.is_empty());
        assert_eq!(o.push(event("j1")), 1);
    }

    #[test]
    fn in_memory_has_no_markers() {
        let o = Outbox::default();
        o.mark_running("j1");
        assert!(o.interrupted().is_empty());
    }

    #[tokio::test]
    async fn subscribers_wake_on_push() {
        let o = Outbox::default();
        let mut rx = o.subscribe();
        rx.borrow_and_update();
        o.push(event("j1"));
        tokio::time::timeout(std::time::Duration::from_secs(1), rx.changed())
            .await
            .unwrap()
            .unwrap();
        assert_eq!(*rx.borrow(), 1);
    }
}
