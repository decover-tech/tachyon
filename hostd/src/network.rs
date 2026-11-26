//! Per-VM networking and egress controls.
//!
//! Every VM gets its own network namespace holding its tap device, so no two
//! VMs share a link and each VM's firewall and rate limit live where the
//! guest cannot reach them:
//!
//! ```text
//!  guest eth0 172.16.0.2 ── tap0 172.16.0.1 ┐ netns tachyon-vm<N>
//!                                            │  nft: egress policy + masquerade
//!                                            │  tc:  egress rate limit
//!                           uplink 10.200.x.(4k+2)/30
//!                                    │ veth
//!  host                     tvh<N>  10.200.x.(4k+1)/30
//!                           nft: no tvh* -> tvh* forwarding, no host input,
//!                                masquerade out of the host
//! ```
//!
//! The guest side (172.16.0.2/30, gateway 172.16.0.1, virtio-net on `tap0`)
//! is identical in every VM, so one base snapshot serves every slot: only
//! the namespace differs. Cloud Hypervisor joins the namespace in the
//! jailer (`JailSpec::netns`) before it restores the snapshot and opens
//! `tap0`, which is created persistent and owned by the VM's jailed user.
//!
//! Policies:
//! - **Deny** while a VM sits paused in the warm pool.
//! - **Open** for jobs: anything public, plus Tachyon's mirrors and DNS.
//!   Private, link-local (cloud metadata) and other VMs' ranges are blocked.
//! - **Restricted** for prebuild builds: only mirrors, DNS and the prebuild's
//!   egress allowlist.
//!
//! Every policy keeps the per-VM egress rate limit.

use anyhow::{bail, Context, Result};
use std::collections::BTreeSet;
use std::fmt::Write as _;
use std::fs;
use std::net::{Ipv4Addr, ToSocketAddrs};
use std::path::{Path, PathBuf};

use crate::pb;
use crate::storage::CommandRunner;

/// Guest address inside every VM (baked into the base snapshot).
pub const GUEST_IP: Ipv4Addr = Ipv4Addr::new(172, 16, 0, 2);
/// The VM's gateway: tap0 inside the VM's namespace.
pub const GUEST_GATEWAY: Ipv4Addr = Ipv4Addr::new(172, 16, 0, 1);
/// Tap device name inside every VM namespace.
pub const TAP: &str = "tap0";
/// The namespace end of the veth pair.
pub const UPLINK: &str = "uplink";
/// Host veth names start with this; host rules match `tvh*`.
pub const HOST_VETH_PREFIX: &str = "tvh";
/// Per-VM /30 links are carved from 10.200.0.0/16.
const LINK_BASE: u32 = u32::from_be_bytes([10, 200, 0, 0]);
const LINK_SLOTS: u32 = 1 << 14;

/// Ranges no guest may reach unless they are an explicit mirror or DNS
/// server: other VMs, the host's networks, cloud metadata.
const BLOCKED: &[&str] = &[
    "0.0.0.0/8",
    "10.0.0.0/8",
    "100.64.0.0/10",
    "127.0.0.0/8",
    "169.254.0.0/16",
    "172.16.0.0/12",
    "192.168.0.0/16",
    "224.0.0.0/4",
    "240.0.0.0/4",
];

/// What a VM may reach.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EgressPolicy {
    /// Nothing; used for paused VMs in the warm pool.
    Deny,
    /// Public internet plus mirrors and DNS.
    Open,
    /// Mirrors, DNS and these entries only (see [`parse_entry`]).
    Restricted { allow: Vec<String> },
}

impl EgressPolicy {
    /// Prebuild builds are restricted to mirrors plus their allowlist; every
    /// other job gets open egress.
    pub fn for_job(job: &pb::AssignJob) -> Self {
        match &job.prebuild_build {
            Some(b) => EgressPolicy::Restricted {
                allow: b.egress_allowlist.clone(),
            },
            None => EgressPolicy::Open,
        }
    }
}

/// One allowlist entry: an IPv4 address, CIDR, hostname or URL, with an
/// optional port.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Target {
    Net(Ipv4Addr, u8),
    Host(String),
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Entry {
    pub target: Target,
    pub port: Option<u16>,
}

