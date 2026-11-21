# Hostlink mTLS and host identity

hostd dials the control plane and holds one gRPC stream
(`proto/tachyon/hostlink/v1`). That stream carries job assignments, including
references to secrets, so the control plane must know which host is on the
other end. Both sides authenticate with mutual TLS. Each host has its own
client certificate, and the control plane checks that the certificate's
identity equals the `host_id` the host claims in `Hello`.

## Identity

A host certificate identifies exactly one host with a single URI SAN:

```
spiffe://<trust-domain>/host/<host_id>
```

- The trust domain defaults to `tachyon` (`-hostlink-trust-domain` on the
  control plane).
- `host_id` is `[A-Za-z0-9._-]`, up to 128 bytes, and equals hostd's
  `TACHYON_HOST_ID`.
- The certificate must carry the `clientAuth` extended key usage and chain
  to a CA in the control plane's client CA bundle.

The control plane (`internal/hostauth`, `controlplane.HostLink.Auth`):

1. Requires a client certificate on every TLS 1.3 handshake and verifies the
   chain against the client CA bundle. Hosts without a certificate, or with a
   certificate from another CA, fail at the handshake.
2. Reads `Hello` and rejects the stream with `PermissionDenied` unless the
   verified leaf's host URI equals `Hello.host_id`. Host A's certificate
   cannot register as host B, so it cannot take over B's assignments or
   inventory.
3. Closes the stream with `Unauthenticated` once the earliest `NotAfter` in
   the verified chain has passed, at the host's next message. hostd then
   reconnects, which makes it present its current certificate.

hostd (`hostd/src/tls.rs`) verifies the control plane's server certificate
against `TACHYON_TLS_CA`. The name checked is the host in
`TACHYON_CONTROL_PLANE`, or `TACHYON_TLS_SERVER_NAME` if that is set.

## Configuration

Control plane:

| Flag | Meaning |
| --- | --- |
| `-hostlink-cert`, `-hostlink-key` | Server certificate chain and key (PEM) |
| `-hostlink-client-ca` | CA bundle that host certificates must chain to |
| `-hostlink-trust-domain` | SPIFFE trust domain, default `tachyon` |
| `-hostlink-insecure` | No TLS and no host authentication. Local development only |

The control plane refuses to start unless the TLS files are set or
`-hostlink-insecure` is passed.

hostd:

| Variable | Meaning |
| --- | --- |
| `TACHYON_CONTROL_PLANE` | `https://...` with mTLS; `http://...` only with insecure mode |
| `TACHYON_TLS_CERT`, `TACHYON_TLS_KEY` | This host's client certificate and key (PEM) |
| `TACHYON_TLS_CA` | CA bundle for the control plane's server certificate |
| `TACHYON_TLS_SERVER_NAME` | Optional server name override |
| `TACHYON_HOSTLINK_INSECURE=1` | No TLS. Local development only |

hostd refuses to start without the three TLS files unless insecure mode is set.

## Issuance

There are two CAs. They can be the same CA, but they don't have to be:

- The **host CA** signs host client certificates. Its certificate goes in the
  control plane's `-hostlink-client-ca`.
- The **server CA** signs the control plane's server certificate. Its
  certificate goes in each host's `TACHYON_TLS_CA`.

For development and first bootstrap, `tachyon hostcert` creates ECDSA P-256
material. It writes `<prefix>.crt` and `<prefix>.key` (mode 0600) and never
overwrites an existing file:

```sh
tachyon hostcert ca -out hostlink-ca                         # 1 year
tachyon hostcert server -ca hostlink-ca -name cp.internal -out cp   # 90 days
tachyon hostcert host -ca hostlink-ca -id fra-07 -out fra-07        # 30 days
```

In production, the CA private key never lives on a host or in the repo:

- Keep the CA key in the managed key store (the same one that holds job
  secrets; see "Persistence" in the README) and sign through it. Issuing a
  host certificate is part of host provisioning, done by the provisioning
  system, not by the host itself.
- The host generates its own key pair on first boot and keeps the key on
  local disk, readable only by hostd's user. The provisioning system sends
  a CSR for `spiffe://<trust-domain>/host/<host_id>` to the issuer. It checks
  the host's provisioning record (machine serial, expected `host_id`) before
  it signs.
- Issue short-lived host certificates (30 days or less). Short lifetimes
  replace revocation lists: to cut off a host, stop renewing it and drain it.
  To cut it off immediately, rotate the host CA without it (below).

## Rotation

### Host certificate (routine, every few weeks)

1. Before the current certificate reaches two-thirds of its lifetime, the
   renewal agent on the host gets a new certificate for the same `host_id`.
2. It writes the new key and certificate to temporary files beside the
   configured paths and `rename`s them into place: the key first, then the
   certificate.
3. hostd reads the files on every connect. It picks up the new pair on its
   next reconnect, at the latest when the control plane closes the stream as
   the old certificate expires. Running jobs keep running across a reconnect.

No restart is needed on either side.

### Control plane server certificate

Replace the files at `-hostlink-cert` and `-hostlink-key` (rename into
place). The control plane reads them again on every TLS handshake, so new
host connections get the new certificate immediately. Existing streams keep
the old session until they reconnect.

### Host CA

The control plane also reads `-hostlink-client-ca` again on every
handshake. To roll the host CA:

1. Append the new CA certificate to the client CA bundle. Both CAs are now
   trusted.
2. Reissue every host certificate from the new CA (routine host rotation).
3. Once no host presents a certificate from the old CA, remove the old CA
   from the bundle. Hosts still connected with an old-CA certificate are cut
   off when that certificate expires, or earlier on a control plane restart.

To revoke a compromised host right away, rotate the host CA without
reissuing that host. Also drain the host and remove it from the fleet.

### Server CA

Add the new CA to every host's `TACHYON_TLS_CA` bundle first. Then switch the
control plane's server certificate to one from the new CA. Finally, remove
the old CA from the hosts' bundles.

## Not covered yet

- An issuer service backed by the managed key store, and the on-host renewal
  agent. Only `tachyon hostcert` exists, for development and bootstrap.
- Pinning `Hello` fields other than `host_id` (for example region or failure
  domain) to the certificate. Today they are taken from `Hello` as sent.
