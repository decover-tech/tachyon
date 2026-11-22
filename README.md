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
| `cmd/tachyon/` | Go | Developer CLI: `tachyon prebuild validate`, `tachyon prebuild key`, `tachyon hostcert`, `tachyon github-app manifest` |
| `cmd/tachyon-guest/` | Go | Guest agent inside each job VM: takes the job payload from hostd over vsock (port 1024), mounts disks, runs the GitHub runner or prebuild warm steps, reports back and powers off. Linux only |
| `internal/hostauth/` | Go | Hostlink mTLS: server TLS config, host identity from client certificates, development certificate issuance |
| `cmd/tachyon-mirror/` | Go | Regional pull-through package mirror (registry, npm, PyPI, Go, Maven, apt, git), run as a health-checked pair |
| `internal/jobapi/` | Go | Platform-neutral job spec. Nothing below it knows about GitHub |
| `internal/frontend/github/` | Go | GitHub Actions adapter: labels, trust derivation, webhook verification, repo-scoped JIT runners, reconcile loop, minimal App REST client |
| `internal/controlplane/` | Go | Job API core: fork planning, placement, dispatch over hostlink, phase records |
| `internal/scheduler/` | Go | Placement by prebuild locality, packing by measured usage, CPU oversubscription with automatic step-back, deferrable class |
| `internal/burst/` | Go | Burst capacity when owned hosts are full: cheapest on-demand or hourly bare-metal offer, spot only for short profiles, spill to the next region. Cloud APIs sit behind `burst.Provider` |
| `internal/prebuild/` | Go | `.tachyon/prebuild.yml`, prebuild keys, full/partial/none fork planning, credential scrub list, and the prebuild manager: push/nightly triggers, refresh vs. full warm-up, token-free clone commands |
| `cmd/tachyon-cache/` | Go | Regional Actions cache service (GitHub-compatible Twirp API + signed blob URLs) |
| `internal/cache/` | Go | Actions cache: Twirp protocol server, signed blob URLs and per-job tokens, scoping (default branch / PR / fork rules), restore-key lookup, per-repo quota with LRU eviction, `BlobStore` interface for the D4 object store |
| `internal/guest/` | Go | The guest agent's logic and its vsock wire format (golden files shared with hostd's tests), behind a `System` interface so it tests without a VM |
| `internal/mirrors/` | Go | Guest configuration for package mirrors, and the control plane's health check of each region's mirror pair. `proxy/` and `gitmirror/` are the mirror itself |
| `internal/images/` | Go | Base image pipeline: build plan from the runner-images Packer template, manifest, ed25519 signing, SPDX SBOM, canary-first rollout and version retention |
| `cmd/tachyon-image/` | Go | Pipeline tool `images/*/build.sh` calls: `plan`, `sbom`, `manifest`, `sign`, `verify`, `diff` |
| `images/ubuntu-24.04/` | – | `image.yml` (pinned runner-images commit, trims) and `build.sh` (Linux builder: ext4 image, SBOM, unsigned manifest); rebuilt weekly by `.github/workflows/base-image.yml`, which signs the manifest in a separate job that never runs build code |
| `internal/telemetry/` | Go | Per-job phase record (pickup, billable seconds, cost), OpenTelemetry fleet metrics, ClickHouse export |
| `internal/billing/` | Go | Metering from the telemetry record: per-second runner time by VM size, per-repo prebuild allowance, storage above quota per GB-month |
| `internal/secrets/` | Go | Key store for JIT configs and tokens (Vault/OpenBao KV v2, or in-memory for dev): `secret://` refs, short TTLs, redaction ([0002-key-store.md](docs/decisions/0002-key-store.md)) |
| `deploy/observability/` | config | OTel Collector, Prometheus SLO rules, Grafana dashboards, ClickHouse job-record schema, local docker-compose stack |
| `internal/abuse/` | Go | Abuse controls: free/trial plan limits and verification, CPU and network anomaly alerts with automatic cancel/suspend |
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
TACHYON_GITHUB_WEBHOOK_SECRET=... TACHYON_VAULT_TOKEN=... bin/tachyon-controlplane \
  -github-app-id 123 -github-app-key app.pem \
  -image ubuntu-24.04=2026-11-16 \
  -hostlink-cert cp.crt -hostlink-key cp.key -hostlink-client-ca hostlink-ca.crt \
  -vault-addr https://vault.internal:8200
