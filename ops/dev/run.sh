#!/usr/bin/env bash
# Runs a command in the dev toolchain container with the repo at /src.
# Go module, build and cargo caches persist in named volumes, and hostd's
# target dir is kept in a volume so it never mixes with a host build.
#
#   ops/dev/run.sh make check
#   ops/dev/run.sh go test ./internal/telemetry/...
#   ops/dev/run.sh bash -c 'cd hostd && cargo test lifecycle'
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
IMAGE=tachyon-dev:local

docker build -q -t "$IMAGE" "$ROOT/ops/dev" >/dev/null

tty=()
[ -t 0 ] && [ -t 1 ] && tty=(-it)

exec docker run --rm ${tty[@]+"${tty[@]}"} \
  -v "$ROOT:/src" \
  -v tachyon-dev-gomod:/go/pkg/mod \
  -v tachyon-dev-gocache:/root/.cache/go-build \
  -v tachyon-dev-cargo:/usr/local/cargo/registry \
  -v tachyon-dev-hostd-target:/src/hostd/target \
  -w /src \
  "$IMAGE" "$@"
