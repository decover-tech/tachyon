# Project Tachyon — Architecture Design Document

Nov 21, 2026 · @Ravi Tandon

## Summary and goals

Tachyon makes CI jobs faster and cheaper by forking each job from a prebuilt, per-repo warm snapshot instead of rebuilding its state from scratch. It launches as a drop-in GitHub Actions runner (one `runs-on` label change) on owned bare-metal hosts, behind a platform-neutral job API so GitHub is one frontend, not the foundation.

The bet: competitors already removed slow hardware and distant caches, but every job still re-checks-out, restores, reinstalls and re-warms. Tachyon removes that work, and runs its fleet at higher utilization to cut cost per minute.

| Goal | Proposed target | Measured as |
| --- | --- | --- |
| Fast pickup | p50 under 2 s, p95 under 5 s | Webhook `queued` to runner online |
| Less setup | 70% less setup time than GitHub-hosted | Job start to first build or test step, pilot repos |
| Faster jobs | 2x faster end to end than GitHub-hosted | Median job wall time, pilot repos |
| Cheaper for customers | Total CI bill at least 50% below GitHub-hosted | Same workload, same month |
| Efficient fleet | 50%+ average vCPU utilization on owned hosts | Fleet metrics, weekly |

All targets are proposals until Phase 0 measures a baseline on 10 pilot repos.

**Non-goals for v1:** macOS and Windows runners, GPU jobs, Tachyon's own workflow engine and UI, on-prem installs of Tachyon, and non-GitHub frontends (planned for Phase 3).

## Background and context

A GitHub-hosted job spends much of its time before any real work: waiting in the queue, booting a large general-purpose image, checking out, restoring caches and installing dependencies. Faster hardware shortens the build and test steps, but only removing that setup work changes the shape of the job.

| Option | How it works | What it leaves on the table |
| --- | --- | --- |
| GitHub-hosted runners | Fresh cloud VM per job from a large general-purpose image; cache in remote blob storage | Queue and boot time, cache throughput, server-class CPU speed |
| Blacksmith | Bare metal with high-clock consumer-class CPUs, Firecracker microVMs, same-datacenter cache, persistent "sticky disks" for Docker layers and chosen paths | Every job still restores, reinstalls and re-warms; fleet utilization model not public |
| Depot | Remote BuildKit builders, plus GitHub Actions runners | Strongest on Docker builds; general job setup still repeated |
| Self-hosted (ARC and similar) | Customer runs runners on its own Kubernetes or VMs | Ops burden, idle capacity, no shared caching layer |

Competitor details come from memory as of mid-2026 and were not looked up for this doc; verify them before any external use.

**Platform risk.** GitHub controls runner pricing (it announced, then postponed, a per-minute fee for self-hosted runners in late 2025) and the cache internals Tachyon intercepts (its cache backend was rewritten in 2025). The design contains this by keeping GitHub behind an adapter and adding a second frontend in Phase 3.

## Compute layer

Each job runs in its own Cloud Hypervisor microVM on an owned bare-metal host, and the VM is destroyed when the job ends. Cloud Hypervisor is chosen over Firecracker because it supports virtio-pmem (needed to share the base image in RAM, see the storage section), snapshot and restore, and VFIO device passthrough for future GPU jobs.

**Hosts (proposed).** AM5-socket machines with high clock speeds: AMD EPYC 4004/4005 series where available, for ECC memory, otherwise Ryzen 9 9950X class. Each host gets 128 to 192 GB RAM, two local NVMe drives and 10 to 25 GbE. Start on rented dedicated servers (Hetzner, Latitude.sh or OVH) and move to colocation once load is predictable.

**VM sizes.** 2, 4, 8 and 16 vCPU, with 4 GB of RAM per vCPU, x86\_64 first and ARM64 in Phase 3. The guest runs a trimmed Ubuntu 24.04 image built from GitHub's public `actions/runner-images` templates, so workflows behave as they do on `ubuntu-24.04`.

**MicroVM lifecycle on a host:**

1. The host agent (`tachyon-hostd`) keeps a small pool of VMs restored from a base snapshot and paused, so no job waits for a kernel boot.
2. On assignment it attaches the job's writable disk (a copy-on-write fork of the repo's prebuild, or of the base image), passes the job config over vsock, and resumes the VM.
3. The GitHub runner starts with a single-use just-in-time config, registers, and runs the job.
4. When the job ends, hostd destroys the VM, discards its writable disk (unless the run was a prebuild or a trusted sticky-volume write), and flushes metrics.