```

JIT runner configs are kept in a Vault or OpenBao KV v2 engine (`-vault-mount secret`, `-vault-prefix tachyon`, `-jit-ttl 10m`); use `-vault-token-file` instead of `TACHYON_VAULT_TOKEN` when a Vault Agent rotates the token. For local development without Vault, pass `-secrets mem`.

Webhooks go to `POST /webhooks/github` on `:8080`. hostd connects to gRPC on `:9090` over mutual TLS, and each host must present a certificate for its own `TACHYON_HOST_ID`. `tachyon hostcert` issues development certificates; issuance and rotation are in [docs/hostlink-mtls.md](docs/hostlink-mtls.md). Pass `-hostlink-insecure` (and `TACHYON_HOSTLINK_INSECURE=1` to hostd) to run without TLS locally. `-github-allowed-owners org-a,org-b` limits runners to pilot orgs.

Registering the GitHub App (least-privilege permissions, from a generated manifest) and the pilot-org installation flow are in [docs/github-app.md](docs/github-app.md).

Set `TACHYON_DATABASE_URL` (e.g. `postgres://tachyon@db:5432/tachyon`) to keep jobs, pending runners, hosts, the prebuild registry and job records in Postgres; migrations run on start. After a restart, queued jobs are resubmitted and jobs running on hosts keep their reservations until the hosts reconnect and report results. Without it, state is in memory. The Postgres store tests run when `TACHYON_TEST_POSTGRES_DSN` is set.

The nightly clean run is off unless repos opt in with `-nightly-repos owner/name,...`. Each night after `-nightly-hour` (UTC, default 3; `-1` disables it) the control plane re-runs each opted-in repo's default-branch run for its newest commit without a prebuild and reports divergence as a `Tachyon nightly clean run` check on the commit. The re-run repeats every job of that workflow run, non-Tachyon and deploy jobs included, so opt in only repos where that is safe. This needs the App's Actions (write) and Checks (write) permissions.

### Running the package mirrors

Run two `tachyon-mirror` instances per region, on separate machines:

```sh
bin/tachyon-mirror -listen :443 -data /var/cache/tachyon-mirror \
  -public-url https://mirror-a.r1.tachyon.internal \
  -tls-cert mirror.pem -tls-key mirror-key.pem \
  -git-repos acme/api,acme/web
```

and point the control plane at both with `-mirror-primary https://mirror-a.r1.tachyon.internal -mirror-secondary https://mirror-b.r1.tachyon.internal -mirror-git-repos acme/api,acme/web`. `-git-repos` and `-mirror-git-repos` must list the same repos, and only public ones: jobs for any other repo, including every private repo, get no git rewrite and fetch from GitHub directly with actions/checkout's token. It probes `/healthz` on each every 5 seconds; new jobs use the primary while it is up, then the secondary, and get no mirror config at all (straight to upstream) if both are down. Docker requires TLS for registry mirrors.

Only public content is mirrored: the mirror fetches upstream anonymously, requests that carry credentials pass straight through uncached, private images get a 404 so the client falls back to the registry, the apt proxy only serves public Ubuntu/Debian archives, and the git mirror redirects any repo it can't clone anonymously to GitHub.

Observability is optional. Set `OTEL_EXPORTER_OTLP_ENDPOINT` (OTLP/HTTP, e.g. `http://otel-collector:4318`) to export metrics, and `-clickhouse-url` (with `-clickhouse-user` and `TACHYON_CLICKHOUSE_PASSWORD`) to write one row per finished job to `tachyon.job_records`. `-price-owned-vcpu-hour` and `-price-burst-vcpu-hour` set the capacity cost behind the cost-per-job-minute panels. See [deploy/observability](deploy/observability) for the collector, dashboards, SLO alerts and schema.

### Running hostd (Linux host)

