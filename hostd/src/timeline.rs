//! perf-only: where a job's time goes on this host.
//!
//! `run_job` runs a job start to finish on one blocking thread, so a
//! thread-local recorder sees every step of it without threading a handle
//! through the code. Choke points (the command runner, ch-remote, the VM
//! spawn, each lifecycle step) wrap their work in [`span`]; on a thread with
//! no open recorder `span` only runs the closure, so code outside a job
//! (warm-pool refill, image prep) pays nothing beyond a thread-local check.
//!
//! Durations come from the monotonic clock; each span's start is the wall
//! clock read once when it opens, so spans line up with the control plane's
//! phase timestamps.

use std::cell::RefCell;
use std::collections::BTreeMap;
use std::time::{Duration, Instant, SystemTime};

use crate::pb;

#[derive(Debug, Clone, PartialEq)]
pub struct Span {
    pub component: &'static str,
    pub name: String,
    pub start: SystemTime,
    pub duration: Duration,
    pub depth: u32,
    pub attrs: BTreeMap<String, String>,
}

#[derive(Default)]
struct Recorder {
    spans: Vec<Span>,
    /// Indexes into `spans` of the spans still open, innermost last.
    open: Vec<usize>,
}

thread_local! {
    static REC: RefCell<Option<Recorder>> = const { RefCell::new(None) };
}

/// Opens a recorder on this thread; spans recorded until [`finish`] belong
/// to it. A recorder already open is replaced.
pub fn begin() {
    REC.with(|r| *r.borrow_mut() = Some(Recorder::default()));
}

/// Closes this thread's recorder and returns its spans in start order.
pub fn finish() -> Vec<Span> {
    REC.with(|r| r.borrow_mut().take())
        .map(|r| r.spans)
        .unwrap_or_default()
}

fn recording() -> bool {
    REC.with(|r| r.borrow().is_some())
}

/// Runs `f` as a host span named `name`.
pub fn span<T>(name: impl Into<String>, f: impl FnOnce() -> T) -> T {
    span_with(name, &[], f)
}

/// Runs `f` as a host span with attributes.
pub fn span_with<T>(name: impl Into<String>, attrs: &[(&str, &str)], f: impl FnOnce() -> T) -> T {
    if !recording() {
        return f();
    }
    let idx = REC.with(|r| {
        let mut r = r.borrow_mut();
        let rec = r.as_mut().unwrap();
        let depth = rec.open.len() as u32;
        rec.spans.push(Span {
            component: "host",
            name: name.into(),
            start: SystemTime::now(),
            duration: Duration::ZERO,
            depth,
            attrs: attrs
                .iter()
                .map(|(k, v)| (k.to_string(), v.to_string()))
                .collect(),
        });
        let idx = rec.spans.len() - 1;
        rec.open.push(idx);
        idx
    });
    let t0 = Instant::now();
    let out = f();
    let d = t0.elapsed();
    REC.with(|r| {
        if let Some(rec) = r.borrow_mut().as_mut() {
            if let Some(s) = rec.spans.get_mut(idx) {
                s.duration = d;
            }
            if let Some(pos) = rec.open.iter().rposition(|&i| i == idx) {
                rec.open.truncate(pos);
            }
        }
    });
    out
}

/// Adds an attribute to the innermost open span (e.g. a warm-pool hit,
/// known only once the step has looked).
pub fn attr(key: &str, value: impl Into<String>) {
    REC.with(|r| {
        if let Some(rec) = r.borrow_mut().as_mut() {
            if let Some(&i) = rec.open.last() {
                rec.spans[i].attrs.insert(key.to_string(), value.into());
            }
        }
    });
}

/// Adds a finished span timed elsewhere (the guest's, once anchored).
pub fn push(span: Span) {
    REC.with(|r| {
        if let Some(rec) = r.borrow_mut().as_mut() {
            rec.spans.push(span);
        }
    });
}

/// A short, stable span name for a host command: the program and its
/// subcommand, e.g. `exec zfs clone`, `exec ip link`, and for
/// `ip netns exec <ns> <prog> <sub>` the program run inside the namespace.
pub fn command_name(program: &str, args: &[String]) -> String {
    let prog = program.rsplit('/').next().unwrap_or(program);
    let sub = |i: usize| {
        args.get(i)
            .filter(|a| !a.starts_with('-'))
            .map(String::as_str)
    };
    if prog == "ip" && sub(0) == Some("netns") && sub(1) == Some("exec") {
        let inner = args.get(3).map(String::as_str).unwrap_or("?");
        let inner = inner.rsplit('/').next().unwrap_or(inner);
        return format!("exec ip netns exec {inner}");
    }
    // `ip -n <ns> link ...` style: skip leading flags and their values.
    let first = args.iter().position(|a| !a.starts_with('-'));
    match first.and_then(|i| args.get(i)) {
        Some(s) if prog != "sysctl" => format!("exec {prog} {s}"),
        _ => format!("exec {prog}"),
    }
}

impl Span {
    pub fn to_proto(&self) -> pb::TimelineSpan {
        pb::TimelineSpan {
            component: self.component.to_string(),
            name: self.name.clone(),
            start: Some(prost_types::Timestamp::from(self.start)),
            duration: prost_types::Duration::try_from(self.duration).ok(),
            depth: self.depth,
            attrs: self.attrs.clone().into_iter().collect(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nests_and_closes_spans() {
        begin();
        span("outer", || {
            attr("warm", "hit");
            span("inner", || std::thread::sleep(Duration::from_millis(2)));
        });
        span("after", || {});
        let s = finish();
        let names: Vec<_> = s.iter().map(|s| (s.name.as_str(), s.depth)).collect();
        assert_eq!(names, vec![("outer", 0), ("inner", 1), ("after", 0)]);
        assert_eq!(s[0].attrs["warm"], "hit");
        assert!(s[1].duration >= Duration::from_millis(2));
        assert!(s[0].duration >= s[1].duration);
        assert!(finish().is_empty(), "finish closes the recorder");
    }

    #[test]
    fn no_recorder_runs_closure_only() {
        assert!(!recording());
        assert_eq!(span("x", || 7), 7);
        attr("k", "v");
        assert!(finish().is_empty());
    }

    #[test]
    fn command_names() {
        let a = |v: &[&str]| v.iter().map(|s| s.to_string()).collect::<Vec<_>>();
        assert_eq!(
            command_name("zfs", &a(&["clone", "-p", "a@b", "c"])),
            "exec zfs clone"
        );
        assert_eq!(
            command_name(
                "/usr/sbin/ip",
                &a(&["netns", "exec", "tvm1", "nft", "-f", "x"])
            ),
            "exec ip netns exec nft"
        );
        assert_eq!(command_name("ip", &a(&["link", "set"])), "exec ip link");
        assert_eq!(
            command_name("sysctl", &a(&["-qw", "net.ipv4.ip_forward=1"])),
            "exec sysctl"
        );
    }
}
