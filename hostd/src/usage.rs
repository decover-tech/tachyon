//! Per-job resource sampling for abuse detection. A `UsageSampler` reads a
//! VM's cgroup CPU counter and its tap device's network counters and turns
//! successive readings into `JobUsage` deltas for the control plane, which
//! looks for mining and attack patterns.
//!
//! The tap device sits on the host side of the guest's NIC, so the tap's
//! `rx_*` counters are what the guest sent (guest tx) and `tx_*` what it
//! received. Connection and destination counts need conntrack and are left
//! at 0 for now.
//!
//! TODO: run one sampler per running job on a timer once per-VM networking
//! names the tap device; until then nothing sends JobUsage, and the control
//! plane still checks whole-run CPU from each JobResult.

use std::fs;
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant, SystemTime};

use crate::pb;

/// Raw cumulative counters at one instant.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct Counters {
    pub cpu_usec: u64,
    pub guest_tx_bytes: u64,
    pub guest_rx_bytes: u64,
    pub guest_tx_packets: u64,
}

impl Counters {
    /// Reads counters from a cgroup v2 directory and, if given, a network
    /// device's statistics directory (`/sys/class/net/<tap>/statistics`).
    /// Missing files read as 0.
    pub fn read(cgroup: &Path, net_stats: Option<&Path>) -> Self {
        let mut c = Self {
            cpu_usec: cpu_usage_usec(cgroup).unwrap_or(0),
            ..Self::default()
        };
        if let Some(dir) = net_stats {
            let n = |f: &str| read_u64(&dir.join(f)).unwrap_or(0);
            c.guest_tx_bytes = n("rx_bytes");
            c.guest_rx_bytes = n("tx_bytes");
            c.guest_tx_packets = n("rx_packets");
        }
        c
    }
}

fn read_u64(p: &Path) -> Option<u64> {
    fs::read_to_string(p).ok()?.trim().parse().ok()
}

fn cpu_usage_usec(cgroup: &Path) -> Option<u64> {
    let stat = fs::read_to_string(cgroup.join("cpu.stat")).ok()?;
    stat.lines()
        .find_map(|l| l.strip_prefix("usage_usec "))
        .and_then(|v| v.trim().parse().ok())
}

/// Turns successive counter readings for one job into `JobUsage` deltas.
pub struct UsageSampler {
    job_id: String,
    cgroup: PathBuf,
    net_stats: Option<PathBuf>,
    last: Option<(Instant, Counters)>,
}

impl UsageSampler {
    pub fn new(job_id: impl Into<String>, cgroup: PathBuf, net_stats: Option<PathBuf>) -> Self {
        Self {
            job_id: job_id.into(),
            cgroup,
            net_stats,
            last: None,
        }
    }

    /// Reads the counters now. The first call only primes the sampler and
    /// returns None.
    pub fn sample(&mut self, now: Instant) -> Option<pb::JobUsage> {
        let c = Counters::read(&self.cgroup, self.net_stats.as_deref());
        self.observe(now, c)
    }

    /// Computes the delta from the previous reading to `c`. Counters that
    /// went backwards (device re-created) count as 0 for the interval.
    pub fn observe(&mut self, now: Instant, c: Counters) -> Option<pb::JobUsage> {
        let prev = self.last.replace((now, c))?;
        let (then, p) = prev;
        let interval = now.saturating_duration_since(then);
        if interval.is_zero() {
            return None;
        }
        let d = |a: u64, b: u64| a.saturating_sub(b);
        Some(pb::JobUsage {
            job_id: self.job_id.clone(),
            at: Some(prost_types::Timestamp::from(SystemTime::now())),
            interval_ms: u32::try_from(interval.as_millis()).unwrap_or(u32::MAX),
            busy_vcpu: busy_vcpu(d(c.cpu_usec, p.cpu_usec), interval),
            tx_bytes: d(c.guest_tx_bytes, p.guest_tx_bytes),
            rx_bytes: d(c.guest_rx_bytes, p.guest_rx_bytes),
            tx_packets: d(c.guest_tx_packets, p.guest_tx_packets),
            new_connections: 0,
            distinct_destinations: 0,
        })
    }
}

fn busy_vcpu(cpu_usec: u64, interval: Duration) -> f64 {
    cpu_usec as f64 / 1e6 / interval.as_secs_f64()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn reads_cgroup_and_tap_counters() {
        let tmp = tempfile::tempdir().unwrap();
        let cg = tmp.path().join("cg");
        let net = tmp.path().join("net");
        fs::create_dir_all(&cg).unwrap();
        fs::create_dir_all(&net).unwrap();
        fs::write(
            cg.join("cpu.stat"),
            "usage_usec 5000000\nuser_usec 4000000\n",
        )
        .unwrap();
        fs::write(net.join("rx_bytes"), "1000\n").unwrap();
        fs::write(net.join("tx_bytes"), "9000\n").unwrap();
        fs::write(net.join("rx_packets"), "10\n").unwrap();
        let c = Counters::read(&cg, Some(&net));
        assert_eq!(
            c,
            Counters {
                cpu_usec: 5_000_000,
                guest_tx_bytes: 1000, // tap rx is guest tx
                guest_rx_bytes: 9000,
                guest_tx_packets: 10,
            }
        );
        assert_eq!(
            Counters::read(&tmp.path().join("missing"), None),
            Counters::default()
        );
    }

    #[test]
    fn deltas_between_samples() {
        let mut s = UsageSampler::new("j1", PathBuf::from("/nonexistent"), None);
        let t0 = Instant::now();
        assert!(
            s.observe(t0, Counters::default()).is_none(),
            "first call primes"
        );
        let u = s
            .observe(
                t0 + Duration::from_secs(10),
                Counters {
                    cpu_usec: 20_000_000, // 2 vCPU busy for 10s
                    guest_tx_bytes: 5000,
                    guest_rx_bytes: 7000,
                    guest_tx_packets: 50,
                },
            )
            .unwrap();
        assert_eq!(u.job_id, "j1");
        assert_eq!(u.interval_ms, 10_000);
        assert!((u.busy_vcpu - 2.0).abs() < 1e-9);
        assert_eq!((u.tx_bytes, u.rx_bytes, u.tx_packets), (5000, 7000, 50));
        assert!(u.at.is_some());

        // Counters reset (tap re-created): no negative deltas.
        let u = s
            .observe(
                t0 + Duration::from_secs(20),
                Counters {
                    cpu_usec: 30_000_000,
                    ..Counters::default()
                },
            )
            .unwrap();
        assert_eq!((u.tx_bytes, u.tx_packets), (0, 0));
        assert!((u.busy_vcpu - 1.0).abs() < 1e-9);

        // Same instant twice: nothing to report.
        assert!(s
            .observe(t0 + Duration::from_secs(20), Counters::default())
            .is_none());
    }

    #[test]
    fn sample_reads_files() {
        let tmp = tempfile::tempdir().unwrap();
        fs::write(tmp.path().join("cpu.stat"), "usage_usec 0\n").unwrap();
        let mut s = UsageSampler::new("j1", tmp.path().to_path_buf(), None);
        let t0 = Instant::now();
        assert!(s.sample(t0).is_none());
        fs::write(tmp.path().join("cpu.stat"), "usage_usec 4000000\n").unwrap();
        let u = s.sample(t0 + Duration::from_secs(4)).unwrap();
        assert!((u.busy_vcpu - 1.0).abs() < 1e-9);
    }
}
