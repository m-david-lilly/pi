#!/bin/sh
# stage-vpn.sh — pre-stage Surfshark WireGuard + pbr split-tunnel, DEFAULT OFF.
#
# Idempotent. Stages ONLY the structural, no-internet, no-secret parts of the
# VPN design (docs/reference/vpn.md §4, §5, §8, §9). Deliberately does NOT:
#   - inject the WireGuard private key (use scripts/apply-wg-secret.sh on device)
#   - implement the https-dns-proxy wgvpn device-bind / dnsmasq upstream switch
#     (vpn.md §7 item 3 + §13: deferred to hardware Phase 8.6 leak-test ON PURPOSE)
#   - bring the tunnel up (needs the real endpoint + internet)
#
# After this runs, the tunnel is fully configured but disabled=1; vpn-toggle.sh
# on|off|status drives it once the real secret + endpoint are injected.
#
# Placeholders left for the operator to fill via UCI on device:
#   network.@wireguard_wgvpn[0].public_key   = <SERVER_PUBLIC_KEY>
#   network.@wireguard_wgvpn[0].endpoint_host= <xx-yyy.prod.surfshark.com>
#   network.wgvpn.private_key (via apply-wg-secret.sh, NEVER committed)
set -eu

readonly LAN_SUBNET="192.168.1.0/24"   # vpn.md §13: confirmed from network.lan
readonly POLICY_NAME="lan_via_vpn"     # MUST match vpn-toggle.sh POLICY_NAME

# --- WireGuard interface + peer (vpn.md §4) ------------------------------------
# route_allowed_ips '0' => pbr owns routing, WG does not grab the default route.
# addresses MUST carry the netmask (a bare /32 silently fails to route).
if ! uci -q get network.wgvpn >/dev/null 2>&1; then
    uci set network.wgvpn=interface
fi
uci set network.wgvpn.proto='wireguard'
uci set network.wgvpn.private_key='<WG_PRIVATE_KEY>'   # placeholder; inject on device
# `|| true`: a fresh interface has no addresses option; `uci -q delete` of an
# absent option returns nonzero (the -q only mutes output) and set -e would abort.
uci -q delete network.wgvpn.addresses || true
uci add_list network.wgvpn.addresses='10.14.0.2/16'
uci set network.wgvpn.mtu='1412'
uci set network.wgvpn.disabled='1'                     # DEFAULT OFF

# Peer: recreate cleanly so re-runs don't stack duplicate wireguard_wgvpn sections.
while uci -q delete network.@wireguard_wgvpn[0] 2>/dev/null; do :; done
uci add network wireguard_wgvpn >/dev/null
uci set network.@wireguard_wgvpn[-1].public_key='<SERVER_PUBLIC_KEY>'
uci set network.@wireguard_wgvpn[-1].endpoint_host='REPLACE.prod.surfshark.com'
uci set network.@wireguard_wgvpn[-1].endpoint_port='51820'
uci set network.@wireguard_wgvpn[-1].persistent_keepalive='25'
uci set network.@wireguard_wgvpn[-1].route_allowed_ips='0'
uci -q delete network.@wireguard_wgvpn[-1].allowed_ips || true   # absent on fresh peer
uci add_list network.@wireguard_wgvpn[-1].allowed_ips='0.0.0.0/0'
uci add_list network.@wireguard_wgvpn[-1].allowed_ips='::/0'
# NO preshared_key — Surfshark issues none; a bogus one breaks the handshake.
uci commit network

# --- Firewall: dedicated 'vpn' zone (vpn.md §5) --------------------------------
# Own zone, NOT in a WAN zone (else mwan3 would treat the tunnel as an uplink).
vpn_zone_exists=0
i=0
while uci -q get "firewall.@zone[${i}]" >/dev/null 2>&1; do
    if [ "$(uci -q get firewall.@zone[${i}].name)" = "vpn" ]; then
        vpn_zone_exists=1; break
    fi
    i=$((i + 1))