/// Parses `10.1.0.0/16`, `1.2.3.4`, `1.2.3.4:443`, `github.com`,
/// `github.com:443` or a URL such as `https://npm.r1.tachyon.internal/`
/// (whose port defaults from the scheme).
pub fn parse_entry(raw: &str) -> Result<Entry> {
    let s = raw.trim();
    let (scheme, rest) = match s.split_once("://") {
        Some((sc, r)) => (Some(sc.to_ascii_lowercase()), r),
        None => (None, s),
    };
    // Drop any path, then any userinfo.
    let rest = rest.split('/').next().unwrap_or_default();
    let mut prefix: Option<u8> = None;
    let rest = if scheme.is_none() {
        // A bare CIDR keeps its prefix: re-read it from the original.
        match s.split_once('/') {
            Some((a, p)) if a.parse::<Ipv4Addr>().is_ok() => {
                prefix = Some(
                    p.parse()
                        .ok()
                        .filter(|n| *n <= 32)
                        .with_context(|| format!("bad prefix in {raw:?}"))?,
                );
                a
            }
            _ => rest,
        }
    } else {
        rest
    };
    let rest = rest.rsplit('@').next().unwrap_or_default();
    if rest.is_empty() {
        bail!("empty egress entry {raw:?}");
    }
    if rest.starts_with('[') || rest.matches(':').count() > 1 {
        bail!("IPv6 egress entries are not supported: {raw:?}");
    }
    let (host, port) = match rest.split_once(':') {
        Some((h, p)) => (
            h,
            Some(
                p.parse::<u16>()
                    .ok()
                    .filter(|n| *n != 0)
                    .with_context(|| format!("bad port in {raw:?}"))?,
            ),
        ),
        None => (rest, None),
    };
    let port = port.or(match scheme.as_deref() {
        Some("http") => Some(80),
        Some("https") => Some(443),
        Some("git") => Some(9418),
        Some(_) | None => None,
    });
    if let Ok(ip) = host.parse::<Ipv4Addr>() {
        return Ok(Entry {
            target: Target::Net(ip, prefix.unwrap_or(32)),
            port,
        });
    }
    let valid = host.len() <= 253
        && host.split('.').all(|l| {
            !l.is_empty()
                && l.len() <= 63
                && !l.starts_with('-')
                && !l.ends_with('-')
                && l.chars().all(|c| c.is_ascii_alphanumeric() || c == '-')
        });
    if !valid {
        bail!("invalid egress entry {raw:?} (wildcards are not supported)");
    }
    Ok(Entry {
        target: Target::Host(host.to_ascii_lowercase()),
        port,
    })
}

/// Resolves hostnames in allowlists; replaced in tests.
pub trait Resolver: Send + Sync {
    fn resolve(&self, host: &str) -> Result<Vec<Ipv4Addr>>;
}

pub struct SystemResolver;

impl Resolver for SystemResolver {
    fn resolve(&self, host: &str) -> Result<Vec<Ipv4Addr>> {
        let addrs = (host, 0)
            .to_socket_addrs()
            .with_context(|| format!("resolve {host}"))?;
        Ok(addrs
            .filter_map(|a| match a.ip() {
                std::net::IpAddr::V4(v4) => Some(v4),
                std::net::IpAddr::V6(_) => None,
            })
            .collect())
    }
}

/// Host-wide network settings.
#[derive(Debug, Clone, Default)]
pub struct NetConfig {
    /// Per-VM egress limit in Mbit/s; 0 disables the limit.
    pub egress_mbit: u32,
    /// Tachyon mirror endpoints, reachable under every policy.
    pub mirrors: Vec<String>,
    /// DNS servers the guest may query (TCP and UDP 53).
    pub dns: Vec<Ipv4Addr>,
}

/// Addresses and names for one VM slot.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct VmLink {
    pub netns: String,
    pub host_veth: String,
    pub host_ip: Ipv4Addr,
    pub ns_ip: Ipv4Addr,
}

impl VmLink {
    pub fn for_slot(slot: u32) -> Self {
        let base = LINK_BASE + (slot % LINK_SLOTS) * 4;
        Self {
            netns: format!("tachyon-vm{slot}"),
            host_veth: format!("{HOST_VETH_PREFIX}{slot}"),
            host_ip: Ipv4Addr::from(base + 1),
            ns_ip: Ipv4Addr::from(base + 2),
        }
    }