hostd runs each VM under a jailer: its own unprivileged user, namespaces, cgroups and a seccomp filter. One job per VM, never reused.

## Image and storage layer

No job ever copies an image: the read-only base is mapped once per host and shared by every VM, and each job's writable disk is an instant copy-on-write clone on local NVMe.

Inside the guest, the root filesystem is an overlay. The lower layer is the base image, exposed through virtio-pmem with DAX so every VM reads the same pages from the host's page cache. The upper layer is the job's writable disk.

| Layer | Contents | Where it lives | Lifetime |
| --- | --- | --- | --- |
| Base image (read-only) | OS, runner, toolchains matching GitHub's `ubuntu-24.04` | ext4 image file on host NVMe, mapped via virtio-pmem | Rebuilt weekly; old versions kept while jobs use them |
| Prebuild (read-only, per repo) | Checkout, dependencies and caches at a trusted commit | Snapshot on host NVMe | Until a newer prebuild replaces it |
| Job disk (writable) | Everything the job writes | Copy-on-write clone of the prebuild, or an empty disk | Deleted when the job ends |
| Sticky volumes (per repo) | Docker and BuildKit state, opted-in paths | Separate virtual disk, cloned per job | Promoted on trusted runs, evicted least-recently-used |

Docker's data directory must sit on a real filesystem, not on the overlay, so `/var/lib/docker` is always a sticky volume.

**Snapshot engine.** ZFS volumes give instant clones, compression and incremental send for replication. LVM thin provisioning is the fallback if ZFS write throughput or memory use disappoints. Benchmark both in Phase 0 (decision D2).

**Distribution.** The base image is pushed to every host before it goes live, canary hosts first. Each prebuild is replicated incrementally to two hosts in its repo's placement set. A host that lacks a needed snapshot pulls it from a peer, and the job can start before the pull finishes only if the snapshot format supports lazy loading (open question).

## Prebuilds: the core differentiator

A prebuild is a snapshot of a repo's fully prepared job environment at a trusted commit; every job forks it copy-on-write in under a second, so checkout, cache restore, dependency install and warm-up mostly disappear. One prebuild run serves every job on that repo until main moves on.

**When a prebuild runs.** On pushes to the default branch and other protected branches, at most once per configurable interval per repo, plus a nightly refresh. Each prebuild has a key: a hash of the base image version, the repo's declared key files (lockfiles, toolchain versions) and its prebuild config. If the key is unchanged, the refresh only fetches new git objects; if it changed, the warm-up reruns.

**Repo config**, checked into the repo:

```yaml
# .tachyon/prebuild.yml
version: 1
key_files: [package-lock.json, go.sum, .tool-versions]
paths: [~/.npm, ~/go/pkg/mod, ~/.cache/go-build, node_modules]
warm:
  - run: npm ci
  - run: go mod download
  - run: docker pull postgres:16
memory_snapshot: false
```

**How a prebuild is built:**

1. Boot a VM from the base image with an empty writable disk.
2. Clone the repo at the commit using a short-lived, read-only token, and leave no credentials in `.git/config`.
3. Run the warm steps with no repo secrets and network access limited to Tachyon's mirrors and an allowlist.
4. Scrub known credential files (`~/.npmrc` tokens, `~/.docker/config.json`, `~/.git-credentials`) and run a secret scanner over the writable layer.
5. Snapshot the writable disk, register it under its key and commit, and replicate it to two hosts.

**How jobs use it.** Paths outside the workspace (home-directory caches, tool caches, pulled Docker images, git objects) are simply present when the job starts: no workflow change. Paths inside the workspace need one step, because `actions/checkout` cleans untracked files by default:

```yaml
- uses: actions/checkout@v4
- uses: tachyon/warm@v1   # restores node_modules etc. from the prebuild, sets TACHYON_WARM=hit
- run: npm ci
  if: env.TACHYON_WARM != 'hit'
```

This is the one place Tachyon is not fully drop-in. Tier 1 gains (outside the workspace) need nothing; tier 2 gains (inside it) need that step.

