//! The hostd side of hostlink.v1: dial the control plane, keep one stream
//! open, heartbeat, run assigned jobs and report their results. If the
//! stream drops, running jobs keep running and hostd reconnects.

use anyhow::Result;
use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, SystemTime};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;

use crate::hypervisor::Hypervisor;
use crate::lifecycle::Agent;
use crate::pb::{self, host_link_service_client::HostLinkServiceClient};
use crate::storage::Storage;

#[derive(Debug, Clone)]
pub struct HostInfo {
    pub host_id: String,
    pub region: String,
    pub failure_domain: String,
    pub arch: String,
    pub vcpu: u32,
    pub memory_gb: u32,
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
    /// mTLS credentials, re-read on every connect; None only in insecure
    /// development mode.
    pub tls: Option<crate::tls::TlsFiles>,
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
            tls: None,
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
        let channel = crate::tls::endpoint(&self.control_plane, self.tls.as_ref())?
            .connect()
            .await?;
        let mut client = HostLinkServiceClient::new(channel);
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

        let result = async {
            while let Some(m) = inbound.message().await? {
                match m.msg {
                    Some(pb::link_response::Msg::Assign(job)) => self.start_job(job, tx.clone()),
                    Some(pb::link_response::Msg::Drain(d)) => {
                        self.draining.store(d.drain, Ordering::Relaxed);
                        tracing::info!(drain = d.drain, "drain state changed");
                    }
                    Some(pb::link_response::Msg::Cancel(c)) => {
                        // TODO: signal the guest agent to stop the runner.
                        tracing::warn!(job = %c.job_id, "cancel not implemented yet");
                    }
                    Some(pb::link_response::Msg::PreFork(p)) => {
                        // TODO: clone a job disk ahead of the queued event.
                        tracing::debug!(repo = %p.repo, "pre-fork hint ignored for now");
                    }
                    Some(pb::link_response::Msg::PullSnapshot(p)) => {
                        // TODO: incremental zfs send/recv from the peer.
                        tracing::info!(from = %p.from_host_id, "snapshot pull not implemented yet");
                    }
                    None => {}
                }
            }
            Ok(())
        }
        .await;
        hb.abort();
        result
    }

    fn start_job(&self, job: pb::AssignJob, tx: mpsc::Sender<pb::LinkRequest>) {
        let job_id = job.job_id.clone();
        self.running.lock().unwrap().insert(job_id.clone(), ());
        let (agent, inv, running) = (
            self.agent.clone(),
            self.inventory.clone(),
            self.running.clone(),
        );
        tracing::info!(job = %job_id, repo = %job.repo, vcpu = job.vcpu, "assigned");
        tokio::task::spawn_blocking(move || {
            let ev_tx = tx.clone();
            let ev_job = job_id.clone();
            let events = move |phase: pb::Phase| {
                let e = pb::JobEvent {
                    job_id: ev_job.clone(),
                    phase: phase as i32,
                    at: Some(now()),
                };
                let _ = ev_tx.blocking_send(msg(pb::link_request::Msg::JobEvent(e)));
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
            // TODO: buffer results across reconnects instead of dropping
            // them when this session's stream has closed.
            let _ = tx.blocking_send(msg(pb::link_request::Msg::JobResult(result)));
            if let Err(e) = agent.refill_pool() {
                tracing::warn!(error = %format!("{e:#}"), "refill warm pool");
            }
        });
    }
}

fn msg(m: pb::link_request::Msg) -> pb::LinkRequest {
    pb::LinkRequest { msg: Some(m) }
}

fn now() -> prost_types::Timestamp {
    prost_types::Timestamp::from(SystemTime::now())
}

#[cfg(test)]
mod tests {
    use super::*;

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