    /// The bind-mounted namespace file `ip netns add` creates.
    pub fn netns_path(&self) -> PathBuf {
        Path::new("/run/netns").join(&self.netns)
    }
}

/// Sets up and tears down per-VM namespaces with `ip`, `nft` and `tc`.
pub struct HostNetwork {
    pub config: NetConfig,
    /// Rendered nft rulesets are written here before loading.
    pub rules_dir: PathBuf,
    pub runner: Box<dyn CommandRunner>,
    pub resolver: Box<dyn Resolver>,
}

/// A resolved allow rule: network, prefix, optional port.
type Rule = (Ipv4Addr, u8, Option<u16>);

impl HostNetwork {
    fn run(&self, program: &str, args: &[&str]) -> Result<String> {
        let args: Vec<String> = args.iter().map(|s| s.to_string()).collect();
        self.runner.run(program, &args)
    }

    fn in_ns(&self, link: &VmLink, program: &str, args: &[&str]) -> Result<String> {
        let mut a = vec!["netns", "exec", &link.netns, program];
        a.extend_from_slice(args);
        self.run("ip", &a)
    }

    /// Host-wide rules, applied once at startup: forwarding on, no traffic
    /// between VM links, no VM traffic to the host itself, and masquerade
    /// for VM traffic leaving the host.
    pub fn init_host(&self) -> Result<()> {
        self.run("sysctl", &["-qw", "net.ipv4.ip_forward=1"])?;
        let path = self.rules_dir.join("host.nft");
        fs::create_dir_all(&self.rules_dir)?;
        fs::write(&path, render_host_rules())?;
        self.run("nft", &["-f", &path.to_string_lossy()])?;
        Ok(())
    }

    /// Creates the VM's namespace, veth link and tap, with egress denied
    /// until a job's policy is applied. `uid`/`gid` own the tap.
    pub fn setup(&self, slot: u32, uid: u32, gid: u32) -> Result<VmLink> {
        let link = VmLink::for_slot(slot);
        let r = self.setup_inner(&link, uid, gid);
        if r.is_err() {
            let _ = self.teardown(&link);
        }
        r.map(|()| link)
    }

    fn setup_inner(&self, l: &VmLink, uid: u32, gid: u32) -> Result<()> {
        let ns = l.netns.as_str();
        let host_cidr = format!("{}/30", l.host_ip);
        let ns_cidr = format!("{}/30", l.ns_ip);
        let host_ip = l.host_ip.to_string();
        let gw_cidr = format!("{GUEST_GATEWAY}/30");
        let (uid, gid) = (uid.to_string(), gid.to_string());
        self.run("ip", &["netns", "add", ns])?;
        self.run(
            "ip",
            &[
                "link",
                "add",
                &l.host_veth,
                "type",
                "veth",
                "peer",
                "name",
                UPLINK,
                "netns",
                ns,
            ],
        )?;
        self.run("ip", &["addr", "add", &host_cidr, "dev", &l.host_veth])?;
        self.run("ip", &["link", "set", &l.host_veth, "up"])?;
        self.run("ip", &["-n", ns, "link", "set", "lo", "up"])?;
        self.run("ip", &["-n", ns, "addr", "add", &ns_cidr, "dev", UPLINK])?;
        self.run("ip", &["-n", ns, "link", "set", UPLINK, "up"])?;
        self.run(
            "ip",
            &["-n", ns, "route", "add", "default", "via", &host_ip],
        )?;
        self.run(
            "ip",
            &[
                "-n", ns, "tuntap", "add", "dev", TAP, "mode", "tap", "user", &uid, "group", &gid,
            ],
        )?;
        self.run("ip", &["-n", ns, "addr", "add", &gw_cidr, "dev", TAP])?;
        self.run("ip", &["-n", ns, "link", "set", TAP, "up"])?;
        self.in_ns(l, "sysctl", &["-qw", "net.ipv4.ip_forward=1"])?;
        self.apply(l, &EgressPolicy::Deny)?;
        if self.config.egress_mbit > 0 {
            let rate = format!("{}mbit", self.config.egress_mbit);
            let burst = format!("{}kb", burst_kb(self.config.egress_mbit));
            self.in_ns(
                l,
                "tc",
                &[
                    "qdisc", "replace", "dev", UPLINK, "root", "tbf", "rate", &rate, "burst",
                    &burst, "latency", "50ms",
                ],
            )?;
        }
        Ok(())
    }

