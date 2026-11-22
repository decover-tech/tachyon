# Base image pipeline runbook

How an operator builds, signs, verifies and installs a base image, prepares it on a host, and makes a version live. It covers only what exists in this repo and says where a step cannot be done from the repo (it needs a real builder, a real host, or tooling the repo does not provide).

**Status: no image has been built or booted with this pipeline yet.** Every command below is documented from the code and its unit tests. The first real run on a Linux builder and on a KVM host will find problems; treat this document as the plan to follow and correct it when that happens.

Design background: [Compute layer](architecture.md#compute-layer) and [Image and storage layer](architecture.md#image-and-storage-layer). Image definition: `images/ubuntu-24.04/image.yml`.

## Overview

```
builder (Linux, root)            signer (no build code)        each host
build.sh -> rootfs.ext4          tachyon-image sign            copy files into TACHYON_IMAGES
            sbom.spdx.json  ---> manifest.signed.json  ---->   tachyon-hostd prepare-image
            manifest.json                                       (verify, boot once per size, snapshot)
                                                                      |
                                       control plane <-- heartbeat: prepared versions
                                       publish manifest, canary, live
```

An image version is `name@version`, e.g. `ubuntu-24.04@2026-11-16` (a weekly version is that Monday's date; a rebuild the same week is `2026-11-16.1`). The control plane pins each job to one version (`AssignJob.base_image`). A host restores each job's VM from the snapshots of exactly that version, and refuses a job whose version it has not prepared.

## 1. Build (Linux builder, root)

Cannot be done from this repo alone: it needs a Linux machine with about 100 GB of disk, root, network access to GitHub, snapshot.ubuntu.com and cloud-images.ubuntu.com, `curl`, `git`, `xz`, `sha256sum`, and e2fsprogs 1.47.1 or newer (`build.sh` header). `build.sh` refuses to run on non-Linux or as non-root.

```sh
go build -o /usr/local/bin/tachyon-image ./cmd/tachyon-image
sudo TACHYON_IMAGE_VERSION=2026-11-16 images/ubuntu-24.04/build.sh /var/tmp/img
```

Optional environment: `TACHYON_IMAGE` (the `tachyon-image` binary if not on `PATH`), `TACHYON_IMAGE_VERSION` (default: this week's Monday, from `tachyon-image version`), `APT_SNAPSHOT`, `KERNEL` (a `vmlinux` to ship in the image directory).

Output in `/var/tmp/img`:

- `rootfs.ext4`, the read-only lower layer
- `sbom.spdx.json`, the SBOM
- `manifest.json`, what was built from what, with SHA-256 digests
- `vmlinux`, only if `KERNEL` was set

`build.sh` runs upstream install scripts as root in a chroot, which is not a security boundary. It never signs, and the signing key must not be on the builder.

The same build runs weekly from `.github/workflows/base-image.yml` once the repo variable `TACHYON_IMAGE_PIPELINE` is `enabled` and a builder is set in `TACHYON_IMAGE_BUILDER`. That workflow's build job uploads only `manifest.json` and `sbom.spdx.json`; it does not upload `rootfs.ext4` anywhere (see "Getting the image to hosts").

### Reproducibility check

Builds are not yet guaranteed byte-identical: some upstream install scripts download "latest" artifacts outside apt. To measure how far a build is from reproducible, build the same version twice and diff the manifests:

```sh
tachyon-image diff a/manifest.json b/manifest.json
```

It prints the fields that differ and exits non-zero, or prints `reproduced: builds are identical`.

## 2. Sign (separate machine)

Generate the signing key pair once. This uses `openssl`, not a repo tool; the formats the repo expects are an ed25519 PKCS#8 private key and a `PUBLIC KEY` PEM:

```sh
openssl genpkey -algorithm ed25519 -out image-signing.pem
openssl pkey -in image-signing.pem -pubout -out image-trust.pem
```

Keep `image-signing.pem` off every builder and host. In CI it is the secret `TACHYON_IMAGE_SIGNING_KEY` and the public half is the variable `TACHYON_IMAGE_TRUSTED_KEYS`; the `sign` job runs on a fresh runner that ran none of the build's code. A trust file may hold several public keys, which is how keys are rotated.

Sign the manifest from the build, on a machine that did not run the build:

```sh
tachyon-image sign -key image-signing.pem -o manifest.signed.json manifest.json
```

## 3. Verify

Anywhere with the image directory and the trusted public key(s):

```sh
tachyon-image verify -trust image-trust.pem -dir /var/tmp/img manifest.signed.json
```

With `-dir`, every file the manifest lists is re-hashed. It prints `<name@version>: signature ok (key <id>)` or fails. Verify before the image leaves the builder and, as above, `prepare-image` verifies again on each host.

## 4. Install on a host

### Getting the image to hosts

**Not provided by the repo.** There is no image store or push tool yet (the "Distribution" paragraph in the architecture doc is a design, and the CI workflow has a note saying the push is not wired). Today an operator copies the files to each host by hand or with their own tooling (rsync, scp, an object store). Whatever is used must preserve the bytes: `prepare-image` rejects a rootfs whose digest differs from the signed manifest.

On each host, create the version directory in `TACHYON_IMAGES` (default for this runbook: `/var/lib/tachyon/images-store`; any directory works) with exactly these names:

```
$TACHYON_IMAGES/ubuntu-24.04@2026-11-16/
  manifest.signed.json     from step 2
  rootfs.ext4              from step 1
  vmlinux                  from step 1, only if the build shipped one
```

Make the rootfs read-only; hostd refuses a base image that is writable (it is shared by every VM) or whose size is not a non-zero multiple of 2 MiB:

```sh
chmod 0444 "$TACHYON_IMAGES/ubuntu-24.04@2026-11-16/rootfs.ext4"
```

`sbom.spdx.json` and `manifest.json` do not need to be on the host.

`TACHYON_IMAGES` is a different directory from `TACHYON_IMAGES_DIR` (which holds `<name@version>.img` files for peer seeding). With `TACHYON_IMAGES` set, hostd does not report the `.img` files in heartbeats, so a host is never counted as holding a version it has not prepared.

### Prepare

Needs a Linux host with KVM, Cloud Hypervisor, `ch-remote`, cgroup v2, and root. Cannot be done from this repo alone.

```sh
export TACHYON_IMAGES=/var/lib/tachyon/images-store
export TACHYON_IMAGE_TRUST=/etc/tachyon/image-trust.pem      # public key(s) from step 2
export TACHYON_GUEST_KERNEL=/var/lib/tachyon/vmlinux          # only if the image ships no kernel
tachyon-hostd prepare-image ubuntu-24.04@2026-11-16
```

(`TACHYON_CH_BINARY`, `TACHYON_CH_REMOTE`, `TACHYON_RUN_DIR`, `TACHYON_CGROUP`, `TACHYON_BASE_UID` and `TACHYON_GUEST_CMDLINE` have defaults; see the README.)

It verifies the signature against `TACHYON_IMAGE_TRUST` and every file digest, then boots the image once per size listed in the manifest (2, 4, 8 and 16 vCPU, 4 GB per vCPU), waits for the guest agent, pauses the VM and writes `snapshots/<vcpu>/`. A size is complete when its `.tachyon-ready` marker is written; the version is usable when `snapshots/.tachyon-ready` exists, after every size is done. It is safe to re-run: finished sizes are skipped and half-built ones redone. Snapshots are made on the host, not shipped, because a memory snapshot is tied to the host's Cloud Hypervisor build; upgrade Cloud Hypervisor, then prepare again.

The snapshot restore expects a guest with a virtio-net device on `tap0` (address `172.16.0.2/30`, gateway `172.16.0.1`) and the guest agent listening on the ready vsock port; these are properties of the image and kernel, and have not been exercised on real hardware.

### hostd configuration

Run hostd with `TACHYON_IMAGES` set (see the README table for the rest). Then:

- Each job restores from `$TACHYON_IMAGES/<job's name@version>/snapshots/<vcpu>/`. A job for a version that is not prepared fails immediately with outcome `INFRA_ERROR` and the error `base image <ref> is not prepared on this host`, before any disk is forked or VM started. It never falls back to another version.
- Warm VMs (`TACHYON_WARM`) are kept for the newest prepared version of each image name. Older versions restore on demand when a job needs them, so their jobs start a little slower. When a newer version is prepared, hostd (rescanning every minute) starts warming it and discards the warm VMs of the previous one.
- Heartbeats report the prepared versions. A version prepared while hostd runs is picked up within a minute, without a restart; one deleted from disk stops being reported.
- Optional cleanup: `TACHYON_IMAGES_KEEP=N` makes hostd delete versions beyond the newest N per image name (N is raised to at least 2 so the live version and its rollback target stay). A version with a running job or a warm VM is never deleted. Unset, hostd never deletes images and the operator removes old directories by hand.

Single-image hosts can keep using `TACHYON_BASE_SNAPSHOTS=<dir>` (snapshots at `<dir>/<vcpu>/`, no `TACHYON_IMAGES`): every job then runs on that directory regardless of the version it was assigned. Set `TACHYON_BASE_IMAGE=name@version` as well to state which image the directory holds; jobs for any other version are then refused. This mode does not verify or prepare versions and reports no prepared versions in heartbeats, so it cannot take part in the control plane's rollout; use it only with static `-image` pins.

## 5. Rollout: how a version becomes live

The control plane's rollout (`internal/images/rollout.go`) is enabled by starting `tachyon-controlplane` with `-image-trust`, `-image-manifests` and `-canary-hosts`:

```sh
tachyon-controlplane ... \
  -image-trust /etc/tachyon/image-trust.pem \
  -image-manifests /etc/tachyon/ubuntu-24.04@2026-11-16.signed.json \
  -canary-hosts h1,h2
```

Without `-image-trust` it uses static pins instead (`-image ubuntu-24.04=2026-11-16`), with no canary and no check that hosts hold the version.

Steps:

1. **Prepare the version on hosts** (step 4). Hosts report it in heartbeats; the rollout tracks which hosts hold which versions.
2. **Publish the signed manifest.** The only way today is to pass it in `-image-manifests` at control plane startup, so publishing a new version means restarting the control plane with the new manifest listed. The manifest's signature is checked against `-image-trust`. The version starts as `staged`: no job uses it.
3. **Canary.** When every reporting canary host holds the version it moves to `canary`. Jobs placed on a canary host that holds it run on the canary instead of the live version; all other jobs stay on live.
4. **Live.** After the canary has run at least 50 successful jobs, soaked at least 6 hours (defaults of `Policy`), and every non-draining host holds the version, it becomes `live` and new jobs are pinned to it. If the canary's infrastructure error rate exceeds 2% (after at least 10 jobs) it is `rolled_back` and never pinned. The very first version of an image has no canary and goes live once every non-draining host holds it.
5. **Retire.** The previous live version becomes `retired`. Its jobs finish on it, it stays prepared on hosts as a rollback target, and hosts only delete it under the rule above.

The control plane checks the gates every 30 seconds and logs each stage change as `base image rollout`.

Rollback: `Rollout.Rollback` makes the newest retired version live again. **No command or endpoint in `cmd/` calls it today**, so an operator cannot trigger a manual rollback without a control plane change; the automatic canary rollback in step 4 does work in tests. Until that exists, restart the control plane with the previous manifest and static pin if an emergency rollback is needed.

Tuning the canary policy (`MinCanaryJobs`, `MinSoak`, `MaxInfraErrorRate`, `KeepRetired`) is not exposed as flags either; the defaults above apply.

## What this runbook does not cover

- The first real build, signing run and boot. None has happened.
- Pushing `rootfs.ext4` to hosts, and verifying it in transit (see "Getting the image to hosts").
- Byte-for-byte reproducibility (see the reproducibility check).
- Guest-side requirements of the snapshot (networking, guest agent) on real Cloud Hypervisor.
