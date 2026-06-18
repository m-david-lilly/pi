#!/bin/sh
# apply-wg-secret.sh — inject the Surfshark WireGuard private key into UCI from an
# untracked, root-only secrets file (docs/reference/vpn.md §3.1 Pattern A).
#
# The private key is the one true secret in this design. It MUST NOT live in git
# or in the tracked /etc/config/network. This script reads it from a chmod-600
# file on the device and writes it to UCI at provisioning time. It never echoes
# the key and never writes it to syslog.
#
# Run ONCE on the router after stage-vpn.sh, and again only when rotating keys.
set -eu

readonly SECRET_FILE="/etc/wireguard/wgvpn.secret"

if [ ! -f "${SECRET_FILE}" ]; then
    printf 'error: %s not found.\n' "${SECRET_FILE}" >&2
    printf 'Create it (chmod 600, root:root), containing exactly:\n' >&2
    printf "  WG_PRIVATE_KEY='<paste-real-private-key-here>'\n" >&2
    exit 1
fi

# Refuse a world/group-readable secret — fail closed rather than inject from a
# loosely-permissioned file. BusyBox stat supports -c '%a'.
perms="$(stat -c '%a' "${SECRET_FILE}" 2>/dev/null || echo '')"
case "${perms}" in
    600|400) : ;;
    *) printf 'error: %s perms are %s; must be 600 or 400 (chmod 600 it).\n' \
            "${SECRET_FILE}" "${perms:-unknown}" >&2
       exit 1 ;;
esac

# Refuse a non-root-owned secret. This file is sourced as root (arbitrary code
# runs with root privilege), so a 600 file owned by another user would let that
# user execute code as root. Require root:root ownership before sourcing.
owner="$(stat -c '%U' "${SECRET_FILE}" 2>/dev/null || echo '')"
if [ "${owner}" != 'root' ]; then
    printf 'error: %s is owned by %s; must be root (chown root %s).\n' \
        "${SECRET_FILE}" "${owner:-unknown}" "${SECRET_FILE}" >&2
    exit 1
fi

# Source in a subshell-safe way; the file defines WG_PRIVATE_KEY only.
# shellcheck disable=SC1090
. "${SECRET_FILE}"

if [ -z "${WG_PRIVATE_KEY:-}" ]; then
    printf 'error: WG_PRIVATE_KEY is empty/unset in %s\n' "${SECRET_FILE}" >&2
    exit 1
fi

if ! uci -q get network.wgvpn >/dev/null 2>&1; then
    printf 'error: network.wgvpn interface missing; run stage-vpn.sh first.\n' >&2
    exit 1
fi

uci set network.wgvpn.private_key="${WG_PRIVATE_KEY}"
uci commit network
# Do NOT echo the key. Confirm only that a non-placeholder value is now set.
if [ "$(uci -q get network.wgvpn.private_key)" = '<WG_PRIVATE_KEY>' ]; then
    printf 'error: private_key still the placeholder after set.\n' >&2
    exit 1
fi
logger -t apply-wg-secret 'WireGuard private key injected into UCI (value not logged)'
printf 'WireGuard private key injected. (Value intentionally not printed.)\n'
