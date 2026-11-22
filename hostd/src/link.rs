//! The hostd side of hostlink.v1: dial the control plane, keep one stream
//! open, heartbeat, run assigned jobs and report their results. If the
//! stream drops, running jobs keep running and hostd reconnects. Job events
//! and results go through the outbox and are resent until acked.

use anyhow::Result;
use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;

use crate::hypervisor::Hypervisor;
use crate::lifecycle::Agent;
use crate::outbox::Outbox;
use crate::pb::{self, host_link_service_client::HostLinkServiceClient};
use crate::replicate;
use crate::storage::Storage;

#[derive(Debug, Clone)]
pub struct HostInfo {
    pub host_id: String,
    pub region: String,
    pub failure_domain: String,
    pub arch: String,
    pub vcpu: u32,
    pub memory_gb: u32,
    /// Advertised address of this host's peer snapshot service; empty if
    /// it serves no pulls.
    pub peer_address: String,
}

/// Snapshot replication state shared by the link and the peer server.
pub struct Peer {
    pub grants: Arc<replicate::Grants>,
    pub repl: replicate::Replicators,
    pub opts: replicate::PullOptions,
}

/// Snapshots this host holds, reported on every heartbeat.
#[derive(Default)]
pub struct Inventory {
    refs: Mutex<Vec<pb::SnapshotRef>>,
}

impl Inventory {
    /// Adds a snapshot, replacing an older one of the same kind/repo/key
    /// (prebuilds are replaced per repo, sticky heads per repo+key).
    pub fn put(&self, r: pb::SnapshotRef) {
        let mut refs = self.refs.lock().unwrap();
        let prebuild = pb::snapshot_ref::Kind::Prebuild as i32;
        refs.retain(|x| {
            !(x.kind == r.kind && x.repo == r.repo && (r.kind == prebuild || x.key == r.key))
        });
        refs.push(r);
    }
    pub fn list(&self) -> Vec<pb::SnapshotRef> {
        self.refs.lock().unwrap().clone()
    }
}

pub struct Link<H: Hypervisor + 'static, S: Storage + 'static> {
    pub agent: Arc<Agent<H, S>>,
    pub info: HostInfo,
    pub control_plane: String,
    pub inventory: Arc<Inventory>,
    pub draining: Arc<AtomicBool>,
    /// Snapshot replication; None ignores pull requests and grants.
    pub peer: Option<Arc<Peer>>,
    /// Job events and results not yet acked by the control plane.
    pub outbox: Arc<Outbox>,
    running: Arc<Mutex<HashMap<String, ()>>>,
}

