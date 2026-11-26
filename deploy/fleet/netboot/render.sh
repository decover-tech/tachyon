#!/usr/bin/env bash
# Renders the netboot files for one host into OUT_DIR, ready to publish at
# NETBOOT_BASE_URL:
#   OUT_DIR/entry.ipxe                       (same for every host)
#   OUT_DIR/hosts/<MAC>/boot.ipxe            (per host)
#   OUT_DIR/hosts/<MAC>/user-data, meta-data (per host; autoinstall)
#
#   NETBOOT_BASE_URL=https://boot.example/tachyon \
#   BOOTSTRAP_URL=https://boot.example/tachyon/tachyon-host-bootstrap.sh \
#   BOOTSTRAP_SHA256=... HOST_ID=fra-01 HOST_MAC=aa-bb-cc-dd-ee-ff \
#   OS_DISK_SERIAL=... SSH_AUTHORIZED_KEY="ssh-ed25519 ..." FLEET_CONF=fra-01.conf \
#     render.sh OUT_DIR
#
# Only the substitution is tested (test/test_bootstrap.sh), not the boot.
# shellcheck disable=SC2016
set -euo pipefail

out="${1:?usage: render.sh OUT_DIR}"
here=$(cd "$(dirname "$0")" && pwd)
need=(NETBOOT_BASE_URL BOOTSTRAP_URL BOOTSTRAP_SHA256 HOST_ID HOST_MAC OS_DISK_SERIAL SSH_AUTHORIZED_KEY FLEET_CONF)
for v in "${need[@]}"; do
  [ -n "${!v:-}" ] || { echo "render: $v is required" >&2; exit 1; }
done
case "$NETBOOT_BASE_URL" in https://*) ;; *) echo "render: NETBOOT_BASE_URL must be https://" >&2; exit 1 ;; esac
case "$BOOTSTRAP_URL" in https://*) ;; *) echo "render: BOOTSTRAP_URL must be https://" >&2; exit 1 ;; esac
[[ "$HOST_MAC" =~ ^([0-9a-f]{2}-){5}[0-9a-f]{2}$ ]] || { echo "render: HOST_MAC must be lowercase aa-bb-cc-dd-ee-ff" >&2; exit 1; }
[[ "$HOST_ID" =~ ^[A-Za-z0-9._-]{1,128}$ ]] || { echo "render: bad HOST_ID" >&2; exit 1; }
[[ "$BOOTSTRAP_SHA256" =~ ^[0-9a-f]{64}$ ]] || { echo "render: BOOTSTRAP_SHA256 must be 64 hex digits" >&2; exit 1; }
[ -f "$FLEET_CONF" ] || { echo "render: $FLEET_CONF not found" >&2; exit 1; }

FLEET_CONF_INDENTED=$(sed 's/^/          /' "$FLEET_CONF")
export NETBOOT_BASE_URL BOOTSTRAP_URL BOOTSTRAP_SHA256 HOST_ID HOST_MAC OS_DISK_SERIAL SSH_AUTHORIZED_KEY FLEET_CONF_INDENTED

# Only these variables are substituted; iPXE's own ${net0/mac} stays.
vars='${NETBOOT_BASE_URL} ${BOOTSTRAP_URL} ${BOOTSTRAP_SHA256} ${HOST_ID} ${HOST_MAC} ${OS_DISK_SERIAL} ${SSH_AUTHORIZED_KEY} ${FLEET_CONF_INDENTED}'

mkdir -p "$out/hosts/$HOST_MAC"
envsubst "$vars" <"$here/entry.ipxe.tmpl" >"$out/entry.ipxe"
envsubst "$vars" <"$here/boot.ipxe.tmpl" >"$out/hosts/$HOST_MAC/boot.ipxe"
envsubst "$vars" <"$here/user-data.tmpl" >"$out/hosts/$HOST_MAC/user-data"
chmod 0600 "$out/hosts/$HOST_MAC/user-data" # it embeds the host's config
printf 'instance-id: %s\nlocal-hostname: %s\n' "$HOST_ID" "$HOST_ID" >"$out/hosts/$HOST_MAC/meta-data"
echo "render: wrote $out (publish under $NETBOOT_BASE_URL)" >&2
