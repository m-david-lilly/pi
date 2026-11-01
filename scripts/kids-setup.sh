#!/bin/sh
# kids-setup.sh — idempotent setup of the optional kids network.
#
# Run ON the Pi, from the repo copy:
#   SRC=/tmp/pi ./scripts/kids-setup.sh --email you@example.com
#   SRC=/tmp/pi ./scripts/kids-setup.sh --ca /tmp/pi/ca   # install an existing CA
#
# Installs kidproxy + helpers, installs the signing CA (--ca) or creates one if
# missing, creates the WiFi key if missing, applies network/DHCP/firewall/wireless
# UCI, and enables the services and cron jobs. Does NOT touch the adult LAN, the
# WANs, mwan3, or uhttpd's certificate.
set -u

EMAIL=""
CA_SRC=""
while [ $# -gt 0 ]; do
	case "$1" in
	--email) EMAIL="${2:-}"; shift 2 ;;
	--src) SRC="${2:-}"; shift 2 ;;
	--ca) CA_SRC="${2:-}"; shift 2 ;;
	*) shift ;;
	esac
done

# Default to the repo root (this script lives in <repo>/scripts).
SRC=${SRC:-$(cd "$(dirname "$0")/.." && pwd)}
[ -f /etc/openwrt_release ] || { echo "run this on the router" >&2; exit 1; }

log() { logger -t kids-setup "$*"; echo "[kids-setup] $*"; }
die() { echo "[kids-setup] ERROR: $*" >&2; exit 1; }

# put SRC DST MODE. On a re-run the source falls back to the installed copy, so
# SRC may be DST itself (cp refuses "same file") or absent while DST exists.
put() {
	if [ "$1" = "$2" ] || { [ ! -f "$1" ] && [ -f "$2" ]; }; then
		chmod "$3" "$2" || die "cannot chmod $2"
		return 0
	fi
	cp "$1" "$2" && chmod "$3" "$2" || die "cannot install $1"
}

# ---- 1. source files -------------------------------------------------------
BIN="$SRC/proxy/dist/kidproxy"
CFGDIR="$SRC/config/kidproxy"
SCRIPTS="$SRC/scripts"
[ -f "$BIN" ] || BIN=/usr/bin/kidproxy
[ -d "$CFGDIR" ] || CFGDIR=/etc/kidproxy
[ -d "$SCRIPTS" ] || SCRIPTS=/usr/bin
[ -x "$BIN" ] || [ -f "$BIN" ] || die "kidproxy binary not found (build it with scripts/build-kidproxy.ps1)"

log "installing binaries and helpers"
put "$BIN" /usr/bin/kidproxy 0755
put "$SCRIPTS/kids-nft.sh" /usr/bin/kids-nft.sh 0755
put "$SCRIPTS/kids-toggle.sh" /usr/bin/kids-toggle.sh 0755
put "$SCRIPTS/kids-watchdog.sh" /usr/bin/kids-watchdog.sh 0755

mkdir -p /etc/kidproxy
chmod 700 /etc/kidproxy
[ -f /etc/kidproxy/config.yaml ] || put "$CFGDIR/config.yaml" /etc/kidproxy/config.yaml 0600
put "$CFGDIR/kids.nft" /etc/kidproxy/kids.nft 0644
put "$CFGDIR/kidproxy.init" /etc/init.d/kidproxy 0755
put "$CFGDIR/kidproxy-nft.init" /etc/init.d/kidproxy-nft 0755

# ---- 2. packages -----------------------------------------------------------
for pkg in msmtp; do
	apk info -e "$pkg" >/dev/null 2>&1 || {
		log "installing $pkg"
		apk add "$pkg" >/dev/null 2>&1 || log "WARN: could not install $pkg (offline?) — email alerts will not work"
	}
done