impl<H: Hypervisor + 'static, S: Storage + 'static> Link<H, S>
where
    H::Vm: 'static,
{
    pub fn new(agent: Arc<Agent<H, S>>, info: HostInfo, control_plane: String) -> Self {
        Self {
            agent,
            info,
            control_plane,
            inventory: Arc::default(),
            draining: Arc::default(),
            peer: None,
            outbox: Arc::default(),
            running: Arc::default(),
        }
    }

    /// Runs forever, reconnecting with backoff.
    pub async fn run(&self) {
        let mut backoff = Duration::from_millis(500);
        loop {
            match self.session().await {
                Ok(()) => backoff = Duration::from_millis(500),
                Err(e) => tracing::warn!(error = %format!("{e:#}"), "control plane link lost"),
            }
            tokio::time::sleep(backoff).await;
            backoff = (backoff * 2).min(Duration::from_secs(30));
        }
    }

    async fn session(&self) -> Result<()> {
        let mut client = HostLinkServiceClient::connect(self.control_plane.clone()).await?;
        let (tx, rx) = mpsc::channel::<pb::LinkRequest>(256);
        let running: Vec<String> = self.running.lock().unwrap().keys().cloned().collect();
        tx.send(msg(pb::link_request::Msg::Hello(pb::Hello {
            host_id: self.info.host_id.clone(),
            region: self.info.region.clone(),
            failure_domain: self.info.failure_domain.clone(),
            arch: self.info.arch.clone(),
            vcpu: self.info.vcpu,
            memory_gb: self.info.memory_gb,
            hostd_version: env!("CARGO_PKG_VERSION").into(),
            running_job_ids: running,
            peer_address: self.info.peer_address.clone(),
        })))
        .await?;
        let mut inbound = client.link(ReceiverStream::new(rx)).await?.into_inner();
        tracing::info!(control_plane = %self.control_plane, "linked");

        let hb = {
            let (tx, agent, inv, draining) = (
                tx.clone(),
                self.agent.clone(),
                self.inventory.clone(),
                self.draining.clone(),
            );
            tokio::spawn(async move {
                let mut tick = tokio::time::interval(Duration::from_secs(5));
                loop {
                    tick.tick().await;
                    let beat = pb::Heartbeat {
                        at: Some(now()),
                        warm_vms_by_vcpu: agent.warm_counts(),
                        snapshots: inv.list(),
                        draining: draining.load(Ordering::Relaxed),
                        ..Default::default()
                    };
                    if tx
                        .send(msg(pb::link_request::Msg::Heartbeat(beat)))
                        .await
                        .is_err()
                    {
                        return;
                    }
                }
            })
        };

        let sweep = {
            let agent = self.agent.clone();
            tokio::spawn(async move {
                let mut tick = tokio::time::interval(crate::prefork::SWEEP_INTERVAL);
                loop {
                    tick.tick().await;
                    let agent = agent.clone();
                    let _ = tokio::task::spawn_blocking(move || {
                        let n = agent.expire_pre_forks(SystemTime::now());
                        if n > 0 {
                            tracing::info!(count = n, "expired pre-forks destroyed");
                        }
                    })
                    .await;
                }
            })
        };
        // Replays everything unacked, then sends new events and results.
        let fwd = tokio::spawn(forward(self.outbox.clone(), tx.clone()));

        let result = async {
            while let Some(m) = inbound.message().await? {
                match m.msg {
                    Some(pb::link_response::Msg::Assign(job)) => {
                        self.start_job(job);
                    }
                    Some(pb::link_response::Msg::Ack(a)) => self.outbox.ack(a.seq),
                    Some(pb::link_response::Msg::Drain(d)) => {
                        self.draining.store(d.drain, Ordering::Relaxed);
                        tracing::info!(drain = d.drain, "drain state changed");
                    }
                    Some(pb::link_response::Msg::Cancel(c)) => {
                        // The job's own task reports OUTCOME_CANCELLED once
                        // the VM is gone. Signalling the guest is a small
                        // vsock write, but keep it off the reactor anyway.
                        let agent = self.agent.clone();
                        tokio::task::spawn_blocking(move || {
                            if agent.cancel(&c.job_id) {
                                tracing::info!(job = %c.job_id, "cancelling");
                            } else {
                                tracing::warn!(job = %c.job_id, "cancel for unknown job");
                            }
                        });
                    }
                    Some(pb::link_response::Msg::PreFork(p)) => self.start_pre_fork(p),
                    Some(pb::link_response::Msg::PullSnapshot(p)) => self.pull_snapshot(p),
                    Some(pb::link_response::Msg::ServeSnapshot(g)) => match &self.peer {
                        Some(peer) => peer.grants.allow(&g, SystemTime::now()),
                        None => tracing::warn!("snapshot grant ignored: peer service disabled"),
                    },
                    None => {}
                }
            }
            Ok(())
        }
        .await;
        hb.abort();
        sweep.abort();
        fwd.abort();
        result
    }

    /// Clones a job disk for a hinted repo/commit off the async runtime.
    /// Failures only cost the later job its head start.
    fn start_pre_fork(&self, p: pb::PreFork) {
        let agent = self.agent.clone();
        tokio::task::spawn_blocking(move || match agent.pre_fork(&p, SystemTime::now()) {
            Ok(()) => tracing::debug!(repo = %p.repo, commit = %p.commit, "pre-forked job disk"),
            Err(e) => tracing::warn!(repo = %p.repo, error = %format!("{e:#}"), "pre-fork"),
        });
    }

    /// Pulls a snapshot from a peer in the background and adds it to the
    /// inventory, which tells the control plane on the next heartbeat.
    fn pull_snapshot(&self, p: pb::PullSnapshot) {
        let Some(peer) = self.peer.clone() else {
            tracing::warn!(from = %p.from_host_id, "snapshot pull ignored: peer service disabled");
            return;
        };
        let (inv, host_id) = (self.inventory.clone(), self.info.host_id.clone());
        tokio::spawn(async move {
            match replicate::pull(&host_id, &p, &peer.repl, &peer.opts).await {
                Ok(r) => {
                    tracing::info!(snapshot = %r.id, from = %p.from_host_id, "snapshot pulled");
                    inv.put(r);
                }
                Err(e) => tracing::warn!(error = %format!("{e:#}"), "snapshot pull failed"),
            }
        });
    }

    fn start_job(&self, job: pb::AssignJob) -> tokio::task::JoinHandle<()> {
        let job_id = job.job_id.clone();
        self.running.lock().unwrap().insert(job_id.clone(), ());
        self.outbox.mark_running(&job_id);
        let (agent, inv, running, outbox) = (
            self.agent.clone(),
            self.inventory.clone(),
            self.running.clone(),
            self.outbox.clone(),
        );
        tracing::info!(job = %job_id, repo = %job.repo, vcpu = job.vcpu, "assigned");
        tokio::task::spawn_blocking(move || {
            let ev_outbox = outbox.clone();
            let ev_job = job_id.clone();
            let events = move |phase: pb::Phase| {
                let e = pb::JobEvent {
                    job_id: ev_job.clone(),
                    phase: phase as i32,
                    at: Some(now()),
                };
                ev_outbox.push(pb::link_request::Msg::JobEvent(e));
            };
            let result = agent.run_job(&job, &events);
            if let Some(p) = &result.prebuild {
                inv.put(p.clone());
            }
            for p in &result.promoted {
                inv.put(p.clone());
            }
            running.lock().unwrap().remove(&job_id);
            tracing::info!(job = %job_id, outcome = ?result.outcome(), "finished");
            // Kept until acked, across reconnects and restarts.
            outbox.push(pb::link_request::Msg::JobResult(result));
            outbox.mark_done(&job_id);
            if let Err(e) = agent.refill_pool() {
                tracing::warn!(error = %format!("{e:#}"), "refill warm pool");
            }
        })
    }
}

