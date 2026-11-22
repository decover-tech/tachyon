//! Host CPU busy and steal fractions from /proc/stat, reported on every
//! heartbeat. The control plane exports them as host utilization metrics.

use std::sync::Mutex;

/// Aggregate CPU jiffies from the first ("cpu ") line of /proc/stat.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub struct CpuTimes {
    pub busy: u64,  // user + nice + system + irq + softirq
    pub idle: u64,  // idle + iowait
    pub steal: u64, // time a hypervisor ran something else
}

impl CpuTimes {
    /// Parses the aggregate line of /proc/stat. guest and guest_nice are
    /// already counted in user and nice, so they are not added again.
    pub fn parse(stat: &str) -> Option<Self> {
        let line = stat.lines().find(|l| l.starts_with("cpu "))?;
        let f: Vec<u64> = line
            .split_whitespace()
            .skip(1)
            .map(|v| v.parse().ok())
            .collect::<Option<_>>()?;
        let at = |i: usize| f.get(i).copied().unwrap_or(0);
        if f.len() < 4 {
            return None;
        }
        Some(Self {
            busy: at(0) + at(1) + at(2) + at(5) + at(6),
            idle: at(3) + at(4),
            steal: at(7),
        })
    }

    fn total(&self) -> u64 {
        self.busy + self.idle + self.steal
    }

    /// Busy and steal fractions of the time between `prev` and `self`.
    pub fn since(&self, prev: &Self) -> Option<(f64, f64)> {
        let total = self.total().checked_sub(prev.total())?;
        if total == 0 {
            return None;
        }
        let busy = self.busy.saturating_sub(prev.busy) as f64 / total as f64;
        let steal = self.steal.saturating_sub(prev.steal) as f64 / total as f64;
        Some((busy.min(1.0), steal.min(1.0)))
    }
}

/// Samples /proc/stat and reports fractions since the previous sample.
pub struct CpuSampler {
    path: std::path::PathBuf,
    prev: Mutex<Option<CpuTimes>>,
}

impl Default for CpuSampler {
    fn default() -> Self {
        Self::new("/proc/stat")
    }
}

impl CpuSampler {
    pub fn new(path: impl Into<std::path::PathBuf>) -> Self {
        Self {
            path: path.into(),
            prev: Mutex::new(None),
        }
    }

    /// Busy and steal fractions since the last call; None on the first call
    /// or when /proc/stat is unreadable (e.g. not Linux).
    pub fn sample(&self) -> Option<(f64, f64)> {
        let cur = CpuTimes::parse(&std::fs::read_to_string(&self.path).ok()?)?;
        let prev = self.prev.lock().unwrap().replace(cur)?;
        cur.since(&prev)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const STAT_A: &str = "cpu  100 0 50 800 50 0 0 0 0 0\ncpu0 100 0 50 800 50 0 0 0 0 0\nintr 1\n";
    const STAT_B: &str = "cpu  160 0 70 880 60 5 5 20 0 0\ncpu0 1 2 3 4\n";

    #[test]
    fn parse_aggregate_line() {
        let t = CpuTimes::parse(STAT_A).unwrap();
        assert_eq!(
            t,
            CpuTimes {
                busy: 150,
                idle: 850,
                steal: 0
            }
        );
        assert!(CpuTimes::parse("cpu0 1 2 3 4\n").is_none());
        assert!(CpuTimes::parse("cpu  1 2\n").is_none());
        assert!(CpuTimes::parse("cpu  1 x 3 4\n").is_none());
    }

    #[test]
    fn fractions_between_samples() {
        let a = CpuTimes::parse(STAT_A).unwrap();
        let b = CpuTimes::parse(STAT_B).unwrap();
        // busy +90, idle +90, steal +20: total 200.
        let (busy, steal) = b.since(&a).unwrap();
        assert!((busy - 0.45).abs() < 1e-9, "busy {busy}");
        assert!((steal - 0.10).abs() < 1e-9, "steal {steal}");
        assert!(a.since(&a).is_none());
        assert!(a.since(&b).is_none(), "counter went backwards");
    }

    #[test]
    fn sampler_reads_file() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("stat");
        std::fs::write(&path, STAT_A).unwrap();
        let s = CpuSampler::new(&path);
        assert!(s.sample().is_none(), "first sample has no baseline");
        std::fs::write(&path, STAT_B).unwrap();
        let (busy, _) = s.sample().unwrap();
        assert!((busy - 0.45).abs() < 1e-9);
        assert!(CpuSampler::new(dir.path().join("missing"))
            .sample()
            .is_none());
    }
}
