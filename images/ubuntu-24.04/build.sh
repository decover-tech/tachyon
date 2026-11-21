#!/usr/bin/env bash
# Builds Tachyon's ubuntu-24.04 base image from actions/runner-images:
#
#   OUT_DIR/rootfs.ext4             the read-only lower layer hosts map via virtio-pmem
#   OUT_DIR/sbom.spdx.json          SPDX 2.3 SBOM (dpkg packages + tool cache)
#   OUT_DIR/manifest.json           what was built, from what, with digests
#   OUT_DIR/manifest.signed.json    ed25519-signed manifest (if a key is given)
#
# Per-size paused Cloud Hypervisor snapshots are not made here: each host
# makes them from the verified ext4 file (`tachyon-hostd prepare-image`),
# because a memory snapshot is tied to the host's Cloud Hypervisor build.
#
# Runs on a Linux builder as root (it chroots into the image). Needs curl,
# git, xz, sha256sum and e2fsprogs >= 1.47.1 (for E2FSPROGS_FAKE_TIME and
# mkfs.ext4 -d), and tachyon-image (go build ./cmd/tachyon-image).
#
#   sudo TACHYON_IMAGE_SIGNING_KEY=/secure/key.pem images/ubuntu-24.04/build.sh /var/tmp/img
#
# Environment:
#   TACHYON_IMAGE                 tachyon-image binary (default: on PATH)
#   TACHYON_IMAGE_VERSION         image version (default: this week's Monday)
#   APT_SNAPSHOT                  snapshot.ubuntu.com timestamp (default: the version's date)
#   TACHYON_IMAGE_SIGNING_KEY     ed25519 PKCS#8 PEM; unset = unsigned (dev builds only)
#   KERNEL                        guest kernel (vmlinux) to ship alongside, optional
#
# Reproducibility: every input is pinned (runner-images commit, base rootfs
# digest, apt snapshot, SOURCE_DATE_EPOCH) and the ext4 file is written
# deterministically (fixed UUID, hash seed and timestamps). Some upstream
# install scripts still download "latest" artifacts outside apt; build twice
# and compare with `tachyon-image diff a/manifest.json b/manifest.json`.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
out=${1:?usage: build.sh OUT_DIR}
ti=${TACHYON_IMAGE:-tachyon-image}

[[ $(uname -s) == Linux ]] || { echo "build.sh runs on Linux" >&2; exit 1; }
[[ $EUID -eq 0 ]] || { echo "build.sh must run as root (it chroots into the image)" >&2; exit 1; }

eval "$("$ti" env -config "$here/image.yml")"
version=${TACHYON_IMAGE_VERSION:-$("$ti" version)}
day=${version%%.*}
apt_snapshot=${APT_SNAPSHOT:-$(date -u -d "$day" +%Y%m%dT000000Z)}
SOURCE_DATE_EPOCH=$(date -u -d "$day" +%s)
export SOURCE_DATE_EPOCH
ref="$IMAGE_NAME@$version"

mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d /var/tmp/tachyon-image.XXXXXX)
root=$work/root
mounts=()

