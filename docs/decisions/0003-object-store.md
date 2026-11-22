# Object store behind the cache service

Status: **Proposed, pending sign-off** (issue #26). The decision belongs to the maintainers. Facts below were checked on Nov 21, 2026.

## Decision (proposed)

Run **Garage** as the per-region object store for the pilot, behind the `cache.S3Store` client (`internal/cache/s3store.go`). Treat **SeaweedFS** as the fallback if the pilot measurements in "What we would measure first" show Garage is too slow. Do not use MinIO. Ceph RGW is the answer only if Tachyon later needs a general-purpose storage cluster, which the cache alone does not justify.

Because the service speaks plain S3 with path-style addressing and SigV4, the store is a deployment choice, not a code dependency: switching is a change of flags, not a rewrite.

## What the cache needs from the store

From `cache.BlobStore` and `docs/architecture.md`: put whole objects (a few KB to a few GB, written in 16 MiB parts above that size), read byte ranges, delete, and tolerate an eviction sweep deleting thousands of keys. No listing, versioning, ACLs, policies, or server-side encryption are needed: blobs are named by the service, reached only through short-lived signed URLs the service itself issues, and a lost blob is just a cache miss. This is a small slice of S3, so ops burden and licence should weigh more than feature breadth.

## Comparison

| | Garage | SeaweedFS | Ceph RGW | MinIO (community) |
| --- | --- | --- | --- | --- |
| Licence | AGPLv3 | Apache 2.0 core; separate proprietary Enterprise edition | LGPL (Ceph as a whole; mix of 2.1/3 in sources) | AGPLv3 source, but see status |
| Maintenance (Nov 2026) | Active; 2.x series | Active; frequent releases | Active; Tentacle (v20) point releases | **Repository archived, "no longer maintained"** |
| Ops burden | Lowest: one static binary, one process per node, gossip-based membership | Medium: master, volume servers, filer and S3 gateway to run and size | Highest: a RADOS cluster (MON, MGR, OSD) plus RGW, and the skills to run it | Was low; now no upstream fixes |
| Redundancy model | Replication across zones (3 copies); no erasure coding | Replication; erasure coding for warm data | Replication or erasure coding, mature | n/a |
| Throughput | **Unmeasured by us** | **Unmeasured by us** | **Unmeasured by us** | **Unmeasured by us** |

Throughput: we have no measurements, and none of the sources we found gave benchmark numbers we trust, so none are quoted here. Garage's own site gives hardware minimums but no throughput figures. All four serve ranged GETs from local disk or NVMe through an HTTP gateway; the real differences (small-object overhead, many-connection concurrency, behaviour during rebalance) have to be measured on our hardware.

### MinIO

The facts changed in 2025 and 2026. The `minio/minio` repository's README now says "THIS REPOSITORY IS NO LONGER MAINTAINED" and the repository was archived (read-only) on Apr 25, 2026. It is distributed as source only, old pre-built binaries get no updates, and the README points users at the vendor's commercial AIStor editions. Secondary sources report a "maintenance mode" notice on Dec 3, 2025 and a "no longer maintained" notice on Feb 12, 2026, and that related repositories (operator, `mc`) were archived later; we confirmed the archive date and README text on the primary page but not the other dates. The code is still AGPLv3, so we could fork it, but then we would own security patches for an object store that holds customer build artifacts. That is the wrong trade for a pilot. Rejected.

### Ceph RGW

The most capable and the most proven at scale, and a good fit if we ever run a large shared storage cluster. For a cache whose entire loss is a slower build, running RADOS (monitors, managers, OSDs, CRUSH maps, upgrades) in every region is a lot of operational weight. Not recommended now; the S3 client works with it unchanged if that changes.

### SeaweedFS

Apache 2.0 is the most permissive licence here, and it is actively developed with an S3 gateway. The costs are more moving parts than Garage (master, volume and filer layers, plus the S3 gateway) and an open-core split: some features, including self-healing and some erasure-coding tooling, are in a proprietary Enterprise edition. We would need to check, before relying on any feature, that it is in the Apache-licensed core.

### Garage

Built by the non-profit Deuxfleurs for self-hosting on modest hardware: a single binary per node, no external coordinator, data replicated across zones. Its S3 compatibility page lists multipart upload, HeadBucket, path-style addressing and SigV4 as implemented, which covers everything `S3Store` calls except two behaviours it does not document: `Range` on GetObject and `If-Match`. **Both must be verified against a real Garage before pilot** (see below). Garage has no erasure coding, so storage overhead is the replica count (3x for a three-zone cluster), which matters for NVMe cost. It is AGPLv3: running an unmodified build as an internal service puts no obligation on Tachyon's own code, because `S3Store` talks to it over HTTP and links nothing from it. If we ever patch Garage and offer it to customers over a network we must publish the patch. That is a legal question for sign-off, not a technical one.

## Why Garage first

- **Smallest ops surface** that meets the cache's needs, and the cache tolerates loss, so replication-only storage and a simple cluster are acceptable.
- **A live upstream**, unlike MinIO, and no commercial edition steering features out of the open build, unlike SeaweedFS.
- **Cheap to be wrong:** the client is store-agnostic and the fallback is a flag change.

Against it: AGPL (needs sign-off), 3x replication overhead, a smaller community than Ceph or SeaweedFS, and unverified throughput.

## What we would measure first

Run on one region's NVMe hardware before committing, with `S3Store` against each candidate (Garage first, SeaweedFS second):

1. Whole-object PUT throughput for 1 MiB, 100 MiB and 2 GiB blobs, at 1, 8 and 64 concurrent uploads (the 16 MiB part path included).
2. Ranged GET throughput and p99 latency under the same concurrency, since restores read ranges.
3. Behaviour while a node is down and while it rejoins.
4. Eviction: delete 100k small objects and check request latency for foreground traffic.
5. Smoke test that `Range` and `If-Match` behave as `S3Store` expects. The store refuses a 200 response to a ranged GET, so a store that ignores `Range` fails loudly, not silently.

Until then, throughput is unmeasured and this record makes no performance claim.

## How the service uses the store

- One bucket per region, created ahead of time by an operator (see "Creating the bucket" in the README). The service checks it with `HEAD bucket` at start and exits if it is unreachable.
- Keys are the opaque ids the service chooses, optionally under `-s3-prefix`. Index metadata (keys, versions, scope, LRU order) stays in the service, not the store.
- Blobs under one part (16 MiB by default) are one `PutObject`; larger ones use multipart upload and abort it on failure. Memory is about one part per concurrent upload.
- Reads use `Range` with `If-Match` on the ETag seen at open, so a blob replaced mid-restore fails the read instead of returning mixed bytes.
- Credentials come from `TACHYON_S3_ACCESS_KEY_ID` and `TACHYON_S3_SECRET_KEY` (or files), never flags, and the credential type prints as `[REDACTED]`. The key pair needs read, write and delete on the one bucket and nothing else.

## Not yet done

- No real object store has been run against `S3Store`; it is tested against an in-process fake that verifies SigV4 signatures. The signer also matches the AWS documentation's published example signature.
- No retries or backoff in the client: a transient store error is a cache miss or a failed save for that job.
- TLS to the store, bucket lifecycle rules for orphaned multipart uploads, and cross-region replication are deployment work, not covered here.

## Sources

Checked Nov 21, 2026.

- MinIO repository (archived, README notice, AGPLv3, AIStor pointer): <https://github.com/minio/minio>
- Timeline of MinIO's maintenance-mode and archive dates (secondary): <https://ttlab.hopto.org/blog/2026-07-17-minio-archived-garage-seaweedfs-ceph-rgw?lang=en>, <https://bizety.com/2025/12/06/minio-in-maintenance-mode-open-source-alternatives/>
- Garage site (design, hardware minimums, no benchmark figures): <https://garagehq.deuxfleurs.fr/>
- Garage S3 compatibility: <https://garagehq.deuxfleurs.fr/documentation/reference-manual/s3-compatibility/>
- Garage licence (AGPLv3) and releases (2.x): <https://github.com/deuxfleurs-org/garage>, <https://docs.rs/crate/garage/2.4.1>, <https://lwn.net/Articles/1077739/>
- SeaweedFS (Apache 2.0, Enterprise edition): <https://github.com/seaweedfs/seaweedfs>
- Ceph Tentacle releases: <https://docs.ceph.com/en/latest/releases/tentacle>, <https://ceph.io/en/news/blog/2026/v20-2-2-tentacle-released/>
- Ceph licence (LGPL, from a mirror and package metadata; no RGW-specific statement found): <https://wikipedia.com/wiki/RADOS>