    /// Replaces the VM's egress policy. Hostnames are resolved now; an entry
    /// that does not resolve is left out (blocked), never widened.
    pub fn apply(&self, l: &VmLink, policy: &EgressPolicy) -> Result<()> {
        let mut always = Vec::new();
        for m in &self.config.mirrors {
            always.extend(self.resolve(&parse_entry(m)?));
        }
        for d in &self.config.dns {
            always.push((*d, 32, Some(53)));
        }
        let allow = match policy {
            EgressPolicy::Restricted { allow } => {
                let mut v = Vec::new();
                for a in allow {
                    v.extend(self.resolve(&parse_entry(a)?));
                }
                v
            }
            _ => Vec::new(),
        };
        let path = self.rules_dir.join(format!("{}.nft", l.netns));
        fs::create_dir_all(&self.rules_dir)?;
        fs::write(&path, render_vm_rules(policy, &always, &allow))?;
        self.in_ns(l, "nft", &["-f", &path.to_string_lossy()])?;
        Ok(())
    }

    fn resolve(&self, e: &Entry) -> Vec<Rule> {
        match &e.target {
            Target::Net(ip, p) => vec![(*ip, *p, e.port)],
            Target::Host(h) => match self.resolver.resolve(h) {
                Ok(ips) => ips.into_iter().map(|ip| (ip, 32, e.port)).collect(),
                Err(err) => {
                    tracing::warn!(host = %h, error = %err, "egress entry did not resolve; blocked");
                    Vec::new()
                }
            },
        }
    }

    /// Deletes the namespace, which also removes its tap and both ends of
    /// the veth pair.
    /// Deletes the VM namespaces a previous hostd left behind (their veth
    /// pairs go with them); slots restart at 0, so they would collide.
    pub fn reap(&self) -> Result<Vec<String>> {
        let list = self.run("ip", &["netns", "list"])?;
        let stale: Vec<String> = vm_namespaces(&list).map(str::to_owned).collect();
        for ns in &stale {
            let _ = fs::remove_file(self.rules_dir.join(format!("{ns}.nft")));
            self.run("ip", &["netns", "del", ns])?;
        }
        // The kernel frees a deleted namespace, and the veth pair in it,
        // asynchronously; delete the host ends now so setup can reuse them.
        let links = self.run("ip", &["-o", "link", "show", "type", "veth"])?;
        for veth in host_veths(&links) {
            // Fails harmlessly if the kernel freed it since the listing.
            if let Err(e) = self.run("ip", &["link", "del", veth]) {
                tracing::warn!(veth, error = %format!("{e:#}"), "delete orphaned host veth");
            }
        }
        Ok(stale)
    }

    pub fn teardown(&self, l: &VmLink) -> Result<()> {
        let _ = fs::remove_file(self.rules_dir.join(format!("{}.nft", l.netns)));
        self.run("ip", &["netns", "del", &l.netns])?;
        Ok(())
    }
}

/// The `tachyon-vm<slot>` names in `ip netns list` output, which may carry
/// an `(id: N)` suffix.
fn vm_namespaces(list: &str) -> impl Iterator<Item = &str> {
    list.lines()
        .filter_map(|l| l.split_whitespace().next())
        .filter(|n| {
            n.strip_prefix("tachyon-vm")
                .is_some_and(|s| s.parse::<u32>().is_ok())
        })
}

/// The `tvh<slot>` names in `ip -o link show` output (`7: tvh0@if2: <...`).
fn host_veths(list: &str) -> impl Iterator<Item = &str> {
    list.lines()
        .filter_map(|l| l.split(": ").nth(1))
        .map(|n| n.split('@').next().unwrap_or(n))
        .filter(|n| {
            n.strip_prefix(HOST_VETH_PREFIX)
                .is_some_and(|s| s.parse::<u32>().is_ok())
        })
}

/// Token bucket of 10 ms at the configured rate, at least 32 KiB.
fn burst_kb(mbit: u32) -> u32 {
    (mbit.saturating_mul(1000) / 8 / 100).max(32)
}

fn network(ip: Ipv4Addr, prefix: u8) -> Ipv4Addr {
    let mask = if prefix == 0 {
        0
    } else {
        u32::MAX << (32 - u32::from(prefix))
    };
    Ipv4Addr::from(u32::from(ip) & mask)
}

