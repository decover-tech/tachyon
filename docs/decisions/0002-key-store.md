# Key store for secrets

Status: **decided**, Nov 21, 2026 (issue #23)

## Decision

Keep JIT runner configs and tokens in a **HashiCorp Vault or OpenBao KV
version 2** secrets engine, reached over its HTTP API by
`internal/secrets.Vault`. Everything else in the control plane sees only
`secret://<path>#<random id>` references.

## Why

- **It runs where we run.** Tachyon runs on owned bare metal. Vault/OpenBao
  runs on our own hosts, so the key store doesn't tie the control plane to
  one cloud's KMS.
- **TTLs live in the store.** KV v2 `delete_version_after` makes the store
  delete each value when its TTL passes, even if the control plane crashes
  before it cleans up.
- **No new dependency.** The client is a small `net/http` wrapper; the
  Vault Go SDK is not needed. OpenBao speaks the same API, so the license
  question stays open without blocking us.
- **Least privilege is easy to express.** The control plane's token needs
  create, update, read and delete on `<mount>/{data,metadata}/<prefix>/*` and nothing
  else.

Cloud KMS or Secrets Manager products would also work. `secrets.Store` is
the seam if a deployment wants one.

## How secrets are handled

- `Put` writes a fresh key per secret (`<prefix>/<path>/<id>`), with
  `max_versions=1`, `delete_version_after` set to the TTL, and `cas=0` so it
  never overwrites. The value is base64 in a `value` field next to an
  `expires_at` time, which `Get` checks too, so an expired value is never
  returned while it waits for the server's sweep.
- TTLs come from `secrets.Policy`, by path prefix. JIT configs (`jit/`) get
  **10 minutes** (`-jit-ttl`): the GitHub adapter mints a new config when a
  job isn't picked up within 2 minutes, so a config never needs to live
  longer. Everything else gets 1 hour.
- Never logged: errors from the store carry only the reference, the HTTP
  method and the status code, never request or response bodies.
  `secrets.Redacted` wraps secret bytes (including the Vault token) so that
  `fmt`, `slog` and JSON print `[REDACTED]`. hostd already redacts the
  runner config in `hypervisor.rs`.
- The token comes from `TACHYON_VAULT_TOKEN` or `-vault-token-file`, which is
  re-read per request so a Vault Agent sidecar can rotate it. It is never a
  command-line flag.
- `secrets.Mem` (still reachable as `controlplane.MemSecrets`) implements the
  same TTLs in memory for tests and `-secrets=mem` development runs.

## Not yet done

- Authentication beyond a token (AppRole, Kubernetes or cloud IAM auth) is
  left to a Vault Agent sidecar that writes the token file.
- The JIT config still crosses the hostlink stream in plaintext inside
  `AssignJob` until hostlink mTLS lands.
