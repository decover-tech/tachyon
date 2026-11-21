# Tachyon

Tachyon makes CI jobs faster and cheaper by forking each job from a prebuilt, per-repo warm snapshot instead of rebuilding its state from scratch. It launches as a drop-in GitHub Actions runner (one `runs-on` label change) on owned bare-metal hosts, behind a platform-neutral job API.

The design is in [docs/architecture.md](docs/architecture.md). This repo is the Phase 1 skeleton of that design.

## Layout

D1 is decided ([docs/decisions/0001-language.md](docs/decisions/0001-language.md)): **Rust** for the privileged host agent, **Go** for everything above it, with **protobuf** at the boundary.

| Path | Language | What it is |
| --- | --- | --- |
| `proto/tachyon/hostlink/v1/` | protobuf | The only contract between hostd and the control plane. hostd dials out and holds one bidirectional stream |
| `hostd/` | Rust | `tachyon-hostd`: warm VM pool, copy-on-write job disks (ZFS), per-VM jailer, Cloud Hypervisor control, sticky-volume promotion, prebuild snapshots |
| `cmd/tachyon-controlplane/` | Go | Serves GitHub webhooks (HTTP) and the host link (gRPC). In-memory state for now |
| `cmd/tachyon/` | Go | Developer CLI: `tachyon prebuild validate`, `tachyon prebuild key` |
| `internal/jobapi/` | Go | Platform-neutral job spec. Nothing below it knows about GitHub |
| `internal/frontend/github/` | Go | GitHub Actions adapter: labels, trust derivation, webhook verification, repo-scoped JIT runners, reconcile loop, minimal App REST client |
| `internal/controlplane/` | Go | Job API core: fork planning, placement, dispatch over hostlink, phase records |
| `internal/scheduler/` | Go | Placement by prebuild locality, packing by measured usage, CPU oversubscription with automatic step-back, deferrable class |
| `internal/prebuild/` | Go | `.tachyon/prebuild.yml`, prebuild keys, full/partial/none fork planning, credential scrub list |
| `internal/cache/` | Go | Actions cache scoping (default branch / PR / fork rules), restore-key lookup, per-repo quota with LRU eviction |
| `internal/mirrors/` | Go | Guest configuration for package mirrors (npm, PyPI, Go, Maven/Gradle, apt, registry, git) |
| `internal/telemetry/` | Go | Per-job phase timeline: pickup latency, billable seconds |
| `actions/warm/` | Action | `tachyon/warm`: restores workspace paths from the prebuild after `actions/checkout` |
| `examples/node-go/` | – | A repo config and workflow using Tachyon |
| `gen/` | Go | Generated from `proto/` by `buf generate`; checked in |

## Using it in a repo

```yaml
jobs:
  test:
    runs-on: tachyon-ubuntu-24.04-8x        # tachyon-<image>[-arm][-<vcpu>x]
    steps:
      - uses: actions/checkout@v4
      - uses: decover-tech/tachyon/actions/warm@main
      - run: npm ci
        if: env.TACHYON_WARM != 'hit'
```

Optional labels: `tachyon-clean` (fork the base image, not the prebuild) and `tachyon-deferrable` (discounted off-peak class). See [examples/node-go](examples/node-go) for `.tachyon/prebuild.yml`.

## Development

Requires Go 1.26+, Rust stable and [buf](https://buf.build). hostd builds its protobuf code with `protox`, so `protoc` is not needed.

```sh
make test     # go test -race ./... and cargo test
make lint     # go vet, buf lint, cargo fmt --check, cargo clippy -D warnings
make proto    # regenerate gen/ after editing proto/
make build    # bin/tachyon, bin/tachyon-controlplane, hostd debug build
```

hostd compiles on macOS for tests, but only runs on Linux with KVM, ZFS and Cloud Hypervisor.

### Cache contract tests

`.github/workflows/cache-contract.yml` runs daily: it drives the newest `actions/cache` release and the newest `@actions/cache` package against the cache service (`internal/cache/contract`) and pages the cache team on failure. To run it locally (needs Node 24):

```sh
git clone --depth 1 --branch <tag> https://github.com/actions/cache /tmp/actions-cache
(mkdir -p /tmp/toolkit && cd /tmp/toolkit && npm init -y && npm install @actions/cache@latest)
TACHYON_CONTRACT_ACTIONS_CACHE=/tmp/actions-cache TACHYON_CONTRACT_TOOLKIT=/tmp/toolkit \
  go test -tags contract -run '^TestLive$' -v ./internal/cache/contract
```

Paging uses the `CACHE_CONTRACT_OWNERS` repository variable (the team to mention on the tracking issue) and the optional `CACHE_CONTRACT_PAGERDUTY_KEY` secret (a PagerDuty Events v2 routing key).

### Running the control plane

```sh
TACHYON_GITHUB_WEBHOOK_SECRET=... bin/tachyon-controlplane \
  -github-app-id 123 -github-app-key app.pem \
  -image ubuntu-24.04=2026-11-16
```

Webhooks go to `POST /webhooks/github` on `:8080`. hostd connects to gRPC on `:9090`.

### Running hostd (Linux host)

| Variable | Required | Default |
| --- | --- | --- |
| `TACHYON_HOST_ID`, `TACHYON_REGION` | yes | |
| `TACHYON_VCPU`, `TACHYON_MEMORY_GB` | yes | |
| `TACHYON_CONTROL_PLANE` | yes | e.g. `http://cp.internal:9090` |
| `TACHYON_ZFS_POOL` | yes | e.g. `tank/tachyon` |
| `TACHYON_BASE_SNAPSHOTS` | yes | dir with one Cloud Hypervisor snapshot per VM size: `<dir>/<vcpu>/` |
| `TACHYON_WARM` | no | `2:2,4:2,8:1` (vcpu:paused VMs) |
| `TACHYON_FAILURE_DOMAIN`, `TACHYON_ARCH` | no | `default`, `x86_64` |
| `TACHYON_CH_BINARY`, `TACHYON_CH_REMOTE` | no | `/usr/local/bin/cloud-hypervisor`, `/usr/local/bin/ch-remote` |
| `TACHYON_RUN_DIR`, `TACHYON_CGROUP`, `TACHYON_BASE_UID` | no | `/run/tachyon/vms`, `/sys/fs/cgroup/tachyon`, `100000` |

## Status

Built and unit-tested: the job spec, GitHub adapter, scheduler, prebuild config/keys/fork planning, cache scoping, mirror config, the hostlink protocol on both sides, and the hostd job lifecycle against fake hypervisor and storage backends.

Not built yet:

- **Guest agent.** The in-VM process that receives the vsock payload, mounts disks, stashes warm paths, starts the runner and reports results. hostd's side of that protocol exists (`hostd/src/hypervisor.rs`).
- **Base image and snapshot pipeline**, built from `actions/runner-images`, plus virtio-pmem/DAX sharing.
- **Prebuild manager.** Triggering builds on trusted pushes and nightly, and the host-side secret scan.
- **Actions cache protocol server** and its object store (D4). Only the scoping and index rules exist.
- **Persistence.** The control plane keeps all state in memory, and secrets need a managed key store.
- **mTLS** on the host link, plus **hostd**: cancel, pre-fork, peer snapshot pull, inventory rebuild on restart, and buffering results across reconnects.
- **Observability export** (OpenTelemetry/Prometheus/ClickHouse) and billing.

Open decisions D2 to D6 and the open questions are listed at the end of the [design doc](docs/architecture.md#risks-open-questions-and-decisions).
