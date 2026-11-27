#!/usr/bin/env bash
# Takes a plain Ubuntu 24.04 host to a running tachyon-hostd. Provider
# agnostic: it needs only root, network access to the artifact sources, and
# a config file (see fleet.conf.example). Idempotent: every step can be
# re-run, and re-running never restarts a running hostd (a restart kills the
# jobs on the host; drain it first, see docs/fleet-operations.md).
#
#   tachyon-host-bootstrap.sh [-c CONF] [-n -r ROOT] STEP...
#
# Steps, in the order "all" runs them:
#   preflight packages sysctl zfs chv hostd cgroup tls env unit start
# Other steps: csr (make a key and CSR for the host certificate).
#
# -r ROOT writes files under ROOT and skips host checks (staged installs and
# tests). -n additionally prints the commands instead of running them, and
# needs -r. The tests use both. A dry run does not prove the commands work
# on a real host.
set -euo pipefail

CONF=/etc/tachyon/fleet.conf
DRY_RUN=0
ROOT=""

die() { echo "bootstrap: $*" >&2; exit 1; }
say() { echo "bootstrap: $*" >&2; }

run() {
  if [ "$DRY_RUN" = 1 ]; then
    printf '+ %s\n' "$*"
  else
    "$@"
  fi
}

# write_file PATH MODE < content: writes under ROOT, atomically, and only
# when the content changed.
write_file() {
  local dest="$ROOT$1" mode="$2" tmp
  mkdir -p "$(dirname "$dest")"
  tmp="$dest.new.$$"
  cat >"$tmp"
  chmod "$mode" "$tmp"
  if [ -f "$dest" ] && cmp -s "$tmp" "$dest"; then
    rm -f "$tmp"
    chmod "$mode" "$dest"
    say "unchanged $1"
  else
    mv "$tmp" "$dest"
    say "wrote $1"
  fi
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# fetch SRC DEST SHA256: SRC is a URL or a local path (an artifact store
# mounted on the host, or a file the provisioning system copied over).
# The checksum is mandatory: nothing unverified is installed as root.
fetch() {
  local src="$1" dest="$2" want="$3" tmp
  [ -n "$src" ] || die "no source configured for $dest"
  [ -n "$want" ] || die "no SHA256 configured for $src"
  if [ "$DRY_RUN" = 1 ]; then
    printf '+ fetch %s -> %s (sha256 %s)\n' "$src" "$dest" "$want"
    return 0
  fi
  mkdir -p "$(dirname "$dest")"
  tmp="$dest.new.$$"
  case "$src" in
    http://*) die "refusing plain http source $src" ;;
    https://*) curl -fsSL --retry 3 -o "$tmp" "$src" ;;
    *) cp "$src" "$tmp" ;;
  esac
  if [ "$(sha256_of "$tmp")" != "$want" ]; then
    rm -f "$tmp"
    die "checksum mismatch for $src"
  fi
  chmod 0755 "$tmp"
  mv "$tmp" "$dest"
}

# ---- config ----------------------------------------------------------

