#!/bin/bash
# perf-only: one-time, idempotent setup of the perf eval host (root).
# Packages and the toolchain container, Cloud Hypervisor and the guest
# kernel, the ZFS pool on the data volume, dev hostlink certificates, the
# staging image-signing key, Postgres and the observability stack.
# Nothing here is a production procedure: the CA and signing keys live on
# the host because it is a throwaway test machine.
set -euo pipefail
T=/opt/tachyon
SRC=${SRC:-$T/src}
CH_VERSION=${CH_VERSION:-v55.0}
KERNEL_RELEASE=${KERNEL_RELEASE:-ch-release-v7.2.8-20261009}
DATA_DEV_SIZE=${DATA_DEV_SIZE:-150G}

export DEBIAN_FRONTEND=noninteractive
if ! command -v docker >/dev/null || ! command -v zpool >/dev/null; then
	apt-get update -q
	apt-get install -yq docker.io docker-compose-v2 zfsutils-linux nftables iproute2 curl \
		ca-certificates openssl jq e2fsprogs xz-utils git
fi
systemctl enable --now docker
mkdir -p $T/artifacts $T/secrets $T/state /etc/tachyon
chmod 700 $T/secrets

docker image inspect tachyon-dev:local >/dev/null 2>&1 || docker build -q -t tachyon-dev:local "$SRC/ops/dev"

cd $T/artifacts
[ -f cloud-hypervisor ] || curl -sSfL -o cloud-hypervisor "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$CH_VERSION/cloud-hypervisor-static"
[ -f ch-remote ] || curl -sSfL -o ch-remote "https://github.com/cloud-hypervisor/cloud-hypervisor/releases/download/$CH_VERSION/ch-remote-static"
[ -f vmlinux ] || curl -sSfL -o vmlinux "https://github.com/cloud-hypervisor/linux/releases/download/$KERNEL_RELEASE/vmlinux-x86_64"
chmod 755 cloud-hypervisor ch-remote

# ZFS on the EBS data volume (the one that is DATA_DEV_SIZE and unmounted).
if ! zpool list tank >/dev/null 2>&1; then
	dev=$(lsblk -dnpo NAME,SIZE,MOUNTPOINT | awk -v s="$DATA_DEV_SIZE" '$2==s && $3=="" {print $1}' | head -1)
	[ -n "$dev" ] || { echo "no unmounted $DATA_DEV_SIZE data volume" >&2; exit 1; }
	zpool create -f -o ashift=12 -o autoexpand=on -O compression=lz4 -O atime=off tank "$dev"
	zfs create tank/tachyon
fi
# After the data volume is grown: expand into it (ZFS wants the disk's
# by-id name; the vdev itself is its first partition).
part=$(zpool status -P tank | awk '/\/dev\//{print $1; exit}')
disk=$(lsblk -no pkname "$(readlink -f "$part")" 2>/dev/null || true)
byid=$(find /dev/disk/by-id -lname "*/$disk" -name 'nvme-Amazon_Elastic_Block_Store_*' 2>/dev/null | head -1)
[ -z "$byid" ] || zpool online -e tank "$byid-part1" 2>/dev/null || true

cd $T/secrets
B=$SRC/bin
[ -f hostlink-ca.crt ] || "$B"/tachyon hostcert ca -out hostlink-ca
[ -f cp.crt ] || "$B"/tachyon hostcert server -ca hostlink-ca -name localhost,127.0.0.1 -out cp
[ -f host.crt ] || "$B"/tachyon hostcert host -ca hostlink-ca -id perf-01 -out host
[ -f image-signing.pem ] || {
	openssl genpkey -algorithm ed25519 -out image-signing.pem
	openssl pkey -in image-signing.pem -pubout -out image-trust.pem
}
[ -f webhook-secret ] || openssl rand -hex 32 >webhook-secret
[ -f fake-app.pem ] || openssl genrsa -out fake-app.pem 2048 2>/dev/null
[ -f admin-token ] || openssl rand -hex 32 >admin-token
[ -f pg-password ] || openssl rand -hex 16 >pg-password
mkdir -p $T/tls && cp host.crt host.key $T/tls/ && cp hostlink-ca.crt $T/tls/ca.crt
chmod 600 ./*.key ./*.pem webhook-secret admin-token pg-password

# Observability stack (ClickHouse job records, OTel collector, Prometheus,
# Grafana). The collector exits if ClickHouse is not up yet or its otel
# database is missing (finding F11), so wait for ClickHouse, create the
# database, and start the collector last. The job_records schema is
# applied by ClickHouse's initdb from deploy/observability/clickhouse.
docker compose -p obs -f "$SRC/deploy/observability/docker-compose.yml" up -d clickhouse
for _ in $(seq 60); do curl -sf "http://tachyon:tachyon@127.0.0.1:8123/ping" >/dev/null && break; sleep 2; done
curl -sf "http://tachyon:tachyon@127.0.0.1:8123/" --data-binary "CREATE DATABASE IF NOT EXISTS otel" >/dev/null
docker compose -p obs -f "$SRC/deploy/observability/docker-compose.yml" up -d

if ! docker inspect tachyon-postgres >/dev/null 2>&1; then
	docker run -d --name tachyon-postgres --restart unless-stopped -p 127.0.0.1:5432:5432 \
		-e POSTGRES_DB=tachyon -e POSTGRES_USER=tachyon -e POSTGRES_PASSWORD="$(cat pg-password)" \
		-v tachyon-pg:/var/lib/postgresql/data postgres:16
fi
docker start tachyon-postgres >/dev/null
# The data volume may predate pg-password (POSTGRES_PASSWORD applies only at
# init), so set it every time; the local socket needs no password.
for _ in $(seq 30); do docker exec tachyon-postgres pg_isready -q && break; sleep 1; done
docker exec tachyon-postgres psql -qU tachyon -d tachyon -c "ALTER USER tachyon PASSWORD '$(cat pg-password)'"
echo "setup: ok"