# ---- 3. CA + WiFi key ------------------------------------------------------
# An existing CA can be installed with --ca (e.g. the Pi Home CA from
# .claude/.secrets/ca/), which is what the live router uses so one root covers
# both the dashboard cert and interception. Without --ca a fresh "Kids Filter CA"
# is generated the first time only.
if [ -n "$CA_SRC" ] && [ -f "$CA_SRC/ca.crt" ] && [ -f "$CA_SRC/ca.key" ]; then
	if [ -f /etc/kidproxy/ca.key ] && ! cmp -s "$CA_SRC/ca.key" /etc/kidproxy/ca.key; then
		log "replacing the signing CA with $CA_SRC (old copy kept as *.bak)"
		cp -p /etc/kidproxy/ca.crt /etc/kidproxy/ca.crt.bak 2>/dev/null
		cp -p /etc/kidproxy/ca.key /etc/kidproxy/ca.key.bak 2>/dev/null
		chmod 600 /etc/kidproxy/ca.key.bak 2>/dev/null
	fi
	put "$CA_SRC/ca.crt" /etc/kidproxy/ca.crt 0644
	put "$CA_SRC/ca.key" /etc/kidproxy/ca.key 0600
	log "signing CA installed from $CA_SRC"
elif [ ! -f /etc/kidproxy/ca.key ]; then
	log "creating Kids Filter CA"
	/usr/bin/kidproxy ca init --dir /etc/kidproxy || die "CA init failed"
else
	log "CA already exists; leaving it alone"
fi

if [ ! -f /etc/kidproxy/wifi.key ]; then
	dd if=/dev/urandom bs=16 count=1 2>/dev/null | md5sum | cut -c1-20 > /etc/kidproxy/wifi.key
	chmod 600 /etc/kidproxy/wifi.key
fi
WIFI_KEY=$(cat /etc/kidproxy/wifi.key)

# ---- 4. email --------------------------------------------------------------
if [ -n "$EMAIL" ]; then
	sed -i "s|^    enabled: false|    enabled: true|" /etc/kidproxy/config.yaml
	sed -i "s|^    to: \[\]|    to: [\"$EMAIL\"]|" /etc/kidproxy/config.yaml
	log "email alerts enabled for $EMAIL (configure /etc/msmtprc first)"
fi

# ---- 5. network + DHCP -----------------------------------------------------
log "configuring br-kids / 192.168.30.0/24"
uci -q get network.brkids >/dev/null || uci set network.brkids=device
uci set network.brkids.name='br-kids'
uci set network.brkids.type='bridge'
# The bridge has no ports until the wifi iface is attached; without
# bridge_empty netifd refuses to create it at boot and the interface ends up
# DISABLED (DEVICE_CLAIM_FAILED). force_link keeps it up before carrier.
uci set network.brkids.bridge_empty='1'
uci -q get network.kids >/dev/null || uci set network.kids=interface
uci set network.kids.device='br-kids'
uci set network.kids.proto='static'
uci set network.kids.force_link='1'
uci -q delete network.kids.ipaddr
uci add_list network.kids.ipaddr='192.168.30.1/24'
uci -q delete network.kids.ip6assign

uci set dhcp.kids=dhcp
uci set dhcp.kids.interface='kids'
uci set dhcp.kids.start='100'
uci set dhcp.kids.limit='100'
uci set dhcp.kids.leasetime='12h'
uci set dhcp.kids.dhcpv4='server'
uci set dhcp.kids.dhcpv6='disabled'
uci set dhcp.kids.ra='disabled'
uci set dhcp.kids.ndp='disabled'
uci set dhcp.kids.force='1'
uci -q delete dhcp.kids.dhcp_option
uci add_list dhcp.kids.dhcp_option='3,192.168.30.1'
uci add_list dhcp.kids.dhcp_option='6,192.168.30.1'
# The CA install page also answers to config.yaml's gateway_names; without a
# local record those names do not resolve.
for name in setup.kids kidproxy.lan; do
	uci -q del_list "dhcp.@dnsmasq[0].address=/$name/192.168.30.1"
	uci add_list "dhcp.@dnsmasq[0].address=/$name/192.168.30.1"
done
uci commit network
uci commit dhcp