load_config() {
  [ -f "$CONF" ] || die "config $CONF not found (copy fleet.conf.example)"
  # The config is shell syntax and runs as root: it must be root-owned and
  # not writable by others.
  if [ -z "$ROOT" ]; then
    local perm
    perm=$(stat -c %a "$CONF")
    case "$perm" in *[2367]) die "$CONF is writable by group/others" ;; esac
    [ "$(stat -c %u "$CONF")" = 0 ] || die "$CONF is not owned by root"
  fi
  # shellcheck disable=SC1090
  . "$CONF"

  : "${REGION:=}" "${HOST_ID:=}" "${CONTROL_PLANE:=}" "${ZFS_POOL:=}"
  : "${FAILURE_DOMAIN:=default}" "${HOST_GROUP:=stable}"
  : "${ARCH:=}" "${VCPU:=}" "${MEMORY_GB:=}" "${RESERVE_MEMORY_GB:=8}"
  : "${ZFS_DEVICES:=}" "${ZFS_LAYOUT:=}" "${ZFS_CONFIRM_WIPE:=no}" "${ZFS_ARC_MAX_GB:=4}"
  : "${BASE_SNAPSHOTS:=}" "${WARM:=2:2,4:2,8:1}" "${IMAGES:=}" "${IMAGE_TRUST_SOURCE:=}"
  : "${CH_SOURCE:=}" "${CH_SHA256:=}" "${CH_REMOTE_SOURCE:=}" "${CH_REMOTE_SHA256:=}"
  : "${HOSTD_VERSION:=}" "${HOSTD_SOURCE:=}" "${HOSTD_SHA256:=}"
  : "${TLS_SOURCE_DIR:=}" "${TLS_SERVER_NAME:=}" "${TRUST_DOMAIN:=tachyon}"
  : "${EGRESS_MBIT:=}" "${MIRRORS:=}" "${DNS:=}" "${PEER_ADDRESS:=}" "${OTLP_ENDPOINT:=}"

  if [ -z "$ARCH" ]; then
    case "$(uname -m)" in
      x86_64) ARCH=x86_64 ;;
      aarch64 | arm64) ARCH=arm64 ;;
      *) die "unsupported machine $(uname -m)" ;;
    esac
  fi
  if [ -z "$VCPU" ]; then
    [ -z "$ROOT" ] || die "VCPU must be set with -r"
    VCPU=$(nproc)
  fi
  if [ -z "$MEMORY_GB" ]; then
    [ -z "$ROOT" ] || die "MEMORY_GB must be set with -r"
    # Total RAM minus a reserve for the host and the ZFS ARC. Memory is not
    # oversubscribed, so everything hostd advertises is promised to VMs.
    MEMORY_GB=$(awk -v r="$RESERVE_MEMORY_GB" '/MemTotal/ {print int($2/1048576) - r}' /proc/meminfo)
  fi

  [[ "$HOST_ID" =~ ^[A-Za-z0-9._-]{1,128}$ ]] || die "HOST_ID must match [A-Za-z0-9._-]{1,128} (it is the certificate identity)"
  [[ "$HOST_GROUP" =~ ^[a-z0-9][a-z0-9._-]*$ ]] || die "HOST_GROUP must be lowercase [a-z0-9._-]"
  [ -n "$REGION" ] || die "REGION is required"
  [ -n "$ZFS_POOL" ] || die "ZFS_POOL is required (e.g. tank/tachyon)"
  [ -n "$BASE_SNAPSHOTS" ] || die "BASE_SNAPSHOTS is required"
  case "$CONTROL_PLANE" in https://*) ;; *) die "CONTROL_PLANE must be an https:// URL; this script never configures an insecure hostlink" ;; esac
  [ "$MEMORY_GB" -ge 1 ] 2>/dev/null || die "MEMORY_GB is $MEMORY_GB; the reserve leaves nothing"
  [ -z "$HOSTD_VERSION" ] || [[ "$HOSTD_VERSION" =~ ^[A-Za-z0-9._+-]+$ ]] || die "HOSTD_VERSION has odd characters"
  local v
  for v in HOST_ID REGION FAILURE_DOMAIN HOST_GROUP CONTROL_PLANE TLS_SERVER_NAME ZFS_POOL BASE_SNAPSHOTS \
    WARM IMAGES EGRESS_MBIT MIRRORS DNS PEER_ADDRESS OTLP_ENDPOINT; do
    case "${!v}" in *$'\n'* | *\"* | *\\* | *\ *) die "$v contains a newline, quote, backslash or space" ;; esac
  done
}

pool_name() { echo "${ZFS_POOL%%/*}"; }

# ---- steps -----------------------------------------------------------

