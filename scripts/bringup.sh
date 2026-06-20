#!/bin/sh
# bringup.sh — one-shot post-flash bring-up for the OpenWrt Pi 5 dual-WAN router.
#
# Runs ON the Pi after flashing a custom Firmware-Selector image (the package set
# in docs/reference/hardware.md §7 + https-dns-proxy + coreutils-timeout). Applies
# the full stack — WANs, DNS/DoH, adblock, capacity weighting — idempotently.
#
# DELIBERATELY does NOT renumber the LAN. The Pi stays at 192.168.1.1. Renumbering
# the LAN to 192.168.10.1 *appeared* to strand the box (IPv4 to the new subnet
# filtered) across several attempts — but this was very likely the same Zscaler
# artifact that blocks the admin Mac's IPv4 to RFC1918 (see docs/runbooks/setup.md
# management-access gotcha), not a real Pi fault. It was never conclusively
# root-caused, so renumber is treated as KNOWN-BAD here: a downstream NAT WiFi
# router uses its OWN non-192.168.1.x LAN subnet; its WAN takes a DHCP lease from
# this Pi on 192.168.1.x. The deployed downstream is a NETGEAR Orbi MR60 (NAT mode,
# LAN 10.0.0.0/24) — no collision with the Pi's subnets.
#
# Assumes: config files staged alongside in the same dir, OR already on the Pi.
# Re-runnable: every step is idempotent (uci set / enable / reload).
set -u

LOG() { logger -t bringup "$*"; echo "[bringup] $*"; }

# --- 1. WAN device pins + interfaces (distinct metrics = dual default route) ---
LOG "configuring WAN interfaces"
uci set network.dev_wan1=device
uci set network.dev_wan1.name='uwan1'
uci set network.dev_wan1.macaddr='44:ed:57:10:00:30'
uci set network.dev_wan2=device
uci set network.dev_wan2.name='uwan2'
uci set network.dev_wan2.macaddr='00:e0:4c:68:01:1e'
uci set network.wan1=interface
uci set network.wan1.device='uwan1'
uci set network.wan1.proto='dhcp'
uci set network.wan1.peerdns='0'
uci set network.wan1.metric='10'
uci set network.wan2=interface
uci set network.wan2.device='uwan2'
uci set network.wan2.proto='dhcp'
uci set network.wan2.peerdns='0'
uci set network.wan2.metric='20'
uci commit network

# --- 2. Firewall: wan zone -> wan1/wan2 (keep zone NAME 'wan' so rules stay valid) ---
LOG "pointing firewall wan zone at wan1/wan2"
# Find the wan zone index by name (don't assume @zone[1]).
wan_zone=$(uci show firewall | sed -ne "s/^firewall\.\(@zone\[[0-9]*\]\)\.name='wan'$/\1/p" | head -1)
if [ -n "$wan_zone" ]; then
    uci -q delete "firewall.${wan_zone}.network"
    uci add_list "firewall.${wan_zone}.network=wan1"
    uci add_list "firewall.${wan_zone}.network=wan2"
    uci commit firewall
else
    LOG "WARN: could not find a firewall zone named 'wan' — check manually"
fi

# --- 3. NTP: IP-literal servers FIRST (cold-boot clock/DoH deadlock fix) ---
LOG "setting IP-literal NTP servers ahead of pool hostnames"
uci -q delete system.ntp.server
uci add_list system.ntp.server='162.159.200.123'   # Cloudflare
uci add_list system.ntp.server='162.159.200.1'     # Cloudflare
uci add_list system.ntp.server='216.239.35.0'      # Google
uci add_list system.ntp.server='0.openwrt.pool.ntp.org'
uci add_list system.ntp.server='1.openwrt.pool.ntp.org'
uci commit system

# --- 4. DNS: enable the pre-baked DoH proxy (auto-wires dnsmasq + force-DNS + DoT block) ---
LOG "enabling https-dns-proxy"
/etc/init.d/https-dns-proxy enable
/etc/init.d/https-dns-proxy restart

# --- 5. adblock: feeds + WAN-ifup trigger (skips the empty-list boot run) ---
LOG "configuring adblock"
uci set adblock.global.adb_enabled='1'
uci set adblock.global.adb_dns='dnsmasq'
uci set adblock.global.adb_nftforce='1'
uci set adblock.global.adb_feed='oisd_big certpl hagezi'
uci set adblock.global.adb_trigger='wan1 wan2'
uci set adblock.global.adb_triggerdelay='20'
uci commit adblock

# --- 6. mwan3: deploy config (must already be at /etc/config/mwan3) + enable ---
LOG "enabling mwan3"
/etc/init.d/mwan3 enable

# --- 7. crons: capacity reweight every 30 min + daily adblock refresh ---
LOG "installing crons"
touch /etc/crontabs/root
grep -q '/usr/bin/wan-weight.sh' /etc/crontabs/root || \
    echo '*/30 * * * * /usr/bin/wan-weight.sh' >> /etc/crontabs/root
grep -q '/etc/init.d/adblock reload' /etc/crontabs/root || \
    echo '0 5 * * * /etc/init.d/adblock reload' >> /etc/crontabs/root
/etc/init.d/cron enable

# --- 8. apply network state (still at 192.168.1.1 — mgmt link safe) ---
LOG "reloading network + firewall"
/etc/init.d/network reload
/etc/init.d/firewall reload

LOG "bring-up config applied. Reboot to let the hotplug rule rename uwan1/uwan2,"
LOG "then run: mwan3 restart; /etc/init.d/adblock reload; /usr/bin/wan-weight.sh"
LOG "DONE"