# ---- 6. wireless (kids-only SSID on the onboard radio) ---------------------
if uci -q get wireless.radio0 >/dev/null; then
	# The world (00) regdomain marks 5 GHz ch36 no-IR, so the AP will not start
	# without a country. Keep an existing one; set yours if you are not in the US.
	uci -q get wireless.radio0.country >/dev/null || uci set wireless.radio0.country='US'
	uci set wireless.radio0.band='5g'
	uci set wireless.radio0.channel='36'
	uci set wireless.radio0.htmode='VHT80'
	uci set wireless.radio0.disabled='0'
fi
uci set wireless.kids=wifi-iface
uci set wireless.kids.device='radio0'
uci set wireless.kids.network='kids'
uci set wireless.kids.mode='ap'
uci set wireless.kids.ssid='Home-Kids'
uci set wireless.kids.encryption='sae-mixed'
uci set wireless.kids.key="$WIFI_KEY"
uci set wireless.kids.isolate='1'
uci set wireless.kids.disabled='0'
uci commit wireless

# ---- 7. firewall zone (isolation: only DNS, proxy, ICMP reach the router) --
log "configuring kids firewall zone"
uci set firewall.kids=zone
uci set firewall.kids.name='kids'
uci set firewall.kids.network='kids'
uci set firewall.kids.input='REJECT'
uci set firewall.kids.output='ACCEPT'
uci set firewall.kids.forward='REJECT'
uci set firewall.kids.masq='1'
uci set firewall.kids.mtu_fix='1'

uci set firewall.kids_wan=forwarding
uci set firewall.kids_wan.src='kids'
uci set firewall.kids_wan.dest='wan'

# DHCP arrives from 0.0.0.0 before the client has a lease, and the zone input
# policy is REJECT, so it needs its own rule.
uci set firewall.kids_dhcp=rule
uci set firewall.kids_dhcp.name='Kids-DHCP'
uci set firewall.kids_dhcp.src='kids'
uci set firewall.kids_dhcp.proto='udp'
uci set firewall.kids_dhcp.src_port='68'
uci set firewall.kids_dhcp.dest_port='67'
uci set firewall.kids_dhcp.target='ACCEPT'

uci set firewall.kids_dns=rule
uci set firewall.kids_dns.name='Kids-DNS'
uci set firewall.kids_dns.src='kids'
uci set firewall.kids_dns.proto='tcp udp'
uci set firewall.kids_dns.dest_port='53'
uci set firewall.kids_dns.target='ACCEPT'

uci set firewall.kids_proxy=rule
uci set firewall.kids_proxy.name='Kids-Proxy'
uci set firewall.kids_proxy.src='kids'
uci set firewall.kids_proxy.proto='tcp'
uci set firewall.kids_proxy.dest_port='8080'
uci set firewall.kids_proxy.target='ACCEPT'

uci set firewall.kids_ping=rule
uci set firewall.kids_ping.name='Kids-Ping'
uci set firewall.kids_ping.src='kids'
uci set firewall.kids_ping.proto='icmp'
uci set firewall.kids_ping.icmp_type='echo-request'
uci set firewall.kids_ping.target='ACCEPT'

# DNS-filter baseline enforced by fw4 itself, so it holds while the kidproxy
# nft table is flushed (pause, toggle, failed load). The live box may hold these
# as anonymous sections, so skip any rule that already exists by name.
fw_has() { uci show firewall 2>/dev/null | grep -q "\.name='$1'\$"; }
if ! fw_has Kids-Force-DNS; then
	uci set firewall.kids_force_dns=redirect
	uci set firewall.kids_force_dns.name='Kids-Force-DNS'
	uci set firewall.kids_force_dns.src='kids'
	uci set firewall.kids_force_dns.src_dport='53'
	uci set firewall.kids_force_dns.dest_port='53'
	uci set firewall.kids_force_dns.proto='tcp udp'
	uci set firewall.kids_force_dns.target='DNAT'
