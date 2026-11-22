#!/usr/bin/env bash
# Tests for tachyon-host-bootstrap.sh and the netboot renderer. They check
# what the scripts render, the order and arguments of the commands they would
# run, and their guards (refusing to wipe disks, to install unverified
# binaries, to start hostd that cannot run). They do NOT run apt, ZFS,
# systemd or Cloud Hypervisor: nothing here proves a host comes up.
# shellcheck disable=SC2015,SC2016
set -u
HERE=$(cd "$(dirname "$0")" && pwd)
BOOT="$HERE/../tachyon-host-bootstrap.sh"
RENDER="$HERE/../netboot/render.sh"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
FAILS=0

ok() { echo "ok   $1"; }
fail() { echo "FAIL $1"; FAILS=$((FAILS + 1)); }
contains() { grep -qF -- "$2" <<<"$1"; }
check() { # name haystack needle
  if contains "$2" "$3"; then ok "$1"; else fail "$1: missing: $3"; fi
}
refute() { # name haystack needle
  if contains "$2" "$3"; then fail "$1: unexpected: $3"; else ok "$1"; fi
}

# A sha256 of a fake artifact, computed the way the script does.
sha() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

printf '#!/bin/sh\necho hostd 0.1.0\n' >"$TMP/hostd-0.1.0"
printf '#!/bin/sh\necho hostd 0.2.0\n' >"$TMP/hostd-0.2.0"
printf 'ch' >"$TMP/ch"
printf 'chr' >"$TMP/chr"

conf() { # path [extra lines]
  local f="$1"
  shift
  cat >"$f" <<E
HOST_ID=fra-01
REGION=eu-central
FAILURE_DOMAIN=rack-a
HOST_GROUP=canary
CONTROL_PLANE=https://cp.internal:9090
VCPU=64
MEMORY_GB=200
ZFS_POOL=tank/tachyon
ZFS_DEVICES="/dev/disk/by-id/nvme-A /dev/disk/by-id/nvme-B"
BASE_SNAPSHOTS=/var/lib/tachyon/base/snapshots
CH_SOURCE=$TMP/ch
CH_SHA256=$(sha "$TMP/ch")
CH_REMOTE_SOURCE=$TMP/chr
CH_REMOTE_SHA256=$(sha "$TMP/chr")
HOSTD_VERSION=0.1.0
HOSTD_SOURCE=$TMP/hostd-0.1.0
HOSTD_SHA256=$(sha "$TMP/hostd-0.1.0")
E
  for l in "$@"; do echo "$l" >>"$f"; done
}

# ---- 1. dry run of every step, pool already present --------------------
R1="$TMP/r1"
conf "$TMP/a.conf"
out=$(FLEET_TEST_POOL_EXISTS=1 "$BOOT" -c "$TMP/a.conf" -n -r "$R1" all 2>&1); rc=$?
[ $rc -eq 3 ] && ok "all stops before start when TLS and snapshots are missing" || fail "all rc=$rc, want 3: $out"
check "start explains what is missing" "$out" "missing /etc/tachyon/tls/host.crt"
check "start names the missing snapshot dir" "$out" "missing base snapshot dir /var/lib/tachyon/base/snapshots/2"
refute "start did not enable hostd" "$out" "systemctl enable"
check "packages step" "$out" "+ env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq zfsutils-linux nftables iproute2"
check "existing pool is left alone" "$out" "zpool tank exists; not touching its devices"
refute "no zpool create on existing pool" "$out" "zpool create"
check "dataset created" "$out" "+ zfs create -p -o mountpoint=none tank/tachyon"
check "ch-remote verified install" "$out" "fetch $TMP/chr -> $R1/usr/local/bin/ch-remote (sha256 $(sha "$TMP/chr"))"

env_file="$R1/etc/tachyon/hostd.env"
envtxt=$(cat "$env_file")
for kv in TACHYON_HOST_ID=fra-01 TACHYON_REGION=eu-central TACHYON_HOST_GROUP=canary TACHYON_FAILURE_DOMAIN=rack-a \
  TACHYON_VCPU=64 TACHYON_MEMORY_GB=200 TACHYON_CONTROL_PLANE=https://cp.internal:9090 \
  TACHYON_TLS_CERT=/etc/tachyon/tls/host.crt TACHYON_TLS_KEY=/etc/tachyon/tls/host.key TACHYON_TLS_CA=/etc/tachyon/tls/ca.crt \
  TACHYON_ZFS_POOL=tank/tachyon TACHYON_BASE_SNAPSHOTS=/var/lib/tachyon/base/snapshots TACHYON_WARM=2:2,4:2,8:1 \
  TACHYON_CGROUP=/sys/fs/cgroup/tachyon; do
  check "env has $kv" "$envtxt" "$kv"