cleanup() {
	local i
	for ((i = ${#mounts[@]} - 1; i >= 0; i--)); do
		umount -l "${mounts[i]}" 2>/dev/null || true
	done
	rm -rf "$work"
}
trap cleanup EXIT

log() { printf '==> %s\n' "$*" >&2; }

# ---- 1. Pinned sources ------------------------------------------------------
log "$ref: actions/runner-images@$RUNNER_IMAGES_COMMIT, apt snapshot $apt_snapshot"
ri=$work/runner-images
git init -q "$ri"
git -C "$ri" fetch -q --depth 1 "$RUNNER_IMAGES_REPO" "$RUNNER_IMAGES_COMMIT"
git -C "$ri" checkout -q FETCH_HEAD

curl -fsSL --retry 3 -o "$work/base.tar.xz" "$BASE_ROOTFS_URL"
echo "$BASE_ROOTFS_SHA256  $work/base.tar.xz" | sha256sum -c --quiet -

# ---- 2. Build plan: upstream template minus image.yml's trims ---------------
"$ti" plan -config "$here/image.yml" -runner-images "$ri" \
	-version "$version" -apt-snapshot "$apt_snapshot" -o "$work/plan.sh"
plan_sha=$(sed -n 's/^# plan sha256 //p' "$work/plan.sh")
grep '^# skipped' "$work/plan.sh" >&2 || true

# ---- 3. Image root ----------------------------------------------------------
mkdir -p "$root"
tar -xJf "$work/base.tar.xz" -C "$root" --numeric-owner

bind() {
	mount "$@"
	mounts+=("${@: -1}")
}
bind -t proc proc "$root/proc"
bind -t sysfs sys "$root/sys"
bind --bind /dev "$root/dev"
bind -t devpts devpts "$root/dev/pts"
bind -t tmpfs tmpfs "$root/run"
cp -L /etc/resolv.conf "$root/etc/resolv.conf"
# No services start inside the chroot.
printf '#!/bin/sh\nexit 101\n' >"$root/usr/sbin/policy-rc.d"
chmod 755 "$root/usr/sbin/policy-rc.d"

# Upstream's file provisioners.
ig=$root/imagegeneration
up=$ri/images/ubuntu
mkdir -p "$ig/helpers" "$ig/installers"
cp -a "$up/scripts/helpers/." "$ig/helpers/"
cp -a "$up/scripts/build/." "$ig/installers/"
cp "$ri/$RUNNER_IMAGES_TOOLSET" "$ig/installers/toolset.json"
cp -a "$up/assets/post-gen" "$ig/post-generation"
cp -a "$up/scripts/tests" "$ig/tests"
cp -a "$up/scripts/docs-gen" "$ig/SoftwareReport"
cp -a "$ri/helpers/software-report-base/." "$ig/SoftwareReport/"
chmod -R a+rx "$ig/installers"

# The runner user GitHub's image has; upstream's non-sudo steps run as it.
chroot "$root" id runner >/dev/null 2>&1 || chroot "$root" useradd -m -u 1001 -s /bin/bash runner

# run_step <root|user> <sh|pwsh> <script> [KEY=VALUE...], called by plan.sh.
run_step() {
	local who=$1 shell=$2 script=$3
	shift 3
	log "step: $script"
	local -a cmd=("/imagegeneration/installers/$script")
	[[ $shell == pwsh ]] && cmd=(pwsh -f "/imagegeneration/installers/$script")
	local -a spec=()
	[[ $who == user ]] && spec=(--userspec=runner:runner)
	chroot "${spec[@]}" "$root" /usr/bin/env -i \
		PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
		HOME="$([[ $who == user ]] && echo /home/runner || echo /root)" \
		LANG=C.UTF-8 "$@" "${cmd[@]}"
}
# shellcheck source=/dev/null
source "$work/plan.sh"

# ---- 4. SBOM inputs ---------------------------------------------------------
# shellcheck disable=SC2016 # dpkg-query expands these, not the shell
chroot "$root" dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' >"$work/dpkg.tsv"
: >"$work/toolcache.tsv"
if [[ -d $root/opt/hostedtoolcache ]]; then
	find "$root/opt/hostedtoolcache" -mindepth 2 -maxdepth 2 -type d -printf '%P\n' |
		LC_ALL=C sort | tr '/' '\t' >"$work/toolcache.tsv"
fi

# ---- 5. Normalize for reproducibility ---------------------------------------
for ((i = ${#mounts[@]} - 1; i >= 0; i--)); do umount -l "${mounts[i]}"; done
mounts=()
rm -f "$root/usr/sbin/policy-rc.d" "$root/etc/ssh/"ssh_host_*
rm -rf "$root"/var/lib/apt/lists/* "$root"/var/cache/apt/* "$root"/var/log/* \
	"$root"/tmp/* "$root"/var/tmp/* "$root"/root/.cache "$root"/home/runner/.cache
: >"$root/etc/machine-id"
: >"$root/etc/resolv.conf"
find "$root" -xdev -newermt "@$SOURCE_DATE_EPOCH" -print0 |
	xargs -0r touch --no-dereference --date="@$SOURCE_DATE_EPOCH"

# ---- 6. ext4 ----------------------------------------------------------------
# 4 KiB blocks (page size, for DAX over virtio-pmem); a UUID and hash seed
# derived from the ref, and a fixed clock, so the same tree gives the same
# bytes. No journal: hosts map it read-only.
uuid=$(printf '%s' "$ref" | sha256sum | sed -E 's/^(.{8})(.{4})(.{4})(.{4})(.{12}).*/\1-\2-\3-\4-\5/')
rm -f "$out/rootfs.ext4"
E2FSPROGS_FAKE_TIME=$SOURCE_DATE_EPOCH mkfs.ext4 -q -F -b 4096 -O ^has_journal \
	-L tachyon-root -U "$uuid" -E "hash_seed=$uuid,root_owner=0:0,lazy_itable_init=0" \
	-d "$root" "$out/rootfs.ext4" "${ROOTFS_SIZE_GB}G"
e2fsck -fn "$out/rootfs.ext4" >/dev/null

# ---- 7. SBOM, manifest, signature -------------------------------------------
"$ti" sbom -ref "$ref" -source-date-epoch "$SOURCE_DATE_EPOCH" \
	-dpkg "$work/dpkg.tsv" -toolcache "$work/toolcache.tsv" -o "$out/sbom.spdx.json"

kernel_args=()
if [[ -n ${KERNEL:-} ]]; then
	cp "$KERNEL" "$out/vmlinux"
	kernel_args=(-kernel vmlinux)
fi
"$ti" manifest -config "$here/image.yml" -dir "$out" -version "$version" \
	-apt-snapshot "$apt_snapshot" -plan-sha256 "$plan_sha" \
	-source-date-epoch "$SOURCE_DATE_EPOCH" "${kernel_args[@]}" -o "$out/manifest.json"

if [[ -n ${TACHYON_IMAGE_SIGNING_KEY:-} ]]; then
	"$ti" sign -key "$TACHYON_IMAGE_SIGNING_KEY" -o "$out/manifest.signed.json" "$out/manifest.json"
	log "$ref: signed"
else
	log "$ref: TACHYON_IMAGE_SIGNING_KEY unset; manifest NOT signed (hosts will refuse it)"
fi
log "$ref: built in $out"
