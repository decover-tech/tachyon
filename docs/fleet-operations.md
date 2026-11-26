# Fleet operations

How to stand up bare-metal hosts, join them to the control plane, drain them,
and roll a new hostd release through canaries. It implements the
"Operations" paragraph of
[Reliability and operations](architecture.md#reliability-and-operations)
(issue #35).

**Read this first: nothing here has been run on real hardware.** The scripts
are linted and tested in a dry run (see [What has not been exercised](#what-has-not-been-exercised)).
The first real host will find mistakes. Expect to fix them and send the fix
back.

The server provider (decision D3) is not chosen. Nothing below assumes one: it
needs a Linux host with KVM and NVMe, root over SSH, and an HTTPS place to
fetch three artifacts from.

## What is where

| Piece | Path |
| --- | --- |
| Bootstrap script (host to running hostd) | `deploy/fleet/tachyon-host-bootstrap.sh` |
| Per-host config template | `deploy/fleet/fleet.conf.example` |
| cloud-init user-data for provider images | `deploy/fleet/cloud-init/user-data.example` |
| iPXE and autoinstall templates, dnsmasq example, renderer | `deploy/fleet/netboot/` |
| Tests for the above | `deploy/fleet/test/test_bootstrap.sh` |
| Operator API (drain, undrain, host list) | `internal/controlplane/admin.go`, `-admin-addr` |

### Why a bootstrap script and not Ansible

A single shell script was chosen over an Ansible role because it can be run
three ways with no controller: pasted into a provider's first-boot field
(cloud-init), run by the Ubuntu autoinstaller after a network boot, or run by
hand over SSH. It can also be linted with shellcheck and tested here in a dry
run, and neither `ansible-lint` nor `ansible-playbook` is installed in the
environment this was written in, so an Ansible role could not have been
checked at all. The cost is that it is imperative where Ansible is declarative:
each step is written to be safe to re-run, but there is no drift detection and
no inventory. If the fleet grows past a handful of hosts, wrap the script in an
Ansible role (the steps map one to one onto tasks) rather than extending it.

## 1. Provision a host

Supported: Ubuntu 24.04, x86_64 (arm64 is accepted by the script but nothing
here has considered it), cgroup v2, `/dev/kvm` present.

1. **Artifacts.** Publish three files somewhere HTTPS reachable from the host
   (an object store you control, or copy them to the host and use a local path):
   `cloud-hypervisor`, `ch-remote` (static builds from the Cloud Hypervisor
   releases) and `tachyon-hostd` (built from `hostd/`). Record each SHA-256.
   The script refuses to install a binary without a checksum, and refuses
   plain `http://`.
2. **Config.** Copy `deploy/fleet/fleet.conf.example` to `/etc/tachyon/fleet.conf`
   on the host (mode 0600) and fill it in. Variables map to the hostd table in
   the [README](../README.md#running-hostd-linux-host); the script derives
   `hostd.env` from them. `HOST_GROUP` is `canary` or `stable` (any label).
   `ZFS_DEVICES` must be `/dev/disk/by-id/...` paths.
3. **Run it.**
   ```sh
   sudo ./tachyon-host-bootstrap.sh -c /etc/tachyon/fleet.conf all
   ```
   Steps (`preflight packages sysctl zfs chv hostd cgroup tls env unit start`)
   can also be run one at a time. What each does:
   - `preflight`: root, Ubuntu 24.04, `/dev/kvm`, cgroup v2, systemd.
   - `packages`: `zfsutils-linux nftables iproute2 curl ca-certificates openssl`.
     hostd calls `ip`, `nft`, `tc` and `sysctl` itself and sets up its own
     nftables table (`inet tachyon_host`) and per-VM namespaces; the host needs
     only the tools and kernel modules.
   - `sysctl`: `/etc/sysctl.d/90-tachyon.conf` (IP forwarding, conntrack size)
     and `/etc/modules-load.d/tachyon.conf` (`tun veth nf_tables nf_nat
     nf_conntrack sch_tbf`).
   - `zfs`: creates the pool only if it does not exist, and only with
     `ZFS_CONFIRM_WIPE=yes` (it runs `wipefs -a` on the devices first). Caps the
     ARC. Never touches an existing pool.
   - `chv`, `hostd`: verified installs. hostd goes to
     `/usr/local/lib/tachyon/releases/<version>/` and `/usr/local/bin/tachyon-hostd`
     is a symlink to the active release (used for upgrade and rollback below).
   - `cgroup`: writes `/usr/local/lib/tachyon/prepare-host.sh`, which the unit
     runs before every start to create `/sys/fs/cgroup/tachyon` and enable the
     `cpu` and `memory` controllers for it (cgroups are not persistent).
   - `tls`, `csr`: see step 2.
   - `env`, `unit`: `/etc/tachyon/hostd.env` (no secrets) and
     `tachyon-hostd.service`.
   - `start`: enables and starts hostd, but **refuses and exits 3** if the TLS
     files or a base snapshot directory for each `TACHYON_WARM` size are missing.
     hostd exits at startup in both cases, so starting it would only crash-loop.
   Re-running never restarts a running hostd.
4. **Certificate.** The identity is `spiffe://<trust-domain>/host/<HOST_ID>`
   ([hostlink-mtls.md](hostlink-mtls.md)). Either have the provisioning system
   place `host.crt`, `host.key`, `ca.crt` in a directory and set
   `TLS_SOURCE_DIR`, or run `bootstrap csr` on the host (it keeps the private
   key on the host, 0600, and writes `/etc/tachyon/tls/host.csr` with the right
   SAN), have your issuer sign it (check the host record first; add the
   `clientAuth` EKU; 30 days or less), put the result at `host.crt` and the
   control plane's server CA at `ca.crt`, then run `bootstrap tls start`.
   For a lab, `tachyon hostcert` makes development certificates. The script
   never configures `TACHYON_HOSTLINK_INSECURE`.
5. **Base image.** See the blocker below. Until a prepared snapshot exists
   under `BASE_SNAPSHOTS/<vcpu>/` for each warm size, `start` will not start
   hostd.

**Blocker for the first real pilot:** no base image has ever been built on a
real builder or booted under Cloud Hypervisor (README, Status), and nothing
distributes images to hosts. Provisioning a host does not change that.
`tachyon-hostd prepare-image` exists, but this has never been run end to end.
Plan on the first host being a bring-up of the image pipeline, not just of
this script.

### Network boot (optional)

Many bare-metal providers offer only a rescue system or a stock image and no
PXE; for those, use cloud-init or SSH and skip this section. Network boot makes
sense for your own racks or a provider that supports iPXE.

`deploy/fleet/netboot/` renders, for one host: `entry.ipxe` (the same for every
host; chain-loads by MAC), `hosts/<mac>/boot.ipxe` (Ubuntu live-server kernel,
initrd and ISO URLs plus `autoinstall ds=nocloud-net`), and
`hosts/<mac>/user-data` (autoinstall: installs the OS on one disk selected by
**serial number**, leaves all other disks alone, then runs the bootstrap script
with the host's `fleet.conf` embedded; it embeds config, so it is mode 0600 and
should be served only on the provisioning network). `dnsmasq.conf.example`
shows the DHCP side.

```sh
NETBOOT_BASE_URL=https://boot.example/tachyon BOOTSTRAP_URL=https://boot.example/tachyon/tachyon-host-bootstrap.sh \
BOOTSTRAP_SHA256=$(sha256sum deploy/fleet/tachyon-host-bootstrap.sh | cut -d' ' -f1) \
HOST_ID=fra-01 HOST_MAC=aa-bb-cc-dd-ee-ff OS_DISK_SERIAL=... \
SSH_AUTHORIZED_KEY="ssh-ed25519 ..." FLEET_CONF=fra-01.conf \
  deploy/fleet/netboot/render.sh ./out
```

The templates were written from the public iPXE, dnsmasq and Ubuntu autoinstall
documentation. Only the variable substitution is tested. **No machine has been
network-booted with them.** Details that may be wrong: the exact autoinstall
keys for your subiquity version, the `ds=nocloud-net;s=` argument quoting in
iPXE, `storage.layout.match.serial`, and whether the firmware loads
`snponly.efi`. You also supply the Ubuntu kernel, initrd and ISO and should pin
their checksums.

## 2. Join

After `start` succeeds, hostd dials the control plane. Check from the operator
API (next section):

```sh
curl -s -H "Authorization: Bearer $(cat admin.token)" http://127.0.0.1:8081/admin/hosts | jq .
```

The host should appear `connected: true`, `state: active`, with the
`hostd_version` and `group` it reported. If it does not, read
`journalctl -u tachyon-hostd` (JSON logs). A certificate whose SAN does not
equal `HOST_ID` is rejected with `PermissionDenied`.

## 3. The operator API

Off unless both flags are set (the control plane exits if only one is):

```
-admin-addr 127.0.0.1:8081          # its own listener, never the webhook port
-admin-token-file /etc/tachyon/admin.token   # at least 32 bytes
```

Generate a token with `openssl rand -hex 32 > admin.token` (mode 0600). The
token is read once at start, compared in constant time (after SHA-256, so the
comparison does not leak its length), and never logged. Every path on the
listener requires `Authorization: Bearer <token>`; with no or a wrong token the
answer is `401` and no host data. The API is plain HTTP. Bind it to loopback
or a private interface and reach it over SSH or a VPN; put TLS in front of it
before exposing it anywhere else. Restart the control plane to rotate the token.

| Request | Effect |
| --- | --- |
| `GET /admin/hosts[?group=G][&version=V]` | Hosts, with `state`, `jobs`, `empty`, `hostd_version`, `group`, `connected`, and a `versions` count |
| `POST /admin/hosts/{id}/drain` | Stop placing jobs; tell hostd (hostlink `Drain`) |
| `POST /admin/hosts/{id}/undrain` | Place jobs again |

`state` is `active`, `draining` (no new jobs, some still running) or `drained`
(no new jobs, none running). `empty: true` means draining and zero jobs: the
host can be stopped. The job count is the scheduler's reservations, so a job
that is still reserved counts even if its host is disconnected, and a host is
never reported empty while it might still be running something.

Behaviour worth knowing:

- Draining does not interrupt running jobs. It takes up to the longest job
  (GitHub's default job timeout is 6 hours) for a busy host to empty.
- The drain is the operator's intent and wins over hostd's own heartbeat flag.
  It is remembered across the host's reconnects: a host that restarts for an
  upgrade is told to drain again before any job can be placed on it.
- It is held in the control plane's memory. After a control plane restart, a
  host that stays up keeps reporting that it is draining; a host that restarted
  at the same time does not. **Check `state` after any control plane restart.**
- Restored hosts are already treated as draining until they reconnect.

## 4. Drain, take down, return

For a kernel or hypervisor update, or any maintenance:

```sh
H=fra-01; A="Authorization: Bearer $(cat admin.token)"; U=http://127.0.0.1:8081/admin/hosts
curl -s -X POST -H "$A" $U/$H/drain
until curl -s -H "$A" $U | jq -e ".hosts[] | select(.id==\"$H\" and .empty)" >/dev/null; do sleep 30; done
# ... update, reboot ...
# after the host reconnects, confirm it is still drained, then:
curl -s -X POST -H "$A" $U/$H/undrain
```

Rebooting a host kills the VMs on it; the next hostd start reports them as
infrastructure errors (with the customer's opt-in they are re-run). Always wait
for `empty` first.

## 5. hostd release and canary rollout

hostd restarts end the jobs on a host, so a hostd upgrade is a drain, an
upgrade, and an undrain. Canary hosts are the ones with `HOST_GROUP=canary` in
their `fleet.conf` (reported in Hello; shown by the API). This is a procedure,
not an orchestrator: you run each step.

Note that base images have their own canary mechanism, `-canary-hosts` by host
ID on the control plane (`internal/images/rollout.go`). It is separate; keep
the two lists the same if you want one set of canary hosts for both.

1. **Publish** the new `tachyon-hostd` binary and record its SHA-256.
2. **See where you are:** `GET /admin/hosts` shows the `versions` count and
   each host's version and group.
3. **Canary, one host at a time** (start with one):
   1. Drain it and wait for `empty` (section 4).
   2. On the host, set `HOSTD_VERSION`, `HOSTD_SOURCE`, `HOSTD_SHA256` in
      `fleet.conf` and run `bootstrap -c /etc/tachyon/fleet.conf hostd`. This
      verifies the binary and moves the symlink; it does not restart anything.
      Use the same string for `HOSTD_VERSION` that hostd reports (its Cargo
      version) so the API shows what you expect.
   3. `systemctl restart tachyon-hostd`. It takes a while to refill its warm VMs.
   4. Check `GET /admin/hosts?group=canary`: `connected`, new `hostd_version`,
      still `drain_requested` and `drained`.
   5. Undrain it.
4. **Watch.** Compare the canary against the rest, by `host_id`, in the job
   records (ClickHouse `tachyon.job_records`): the share of `infra_error`
   outcomes, job and pickup times, and the `TachyonHostCPUSteal` alert. How long
   to watch is your call and has not been calibrated; a sensible start is long
   enough for the canary to run a full mix of your pilot's job types.
5. **Roll on** to the remaining hosts, one failure domain at a time, never more
   hosts draining at once than the fleet can spare.
6. **Roll back** the same way, with the previous version's settings. Old
   releases stay on disk, so rollback needs no source or checksum:
   drain, `HOSTD_VERSION=<old> bootstrap hostd`, restart, check, undrain.

Hosts on different hostd versions run side by side as long as the hostlink
protocol is compatible. The only change in this release is an added `Hello`
field (`host_group`), which older control planes and hostds ignore. When you
change the protocol incompatibly, roll the control plane first or last as
the change requires; the API's `versions` count shows the mix.

## What has not been exercised

Be explicit with the team: none of the following has run on real hardware or
on a real provider.

- **The bootstrap script on any real host.** `apt`, `zpool create`, the module
  loading, the sysctl file, the systemd unit and the `start` step have never run.
  What is tested (`deploy/fleet/test/test_bootstrap.sh`, also in CI): rendered
  files, the commands and their order in a dry run, refusing to create a pool
  without confirmation, refusing unverified or `http://` artifacts, the release
  symlink and rollback, certificate file modes, and refusing to start hostd
  when it cannot run. Linted with shellcheck. `ansible-lint` and `yamllint`
  are not used (not installed here); the YAML templates were not linted, except
  that the rendered autoinstall file is parsed as YAML when PyYAML is present.
- **Cloud Hypervisor and ch-remote install.** Artifact URLs and checksums are
  yours to supply; no version has been chosen or run with hostd.
- **cgroup setup.** The `prepare-host.sh` logic (enabling `cpu` and `memory`
  for `/sys/fs/cgroup/tachyon`) was written from the cgroup v2 docs and hostd's
  `jailer.rs` and has not been run. Systemd also manages the cgroup tree and may
  interfere.
- **nftables, `tc`, netns per VM.** hostd's own code, unit-tested against a
  command recorder only. The host prerequisites here are the packages and kernel
  modules; whether anything is missing is unknown.
- **ZFS sizing defaults.** ARC cap, `ashift=12`, compression and the memory
  reserve are starting values, not measured. `MEMORY_GB` (total RAM minus the
  reserve) is a guess at how much to advertise.
- **Network boot**, as above.
- **Certificate issuance.** There is no issuer integration; the `csr` step and
  `TLS_SOURCE_DIR` leave that to your provisioning system. No on-host renewal
  agent exists (README, Status).
- **Base images on a host.** Never built or booted; no image distribution.
- **The operator API against a real fleet.** It is unit-tested through the
  in-process hostlink with fake streams (drain stops placement, the host is
  told, empty detection, drain across reconnect, token handling). It has not
  been run against a live hostd, and hostd's response to `Drain` (it sets a
  flag and reports it in heartbeats; the scheduler does the actual skipping)
  has only its existing unit coverage.
- **Canary rollout of hostd** is a documented manual procedure, not
  automation; it has never been followed on real hosts.
- **Peer snapshot port** (`TACHYON_PEER_ADDRESS`): the peer service has no mTLS
  yet. The script leaves it unset by default, so no port is opened. If you set
  it, firewall the port to other fleet hosts yourself; this change adds no host
  firewall.
- **Control plane high availability.** One control plane process; the drain
  intent is in its memory.