done
if [ "${vpn_zone_exists}" = 0 ]; then
    uci add firewall zone >/dev/null
    uci set firewall.@zone[-1].name='vpn'
    uci add_list firewall.@zone[-1].network='wgvpn'
    uci set firewall.@zone[-1].input='REJECT'
    uci set firewall.@zone[-1].output='ACCEPT'
    uci set firewall.@zone[-1].forward='REJECT'
    uci set firewall.@zone[-1].masq='1'        # NAT LAN clients to the tunnel addr
    uci set firewall.@zone[-1].mtu_fix='1'     # MSS clamp; prevents bulk-xfer hangs
fi
# lan -> vpn forwarding (idempotent: skip if an identical one exists)
fwd_exists=0
i=0
while uci -q get "firewall.@forwarding[${i}]" >/dev/null 2>&1; do
    if [ "$(uci -q get firewall.@forwarding[${i}].src)" = "lan" ] && \
       [ "$(uci -q get firewall.@forwarding[${i}].dest)" = "vpn" ]; then
        fwd_exists=1; break
    fi
    i=$((i + 1))
done
if [ "${fwd_exists}" = 0 ]; then
    uci add firewall forwarding >/dev/null
    uci set firewall.@forwarding[-1].src='lan'
    uci set firewall.@forwarding[-1].dest='vpn'
fi
uci commit firewall

# --- pbr: split-tunnel policy, DEFAULT OFF (vpn.md §8, §9) ----------------------
# The one mandatory knob: priority 900 (below mwan3's 2001-2254) or pbr never wins.
uci set pbr.config.enabled='0'                       # DEFAULT OFF
uci set pbr.config.uplink_ip_rules_priority='900'
uci set pbr.config.strict_enforcement='1'            # kill-switch (vpn.md §9)
# resolver_set enables domain-based dest_addr policies, but those are SILENT
# no-ops until the stock dnsmasq -> dnsmasq-full swap (vpn.md §2). The staged
# lan_via_vpn policy is src_addr-based, so this is latent-safe for now.
uci set pbr.config.resolver_set='dnsmasq.nftset'
# Reconcile pbr's uplink interface to the live WAN names. The stock value points
# at 'wan'/'wan6', which no longer exist (network uses wan1/wan2) — pbr would
# resolve a phantom uplink and the kill-switch fallback path goes undefined.
# IPv4-only: drop the v6 uplink rather than point it at a dead interface.
uci set pbr.config.uplink_interface='wan1'
uci -q delete pbr.config.uplink_interface6 || true   # may already be unset
# supported_interface safety net so pbr force-adds wgvpn if auto-detect misses it.
# An absent option makes `uci -q get` print nothing, so the grep alone covers
# the missing case — no separate existence check needed.
if ! uci -q get pbr.config.supported_interface | grep -qw wgvpn; then
    uci add_list pbr.config.supported_interface='wgvpn'
fi

# The lan_via_vpn policy vpn-toggle.sh flips. Recreate cleanly by name.
i=0
while uci -q get "pbr.@policy[${i}]" >/dev/null 2>&1; do
    if [ "$(uci -q get pbr.@policy[${i}].name)" = "${POLICY_NAME}" ]; then
        uci delete "pbr.@policy[${i}]"
        continue   # indices shift after delete; re-check same index
    fi
    i=$((i + 1))
done
uci add pbr policy >/dev/null
uci set pbr.@policy[-1].name="${POLICY_NAME}"
uci set pbr.@policy[-1].src_addr="${LAN_SUBNET}"
uci set pbr.@policy[-1].interface='wgvpn'
uci set pbr.@policy[-1].enabled='0'                  # toggle flips to 1
uci commit pbr

echo "VPN staged DEFAULT OFF. Remaining manual steps (need real Surfshark data):"
echo "  1. scripts/apply-wg-secret.sh   (inject private key from /etc/wireguard/wgvpn.secret)"
echo "  2. uci set network.@wireguard_wgvpn[0].public_key=<SERVER_PUBLIC_KEY>"
echo "  3. uci set network.@wireguard_wgvpn[0].endpoint_host=<xx-yyy.prod.surfshark.com>; uci commit network"
echo "  4. Phase 8.6 on hardware: https-dns-proxy wgvpn device-bind + dnsmasq upstream switch (vpn.md §7/§13)"