fn fmt_net(ip: Ipv4Addr, prefix: u8) -> String {
    format!("{}/{prefix}", network(ip, prefix))
}

/// Renders `name` and `name_port` sets for these rules.
fn render_sets(out: &mut String, name: &str, rules: &[Rule]) {
    let any: BTreeSet<String> = rules
        .iter()
        .filter(|r| r.2.is_none())
        .map(|r| fmt_net(r.0, r.1))
        .collect();
    let ported: BTreeSet<String> = rules
        .iter()
        .filter_map(|r| r.2.map(|p| format!("{} . {p}", fmt_net(r.0, r.1))))
        .collect();
    let _ = writeln!(
        out,
        "  set {name} {{\n    type ipv4_addr\n    flags interval\n    auto-merge"
    );
    if !any.is_empty() {
        let _ = writeln!(
            out,
            "    elements = {{ {} }}",
            any.into_iter().collect::<Vec<_>>().join(", ")
        );
    }
    let _ = writeln!(out, "  }}");
    let _ = writeln!(
        out,
        "  set {name}_port {{\n    type ipv4_addr . inet_service\n    flags interval"
    );
    if !ported.is_empty() {
        let _ = writeln!(
            out,
            "    elements = {{ {} }}",
            ported.into_iter().collect::<Vec<_>>().join(", ")
        );
    }
    let _ = writeln!(out, "  }}");
}

/// The nft ruleset loaded inside a VM's namespace. Loading it replaces the
/// previous one atomically.
pub fn render_vm_rules(policy: &EgressPolicy, always: &[Rule], allow: &[Rule]) -> String {
    let mut o = String::new();
    o.push_str("table inet tachyon {}\ndelete table inet tachyon\ntable inet tachyon {\n");
    render_sets(&mut o, "always", always);
    render_sets(&mut o, "allow", allow);
    o.push_str("  chain forward {\n    type filter hook forward priority 0; policy drop;\n");
    o.push_str("    ct state established,related accept\n");
    let _ = writeln!(o, "    iifname != \"{TAP}\" drop");
    match policy {
        EgressPolicy::Deny => {}
        EgressPolicy::Open | EgressPolicy::Restricted { .. } => {
            o.push_str("    meta nfproto != ipv4 drop\n");
            o.push_str("    ip daddr @always accept\n");
            o.push_str("    meta l4proto { tcp, udp } ip daddr . th dport @always_port accept\n");
            let _ = writeln!(o, "    ip daddr {{ {} }} drop", BLOCKED.join(", "));
            if let EgressPolicy::Open = policy {
                o.push_str("    accept\n");
            } else {
                o.push_str("    ip daddr @allow accept\n");
                o.push_str(
                    "    meta l4proto { tcp, udp } ip daddr . th dport @allow_port accept\n",
                );
            }
        }
    }
    o.push_str("  }\n");
    // Nothing in the VM may talk to the namespace itself.
    o.push_str("  chain input {\n    type filter hook input priority 0; policy drop;\n");
    o.push_str("    ct state established,related accept\n    iifname \"lo\" accept\n  }\n");
    o.push_str("  chain postrouting {\n    type nat hook postrouting priority 100;\n");
    let _ = writeln!(o, "    oifname \"{UPLINK}\" masquerade\n  }}");
    o.push_str("}\n");
    o
}

/// The host-wide nft ruleset (see [`HostNetwork::init_host`]).
pub fn render_host_rules() -> String {
    let p = HOST_VETH_PREFIX;
    let links = fmt_net(Ipv4Addr::from(LINK_BASE), 16);
    format!(
        "table inet tachyon_host {{}}
delete table inet tachyon_host
table inet tachyon_host {{
  chain forward {{
    type filter hook forward priority 0; policy accept;
    iifname \"{p}*\" oifname \"{p}*\" drop
  }}
  chain input {{
    type filter hook input priority 0; policy accept;
    iifname \"{p}*\" ct state established,related accept
    iifname \"{p}*\" drop
  }}
  chain postrouting {{
    type nat hook postrouting priority 100;
    ip saddr {links} oifname != \"{p}*\" masquerade
  }}
}}
"
    )
}

/// Parses a comma-separated env value into entries, validating each.
pub fn parse_list(s: &str) -> Result<Vec<String>> {
    let v: Vec<String> = s
        .split(',')
        .map(str::trim)
        .filter(|p| !p.is_empty())
        .map(String::from)
        .collect();
    for e in &v {
        parse_entry(e)?;
    }
    Ok(v)
}