| Variable | Required | Default |
| --- | --- | --- |
| `TACHYON_HOST_ID`, `TACHYON_REGION` | yes | |
| `TACHYON_VCPU`, `TACHYON_MEMORY_GB` | yes | |
| `TACHYON_CONTROL_PLANE` | yes | e.g. `https://cp.internal:9090` |
| `TACHYON_TLS_CERT`, `TACHYON_TLS_KEY`, `TACHYON_TLS_CA` | yes, unless `TACHYON_HOSTLINK_INSECURE=1` | host client cert/key and the control plane's CA; see [docs/hostlink-mtls.md](docs/hostlink-mtls.md) |
| `TACHYON_TLS_SERVER_NAME` | no | host from `TACHYON_CONTROL_PLANE` |
| `TACHYON_ZFS_POOL` | yes | e.g. `tank/tachyon` |
| `TACHYON_BASE_SNAPSHOTS` | yes | dir with one Cloud Hypervisor snapshot per VM size: `<dir>/<vcpu>/` |
| `TACHYON_WARM` | no | `2:2,4:2,8:1` (vcpu:paused VMs) |
| `TACHYON_FAILURE_DOMAIN`, `TACHYON_ARCH` | no | `default`, `x86_64` |
| `TACHYON_CH_BINARY`, `TACHYON_CH_REMOTE` | no | `/usr/local/bin/cloud-hypervisor`, `/usr/local/bin/ch-remote` |
| `TACHYON_RUN_DIR`, `TACHYON_CGROUP`, `TACHYON_BASE_UID` | no | `/run/tachyon/vms`, `/sys/fs/cgroup/tachyon`, `100000` |
| `TACHYON_IMAGES` | no | base image store, `<dir>/<name>@<version>/`; prepared versions are reported in heartbeats |
| `TACHYON_EGRESS_MBIT` | no | `1000` (per-VM egress limit; `0` disables) |
| `TACHYON_MIRRORS` | no | comma list of mirror URLs, hosts or CIDRs every VM (including prebuild builds) may reach |
| `TACHYON_DNS` | no | comma list of IPv4 DNS servers guests may query |
| `TACHYON_PEER_ADDRESS` | no | address peers use to pull snapshots from this host, e.g. `http://h1.internal:7443`; unset = serve no pulls |
| `TACHYON_PEER_LISTEN`, `TACHYON_IMAGES_DIR` | no | `0.0.0.0:7443`, `/var/lib/tachyon/images` (base images as `<name@version>.img`) |
| `TACHYON_STATE_DIR` | no | `/var/lib/tachyon/hostd`: unacked job events/results and running-job markers, kept across restarts |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | no | unset: no metrics export. OTLP/gRPC, e.g. `http://otel-collector:4317` |

`tachyon-hostd prepare-image ubuntu-24.04@2026-11-16` verifies an image in `TACHYON_IMAGES` against the keys in `TACHYON_IMAGE_TRUST` (PEM), then boots it once per VM size and leaves paused snapshots in `<image>/snapshots/<vcpu>/` (point `TACHYON_BASE_SNAPSHOTS` there). It boots `TACHYON_GUEST_KERNEL` when the image ships no kernel.

The control plane runs signed images with a canary rollout instead of static `-image` pins when given `-image-trust keys.pem -image-manifests a/manifest.signed.json -canary-hosts h1,h2`.

Each VM gets its own network namespace and tap (`hostd/src/network.rs`); hostd needs `ip`, `nft` and `tc` on the host, and base snapshots must use a virtio-net device on `tap0` with guest address `172.16.0.2/30`, gateway `172.16.0.1`.

## Status

Built and unit-tested: the job spec, GitHub adapter (per-installation API budget, opt-in re-runs of infrastructure failures, nightly clean run, pilot-org allowlist), scheduler, prebuild config/keys/fork planning, the prebuild manager (against a fake platform source), the Actions cache service and its scoping rules, mirror config and the mirror pair, the guest agent, the hostlink protocol on both sides (mTLS, acks and replay), hostd (networking, cancel, pre-fork, peer snapshot pull, restart recovery, base image pipeline pieces), Postgres persistence, the key store, metering and billing, burst capacity selection, abuse controls, and the observability stack's configuration. Everything runs against fakes and in-memory backends in tests.

Not built or not proven on real infrastructure:

- **Base image pipeline, the parts that need real infrastructure.** The pipeline (build plan, `build.sh`, signing, SBOM, rollout, per-size snapshot builder) exists, but no image has been built on a real builder or booted under Cloud Hypervisor yet, there is no image store to push images to hosts, and hostd still restores every VM from the single `TACHYON_BASE_SNAPSHOTS` version rather than the version a job is assigned. Builds are not yet byte-reproducible: some upstream install scripts download "latest" artifacts outside apt.
- **Prebuild manager in production.** Triggers, planning and dispatch exist, but trigger state is in memory, and no build has run on a real host.
- **Actions cache object store (D4).** The protocol server writes through a `BlobStore` interface; no real store backs it yet.
- **Mirror rollout.** `tachyon-mirror` is built and tested against fake upstreams only; provisioning the instances, TLS certificates, and feeding the git mirror the list of repos Tachyon builds (a flag for now) are not done.
- **Burst providers and abuse account data.** The burst selector has no cloud provider behind it, and the abuse gate uses an in-memory account store (every tenant gets `-unknown-tenant-plan`).
- **Host certificate issuer** backed by the key store, and on-host renewal. Only the `tachyon hostcert` development issuer exists.

Open decisions D2 to D6 and the open questions are listed at the end of the [design doc](docs/architecture.md#risks-open-questions-and-decisions).