fi
if ! fw_has Kids-Block-DoT; then
	uci set firewall.kids_block_dot=rule
	uci set firewall.kids_block_dot.name='Kids-Block-DoT'
	uci set firewall.kids_block_dot.src='kids'
	uci set firewall.kids_block_dot.dest='wan'
	uci set firewall.kids_block_dot.proto='tcp udp'
	uci set firewall.kids_block_dot.dest_port='853'
	uci set firewall.kids_block_dot.target='REJECT'
fi
if ! fw_has Kids-Block-VPN-TCP; then
	uci set firewall.kids_block_vpn_tcp=rule
	uci set firewall.kids_block_vpn_tcp.name='Kids-Block-VPN-TCP'
	uci set firewall.kids_block_vpn_tcp.src='kids'
	uci set firewall.kids_block_vpn_tcp.dest='wan'
	uci set firewall.kids_block_vpn_tcp.proto='tcp'
	uci set firewall.kids_block_vpn_tcp.dest_port='1194 1723 1701 5000 9001 9030 9050 9051'
	uci set firewall.kids_block_vpn_tcp.target='REJECT'
fi
if ! fw_has Kids-Block-VPN-UDP; then
	uci set firewall.kids_block_vpn_udp=rule
	uci set firewall.kids_block_vpn_udp.name='Kids-Block-VPN-UDP'
	uci set firewall.kids_block_vpn_udp.src='kids'
	uci set firewall.kids_block_vpn_udp.dest='wan'
	uci set firewall.kids_block_vpn_udp.proto='udp'
	uci set firewall.kids_block_vpn_udp.dest_port='1194 1701 4500 51820'
	uci set firewall.kids_block_vpn_udp.target='REJECT'
fi
uci commit firewall

# ---- 8. apply + start ------------------------------------------------------
log "reloading network, firewall, wireless"
/etc/init.d/network reload
sleep 2
/etc/init.d/dnsmasq reload
/etc/init.d/firewall reload
wifi reload 2>/dev/null || wifi up

# First-time setup can race two concurrent wifi-scripts starts and leave the
# brand-new br-kids administratively down (DEVICE_CLAIM_FAILED). Wait for the
# address and nudge the interface a few times before giving up.
i=0
while [ $i -lt 6 ]; do
	sleep 5
	ip -4 addr show br-kids 2>/dev/null | grep -q '192.168.30.1' && break
	i=$((i + 1))
	log "br-kids not up yet (attempt $i); nudging wireless + ifup"
	wifi reload 2>/dev/null || wifi up
	ifup kids 2>/dev/null
done
if ! ip -4 addr show br-kids 2>/dev/null | grep -q '192.168.30.1'; then
	log "WARN: br-kids has no address yet — reboot once to let netifd settle"
fi

/etc/init.d/kidproxy enable
/etc/init.d/kidproxy-nft enable
/usr/bin/kidproxy check --config /etc/kidproxy/config.yaml || die "config check failed"
/etc/init.d/kidproxy restart
sleep 1
/usr/bin/kids-nft.sh apply

# ---- 9. cron ---------------------------------------------------------------
touch /etc/crontabs/root
grep -q '/usr/bin/kids-watchdog.sh' /etc/crontabs/root || \
	echo '*/5 * * * * /usr/bin/kids-watchdog.sh' >> /etc/crontabs/root
grep -q 'kids-toggle.sh resume-if-due' /etc/crontabs/root || \
	echo '* * * * * /usr/bin/kids-toggle.sh resume-if-due' >> /etc/crontabs/root
/etc/init.d/cron enable
/etc/init.d/cron restart

# ---- 10. summary -----------------------------------------------------------
echo
log "done"
echo "  SSID:        Home-Kids"
echo "  passphrase:  $WIFI_KEY   (stored at /etc/kidproxy/wifi.key)"
echo "  gateway:     192.168.30.1"
echo "  CA cert:     /etc/kidproxy/ca.crt"
echo "  CA install:  open http://192.168.30.1/ on a kids device"
echo "  dashboard:   /admin.html -> Kids panel"
echo
echo "Email: configure /etc/msmtprc, then test with:"
echo "  /usr/bin/kidproxy notify-test --config /etc/kidproxy/config.yaml"