/// Sends every unacked outbox message on this session's stream, oldest
/// first, then each new one as it is pushed. Returns when the stream closes.
async fn forward(outbox: Arc<Outbox>, tx: mpsc::Sender<pb::LinkRequest>) {
    let mut pushed = outbox.subscribe();
    let mut sent = 0;
    loop {
        pushed.borrow_and_update();
        for m in outbox.pending_after(sent) {
            sent = m.seq;
            if tx.send(m).await.is_err() {
                return;
            }
        }
        if pushed.changed().await.is_err() {
            return;
        }
    }
}

fn msg(m: pb::link_request::Msg) -> pb::LinkRequest {
    pb::LinkRequest {
        msg: Some(m),
        seq: 0,
    }
}

fn now() -> prost_types::Timestamp {
    prost_types::Timestamp::from(SystemTime::now())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::hypervisor::fake::FakeHypervisor;
    use crate::storage::fake::FakeStorage;

    fn event(job: &str) -> pb::link_request::Msg {
        pb::link_request::Msg::JobEvent(pb::JobEvent {
            job_id: job.into(),
            ..Default::default()
        })
    }

    async fn recv(rx: &mut mpsc::Receiver<pb::LinkRequest>) -> u64 {
        tokio::time::timeout(Duration::from_secs(2), rx.recv())
            .await
            .expect("forwarded")
            .expect("open")
            .seq
    }

    #[tokio::test]
    async fn forward_replays_unacked_after_reconnect() {
        let outbox = Arc::new(Outbox::default());
        outbox.push(event("j1")); // queued while disconnected
        outbox.push(event("j1"));

        let (tx, mut rx) = mpsc::channel(16);
        let fwd = tokio::spawn(forward(outbox.clone(), tx));
        assert_eq!(recv(&mut rx).await, 1);
        assert_eq!(recv(&mut rx).await, 2);
        outbox.push(event("j2")); // pushed while linked
        assert_eq!(recv(&mut rx).await, 3);
        outbox.ack(1);
        fwd.abort(); // stream lost before 2 and 3 were acked

        let (tx, mut rx) = mpsc::channel(16);
        let fwd = tokio::spawn(forward(outbox.clone(), tx));
        assert_eq!(recv(&mut rx).await, 2);
        assert_eq!(recv(&mut rx).await, 3);
        fwd.abort();
    }

    #[tokio::test]
    async fn forward_stops_when_stream_closes() {
        let outbox = Arc::new(Outbox::default());
        outbox.push(event("j1"));
        let (tx, rx) = mpsc::channel(16);
        drop(rx);
        tokio::time::timeout(Duration::from_secs(2), forward(outbox, tx))
            .await
            .unwrap();
    }

    #[tokio::test]
    async fn job_result_buffered_while_disconnected() {
        let tmp = tempfile::tempdir().unwrap();
        let agent = Arc::new(Agent::new(
            FakeHypervisor::default(),
            FakeStorage::default(),
            HashMap::new(),
        ));
        let mut link = Link::new(
            agent,
            HostInfo {
                host_id: "h1".into(),
                region: "r".into(),
                failure_domain: "f".into(),
                arch: "x86_64".into(),
                vcpu: 4,
                memory_gb: 16,
                peer_address: String::new(),
            },
            "http://127.0.0.1:1".into(),
        );
        link.outbox = Arc::new(Outbox::open(tmp.path()).unwrap());
        // No session is open: the result must still be kept.
        link.start_job(pb::AssignJob {
            job_id: "gh/1".into(),
            vcpu: 4,
            memory_gb: 16,
            ..Default::default()
        })
        .await
        .unwrap();
        assert!(link.outbox.has_result("gh/1"));
        assert!(link.outbox.interrupted().is_empty(), "marker cleared");
        let phases: Vec<_> = link
            .outbox
            .pending_after(0)
            .into_iter()
            .filter_map(|m| match m.msg {
                Some(pb::link_request::Msg::JobEvent(e)) => Some(e.phase()),
                _ => None,
            })
            .collect();
        assert_eq!(phases.first(), Some(&pb::Phase::VmResumed));
        // And it survives a restart.
        assert!(Outbox::open(tmp.path()).unwrap().has_result("gh/1"));
    }

    #[test]
    fn inventory_replaces_by_kind() {
        let inv = Inventory::default();
        let pre = |id: &str, key: &str| pb::SnapshotRef {
            kind: pb::snapshot_ref::Kind::Prebuild as i32,
            repo: "a/b".into(),
            key: key.into(),
            id: id.into(),
        };
        let sticky = |id: &str, key: &str| pb::SnapshotRef {
            kind: pb::snapshot_ref::Kind::Sticky as i32,
            repo: "a/b".into(),
            key: key.into(),
            id: id.into(),
        };
        inv.put(pre("p1", "k1"));
        inv.put(pre("p2", "k2")); // newer prebuild replaces older key
        inv.put(sticky("s1", "docker"));
        inv.put(sticky("s2", "docker"));
        inv.put(sticky("s3", "gradle"));
        let ids: Vec<_> = inv.list().into_iter().map(|r| r.id).collect();
        assert_eq!(ids, vec!["p2", "s2", "s3"]);
    }
}
