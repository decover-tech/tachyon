#!/bin/bash
# perf-only: deploy one source tree on the perf eval host (root) and start
# it: build every binary, set the host up (setup.sh), build and prepare the
# base image when its inputs changed, install hostd through the fleet
# bootstrap script (the production path), and (re)start the fake GitHub
# API, the control plane and hostd. Ends when hostd is connected.
#
#   deploy.sh <source.tgz> <ref>
#
# EVAL_GITHUB=fake (default) points the control plane at tachyon-eval
# fakegh; real GitHub needs the App's id, key and webhook secret in
# /opt/tachyon/secrets/github-app.{id,pem} and webhook-secret.
set -euo pipefail
TGZ=$1 REF=${2:-unknown}
T=/opt/tachyon SRC=/opt/tachyon/src
EVAL_GITHUB=${EVAL_GITHUB:-fake}
log() { echo "deploy: $*" >&2; }

# ---- source and binaries ----
rm -rf $SRC.new && mkdir -p $SRC.new && tar -xzf "$TGZ" -C $SRC.new
# Keep build outputs and cargo's target dir across deploys (incremental).
[ -d $SRC/hostd/target ] && mv $SRC/hostd/target $SRC.new/hostd/target
rm -rf $SRC && mv $SRC.new $SRC
echo "$REF" >$T/state/ref 2>/dev/null || { mkdir -p $T/state; echo "$REF" >$T/state/ref; }
if ! command -v docker >/dev/null; then
	apt-get update -q && DEBIAN_FRONTEND=noninteractive apt-get install -yq docker.io docker-compose-v2
fi
docker image inspect tachyon-dev:local >/dev/null 2>&1 || docker build -q -t tachyon-dev:local $SRC/ops/dev
log "building"
docker run --rm -v $SRC:/src -v tachyon-gocache:/root/.cache -v tachyon-cargo:/usr/local/cargo/registry -w /src \
	tachyon-dev:local bash -c '
	set -e; mkdir -p bin
	go build -o bin/ ./cmd/tachyon ./cmd/tachyon-controlplane ./cmd/tachyon-image ./cmd/tachyon-eval
	CGO_ENABLED=0 go build -o bin/tachyon-guest ./cmd/tachyon-guest
	cd hostd && cargo build --release -q && cp target/release/tachyon-hostd /src/bin/'
B=$SRC/bin

bash $SRC/ops/loop/host/setup.sh

# ---- base image: rebuilt only when what goes into it changed ----
IMAGES=/var/lib/tachyon/images-store
inputs=$( (cd $SRC && tar -c --sort=name --mtime=@0 --owner=0 --group=0 images cmd/tachyon-guest internal/guest go.mod go.sum) | sha256sum | cut -c1-16)
inputs="$inputs-$(sha256sum $T/artifacts/vmlinux | cut -c1-8)"
V=
if [ -f $T/state/image-inputs ] && [ "$(cat $T/state/image-inputs)" = "$inputs" ]; then
	V=$(cat $T/state/image-version)
fi
zfs list tank/tachyon/images >/dev/null 2>&1 || zfs create -o mountpoint=$IMAGES tank/tachyon/images
if [ -z "$V" ] || [ ! -f "$IMAGES/ubuntu-24.04@$V/manifest.signed.json" ]; then
	n=$(($(cat $T/state/image-build 2>/dev/null || echo 0) + 1))
	V="$($B/tachyon-image version).$n"
	log "building base image ubuntu-24.04@$V (inputs $inputs); this takes a while"
	OUT=/var/tmp/img-$V
	rm -rf /var/tmp/img-* "$OUT"
	(cd $SRC && TACHYON_IMAGE=$B/tachyon-image GUEST_BIN=$B/tachyon-guest TACHYON_IMAGE_VERSION=$V \
		KERNEL=$T/artifacts/vmlinux images/ubuntu-24.04/build.sh "$OUT") >$T/state/image-build.log 2>&1 ||
		{ tail -40 $T/state/image-build.log >&2; exit 1; }
	# Only the sizes this host can hold (4 GB per vCPU). Signed after the
	# edit, with the staging key.
	jq --argjson s "${EVAL_SIZES:-[2]}" '.sizes = $s' "$OUT/manifest.json" >"$OUT/m.json" && mv "$OUT/m.json" "$OUT/manifest.json"
	$B/tachyon-image sign -key $T/secrets/image-signing.pem -o "$OUT/manifest.signed.json" "$OUT/manifest.json"
	$B/tachyon-image verify -trust $T/secrets/image-trust.pem -dir "$OUT" "$OUT/manifest.signed.json"
	# Older versions go; one version fits the data volume.
	find $IMAGES -mindepth 1 -maxdepth 1 -name 'ubuntu-24.04@*' ! -name "ubuntu-24.04@$V" -exec rm -rf {} +
	mkdir -p "$IMAGES/ubuntu-24.04@$V"
	cp --sparse=always "$OUT/manifest.signed.json" "$OUT/rootfs.ext4" "$OUT/vmlinux" "$IMAGES/ubuntu-24.04@$V/"
	chmod 0444 "$IMAGES/ubuntu-24.04@$V/rootfs.ext4"
	rm -rf "$OUT"
	echo "$n" >$T/state/image-build
	echo "$inputs" >$T/state/image-inputs
	echo "$V" >$T/state/image-version
fi
log "base image ubuntu-24.04@$V"
# build.sh's dockerd runs with --bridge=none in the host's network
# namespace, and dockerd then deletes the host's docker0 (finding F12),
# cutting off Postgres and the obs stack. Recreate it.
if ! ip link show docker0 >/dev/null 2>&1; then
	log "docker0 was removed by the image build; restarting docker"
	systemctl restart docker
	bash $SRC/ops/loop/host/setup.sh