step_preflight() {
  if [ -n "$ROOT" ]; then say "preflight skipped with -r"; return; fi
  [ "$(id -u)" = 0 ] || die "must run as root"
  [ -r /etc/os-release ] || die "no /etc/os-release"
  # shellcheck disable=SC1091
  . /etc/os-release
  if [ "${ID:-}" != ubuntu ] || [ "${VERSION_ID:-}" != 24.04 ]; then
    die "only Ubuntu 24.04 is supported (this host: ${PRETTY_NAME:-unknown})"
  fi
  [ -c /dev/kvm ] || die "/dev/kvm is missing: enable virtualization in firmware, or this is a VM without nested virtualization"
  [ "$(stat -fc %T /sys/fs/cgroup)" = cgroup2fs ] || die "cgroup v2 is required"
  command -v systemctl >/dev/null || die "systemd is required"
}

step_packages() {
  run env DEBIAN_FRONTEND=noninteractive apt-get update -qq
  run env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    zfsutils-linux nftables iproute2 curl ca-certificates openssl util-linux
}

step_sysctl() {
  write_file /etc/sysctl.d/90-tachyon.conf 0644 <<'E'
# Managed by tachyon-host-bootstrap.sh. hostd also sets ip_forward itself;
# this keeps it across reboots and before hostd starts.
net.ipv4.ip_forward = 1
# Every job VM is a few conntrack entries and many short connections; the
# default table is small. Starting value, not measured. The leading "-" makes
# sysctl ignore it where the conntrack module is not loaded yet.
-net.netfilter.nf_conntrack_max = 1048576
E
  write_file /etc/modules-load.d/tachyon.conf 0644 <<'E'
# Managed by tachyon-host-bootstrap.sh. hostd builds a netns, veth pair, tap,
# nftables rules and a tbf qdisc per VM.
tun
veth
nf_tables
nf_nat
nf_conntrack
sch_tbf
E
  local m
  for m in tun veth nf_tables nf_nat nf_conntrack sch_tbf; do
    # Built in on some kernels; the unit's preflight is the real check.
    run modprobe "$m" || say "modprobe $m failed (built in?)"
  done
  run sysctl --system
}

# Base snapshots hold full guest memory (4 GiB per vCPU, about 120 GB for
# sizes 2-16 per image version): too big for the root volume.
zfs_images() {
  local ds="$ZFS_POOL/images"
  if [ "$DRY_RUN" = 1 ]; then
    run zfs create -p -o mountpoint="$IMAGES" "$ds"
    return
  fi
  if ! zfs list -H -o name "$ds" >/dev/null 2>&1; then
    # Mounting over a populated store would hide its images from hostd
    # while they keep filling the root volume.
    if [ -d "$IMAGES" ] && [ -n "$(ls -A "$IMAGES")" ]; then
      die "$IMAGES already holds images on the root volume: move them aside, rerun, then copy them into $IMAGES"
    fi
    run zfs create -p -o mountpoint="$IMAGES" "$ds"
  fi
  [ "$(findmnt -n -o SOURCE --target "$IMAGES")" = "$ds" ] ||
    die "$IMAGES is not mounted from $ds; prepare-image would fill the root volume"
}

