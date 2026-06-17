#!/bin/sh
# vpn-toggle.sh on|off|status — on-demand Surfshark WireGuard toggle.
#
# Default state is OFF: full dual-WAN per-flow balancing + failover. Turning ON
# brings the tunnel up, waits for a FRESH handshake, and only then enables the
# pbr policy that steers matched traffic into the tunnel. Turning OFF reverses
# it. When the tunnel is up it rides exactly ONE WAN — no bonding; dual-WAN
# degrades to failover for tunneled traffic (docs/reference/vpn.md §10).
#
# Order matters (requirements FR-V6):
#   ON  : iface up -> wait handshake -> enable pbr   (pbr must see a live iface)
#   OFF : disable pbr -> iface down                  (kill-switch gone before iface)
#
# SAFETY: with pbr strict_enforcement=1, enabling pbr against a DEAD tunnel
# black-holes the policied LAN. So ON enables pbr ONLY after a verified fresh
# handshake; on failure it backs out and leaves VPN OFF.
#
# NOTE on `set -e`: kept as `set -eu` for fail-loud on unset vars; mutating steps
# that may legitimately fail use explicit `|| true`. (Org Bash standard
# discourages `set -e`; retained here for parity with the reviewed runbook.)
set -eu

readonly POLICY_NAME="lan_via_vpn"
readonly HANDSHAKE_WAIT=15    # seconds to wait for a fresh handshake
readonly HANDSHAKE_FRESH=180  # a handshake epoch within N seconds counts as live

# policy_index — print the @policy[] index whose name matches POLICY_NAME, or
# return 1 if none. Used so we flip the right anonymous pbr policy section.
policy_index() {
    i=0
    # Check section existence (not .name) so an unnamed sibling policy added
    # later doesn't terminate the scan early.
    while uci -q get "pbr.@policy[${i}]" >/dev/null 2>&1; do
        if [ "$(uci -q get "pbr.@policy[${i}].name" 2>/dev/null)" = "${POLICY_NAME}" ]; then
            printf '%s\n' "${i}"
            return 0
        fi
        i=$((i + 1))
    done
    return 1
}

# handshake_is_fresh — true if wgvpn shows >=1 peer with a handshake epoch within
# HANDSHAKE_FRESH of now. Counting (not awk `exit`) avoids two traps: awk `exit`
# fires on the FIRST line only, and on EMPTY input (iface never came up) awk runs
# no block and exits 0 == "success" — a dead tunnel would falsely report up. The
# freshness window rejects a stale prior-session timestamp after an OFF->ON cycle.
handshake_is_fresh() {
    now="$(date +%s)"
    count="$(wg show wgvpn latest-handshakes 2>/dev/null \
             | awk -v now="${now}" -v fresh="${HANDSHAKE_FRESH}" \
                   '$2 > 0 && (now - $2) < fresh { c++ } END { print c + 0 }')"
    [ "${count:-0}" -gt 0 ]
}

vpn_on() {
    uci set network.wgvpn.disabled='0'
    uci commit network
    ifup wgvpn

    waited=0
    handshook=0
    while [ "${waited}" -lt "${HANDSHAKE_WAIT}" ]; do
        if handshake_is_fresh; then
            handshook=1
            break
        fi
        waited=$((waited + 1))
        sleep 1
    done

    if [ "${handshook}" != 1 ]; then
        # No fresh handshake — do NOT enable pbr (strict_enforcement would
        # black-hole the policied LAN). Back out to the same clean state as a
        # normal OFF: stop/disable pbr (in case procd started it as an ifup
        # dependency), down the interface, restore the steady-state flag.
        logger -t vpn-toggle "VPN ON FAILED: no fresh handshake in ${HANDSHAKE_WAIT}s; leaving VPN off"
        /etc/init.d/pbr stop 2>/dev/null || true
        /etc/init.d/pbr disable 2>/dev/null || true
        ifdown wgvpn 2>/dev/null || true
        uci set network.wgvpn.disabled='1'
        uci commit network
        printf 'VPN handshake failed; VPN left OFF (LAN egress unaffected).\n' >&2
        return 1
    fi

    # Resolve the policy BEFORE enabling anything. A missing/renamed policy means
    # turning ON would set the service-level enable with nothing to steer — a
    # silent no-VPN. Treat it as a config error: back out and abort, don't leave
    # a half-on state.
    if ! idx="$(policy_index)"; then
        logger -t vpn-toggle "VPN ON FAILED: pbr policy '${POLICY_NAME}' not found; leaving VPN off"
        ifdown wgvpn 2>/dev/null || true
        uci set network.wgvpn.disabled='1'
        uci commit network
        printf "pbr policy '%s' not found; VPN left OFF.\n" "${POLICY_NAME}" >&2
        return 1
    fi

    # Single authority for the OFF-by-default invariant: flip BOTH the
    # service-level enable and the per-policy enable together.
    uci set pbr.config.enabled='1'
    uci set "pbr.@policy[${idx}].enabled=1"
    uci commit pbr
    /etc/init.d/pbr enable
    /etc/init.d/pbr reload

    # TODO(deploy): route the router's OWN upstream DNS through the tunnel here.
    # Per docs/reference/vpn.md §7 item 3, switch dnsmasq's upstream to the
    # wgvpn-bound https-dns-proxy so router-originated queries egress the tunnel
    # (NFR-S2). Deliberately NOT implemented blind — implement + leak-test on
    # hardware (Phase 8.6). Until then, the router's upstream DNS egresses a WAN
    # while ON (encrypted under DoH, but not tunnel-routed).

    logger -t vpn-toggle "VPN ON (pbr policy '${POLICY_NAME}' enabled)"
}

vpn_off() {
    # Tear down routing FIRST (kill-switch gone before the iface), then the iface.
    # A missing policy here is non-fatal — the service-level disable below still
    # takes effect — but warn, since it signals config drift from POLICY_NAME.
    if idx="$(policy_index)"; then
        uci set "pbr.@policy[${idx}].enabled=0"
    else
        logger -t vpn-toggle "WARNING: pbr policy '${POLICY_NAME}' not found on OFF; service-level disable only"
    fi
    uci set pbr.config.enabled='0'
    uci commit pbr
    /etc/init.d/pbr reload 2>/dev/null || true

    # TODO(deploy): restore dnsmasq's non-tunnel DNS upstream here, BEFORE the
    # ifdown below — if dnsmasq is still pointed at the (about-to-be-down)
    # wgvpn-bound proxy, default-OFF DNS fails closed (docs/reference/vpn.md §7).

    ifdown wgvpn
    uci set network.wgvpn.disabled='1'
    uci commit network
    /etc/init.d/pbr stop 2>/dev/null || true
    logger -t vpn-toggle "VPN OFF (full dual-WAN restored)"
}

vpn_status() {
    printf 'iface disabled = %s\n' "$(uci -q get network.wgvpn.disabled)"
    wg show wgvpn 2>/dev/null || printf 'wgvpn: down\n'
    /etc/init.d/pbr status 2>/dev/null || true
}

case "${1:-status}" in
    on)     vpn_on ;;
    off)    vpn_off ;;
    status) vpn_status ;;
    *)      printf 'usage: %s on|off|status\n' "$0" >&2; exit 1 ;;
esac