done
refute "env never sets insecure hostlink" "$envtxt" "INSECURE"
refute "env has no peer address unless configured" "$envtxt" "TACHYON_PEER_ADDRESS"
unit=$(cat "$R1/etc/systemd/system/tachyon-hostd.service")
check "unit reads env file" "$unit" "EnvironmentFile=/etc/tachyon/hostd.env"
check "unit prepares cgroup" "$unit" "ExecStartPre=/usr/local/lib/tachyon/prepare-host.sh"
check "unit uses the active release symlink" "$unit" "ExecStart=/usr/local/bin/tachyon-hostd"
check "unit stops hostd with SIGINT" "$unit" "KillSignal=SIGINT"
check "cgroup helper enables controllers" "$(cat "$R1/usr/local/lib/tachyon/prepare-host.sh")" '"+cpu +memory"'
check "sysctl enables forwarding" "$(cat "$R1/etc/sysctl.d/90-tachyon.conf")" "net.ipv4.ip_forward = 1"
check "modules list has sch_tbf" "$(cat "$R1/etc/modules-load.d/tachyon.conf")" "sch_tbf"
check "arc cap rendered" "$(cat "$R1/etc/modprobe.d/zfs-tachyon.conf")" "zfs_arc_max=4294967296"
check "hostd release fetched with checksum" "$out" "fetch $TMP/hostd-0.1.0 -> $R1/usr/local/lib/tachyon/releases/0.1.0/tachyon-hostd (sha256 $(sha "$TMP/hostd-0.1.0"))"
[ "$(cat "$R1/etc/tachyon/hostd.version")" = 0.1.0 ] && ok "version recorded" || fail "version file"

# ---- 2. start succeeds once TLS and snapshots exist ----------------------
mkdir -p "$R1/etc/tachyon/tls" "$R1/var/lib/tachyon/base/snapshots/2" "$R1/var/lib/tachyon/base/snapshots/4" "$R1/var/lib/tachyon/base/snapshots/8"
for f in host.crt host.key ca.crt; do : >"$R1/etc/tachyon/tls/$f"; done
out=$(FLEET_TEST_POOL_EXISTS=1 "$BOOT" -c "$TMP/a.conf" -n -r "$R1" start 2>&1); rc=$?
[ $rc -eq 0 ] && ok "start proceeds when prerequisites exist" || fail "start rc=$rc: $out"
check "start enables" "$out" "+ systemctl enable tachyon-hostd.service"
check "start does not restart" "$out" "+ systemctl start tachyon-hostd.service"
refute "start never restarts a running hostd" "$out" "restart"

# ---- 3. ZFS guards --------------------------------------------------------
out=$("$BOOT" -c "$TMP/a.conf" -n -r "$TMP/r3" zfs 2>&1); rc=$?
[ $rc -ne 0 ] && ok "pool creation refused without confirmation" || fail "zfs created pool unconfirmed"
check "refusal says it erases" "$out" "ERASES"
refute "no wipe without confirmation" "$out" "wipefs"
conf "$TMP/b.conf" ZFS_CONFIRM_WIPE=yes ZFS_LAYOUT=mirror
out=$("$BOOT" -c "$TMP/b.conf" -n -r "$TMP/r3" zfs 2>&1); rc=$?
[ $rc -eq 0 ] && ok "confirmed pool creation plans" || fail "confirmed zfs rc=$rc: $out"
check "wipes first device" "$out" "+ wipefs -a /dev/disk/by-id/nvme-A"
check "creates mirrored pool" "$out" "+ zpool create -o ashift=12 -O compression=lz4 -O atime=off -O mountpoint=none tank mirror /dev/disk/by-id/nvme-A /dev/disk/by-id/nvme-B"
conf "$TMP/c.conf" ZFS_CONFIRM_WIPE=yes 'ZFS_DEVICES="/dev/nvme0n1"'
out=$("$BOOT" -c "$TMP/c.conf" -n -r "$TMP/r3" zfs 2>&1); rc=$?
[ $rc -ne 0 ] && ok "unstable device names refused" || fail "accepted /dev/nvme0n1"

