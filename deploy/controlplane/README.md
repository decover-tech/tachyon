# Control plane image

`Dockerfile` builds a static `tachyon-controlplane` on distroless.
`docker-compose.yml` runs it with Postgres, with metering and billing on.

## Setup

`./secrets/` (never commit it):

| File | What |
| --- | --- |
| `db-password` | Postgres password |
| `github-app.pem` | GitHub App private key |
| `hostlink.crt`, `hostlink.key`, `hosts-ca.crt` | hostlink mTLS (see `deploy/fleet`) |
| `admin-token` | operator API bearer token, at least 32 bytes |

`./controlplane.env` holds the environment the binary reads, at least
`TACHYON_DATABASE_URL=postgres://tachyon:<password>@postgres:5432/tachyon`,
`TACHYON_GITHUB_WEBHOOK_SECRET` and the key store settings (`VAULT_ADDR`, or
`-secrets=mem` for development).

## Metering and billing

Billing is on when `-bill-runner-per-minute` is set (prices are D5 and still
open, so the compose defaults are placeholders):

| Flag | Meaning |
| --- | --- |
| `-bill-runner-per-minute` | USD per runner minute by vCPUs, e.g. `2=0.002,4=0.004`. Runner time is metered per second from VM resume to job finish |
| `-bill-prebuild-allowance` | prebuild runner time included per repo per month; the rest bills as runner time |
| `-bill-storage-quota-gb`, `-bill-storage-gb-month` | storage included per repo, and the price per GB-month above it |

Usage is rebuilt from Postgres job records on start (this month and last), so
a restart loses nothing. Invoices come from the operator API:

```sh
curl -H "Authorization: Bearer $(cat secrets/admin-token)" \
  'http://127.0.0.1:8081/admin/billing/invoices?period=2026-11'
curl -H "Authorization: Bearer $(cat secrets/admin-token)" \
  'http://127.0.0.1:8081/admin/dashboard?tenant=<installation>'
```

Amounts are micro-USD (`total_micro_usd`) with a formatted copy (`total`).