step_zfs() {
  local pool exists=0
  pool=$(pool_name)
  if [ "$DRY_RUN" = 1 ]; then
    [ "${FLEET_TEST_POOL_EXISTS:-0}" != 1 ] || exists=1
  else
    zpool list -H -o name "$pool" >/dev/null 2>&1 && exists=1
  fi
  if [ "$exists" = 1 ]; then
    say "zpool $pool exists; not touching its devices"
  else
    [ -n "$ZFS_DEVICES" ] || die "zpool $pool does not exist and ZFS_DEVICES is empty"
    [ "$ZFS_CONFIRM_WIPE" = yes ] ||
      die "zpool $pool does not exist: creating it ERASES $ZFS_DEVICES. Set ZFS_CONFIRM_WIPE=yes to proceed"
    local d
    for d in $ZFS_DEVICES; do
      case "$d" in /dev/disk/by-id/*) ;; *) die "use stable /dev/disk/by-id/ paths, not $d" ;; esac
      if [ "$DRY_RUN" != 1 ]; then
        [ -b "$d" ] || die "$d is not a block device"
        ! findmnt -rn -S "$d" >/dev/null || die "$d is mounted"
      fi
      run wipefs -a "$d"
    done
    # ashift=12: 4K sectors, right for NVMe. No redundancy unless
    # ZFS_LAYOUT=mirror: every dataset here is rebuildable from images and
    # peers, and the fleet is sized for losing a host (docs/architecture.md).
    # shellcheck disable=SC2086
    run zpool create -o ashift=12 -O compression=lz4 -O atime=off -O mountpoint=none \
      "$pool" $ZFS_LAYOUT $ZFS_DEVICES
  fi
  if [ "$pool" != "$ZFS_POOL" ]; then
    run zfs create -p -o mountpoint=none "$ZFS_POOL" || say "dataset $ZFS_POOL exists"
  fi
  [ -z "$IMAGES" ] || zfs_images
  write_file /etc/modprobe.d/zfs-tachyon.conf 0644 <<E
# Managed by tachyon-host-bootstrap.sh. Caps the ARC so VM memory is not
# starved; starting value, not measured. Applies at next module load.
options zfs zfs_arc_max=$((ZFS_ARC_MAX_GB * 1073741824))
E
  if [ -z "$ROOT" ] && [ -w /sys/module/zfs/parameters/zfs_arc_max ]; then
    echo $((ZFS_ARC_MAX_GB * 1073741824)) >/sys/module/zfs/parameters/zfs_arc_max
  fi
}

step_chv() {
  fetch "$CH_SOURCE" "$ROOT/usr/local/bin/cloud-hypervisor" "$CH_SHA256"
  fetch "$CH_REMOTE_SOURCE" "$ROOT/usr/local/bin/ch-remote" "$CH_REMOTE_SHA256"
}

# Installs a release under releases/<version>/ and points the active symlink
# at it. Re-running with an older HOSTD_VERSION that is already on disk only
# repoints the symlink: that is the rollback. Neither restarts hostd.
step_hostd() {
  [ -n "$HOSTD_VERSION" ] || die "HOSTD_VERSION is required"
  local rel="/usr/local/lib/tachyon/releases/$HOSTD_VERSION"
  if [ -x "$ROOT$rel/tachyon-hostd" ]; then
    say "release $HOSTD_VERSION already installed"
  else
    mkdir -p "$ROOT$rel"
    fetch "$HOSTD_SOURCE" "$ROOT$rel/tachyon-hostd" "$HOSTD_SHA256"
  fi
  mkdir -p "$ROOT/usr/local/bin"
  run ln -sfn "$rel/tachyon-hostd" "$ROOT/usr/local/bin/tachyon-hostd"
  printf '%s\n' "$HOSTD_VERSION" | write_file /etc/tachyon/hostd.version 0644
  say "hostd $HOSTD_VERSION is the installed release; a running hostd keeps the old binary until restarted"
}

step_cgroup() {
  # hostd creates one cgroup per VM under TACHYON_CGROUP and writes cpu.weight
  # and memory.max there, so cpu and memory must be enabled for its children.
  # The cgroup tree is not persistent: the unit's ExecStartPre redoes this.
  write_file /usr/local/lib/tachyon/prepare-host.sh 0755 <<'E'
#!/bin/sh
# Managed by tachyon-host-bootstrap.sh. Runs before every hostd start.
set -eu
CG="${TACHYON_CGROUP:-/sys/fs/cgroup/tachyon}"
root=$(dirname "$CG")
echo "+cpu +memory" >"$root/cgroup.subtree_control" 2>/dev/null || true
mkdir -p "$CG"
echo "+cpu +memory" >"$CG/cgroup.subtree_control"
mkdir -p "${TACHYON_RUN_DIR:-/run/tachyon/vms}" /run/tachyon/vm
E
}

step_csr() {
  local dir="$ROOT/etc/tachyon/tls"
  mkdir -p "$dir"
  if [ -f "$dir/host.key" ]; then
    say "key exists; keeping it"
  else
    (umask 077 && run openssl ecparam -name prime256v1 -genkey -noout -out "$dir/host.key")
  fi
  # The issuer must check the host's provisioning record before signing, and
  # add the clientAuth extended key usage (docs/hostlink-mtls.md).
  run openssl req -new -key "$dir/host.key" -subj "/CN=$HOST_ID" \
    -addext "subjectAltName=URI:spiffe://$TRUST_DOMAIN/host/$HOST_ID" -out "$dir/host.csr"
  say "CSR for spiffe://$TRUST_DOMAIN/host/$HOST_ID at /etc/tachyon/tls/host.csr; install the signed certificate as host.crt and the server CA as ca.crt, then run the tls step"
}

step_tls() {
  local d="$ROOT/etc/tachyon/tls"
  mkdir -p "$d"
  chmod 0755 "$d"
  if [ -n "$TLS_SOURCE_DIR" ]; then
    local f
    for f in host.crt host.key ca.crt; do
      [ -f "$TLS_SOURCE_DIR/$f" ] || die "$TLS_SOURCE_DIR/$f is missing"
    done
    # Key first, then certificate, each renamed into place, as the rotation
    # procedure in docs/hostlink-mtls.md does.
    (umask 077 && cp "$TLS_SOURCE_DIR/host.key" "$d/host.key.new" && chmod 0600 "$d/host.key.new" && mv "$d/host.key.new" "$d/host.key")
    cp "$TLS_SOURCE_DIR/host.crt" "$d/host.crt.new" && chmod 0644 "$d/host.crt.new" && mv "$d/host.crt.new" "$d/host.crt"
    cp "$TLS_SOURCE_DIR/ca.crt" "$d/ca.crt.new" && chmod 0644 "$d/ca.crt.new" && mv "$d/ca.crt.new" "$d/ca.crt"
    say "installed certificate material for $HOST_ID"
  else
    say "TLS_SOURCE_DIR unset: leaving /etc/tachyon/tls as is"
  fi
  if [ -f "$d/host.key" ]; then chmod 0600 "$d/host.key"; fi
}

step_env() {
  {
    echo "# Managed by tachyon-host-bootstrap.sh from $CONF. No secrets live here."
    echo "TACHYON_HOST_ID=$HOST_ID"
    echo "TACHYON_REGION=$REGION"
    echo "TACHYON_FAILURE_DOMAIN=$FAILURE_DOMAIN"
    echo "TACHYON_HOST_GROUP=$HOST_GROUP"
    echo "TACHYON_ARCH=$ARCH"
    echo "TACHYON_VCPU=$VCPU"
    echo "TACHYON_MEMORY_GB=$MEMORY_GB"
    echo "TACHYON_CONTROL_PLANE=$CONTROL_PLANE"
    echo "TACHYON_TLS_CERT=/etc/tachyon/tls/host.crt"
    echo "TACHYON_TLS_KEY=/etc/tachyon/tls/host.key"
    echo "TACHYON_TLS_CA=/etc/tachyon/tls/ca.crt"
    [ -z "$TLS_SERVER_NAME" ] || echo "TACHYON_TLS_SERVER_NAME=$TLS_SERVER_NAME"
    echo "TACHYON_ZFS_POOL=$ZFS_POOL"
    echo "TACHYON_BASE_SNAPSHOTS=$BASE_SNAPSHOTS"
    echo "TACHYON_WARM=$WARM"
    echo "TACHYON_CGROUP=/sys/fs/cgroup/tachyon"
    echo "TACHYON_RUN_DIR=/run/tachyon/vms"
    echo "TACHYON_STATE_DIR=/var/lib/tachyon/hostd"
    echo "TACHYON_CH_BINARY=/usr/local/bin/cloud-hypervisor"
    echo "TACHYON_CH_REMOTE=/usr/local/bin/ch-remote"
    if [ -n "$IMAGES" ]; then
      echo "TACHYON_IMAGES=$IMAGES"
      [ -z "$IMAGE_TRUST_SOURCE" ] || echo "TACHYON_IMAGE_TRUST=/etc/tachyon/image-trust.pem"
    fi
    [ -z "$EGRESS_MBIT" ] || echo "TACHYON_EGRESS_MBIT=$EGRESS_MBIT"
    [ -z "$MIRRORS" ] || echo "TACHYON_MIRRORS=$MIRRORS"
    [ -z "$DNS" ] || echo "TACHYON_DNS=$DNS"
    # Unset means hostd serves no snapshot pulls and opens no peer port. The
    # peer service has no mTLS yet (hostd/src/main.rs): when you set this,
    # allow the port only from other fleet hosts.
    [ -z "$PEER_ADDRESS" ] || echo "TACHYON_PEER_ADDRESS=$PEER_ADDRESS"
    [ -z "$OTLP_ENDPOINT" ] || echo "OTEL_EXPORTER_OTLP_ENDPOINT=$OTLP_ENDPOINT"
  } | write_file /etc/tachyon/hostd.env 0644
  if [ -n "$IMAGE_TRUST_SOURCE" ]; then
    run cp "$IMAGE_TRUST_SOURCE" "$ROOT/etc/tachyon/image-trust.pem"
  fi
  mkdir -p "$ROOT/var/lib/tachyon/hostd"
}

step_unit() {
  write_file /etc/systemd/system/tachyon-hostd.service 0644 <<'E'
[Unit]
Description=Tachyon host agent
Documentation=https://github.com/decover-tech/tachyon/blob/main/docs/fleet-operations.md
# docker.service: hostd adds its accept rules to the DOCKER-USER chain that
# Docker creates, if Docker is installed (an After= on a missing unit is a no-op).
After=network-online.target zfs-import.target zfs-mount.service docker.service
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/tachyon/hostd.env
ExecStartPre=/usr/local/lib/tachyon/prepare-host.sh
ExecStart=/usr/local/bin/tachyon-hostd
# hostd shuts down on SIGINT. Stopping it ends the jobs on the host (the next
# start reports them as infrastructure errors): drain the host first.
KillSignal=SIGINT
TimeoutStopSec=60
Restart=always
RestartSec=5
LimitNOFILE=1048576
TasksMax=infinity

[Install]
WantedBy=multi-user.target
E
  run systemctl daemon-reload
}

# Starts hostd only when it can run: it exits at startup without TLS files,
# and without a base snapshot for every warm VM size.
step_start() {
  local missing=0 f
  for f in host.crt host.key ca.crt; do
    [ -f "$ROOT/etc/tachyon/tls/$f" ] || { say "missing /etc/tachyon/tls/$f"; missing=1; }
  done
  local part size
  for part in $(echo "$WARM" | tr ',' ' '); do
    size="${part%%:*}"
    [ -d "$ROOT$BASE_SNAPSHOTS/$size" ] || { say "missing base snapshot dir $BASE_SNAPSHOTS/$size"; missing=1; }
  done
  if [ "$missing" = 1 ]; then
    say "hostd NOT started. Provide the above (docs/fleet-operations.md) and run: $0 -c $CONF start"
    exit 3
  fi
  run systemctl enable tachyon-hostd.service
  # start, not restart: a running hostd is left alone.
  run systemctl start tachyon-hostd.service
}

ALL="preflight packages sysctl zfs chv hostd cgroup tls env unit start"

main() {
  while getopts "c:nr:" o; do
    case "$o" in
      c) CONF="$OPTARG" ;;
      n) DRY_RUN=1 ;;
      r) ROOT="${OPTARG%/}" ;;
      *) sed -n '2,15p' "$0"; exit 2 ;;
    esac
  done
  shift $((OPTIND - 1))
  [ $# -gt 0 ] || { sed -n '2,15p' "$0"; exit 2; }
  if [ "$DRY_RUN" = 1 ] && [ -z "$ROOT" ]; then die "-n needs -r ROOT"; fi
  load_config
  local steps="$*" s
  [ "$steps" != all ] || steps="$ALL"
  for s in $steps; do
    case " $ALL csr " in *" $s "*) ;; *) die "unknown step $s" ;; esac
  done
  for s in $steps; do
    say "== $s"
    "step_$s"
  done
}

main "$@"