# ---- 4. config validation -------------------------------------------------
conf "$TMP/d.conf" CONTROL_PLANE=http://cp.internal:9090
out=$("$BOOT" -c "$TMP/d.conf" -n -r "$TMP/r4" env 2>&1); rc=$?
[ $rc -ne 0 ] && ok "http control plane refused" || fail "http control plane accepted"
conf "$TMP/e.conf" HOST_ID=../evil
"$BOOT" -c "$TMP/e.conf" -n -r "$TMP/r4" env >/dev/null 2>&1 && fail "bad HOST_ID accepted" || ok "bad HOST_ID refused"
conf "$TMP/f.conf" 'REGION="eu central"'
"$BOOT" -c "$TMP/f.conf" -n -r "$TMP/r4" env >/dev/null 2>&1 && fail "space in value accepted" || ok "space in env value refused"
"$BOOT" -c "$TMP/a.conf" -n -r "$TMP/r4" nonsense >/dev/null 2>&1 && fail "unknown step accepted" || ok "unknown step refused"
"$BOOT" -c "$TMP/a.conf" -n nonsense >/dev/null 2>&1 && fail "-n without -r accepted" || ok "-n without -r refused"

# ---- 5. artifact verification, release install and rollback (real, staged)
R5="$TMP/r5"
conf "$TMP/g.conf"
out=$("$BOOT" -c "$TMP/g.conf" -r "$R5" hostd 2>&1); rc=$?
[ $rc -eq 0 ] && ok "staged hostd install" || fail "hostd install rc=$rc: $out"
[ "$(readlink "$R5/usr/local/bin/tachyon-hostd")" = /usr/local/lib/tachyon/releases/0.1.0/tachyon-hostd ] && ok "active symlink points at 0.1.0" || fail "symlink: $(readlink "$R5/usr/local/bin/tachyon-hostd")"

conf "$TMP/h.conf" HOSTD_VERSION=0.2.0 "HOSTD_SOURCE=$TMP/hostd-0.2.0" "HOSTD_SHA256=$(sha "$TMP/hostd-0.2.0")"
"$BOOT" -c "$TMP/h.conf" -r "$R5" hostd >/dev/null 2>&1
[ "$(readlink "$R5/usr/local/bin/tachyon-hostd")" = /usr/local/lib/tachyon/releases/0.2.0/tachyon-hostd ] && ok "upgrade repoints to 0.2.0" || fail "upgrade symlink"
[ -x "$R5/usr/local/lib/tachyon/releases/0.1.0/tachyon-hostd" ] && ok "old release kept for rollback" || fail "old release removed"

# Rollback: the old version is on disk, so no source or checksum is needed.
conf "$TMP/i.conf" HOSTD_VERSION=0.1.0 HOSTD_SOURCE= HOSTD_SHA256=
"$BOOT" -c "$TMP/i.conf" -r "$R5" hostd >/dev/null 2>&1
[ "$(readlink "$R5/usr/local/bin/tachyon-hostd")" = /usr/local/lib/tachyon/releases/0.1.0/tachyon-hostd ] && ok "rollback repoints to 0.1.0" || fail "rollback symlink"
[ "$(cat "$R5/etc/tachyon/hostd.version")" = 0.1.0 ] && ok "rollback recorded" || fail "rollback version file"

conf "$TMP/j.conf" HOSTD_VERSION=0.3.0 "HOSTD_SOURCE=$TMP/hostd-0.2.0" HOSTD_SHA256=0000000000000000000000000000000000000000000000000000000000000000
"$BOOT" -c "$TMP/j.conf" -r "$R5" hostd >/dev/null 2>&1 && fail "checksum mismatch accepted" || ok "checksum mismatch refused"
[ ! -e "$R5/usr/local/lib/tachyon/releases/0.3.0/tachyon-hostd" ] && ok "unverified binary not installed" || fail "unverified binary installed"
conf "$TMP/k.conf" HOSTD_VERSION=0.4.0 "HOSTD_SOURCE=$TMP/hostd-0.2.0" HOSTD_SHA256=
"$BOOT" -c "$TMP/k.conf" -r "$R5" hostd >/dev/null 2>&1 && fail "missing checksum accepted" || ok "missing checksum refused"
conf "$TMP/l.conf" HOSTD_VERSION=0.5.0 "HOSTD_SOURCE=http://example.invalid/hostd" "HOSTD_SHA256=$(sha "$TMP/hostd-0.2.0")"
"$BOOT" -c "$TMP/l.conf" -r "$R5" hostd >/dev/null 2>&1 && fail "plain http source accepted" || ok "plain http source refused"