**Correctness safeguards.** A job whose commit changes a key file still forks the latest prebuild but is marked partial, so its installs run. A nightly clean run per repo, without a prebuild, catches drift that only shows on a clean build: the GitHub adapter re-runs the repo's finished default-branch run for its newest commit that used a prebuild as a new attempt with every Tachyon job forked from the base image, compares the two attempts job by job, and reports any job whose result differs as a `Tachyon nightly clean run` check on the commit. A re-run repeats every job of the workflow run, deploy jobs included, so repos must opt in (`-nightly-repos`), and an older commit's run is never re-run (that could roll a deploy back). Workflows whose names suggest side effects (deploy, release, publish) are also skipped. Any job can opt out with the `tachyon-clean` label, and a missing or damaged prebuild falls back to the base image: slower, never failed.

**Memory snapshots** (warm Gradle or Bazel daemons, a hot page cache) are off in v1. They are a Phase 3 experiment, allowed only from prebuild VMs that never held a secret.

## Scheduler and capacity

The scheduler puts each job on a host that already holds its prebuild, packs hosts by measured rather than requested usage, and spills to rented cloud capacity only when owned hosts are full. Utilization, not CPU price, sets Tachyon's cost per minute.

**Placement.** Hard constraints filter hosts first: architecture, VM size, region, and any dedicated-host rule for the tenant. Remaining hosts are scored on locality (prebuild on this host, then in this region, then none), free headroom, and spread across failure domains. Host agents report capacity and snapshot inventory on every heartbeat.

**Warm capacity.** Each host keeps a few paused VMs ready. Push and pull-request webhooks usually arrive before the job's `queued` event, so the scheduler pre-forks a job disk on the likely host as soon as it sees them.

**Scheduling by measured usage.** Every repo, workflow and job name builds a usage profile from past runs: p95 CPU and peak memory. Hosts are packed by profile, with CPU oversubscribed up to a ratio that starts at 1.25x and rises only with evidence (decision D6). Memory is never oversubscribed past a job's p99 plus a margin.

Guardrails: per-VM CPU weights in cgroups, host-level CPU steal monitoring, and an automatic step back in a host's ratio when its jobs run measurably slower than their own history.

**Capacity mix.** Owned hosts are sized to cover roughly the 60th to 70th percentile of weekly demand, so they stay busy. Peaks overflow to on-demand cloud instances or hourly bare metal. Spot instances take only short jobs, since an interruption kills the job.

**Filling the troughs.** Scheduled and nightly workflows can opt into a discounted, deferrable class that runs when the fleet is quiet. This turns idle off-peak capacity into revenue instead of cost. A deferrable job is admitted while fleet CPU use is under half of capacity and waits for owned capacity rather than bursting, but it always starts within a bound (6 hours by default, `-max-deferral`), at which point it is placed like any other job. It is billed at a discount off the runner rate (40% as a placeholder until D5). The control plane measures the class's effect: time-weighted fleet utilization with and without deferrable jobs, and how long deferred jobs waited.

## Cache, Docker layer and mirror services

Three services in each region, on the same network as the hosts, handle what prebuilds don't: a GitHub-compatible cache, persistent Docker volumes, and pull-through package mirrors.

**Actions cache.** Tachyon implements the cache protocol that `actions/cache` and the `setup-*` actions use (as of mid-2026, a Twirp API that hands out signed blob URLs), and points each VM's cache URL at it. Existing workflows get same-datacenter restores with no YAML changes. Storage is an S3-compatible object store on NVMe servers (decision D4).

Scoping copies GitHub's rules: entries from the default branch are readable by every branch, pull-request entries stay with that pull request, and pull requests from forks can read but never write. Quotas are per repo and larger than GitHub's default. Because the protocol is GitHub-internal, contract tests run daily against the newest releases of `actions/cache` and the toolkit.

**Docker and BuildKit volumes.** `/var/lib/docker` is a per-repo sticky volume, cloned copy-on-write for each job. A job from a trusted branch that succeeds has its clone promoted to the new head; pull-request clones are discarded. Clone-then-promote means two jobs never write the same volume, and an untrusted job can never poison it.

**Package mirrors.** Pull-through caches for public packages, set in the guest so tools use them without configuration:

| Ecosystem | Mirror | How the guest is pointed at it |
| --- | --- | --- |
| Docker Hub, GHCR, Quay | Registry proxy | Docker daemon `registry-mirrors` |
| npm | npm proxy | `npm_config_registry` |
| PyPI | PyPI proxy | `PIP_INDEX_URL` |
| Go modules | Go module proxy | `GOPROXY` |
| Maven, Gradle | Maven repository proxy | Gradle init script, Maven settings |
| apt | apt cache | apt proxy config |
| Git | Per-region mirror of repos Tachyon builds | `url.insteadOf` for fetches |

Private registries are never mirrored: their traffic passes straight through, with the customer's own credentials.

## Job API and the GitHub Actions adapter

The core accepts one kind of request (run this spec, from this image and prebuild, with these volumes, on this hardware) and GitHub Actions is the first adapter that turns its events into those requests. No GitHub-specific logic lives below the adapter.

**Job spec (internal API):**

```yaml
job_id: j_01HZX...
tenant: org_acme
repo: github.com/acme/api
commit: 9f1c2ab
hardware: {arch: x86_64, vcpu: 8, memory_gb: 32}
image: ubuntu-24.04@2026-11-16
prebuild: {repo: acme/api, policy: latest-trusted}
volumes:
  - {kind: sticky, key: docker, mount: /var/lib/docker}
bootstrap: {kind: github-runner, jit_config: secret://jit/j_01HZX}
trust: untrusted-fork   # trusted | untrusted | untrusted-fork
```

**GitHub adapter flow:**

1. A customer installs the Tachyon GitHub App. It asks only for what this needs: self-hosted runner management, read access to Actions, and read access to contents for prebuilds.
2. The adapter receives `workflow_job` webhooks and acts on jobs whose labels start with `tachyon-`, such as `tachyon-ubuntu-24.04-8x`.
3. For each queued job it creates a single-use, repository-scoped just-in-time runner config. Repository scope matters: GitHub hands a queued job to any matching idle runner, so org-wide runners would land jobs on hosts without that repo's prebuild.
4. It submits the job spec; the runner registers, GitHub assigns the job, and the VM is destroyed afterward.
5. A reconciliation loop retries jobs still queued after a timeout and removes orphaned runner registrations through the API.
6. Once a night per opted-in repo it re-runs one finished default-branch run clean and reports divergence as a check run (see Prebuilds). This needs two further permissions, write access to Actions and to checks; without them the nightly run is skipped.

The adapter derives `trust` from the event: default and protected branches are trusted, same-repo pull requests are untrusted, and pull requests from forks are untrusted-fork.

**Later adapters** reuse the same spec: GitLab Runner through its custom executor or autoscaler plugin interface, Buildkite agents running in Tachyon VMs, and Forgejo Actions, which shares the GitHub workflow syntax.

## Security and isolation model

Every job runs in a fresh hardware-virtualized microVM that is destroyed afterward, and nothing an untrusted job writes can reach another job, a prebuild, a promoted volume or another tenant.

| Threat | Controls |
| --- | --- |
| Escape from a VM to the host | KVM microVM per job; minimal device model; jailer with unprivileged user, namespaces, cgroups and seccomp; fast patching of host kernels and the hypervisor |
| Data leaking between tenants | No memory deduplication across tenants; job disks discarded; no VM-to-VM network traffic; dedicated hosts available for enterprise tenants |
| Poisoned caches, prebuilds or volumes | Only trusted refs build prebuilds or promote sticky volumes; pull-request cache writes stay with that pull request; forks read only |
| Secrets captured in snapshots | Prebuilds run without secrets; credential files scrubbed; secret scan before a snapshot is registered; memory snapshots only from VMs that never held a secret |
| Stolen or misused credentials | Least-privilege GitHub App; single-use just-in-time runner configs; short-lived tokens; secrets kept in a managed key store |
| Runners used for abuse (mining, attacks) | Egress rate limits, anomaly alerts on CPU and network patterns, limits and verification on free and trial accounts |
| Compromised base image | Reproducible image builds, signed images, a software bill of materials per image, canary rollout |

SOC 2 Type II is a proposed Phase 3 milestone, since larger customers will ask for it before trusting Tachyon with source code and secrets.

## Observability, metering and billing

Tachyon timestamps every phase of every job, from webhook to teardown; the same record drives billing, the customer's "minutes saved" report, and alerts when the fleet gets slower.