#[cfg(test)]
pub mod fake {
    use super::*;
    use std::collections::HashMap;
    use std::sync::{Arc, Mutex};

    /// Records commands; fails any whose rendering contains `fail_on`.
    #[derive(Clone, Default)]
    pub struct Recorder {
        pub log: Arc<Mutex<Vec<String>>>,
        pub fail_on: Option<String>,
    }

    impl CommandRunner for Recorder {
        fn run(&self, program: &str, args: &[String]) -> Result<String> {
            let line = format!("{program} {}", args.join(" "));
            self.log.lock().unwrap().push(line.clone());
            if let Some(f) = &self.fail_on {
                if line.contains(f.as_str()) {
                    bail!("{line}: failed");
                }
            }
            Ok(String::new())
        }
    }

    #[derive(Default)]
    pub struct StaticResolver(pub HashMap<String, Vec<Ipv4Addr>>);

    impl Resolver for StaticResolver {
        fn resolve(&self, host: &str) -> Result<Vec<Ipv4Addr>> {
            self.0
                .get(host)
                .cloned()
                .with_context(|| format!("NXDOMAIN {host}"))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::fake::*;
    use super::*;
    use std::collections::HashMap;

    fn net(rec: &Recorder, dir: &Path) -> HostNetwork {
        HostNetwork {
            config: NetConfig {
                egress_mbit: 800,
                mirrors: vec![
                    "https://npm.r1.tachyon.internal/".into(),
                    "10.50.0.0/16".into(),
                ],
                dns: vec![Ipv4Addr::new(10, 53, 0, 1)],
            },
            rules_dir: dir.to_path_buf(),
            runner: Box::new(rec.clone()),
            resolver: Box::new(StaticResolver(HashMap::from([
                (
                    "npm.r1.tachyon.internal".to_string(),
                    vec![Ipv4Addr::new(10, 60, 0, 7)],
                ),
                (
                    "github.com".to_string(),
                    vec![
                        Ipv4Addr::new(140, 82, 112, 3),
                        Ipv4Addr::new(140, 82, 112, 4),
                    ],
                ),
            ]))),
        }
    }

    #[test]
    fn only_vm_namespaces_are_reaped() {
        let list = "tachyon-vm3 (id: 3)\nsnapshot-4\ntachyon-vm0\ntachyon-vmx\nother (id: 9)\n";
        assert_eq!(
            vm_namespaces(list).collect::<Vec<_>>(),
            ["tachyon-vm3", "tachyon-vm0"]
        );
    }

    #[test]
    fn only_vm_host_veths_are_reaped() {
        let list = "7: tvh0@if2: <BROADCAST,UP> mtu 1500\n9: tvhx@if2: <UP>\n11: veth1@if3: <UP>\n12: tvh12: <UP>\n";
        assert_eq!(host_veths(list).collect::<Vec<_>>(), ["tvh0", "tvh12"]);
    }

    #[test]
    fn parses_entries() {
        let e = |s| parse_entry(s).unwrap();
        assert_eq!(
            e("10.1.2.0/24"),
            Entry {
                target: Target::Net(Ipv4Addr::new(10, 1, 2, 0), 24),
                port: None
            }
        );
        assert_eq!(e("1.2.3.4:8443").port, Some(8443));
        assert_eq!(
            e("1.2.3.4").target,
            Target::Net(Ipv4Addr::new(1, 2, 3, 4), 32)
        );
        assert_eq!(
            e("GitHub.com"),
            Entry {
                target: Target::Host("github.com".into()),
                port: None
            }
        );
        assert_eq!(e("github.com:22").port, Some(22));
        assert_eq!(
            e("https://user@npm.r1.tachyon.internal/registry/"),
            Entry {
                target: Target::Host("npm.r1.tachyon.internal".into()),
                port: Some(443)
            }
        );
        assert_eq!(e("http://apt.internal:3142").port, Some(3142));
        assert_eq!(e("http://apt.internal").port, Some(80));
        for bad in [
            "",
            "*.github.com",
            "1.2.3.4/33",
            "host:0",
            "host:99999",
            "::1",
            "[::1]:443",
            "bad_host.com",
            "-a.com",
        ] {
            assert!(parse_entry(bad).is_err(), "{bad:?} should be rejected");
        }
    }

    #[test]
    fn slots_get_distinct_links() {
        let a = VmLink::for_slot(0);
        let b = VmLink::for_slot(1);
        assert_eq!(a.host_ip, Ipv4Addr::new(10, 200, 0, 1));
        assert_eq!(a.ns_ip, Ipv4Addr::new(10, 200, 0, 2));
        assert_eq!(b.host_ip, Ipv4Addr::new(10, 200, 0, 5));
        assert_eq!(b.netns, "tachyon-vm1");
        assert_eq!(b.host_veth, "tvh1");
        assert_eq!(a.netns_path(), PathBuf::from("/run/netns/tachyon-vm0"));
        let last = VmLink::for_slot(LINK_SLOTS - 1);
        assert_eq!(last.ns_ip, Ipv4Addr::new(10, 200, 255, 254));
        // Linux interface names are at most 15 bytes.
        assert!(VmLink::for_slot(u32::MAX).host_veth.len() <= 15);
    }

    #[test]
    fn policy_for_job() {
        let mut j = pb::AssignJob::default();
        assert_eq!(EgressPolicy::for_job(&j), EgressPolicy::Open);
        j.prebuild_build = Some(pb::PrebuildBuild {
            egress_allowlist: vec!["github.com".into()],
            ..Default::default()
        });
        assert_eq!(
            EgressPolicy::for_job(&j),
            EgressPolicy::Restricted {
                allow: vec!["github.com".into()]
            }
        );
    }

    #[test]
    fn setup_builds_namespace_tap_and_rate_limit() {
        let tmp = tempfile::tempdir().unwrap();
        let rec = Recorder::default();
        let n = net(&rec, tmp.path());
        let l = n.setup(3, 100_003, 100_003).unwrap();
        let log = rec.log.lock().unwrap().clone();
        let has = |s: &str| log.iter().any(|l| l == s);
        assert_eq!(log[0], "ip netns add tachyon-vm3");
        assert!(has(
            "ip link add tvh3 type veth peer name uplink netns tachyon-vm3"
        ));
        assert!(has("ip addr add 10.200.0.13/30 dev tvh3"));
        assert!(has("ip -n tachyon-vm3 addr add 10.200.0.14/30 dev uplink"));
        assert!(has("ip -n tachyon-vm3 route add default via 10.200.0.13"));
        assert!(has(
            "ip -n tachyon-vm3 tuntap add dev tap0 mode tap user 100003 group 100003"
        ));
        assert!(has("ip -n tachyon-vm3 addr add 172.16.0.1/30 dev tap0"));
        assert!(has(
            "ip netns exec tachyon-vm3 tc qdisc replace dev uplink root tbf rate 800mbit burst 1000kb latency 50ms"
        ));
        // Egress starts denied.
        let rules = fs::read_to_string(tmp.path().join("tachyon-vm3.nft")).unwrap();
        assert!(!rules.contains("accept\n  }\n  chain input"));
        assert!(!rules.contains("@always accept"));
        assert!(has(&format!(
            "ip netns exec tachyon-vm3 nft -f {}",
            tmp.path().join("tachyon-vm3.nft").display()
        )));

        n.teardown(&l).unwrap();
        assert_eq!(
            rec.log.lock().unwrap().last().unwrap(),
            "ip netns del tachyon-vm3"
        );
        assert!(!tmp.path().join("tachyon-vm3.nft").exists());
    }

    #[test]
    fn no_rate_limit_when_zero() {
        let tmp = tempfile::tempdir().unwrap();
        let rec = Recorder::default();
        let mut n = net(&rec, tmp.path());
        n.config.egress_mbit = 0;
        n.setup(0, 1, 1).unwrap();
        assert!(!rec.log.lock().unwrap().iter().any(|l| l.contains(" tc ")));
    }

    #[test]
    fn failed_setup_tears_down() {
        let tmp = tempfile::tempdir().unwrap();
        let rec = Recorder {
            fail_on: Some("tuntap".into()),
            ..Default::default()
        };
        let n = net(&rec, tmp.path());
        assert!(n.setup(1, 1, 1).is_err());
        assert_eq!(
            rec.log.lock().unwrap().last().unwrap(),
            "ip netns del tachyon-vm1"
        );
    }

    #[test]
    fn open_policy_blocks_private_ranges_but_allows_mirrors() {
        let tmp = tempfile::tempdir().unwrap();
        let rec = Recorder::default();
        let n = net(&rec, tmp.path());
        let l = VmLink::for_slot(0);
        n.apply(&l, &EgressPolicy::Open).unwrap();
        let r = fs::read_to_string(tmp.path().join("tachyon-vm0.nft")).unwrap();
        assert!(r.starts_with("table inet tachyon {}\ndelete table inet tachyon\n"));
        assert!(r.contains("elements = { 10.50.0.0/16 }"), "{r}");
        assert!(
            r.contains("elements = { 10.53.0.1/32 . 53, 10.60.0.7/32 . 443 }"),
            "{r}"
        );
        // Mirrors are accepted before private ranges are dropped, and open
        // egress is accepted only after.
        let i_mirror = r.find("ip daddr @always accept").unwrap();
        let i_block = r.find("ip daddr { 0.0.0.0/8, 10.0.0.0/8").unwrap();
        let i_accept = r.find("    accept\n").unwrap();
        assert!(i_mirror < i_block && i_block < i_accept, "{r}");
        assert!(r.contains("169.254.0.0/16"), "metadata blocked");
        assert!(r.contains("172.16.0.0/12"), "other VMs blocked");
        assert!(!r.contains("@allow accept"));
        assert!(r.contains("oifname \"uplink\" masquerade"));
    }

    #[test]
    fn restricted_policy_allows_only_mirrors_and_allowlist() {
        let tmp = tempfile::tempdir().unwrap();
        let rec = Recorder::default();
        let n = net(&rec, tmp.path());
        let l = VmLink::for_slot(0);
        let policy = EgressPolicy::Restricted {
            allow: vec![
                "github.com:443".into(),
                "203.0.113.9/24".into(),
                "nx.example.com".into(),
            ],
        };
        n.apply(&l, &policy).unwrap();
        let r = fs::read_to_string(tmp.path().join("tachyon-vm0.nft")).unwrap();
        assert!(r.contains("elements = { 203.0.113.0/24 }"), "{r}");
        assert!(
            r.contains("elements = { 140.82.112.3/32 . 443, 140.82.112.4/32 . 443 }"),
            "{r}"
        );
        assert!(r.contains("ip daddr @allow accept"));
        assert!(r.contains("th dport @allow_port accept"));
        // No catch-all accept in the forward chain: the default is drop.
        assert!(!r.contains("    accept\n"), "{r}");
        assert!(r.contains("policy drop"));
        assert!(!r.contains("nx.example.com"));
    }

    #[test]
    fn malformed_allowlist_is_an_error() {
        let tmp = tempfile::tempdir().unwrap();
        let rec = Recorder::default();
        let n = net(&rec, tmp.path());
        let p = EgressPolicy::Restricted {
            allow: vec!["*.githubusercontent.com".into()],
        };
        assert!(n.apply(&VmLink::for_slot(0), &p).is_err());
        assert!(rec.log.lock().unwrap().is_empty(), "nothing loaded");
    }

    #[test]
    fn host_rules_isolate_vms() {
        let tmp = tempfile::tempdir().unwrap();
        let rec = Recorder::default();
        let n = net(&rec, tmp.path());
        n.init_host().unwrap();
        let r = fs::read_to_string(tmp.path().join("host.nft")).unwrap();
        assert!(r.contains("iifname \"tvh*\" oifname \"tvh*\" drop"));
        assert!(r.contains("iifname \"tvh*\" drop"));
        assert!(r.contains("ip saddr 10.200.0.0/16 oifname != \"tvh*\" masquerade"));
        assert_eq!(
            rec.log.lock().unwrap()[0],
            "sysctl -qw net.ipv4.ip_forward=1"
        );
    }

    #[test]
    fn helpers() {
        assert_eq!(burst_kb(1), 32);
        assert_eq!(burst_kb(1000), 1250);
        assert_eq!(fmt_net(Ipv4Addr::new(1, 2, 3, 4), 0), "0.0.0.0/0");
        assert_eq!(
            parse_list(" a.com, 1.2.3.4:80 ,").unwrap(),
            vec!["a.com", "1.2.3.4:80"]
        );
        assert!(parse_list("a.com,*.b.com").is_err());
    }
}