# ---- 6. certificate placement ---------------------------------------------
R6="$TMP/r6"
mkdir -p "$TMP/tls"
for f in host.crt host.key ca.crt; do echo "$f" >"$TMP/tls/$f"; done
conf "$TMP/m.conf" "TLS_SOURCE_DIR=$TMP/tls"
"$BOOT" -c "$TMP/m.conf" -r "$R6" tls >/dev/null 2>&1 && ok "tls installs" || fail "tls step failed"
perm() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }
[ "$(perm "$R6/etc/tachyon/tls/host.key")" = 600 ] && ok "private key is 0600" || fail "key mode $(perm "$R6/etc/tachyon/tls/host.key")"
[ "$(perm "$R6/etc/tachyon/tls/host.crt")" = 644 ] && ok "certificate is 0644" || fail "crt mode"
rm "$TMP/tls/ca.crt"
"$BOOT" -c "$TMP/m.conf" -r "$R6" tls >/dev/null 2>&1 && fail "tls accepted incomplete source" || ok "tls refuses incomplete source dir"

# ---- 7. idempotent re-run: files unchanged ---------------------------------
out=$(FLEET_TEST_POOL_EXISTS=1 "$BOOT" -c "$TMP/a.conf" -n -r "$R1" sysctl env unit 2>&1)
check "re-run leaves env unchanged" "$out" "unchanged /etc/tachyon/hostd.env"
check "re-run leaves unit unchanged" "$out" "unchanged /etc/systemd/system/tachyon-hostd.service"

# ---- 8. netboot rendering ---------------------------------------------------
if [ -x "$RENDER" ]; then
  MAC=aa-bb-cc-dd-ee-ff
  export NETBOOT_BASE_URL=https://boot.example.invalid/tachyon BOOTSTRAP_URL=https://boot.example.invalid/tachyon/tachyon-host-bootstrap.sh
  BOOTSTRAP_SHA256=$(sha "$BOOT")
  export BOOTSTRAP_SHA256 HOST_ID=fra-01 HOST_MAC=$MAC OS_DISK_SERIAL=SN123 FLEET_CONF="$TMP/a.conf"
  export SSH_AUTHORIZED_KEY="ssh-ed25519 AAAATEST op@example"
  out=$("$RENDER" "$TMP/nb" 2>&1); rc=$?
  [ $rc -eq 0 ] && ok "netboot renders" || fail "render rc=$rc: $out"
  H="$TMP/nb/hosts/$MAC"
  check "entry chains by MAC" "$(cat "$TMP/nb/entry.ipxe")" 'chain https://boot.example.invalid/tachyon/hosts/${net0/mac:hexhyp}/boot.ipxe'
  check "ipxe fetches kernel" "$(cat "$H/boot.ipxe")" "kernel https://boot.example.invalid/tachyon/vmlinuz"
  check "ipxe points installer at the user-data" "$(cat "$H/boot.ipxe")" "ds=nocloud-net;s=https://boot.example.invalid/tachyon/hosts/$MAC/"
  check "user-data carries the key" "$(cat "$H/user-data")" "ssh-ed25519 AAAATEST op@example"
  check "user-data selects the OS disk by serial" "$(cat "$H/user-data")" 'serial: "SN123"'
  check "user-data embeds fleet.conf" "$(cat "$H/user-data")" "          HOST_ID=fra-01"
  check "user-data verifies the bootstrap checksum" "$(cat "$H/user-data")" "$BOOTSTRAP_SHA256  /usr/local/sbin/tachyon-host-bootstrap.sh"
  rendered=$(cat "$TMP/nb/entry.ipxe" "$H/boot.ipxe" "$H/user-data" "$H/meta-data" | grep -v 'net0/mac')
  refute "no unreplaced variables" "$rendered" '${'
  [ "$(perm "$H/user-data")" = 600 ] && ok "user-data is 0600" || fail "user-data mode"
  if command -v python3 >/dev/null && python3 -c 'import yaml' 2>/dev/null; then
    python3 -c 'import sys,yaml; d=yaml.safe_load(open(sys.argv[1])); assert d["autoinstall"]["user-data"]["write_files"][0]["content"].startswith("HOST_ID")' "$H/user-data" \
      && ok "user-data is valid YAML with the conf as file content" || fail "user-data YAML"
  else
    echo "skip user-data YAML parse (PyYAML not installed)"
  fi
  (unset BOOTSTRAP_URL; "$RENDER" "$TMP/nb2" >/dev/null 2>&1) && fail "render accepted missing vars" || ok "render refuses missing variables"
  (NETBOOT_BASE_URL=http://insecure.invalid "$RENDER" "$TMP/nb3" >/dev/null 2>&1) && fail "render accepted http" || ok "render refuses http base URL"
  (HOST_MAC=AA:BB "$RENDER" "$TMP/nb4" >/dev/null 2>&1) && fail "render accepted bad MAC" || ok "render refuses bad MAC"
else
  fail "netboot/render.sh missing"
fi

echo
if [ "$FAILS" -eq 0 ]; then echo "all passed"; else echo "$FAILS failed"; exit 1; fi