**Per-job phases recorded:** webhook received, host assigned, VM resumed, runner online, job started, each step's start and end, job finished, VM destroyed. Each job also records its prebuild match (full, partial or none) and its cache hits.

**Fleet metrics to watch:**

- Pickup latency, p50 and p95, per region and VM size.
- Prebuild hit rate and setup time per repo.
- Cache hit rate and restore throughput.
- Host vCPU utilization and the slowdown of jobs on oversubscribed hosts.
- Cost per job-minute, owned versus burst capacity.

**Stack.** OpenTelemetry from hostd and services, a Prometheus-compatible metrics store with Grafana, and ClickHouse for job-level analytics and logs.

**Billing model (proposed).** Per second of runner time by VM size, with prebuild minutes included up to a per-repo allowance and storage above quota billed per GB-month. The customer dashboard shows the GitHub-hosted equivalent of each job and the minutes prebuilds saved. Prices are an open decision (D5), set after Phase 0 produces a real cost per job-minute.

## Reliability and operations

A Tachyon failure should make a job slower, not make it fail: missing prebuilds fall back to the base image, a full region spills to burst capacity, and in-flight jobs keep running if the control plane goes down.

**Proposed SLOs:** 99.9% of queued jobs picked up, 99.95% control plane availability, p95 pickup under 10 seconds even while a host or service is failing.

| Failure | What happens |
| --- | --- |
| Host dies mid-job | Job fails on GitHub; with the customer's opt-in, the adapter re-runs failed jobs when the cause was infrastructure |
| Prebuild missing or damaged | Job forks the base image instead; alert raised |
| Cache service down | `actions/cache` treats it as a miss and the job continues |
| Mirror down | Mirrors run as health-checked pairs; if both fail, guests are pointed at the upstream registries |
| Control plane down | Running jobs finish; new jobs wait in GitHub's queue and start when it recovers |
| GitHub API errors or rate limits | Backoff with a per-installation rate budget; queued jobs reconciled afterward |
| Region at capacity | Overflow to burst capacity, then to the next region |

**Operations.** Hosts are provisioned from code and network-booted, and drained before kernel or hypervisor updates. Base images and hostd releases roll out to canary hosts first, then the fleet. An on-call rotation starts with the private beta.

The first region (decision D3) should be close to most pilot customers; EU data residency can follow as a second region.

## Risks, open questions and decisions

The biggest risk is that prebuilds save less than expected on real workloads, so Phase 0 measures that before anything else is built.

| Risk | Mitigation |
| --- | --- |
| Setup is a small share of pilot jobs, so prebuilds save little | Measure in Phase 0; proceed only past the gate (setup at least 30% of job time) |
| GitHub changes runner pricing or the cache protocol | Adapter boundary, daily contract tests, second frontend in Phase 3 |
| A stale prebuild hides a failure a clean build would catch | Key-based invalidation, nightly clean runs, `tachyon-clean` opt-out |
| A secret ends up in a snapshot | Secret-free prebuilds, credential scrubbing, secret scan before registering |
| Oversubscription slows jobs | Profile-based packing, a conservative starting ratio, automatic step-back |
| Consumer-class hardware fails more often | ECC-capable EPYC 4004/4005 hosts where possible, spare capacity, fast drain and replace |
| A competitor ships prebuilds first | Move fast to a public prebuild beta; compete also on utilization-driven price and more frontends |

**Decisions needed:**

- [x] D1: Language for hostd and the control plane — **decided:** Rust for `tachyon-hostd`, Go for everything above it, protobuf at the boundary ([0001-language.md](decisions/0001-language.md))
- [ ] D2: Copy-on-write engine (ZFS volumes or LVM thin), from Phase 0 benchmarks
- [ ] D3: First region and server provider
- [ ] D4: Object store behind the cache service
- [ ] D5: Prices and the prebuild-minute allowance
- [ ] D6: Starting CPU oversubscription ratio

**Open questions:**

- Which 10 repos are the pilots, and who are their design-partner owners?
- Do pilots need `/dev/kvm` inside jobs (Android emulators, VM tests)?
- Can prebuild snapshots load lazily on a host that doesn't yet hold them?
- How much NVMe per host do prebuilds and sticky volumes need at 100 active repos?