fi

# ---- hostd through the fleet bootstrap script ----
HV="$(grep -m1 '^version' $SRC/hostd/Cargo.toml | cut -d'"' -f2)+$(sha256sum $B/tachyon-hostd | cut -c1-12)"
cat >/etc/tachyon/fleet.conf <<EOF
HOST_ID=perf-01
REGION=staging
FAILURE_DOMAIN=aws-use1a
HOST_GROUP=canary
CONTROL_PLANE=https://localhost:9090
TRUST_DOMAIN=tachyon
TLS_SOURCE_DIR=$T/tls
VCPU=${EVAL_VCPU:-4}
MEMORY_GB=${EVAL_MEMORY_GB:-12}
WARM=${EVAL_WARM:-2:1}
ZFS_POOL=tank/tachyon
ZFS_ARC_MAX_GB=1
IMAGES=$IMAGES
BASE_SNAPSHOTS=$IMAGES/ubuntu-24.04@$V/snapshots
IMAGE_TRUST_SOURCE=$T/secrets/image-trust.pem
CH_SOURCE=$T/artifacts/cloud-hypervisor
CH_SHA256=$(sha256sum $T/artifacts/cloud-hypervisor | cut -d' ' -f1)
CH_REMOTE_SOURCE=$T/artifacts/ch-remote
CH_REMOTE_SHA256=$(sha256sum $T/artifacts/ch-remote | cut -d' ' -f1)
HOSTD_VERSION=$HV
HOSTD_SOURCE=$B/tachyon-hostd
HOSTD_SHA256=$(sha256sum $B/tachyon-hostd | cut -d' ' -f1)
DNS=169.254.169.253
OTLP_ENDPOINT=http://127.0.0.1:4317
EOF
chmod 600 /etc/tachyon/fleet.conf
BOOT=$SRC/deploy/fleet/tachyon-host-bootstrap.sh
systemctl stop tachyon-hostd 2>/dev/null || true
bash $BOOT -c /etc/tachyon/fleet.conf preflight packages sysctl zfs chv hostd cgroup tls env unit
/usr/local/lib/tachyon/prepare-host.sh
# shellcheck disable=SC1091 # written by the bootstrap env step
set -a && . /etc/tachyon/hostd.env && set +a
log "preparing ubuntu-24.04@$V (skips finished sizes)"
/usr/local/bin/tachyon-hostd prepare-image "ubuntu-24.04@$V" >$T/state/prepare.log 2>&1 ||
	{ tail -40 $T/state/prepare.log >&2; exit 1; }

# ---- control plane and the fake GitHub API ----
S=$T/secrets
if [ "$EVAL_GITHUB" = fake ]; then
	APP_ID=1 APP_KEY=$S/fake-app.pem API="-github-api-url=http://127.0.0.1:8099" OWNERS=tachyon-eval
else
	APP_ID=$(cat $S/github-app.id) APP_KEY=$S/github-app.pem API="" OWNERS=${EVAL_OWNERS:-decover-tech}
fi
umask 077
cat >$T/controlplane.env <<EOF
TACHYON_GITHUB_WEBHOOK_SECRET=$(cat $S/webhook-secret)
TACHYON_DATABASE_URL=postgres://tachyon:$(cat $S/pg-password)@127.0.0.1:5432/tachyon?sslmode=disable
TACHYON_CLICKHOUSE_PASSWORD=tachyon
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
EOF
umask 022
cat >/etc/systemd/system/tachyon-fakegh.service <<EOF
[Unit]
Description=Tachyon eval fake GitHub API (perf only)
[Service]
ExecStart=$B/tachyon-eval fakegh -listen 127.0.0.1:8099
Restart=always
EOF
cat >/etc/systemd/system/tachyon-controlplane.service <<EOF
[Unit]
Description=Tachyon control plane (perf eval)
After=docker.service
[Service]
EnvironmentFile=$T/controlplane.env
ExecStart=$B/tachyon-controlplane -github-app-id=$APP_ID -github-app-key=$APP_KEY $API \\
  -github-allowed-owners=$OWNERS -secrets=mem -image=ubuntu-24.04=$V \\
  -hostlink-cert=$S/cp.crt -hostlink-key=$S/cp.key -hostlink-client-ca=$S/hostlink-ca.crt \\
  -clickhouse-url=http://127.0.0.1:8123 -clickhouse-user=tachyon \\
  -admin-addr=127.0.0.1:8081 -admin-token-file=$S/admin-token
Restart=always
RestartSec=2
EOF
systemctl daemon-reload
if [ "$EVAL_GITHUB" = fake ]; then systemctl restart tachyon-fakegh; else systemctl stop tachyon-fakegh 2>/dev/null || true; fi
systemctl restart tachyon-controlplane
for _ in $(seq 30); do curl -sf http://127.0.0.1:8080/healthz >/dev/null && break; sleep 1; done
bash $BOOT -c /etc/tachyon/fleet.conf start
systemctl restart tachyon-hostd

log "waiting for hostd to connect"
for _ in $(seq 120); do
	if curl -sf -H "Authorization: Bearer $(cat $S/admin-token)" http://127.0.0.1:8081/admin/hosts |
		jq -e '.hosts[] | select(.id=="perf-01" and .connected)' >/dev/null 2>&1; then
		log "ready: $REF on hostd $HV, image ubuntu-24.04@$V"
		exit 0
	fi
	sleep 2
done
log "hostd did not connect"
journalctl -u tachyon-hostd -n 40 --no-pager >&2
journalctl -u tachyon-controlplane -n 20 --no-pager >&2
exit 1
