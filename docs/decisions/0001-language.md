# D1: Language for hostd and the control plane

Status: **decided**, Nov 21, 2026

## Decision

Use both languages, split at the host boundary:

- **Rust** for `tachyon-hostd`, the privileged agent on each host.
- **Go** for everything above it: control plane, scheduler, GitHub adapter,
  prebuild manager and metering.

The two meet only at `proto/tachyon/hostlink/v1/hostlink.proto`.

## Why hostd is Rust

- **It shares an ecosystem with the hypervisor.** Cloud Hypervisor and
  Firecracker are both Rust, built on the shared rust-vmm crates. Patching
  the hypervisor or reusing its memory and snapshot code happens in the same
  language and libraries.
- **It runs privileged next to untrusted code.** hostd handles hostile
  guests' disks, vsock traffic and snapshots as root. Memory safety without a
  garbage collector is worth a lot in that one component.
- **Go is awkward at the jailer layer.** Setting up namespaces, dropping
  privileges and applying seccomp per VM fights Go's multithreaded runtime.
  That's why runc carries a C shim (`nsexec`) for this exact job. Rust does it
  directly in a `pre_exec` hook (see `hostd/src/jailer.rs`).
- **The Phase 3 memory-snapshot work needs it.** Lazy memory restore means a
  userfaultfd handler on the critical path of every VM resume.

Go's garbage-collector pauses are not the reason. They are typically
sub-millisecond and wouldn't decide this on their own.

## Why everything else is Go

- **The CI ecosystem is written in Go.** google/go-github,
  actions-runner-controller, GitLab Runner and its autoscaler plugins, the
  Buildkite agent, BuildKit and containerd are all Go. Every adapter on the
  roadmap has Go reference code to learn from or reuse.
- **It's mostly service glue.** Webhooks, gRPC, Postgres, queues and
  OpenTelemetry are Go's strengths. It compiles and iterates fast, and it's
  easier to hire for.

## The boundary

hostd and the control plane already talk over the network (heartbeats, job
assignment, inventory), so a protobuf contract costs little. hostd dials the
control plane and holds one bidirectional gRPC stream, so hosts need no
inbound ports.

## Caveat: team skills come first

If the virtualization engineers aren't fluent in Rust, start hostd in Go
driving Cloud Hypervisor through its HTTP API, with a small Rust or C helper
for the jailer, and revisit before the Phase 3 memory-snapshot work. Fighting
an unfamiliar language during Phase 1 would cost more than Go's rough edges
at the host layer.
