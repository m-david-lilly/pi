# Setup Runbook: Raspberry Pi 5 OpenWrt Multi-WAN Router

End-to-end build guide from a bare Raspberry Pi 5 to a working dual-WAN router with
DNS filtering and an on-demand Surfshark WireGuard VPN.

## Conventions

- **Placeholders** look like `<THIS>`. Replace every one before running the command.
  Never commit real secrets (private keys, auth tokens) to git.
- Most commands run **on the router** over SSH (`ssh root@192.168.1.1`) unless a step
  says "on your workstation".
- The Pi 5 is **bcm27xx / bcm2712, aarch64 (ARMv8-A Cortex-A76)** — NOT ARMv7. Using an
  armv7 / bcm2709 / bcm2710 image will not boot.
- This build uses OpenWrt **fw4 (nftables)**, the default on 22.03+/23.05/24.10/25.12.

### Topology used throughout

| Role | NIC | Device (typical) | Notes |
| --- | --- | --- | --- |
| WAN1 | onboard GbE | `eth0` | RP1 dedicated lane, fastest path. Primary uplink. |
| WAN2 | USB3 GbE adapter | `eth1` | Second uplink. Use a USB3 (blue) port. |
| LAN | USB3 GbE adapter | `eth2` | To a downstream switch. Use a USB3 (blue) port. |

> Device names (`eth1`/`eth2`) are NOT guaranteed stable across reboots for USB NICs.
> Phase 2 pins them by MAC so mwan3 member-to-WAN mapping never drifts.

### Mark / priority allocation (so subsystems do not collide)

| Subsystem | Mark / mask | ip-rule priority band |
| --- | --- | --- |
| mwan3 | mask `0x3F00` | 1001-1250 (inbound), 2001-2254 (outbound) |
| pbr (VPN) | `uplink_mark 0x00010000`, mask `0x00ff0000` | 900 (set explicitly, below mwan3) |

Do not widen either mask — they are non-overlapping by design.

---

## Phase 1 — Flash OpenWrt aarch64 and first boot

### 1.1 Obtain the image (on your workstation)

Use the current stable release, squashfs flavor, target `bcm27xx/bcm2712`, device `rpi-5`.
Optionally pre-bake packages via the [Firmware Selector](https://firmware-selector.openwrt.org/?target=bcm27xx%2Fbcm2712&id=rpi-5).

```bash
# Replace <VER> with the current stable, e.g. 25.12.4
VER="<VER>"
BASE="https://downloads.openwrt.org/releases/${VER}/targets/bcm27xx/bcm2712"
IMG="openwrt-${VER}-bcm27xx-bcm2712-rpi-5-squashfs-factory.img.gz"
curl -fLO "${BASE}/${IMG}"
curl -fLO "${BASE}/sha256sums"
# Verify integrity
grep "${IMG}" sha256sums | sha256sum -c -
```

**Expected:** `... OK` from `sha256sum -c`.

> Prefer **squashfs** (read-only root + overlay, supports failsafe/factory-reset) over ext4.
> Prefer **NVMe (M.2 HAT+) or a USB3 SSD** over an SD card for an always-on router —
> adblock list churn and logging wear SD cards.

### 1.2 Flash to the boot device (on your workstation)

Easiest: Raspberry Pi Imager → "Use custom" → select the `.img.gz`. Or via `dd`:

```bash
gzip -dk "${IMG}"                       # -> openwrt-...-factory.img
# VERIFY the target device first! Wrong device = data loss.
lsblk
sudo dd if=openwrt-${VER}-bcm27xx-bcm2712-rpi-5-squashfs-factory.img \
        of=/dev/<DISK> bs=2M conv=fsync status=progress
sync
```

**Expected:** `dd` reports bytes written; `sync` returns cleanly.

### 1.3 First boot

1. Insert the SD/SSD/NVMe, connect a cable from your workstation to the **onboard
   Ethernet** port (`eth0`), power the Pi with the **official 27W (5V/5A) USB-C PSU**.
2. OpenWrt's default LAN is `192.168.1.1/24` with a DHCP server. Your workstation should
   get a `192.168.1.x` lease.

```bash
ping -c2 192.168.1.1
ssh root@192.168.1.1          # no password on first boot
```

**Expected:** ping replies; SSH drops you at the OpenWrt `root@OpenWrt:~#` prompt.

**Verify:**

```bash
ubus call system board       # confirm board "Raspberry Pi 5", target bcm27xx/bcm2712
cat /etc/openwrt_release      # confirm DISTRIB_ARCH='aarch64_cortex-a76'
```

### 1.4 Set the root password immediately

```bash
passwd
```

**Verify:** log out, `ssh root@192.168.1.1` now prompts for the password.

> Under-powering note: a non-PD or 3A supply caps total USB peripheral current to ~600mA
> and browns out bus-powered USB NICs, producing intermittent link flaps that look like
> mwan3 marking a WAN down. Use the 27W PSU or a powered USB3 hub.

---

## Phase 2 — USB NICs: identify, install drivers, pin by MAC

### 2.1 Get internet to the router for package installs

Temporarily, the onboard `eth0` is LAN. To install packages, either (a) briefly attach an
uplink, or (b) if you pre-baked drivers in Phase 1.1, skip ahead. Simplest bootstrap:
plug an internet uplink into a USB NIC after drivers are installed (chicken-and-egg).
If drivers are NOT pre-baked, temporarily reconfigure `eth0` as a DHCP WAN client:

```bash
# Temporary: make onboard eth0 a DHCP client to reach the internet for opkg.
uci set network.wan='interface'
uci set network.wan.device='eth0'
uci set network.wan.proto='dhcp'
uci delete network.lan.device 2>/dev/null   # free eth0 from lan temporarily
uci commit network
/etc/init.d/network restart
```

**Expected:** `ip addr show eth0` shows a DHCP-assigned address; `ping -c2 1.1.1.1` works.

> This is a temporary bootstrap. Phase 3 restores the final topology.

### 2.2 Install USB-Ethernet kmod drivers

Identify the chipset first if the adapters are plugged in:

```bash
opkg update
lsusb
dmesg | grep -iE 'rtl815|ax881|usbnet|eth'
```

Install the driver matching your adapters (RTL8153 is the default recommendation):

```bash
# Realtek RTL8152/RTL8153 (most common: UGREEN, Anker, TP-Link UE300)
opkg install kmod-usb-net-rtl8152

# OR ASIX AX88179 / AX88178A
# opkg install kmod-usb-net-asix-ax88179
```

**Expected:** package installs and pulls firmware (e.g. `r8152-firmware`).

### 2.3 Bring up and identify the USB NICs

Plug both USB3 adapters into the **USB3 (blue)** ports, then:

```bash
ip link            # list all NICs
dmesg | tail -30   # watch enumeration order
```

Record the MAC of each device so you can map them deterministically:

```bash
for d in eth0 eth1 eth2; do
  echo -n "$d -> "; cat /sys/class/net/$d/address 2>/dev/null
done
```

**Expected:** three MACs printed. Note which physical adapter is which (label them).

### 2.4 Pin USB NICs by MAC

USB enumeration order is not guaranteed across reboots, which can swap `eth1`/`eth2`.
Pin a stable name to each MAC so mwan3 members never track the wrong uplink.

```bash
# Replace each <MAC_*> with the real address from step 2.3.
# This renames the physical devices to stable aliases at boot.
cat > /etc/config/wan_pin <<'EOF'
EOF

# Use named device sections in /etc/config/network pinned to MAC.
uci set network.dev_wan2='device'
uci set network.dev_wan2.name='wan2dev'
uci set network.dev_wan2.macaddr='<MAC_WAN2>'

uci set network.dev_lan='device'
uci set network.dev_lan.name='landev'
uci set network.dev_lan.macaddr='<MAC_LAN>'

uci commit network
/etc/init.d/network restart
```

**Verify:**

```bash
ip link show wan2dev   # exists, MAC matches <MAC_WAN2>
ip link show landev    # exists, MAC matches <MAC_LAN>
```

> From here on, refer to the WAN2 USB device as `wan2dev`, the LAN USB device as `landev`,
> and the onboard NIC as `eth0`. This keeps the config stable across reboots.
>
> **Naming is load-bearing, not illustrative.** Every later phase that touches the USB NICs
> hard-codes these exact names (Phase 3.1 `network.lan.device`/`network.wanb.device`,
> Phase 8.2 and 8.7 `ip link show`). The pinned names `wan2dev`/`landev` are the canonical
> scheme for this build — they MUST match wherever referenced. The sibling reference docs
> use different example names (`hardware.md` §4 shows `wan2`/`lan_usb`; `load-balancing.md`
> §2 overrides the kernel names `eth1`/`eth2` directly); those are illustrative only. If you
> follow a reference doc first, rename to `wan2dev`/`landev` here or the later phases break.

---

## Phase 3 — Base LAN / DHCP / DNS (final topology)

### 3.1 Define the final interfaces

```bash
# LAN: the USB NIC to your switch.
uci set network.lan='interface'
uci set network.lan.device='landev'
uci set network.lan.proto='static'
uci set network.lan.ipaddr='192.168.1.1'
uci set network.lan.netmask='255.255.255.0'

# WAN1: onboard GbE (fastest path).
uci set network.wan='interface'
uci set network.wan.device='eth0'
uci set network.wan.proto='dhcp'
# Don't accept upstream DNS — we run our own resolver (adblock). Prevents DNS leaks later.
uci set network.wan.peerdns='0'

# WAN2: USB3 GbE.
uci set network.wanb='interface'
uci set network.wanb.device='wan2dev'
uci set network.wanb.proto='dhcp'
uci set network.wanb.peerdns='0'

uci commit network
/etc/init.d/network restart
```

**Expected:** `wan` and `wanb` each obtain a DHCP lease from their respective modems.

### 3.2 Firewall zones (one WAN-type zone per uplink)

```bash
# wan zone covers both physical WANs; masq + MSS clamp.
uci set firewall.@zone[1].network='wan wanb'   # @zone[1] is the default 'wan' zone
uci set firewall.@zone[1].masq='1'
uci set firewall.@zone[1].mtu_fix='1'
uci commit firewall
/etc/init.d/firewall restart
```

**Verify:**

```bash
nft list ruleset | grep -iE 'masquerade|oifname'   # NAT present on wan devices
```

### 3.3 DHCP + DNS on LAN (dnsmasq)

dnsmasq is preinstalled. Adblock (Phase 5) plugs into it. For VPN domain policies later,
swap stock dnsmasq for `dnsmasq-full` now to avoid a reinstall:

```bash
opkg update
opkg install dnsmasq-full      # conflicts with dnsmasq; opkg handles the swap. Skip if domain-based VPN policies are not needed.
/etc/init.d/dnsmasq restart
```

**Verify:**

```bash
# From a LAN client (or the router itself):
nslookup openwrt.org 192.168.1.1     # resolves
ubus call dhcp ipv4leases            # leases being handed out
```

### 3.4 LuCI web UI (if not pre-baked)

```bash
opkg install luci
/etc/init.d/uhttpd restart
```

**Verify:** browse to `http://192.168.1.1` and log in as `root`.

---

## Phase 4 — mwan3: two-WAN weighted + sticky, with failover

mwan3 does **per-flow** load balancing (connmark + ip rules + conntrack). A single TCP
connection rides ONE WAN for its lifetime — this is intrinsic, not optional. mwan3 does
**NOT** bond bandwidth: a single download uses one WAN, so `100+100` gives ~100 Mbps
single-stream, not 200.

### 4.1 Install

```bash
opkg update
opkg install mwan3 luci-app-mwan3
# fw4/nftables systems still need the iptables-nft compatibility shim for mwan3:
opkg install iptables-nft
```

**Verify:**

```bash
mwan3 status      # runs; interfaces listed (may be offline until configured)
```

### 4.2 Configure interfaces (liveness), members, policy, rule

```bash
# --- globals: keep default mark mask, do not collide with pbr ---
uci set mwan3.globals.mmx_mask='0x3F00'

# --- interface tracking (liveness/failover; this owns up/down, NOT the speed test) ---
for IF in wan wanb; do
  uci set mwan3.$IF='interface'
  uci set mwan3.$IF.enabled='1'
  uci set mwan3.$IF.family='ipv4'
  uci delete mwan3.$IF.track_ip 2>/dev/null
  uci add_list mwan3.$IF.track_ip='1.1.1.1'
  uci add_list mwan3.$IF.track_ip='8.8.8.8'
  uci set mwan3.$IF.track_method='ping'
  uci set mwan3.$IF.reliability='2'     # <= number of track_ip entries, else IF never comes up
  uci set mwan3.$IF.count='1'
  uci set mwan3.$IF.timeout='4'
  uci set mwan3.$IF.interval='10'
  uci set mwan3.$IF.down='3'
  uci set mwan3.$IF.up='3'
  # Flush stale flows pinned to a dead WAN so they re-balance to the survivor on failover.
  uci add_list mwan3.$IF.flush_conntrack='ifdown'
  uci add_list mwan3.$IF.flush_conntrack='disconnected'
done

# --- members: SAME metric => active-active load balance; weight = traffic ratio ---
# Initial weights are 1/1; the Phase 6 healthcheck rewrites them from measured Mbps.
uci set mwan3.wan_m1_w1='member'
uci set mwan3.wan_m1_w1.interface='wan'
uci set mwan3.wan_m1_w1.metric='1'
uci set mwan3.wan_m1_w1.weight='1'

uci set mwan3.wanb_m1_w1='member'
uci set mwan3.wanb_m1_w1.interface='wanb'
uci set mwan3.wanb_m1_w1.metric='1'
uci set mwan3.wanb_m1_w1.weight='1'

# --- policy: balance across both; if both down, drop ---
uci set mwan3.balanced='policy'
uci add_list mwan3.balanced.use_member='wan_m1_w1'
uci add_list mwan3.balanced.use_member='wanb_m1_w1'
uci set mwan3.balanced.last_resort='unreachable'

# --- catch-all rule: all traffic uses the balanced policy ---
# Per-connection stickiness (a TCP flow rides ONE WAN for its life) is already guaranteed
# by conntrack and is NOT controlled by this flag. Rule-level sticky '1' is an EXTRA
# source-IP affinity layer that pins a client's *subsequent* connections to the same WAN
# within timeout (helps banking/HTTPS that dislike mid-session IP changes).
# Shipped OFF by default per FR-W8 ("Could … Off by default") and load-balancing §3.1.
# Open Question 5 leaves this to operator preference: to enable, set sticky '1' and add a
# timeout (e.g. 600). The 'timeout' option has no effect while sticky is '0'.
uci set mwan3.default_rule='rule'
uci set mwan3.default_rule.dest_ip='0.0.0.0/0'
uci set mwan3.default_rule.proto='all'
uci set mwan3.default_rule.use_policy='balanced'
uci set mwan3.default_rule.sticky='0'

uci commit mwan3
mwan3 restart
```

**Verify:**

```bash
mwan3 interfaces      # wan + wanb show "online" with tracking ok
mwan3 policies        # 'balanced' shows computed % split (50/50 at weight 1/1)
mwan3 rules           # default_rule listed
# Confirm conntrack mark mechanism is live:
nft list ruleset | grep -i mark | head
```

> **Pitfall:** `reliability` must be `<=` the number of `track_ip` entries or the interface
> never comes up. **Pitfall:** track_ip targets must be reachable *only* via the WAN under
> test for failover detection to be reliable. **Pitfall:** `weight` matters only among
> members of the SAME metric; differing metrics make the lower-metric member take ALL
> traffic (pure standby).

---

## Phase 5 — adblock: blocklists + DNS hardening

adblock returns NXDOMAIN for blocked domains via dnsmasq. The Pi 5's 4GB RAM comfortably
runs XL/XXL tiers.

### 5.1 Install

```bash
opkg update
opkg install adblock luci-app-adblock
```

### 5.2 Backend + feeds

```bash
uci set adblock.global.adb_enabled='1'
uci set adblock.global.adb_dns='dnsmasq'

# Feed layering: breadth (hagezi Pro + oisd) + active threat intel (tif/certpl/urlhaus).
# Enable feeds (exact source names live in /etc/adblock/adblock.feeds; enable via LuCI or UCI).
# Recommended set:
#   hagezi Pro (or Pro++)  - ads/trackers/telemetry
#   oisd Big               - low-false-positive baseline
#   hagezi TIF (or Medium) - malware/phishing threat intel
#   certpl                 - default threat feed
#   urlhaus                - active malware domains (needs abuse.ch Auth-Key, see note)
uci commit adblock
```

> URLhaus hostfile export now requires a **free abuse.ch Auth-Key**. If you enable the
> `urlhaus` feed, configure the key (placeholder `<ABUSE_CH_AUTH_KEY>`) in its feed URL,
> otherwise older keyless URLs may 404. oisd dropped plain HOSTS/DOMAINS format
> (2024-01-01) — use the dnsmasq/wildcard variants (adblock's bundled entries already do).

### 5.3 First download

```bash
# IMPORTANT: only 'reload' (re)downloads feeds. 'start'/'restart' restore the cached backup.
/etc/init.d/adblock reload
/etc/init.d/adblock status
```

**Expected:** status shows feeds loaded and a domain count (hundreds of thousands).

### 5.4 Anti-bypass / DNS hardening

```bash
# Force all LAN port-53 queries to the router resolver (nftables DNAT) so clients
# can't hard-code 8.8.8.8 and skip filtering.
uci set adblock.global.adb_nftforce='1'
uci commit adblock
/etc/init.d/adblock restart

# Block DNS-over-TLS (port 853) so devices can't use encrypted DNS to bypass adblock.
# Omit a single-zone 'dest' so the REJECT applies regardless of which WAN the flow takes.
# WAN2 lives in its own 'wanb' zone, so a dest='wan' rule would leave 853 open whenever a
# client's flow is balanced onto WAN2 — a real DNS-leak hole (FR-F5 / NFR-S2). Leaving dest
# unset means "any zone", which also covers the 'vpn' zone when the tunnel is up.
uci add firewall rule
uci set firewall.@rule[-1].name='Block-DoT'
uci set firewall.@rule[-1].src='lan'
uci set firewall.@rule[-1].dest_port='853'
uci set firewall.@rule[-1].proto='tcp udp'
uci set firewall.@rule[-1].target='REJECT'
uci commit firewall
/etc/init.d/firewall restart
```

> The Block-DoT rule above intentionally has **no `dest` zone** so it rejects 853 over WAN1
> (`wan`), WAN2 (`wanb`), and the tunnel (`vpn`) alike. Scoping it to a single zone would
> leave encrypted DNS open on every other WAN — a flow balanced onto WAN2 would leak around
> adblock.
> Force-DNS (port-53 DNAT) does **NOT** stop DoH on 443. Blocking DoT/853 plus relying on
> hagezi's DoH-bypass entries (which block known public DoH hostnames) closes most of the
> gap. A jail/allowlist-only mode (`adb_jail=1`) black-holes all normal browsing — never
> enable it on a general home router.

### 5.5 Encrypted upstream (optional, privacy)

```bash
opkg install https-dns-proxy        # DoH forwarder (Cloudflare/Quad9 by default)
# Point dnsmasq upstream at the local DoH proxy:
uci set dhcp.@dnsmasq[0].noresolv='1'
uci add_list dhcp.@dnsmasq[0].server='127.0.0.1#5053'
uci commit dhcp
/etc/init.d/dnsmasq restart
```

### 5.6 Daily feed refresh

```bash
# 'reload' (not restart) actually re-downloads.
echo '0 5 * * * /etc/init.d/adblock reload' >> /etc/crontabs/root
/etc/init.d/cron enable
/etc/init.d/cron restart
```

**Verify:**

```bash
# From a LAN client:
nslookup doubleclick.net 192.168.1.1      # expect NXDOMAIN / 0.0.0.0
/etc/init.d/adblock search doubleclick.net  # shows which feed blocked it
```

---

## Phase 6 — Speed-test healthcheck + cron (weight driver)

Two-layer design: **mwan3track owns liveness/failover** (Phase 4); this script ONLY
measures capacity and rewrites member `weight`. The script must never mark interfaces
up/down — that would create two sources of truth fighting each other.

**The whole game is binding the probe to the correct WAN.** With mwan3 active there is a
load-balanced default route; an unbound probe measures "whatever WAN got picked" and
corrupts both weights. We bind by source IP and **verify** the IP actually used.

### 6.1 Install the probe

```bash
opkg update
opkg install librespeed-cli jsonfilter
# curl is an optional lighter fallback:
# opkg install curl
```

### 6.2 The healthcheck script

```bash
cat > /usr/bin/wan-weight.sh <<'EOF'
#!/bin/sh
# wan-weight.sh - per-WAN capacity probe -> mwan3 member weight.
# Liveness/failover is owned by mwan3track; this script only adjusts weights.
# Binding the probe to the correct WAN source IP is mandatory; an unbound probe
# silently measures the load-balanced default route and corrupts the weights.

set -eu

LOG_TAG="wan-weight"
STATE_DIR="/tmp/wan-weight"
mkdir -p "$STATE_DIR"

# WAN interface (netifd name) -> mwan3 member section name.
# Edit if you renamed members in Phase 4.
WAN_MEMBERS="wan:wan_m1_w1 wanb:wanb_m1_w1"

EWMA_ALPHA_NEW=40   # integer percent weight given to the new sample (0.4)
EWMA_ALPHA_OLD=60   # integer percent weight given to the old value   (0.6)
REWEIGHT_THRESHOLD=15  # percent change required before committing+restarting mwan3
PROBE_DURATION=8
PROBE_CONCURRENT=2

changed=0

for pair in $WAN_MEMBERS; do
    wan_if="${pair%%:*}"
    member="${pair##*:}"

    # Skip a WAN that mwan3 already reports down; do not probe a dead link.
    if ! mwan3 interfaces 2>/dev/null | grep -A1 -iE "interface ${wan_if} " | grep -qi "is online"; then
        logger -t "$LOG_TAG" "skip ${wan_if}: not online per mwan3"
        continue
    fi

    # Resolve this WAN's source IP from netifd.
    src_ip="$(ubus call network.interface.${wan_if} status 2>/dev/null \
              | jsonfilter -e '@["ipv4-address"][0].address' || true)"
    if [ -z "$src_ip" ]; then
        logger -t "$LOG_TAG" "skip ${wan_if}: no ipv4 source address"
        continue
    fi

    # Bound capacity probe. --source binds to the WAN's IP; download-only; short burst.
    json="$(librespeed-cli --source "$src_ip" --no-upload \
             --duration "$PROBE_DURATION" --concurrent "$PROBE_CONCURRENT" \
             --json 2>/dev/null || true)"
    if [ -z "$json" ]; then
        logger -t "$LOG_TAG" "probe failed on ${wan_if} (${src_ip}); keeping current weight"
        continue
    fi

    # librespeed --json reports download in Mbps.
    mbps="$(echo "$json" | jsonfilter -e '@[0].download' 2>/dev/null || true)"
    [ -z "$mbps" ] && mbps="$(echo "$json" | jsonfilter -e '@.download' 2>/dev/null || true)"
    if [ -z "$mbps" ]; then
        logger -t "$LOG_TAG" "could not parse Mbps for ${wan_if}; raw=$json"
        continue
    fi

    # Round Mbps to an integer measured weight.
    meas="$(awk -v m="$mbps" 'BEGIN{printf "%d", (m<1?1:m)+0.5}')"
    [ "$meas" -lt 1 ] && meas=1

    # EWMA smoothing against the last applied value (state file), so one bad
    # sample does not slam the routing tables.
    state_file="${STATE_DIR}/${member}.ewma"
    old="$(cat "$state_file" 2>/dev/null || echo "$meas")"
    new="$(awk -v o="$old" -v n="$meas" -v ao="$EWMA_ALPHA_OLD" -v an="$EWMA_ALPHA_NEW" \
           'BEGIN{printf "%d", (o*ao + n*an)/100 + 0.5}')"
    [ "$new" -lt 1 ] && new=1
    [ "$new" -gt 1000 ] && new=1000   # mwan3 caps weight at 1000

    cur="$(uci -q get mwan3.${member}.weight || echo 1)"

    # Only mark changed if the delta exceeds the threshold (avoid churning routes).
    delta="$(awk -v a="$cur" -v b="$new" 'BEGIN{d=(a>b?a-b:b-a); base=(a>0?a:1); printf "%d", (d*100)/base}')"
    if [ "$delta" -ge "$REWEIGHT_THRESHOLD" ]; then
        uci set mwan3.${member}.weight="$new"
        changed=1
        logger -t "$LOG_TAG" "${wan_if}/${member}: ${mbps}Mbps meas=${meas} ewma=${new} (was ${cur}) APPLIED"
    else
        logger -t "$LOG_TAG" "${wan_if}/${member}: ${mbps}Mbps meas=${meas} ewma=${new} (was ${cur}) below threshold, holding"
    fi
    echo "$new" > "$state_file"

    sleep 5   # serialize probes: never run both WANs concurrently (USB/CPU contention)
done

if [ "$changed" = "1" ]; then
    uci commit mwan3
    mwan3 restart
    logger -t "$LOG_TAG" "weights committed; mwan3 restarted"
fi
EOF
chmod +x /usr/bin/wan-weight.sh
```

### 6.3 Verify binding works (run once, by hand)

```bash
# Confirm a bound probe really uses the intended WAN source IP before trusting it.
WAN_IF=wan
SRC="$(ubus call network.interface.${WAN_IF} status | jsonfilter -e '@["ipv4-address"][0].address')"
echo "wan src ip = $SRC"
# If you installed curl, prove the source IP used matches:
# curl --silent --source "$SRC" -o /dev/null -w 'used %{local_ip}\n' https://speed.cloudflare.com/__down?bytes=10000000

/usr/bin/wan-weight.sh
logread | grep wan-weight | tail
```

**Expected:** log lines show a sane Mbps per WAN and the EWMA/applied weight. The source
IP printed matches that WAN's IP. `mwan3 policies` reflects the new split if weights changed.

### 6.4 Schedule (every 30 min)

```bash
echo '*/30 * * * * /usr/bin/wan-weight.sh' >> /etc/crontabs/root
/etc/init.d/cron restart
```

**Verify:** `crontab -l` (or `cat /etc/crontabs/root`) lists the job; after the next
half-hour, `logread | grep wan-weight` shows a fresh run.

> **Data burn:** a librespeed run can move hundreds of MB per WAN. 30 min balances
> freshness vs. data. For a metered uplink, stretch to 2-4h or downgrade that WAN to a
> small curl `--limit-rate` byte-count probe only. **Never** probe through the WireGuard
> tunnel by accident — bind to the physical WAN IP, not `wg0`.

---

## Phase 7 — Surfshark WireGuard + pbr + on-demand toggle

VPN is **OFF by default** (full dual-WAN active). When ON it physically rides ONE WAN at a
time, so for tunneled traffic dual-WAN degrades to **failover only** — mwan3 still
balances all non-VPN flows and still tracks/fails over the underlying WANs.

### 7.1 Install

```bash
opkg update
opkg install wireguard-tools kmod-wireguard pbr luci-app-pbr
# pbr deps (resolveip, ip-full) are pulled automatically. dnsmasq-full (Phase 3.3)
# is required for domain-based VPN policies.
```

### 7.2 Get Surfshark WireGuard credentials (manual, on your workstation)

In the Surfshark dashboard: **VPN → Manual setup → Router → WireGuard**. Register a key
pair (private key stays local) and download a per-server `.conf`. You will need:

- `<WG_PRIVATE_KEY>` — your private key (SECRET, never commit).
- `<WG_ADDRESS>` — e.g. `10.14.0.2/16` (note: a real netmask is mandatory).
- `<WG_PEER_PUBLIC_KEY>` — the server's public key.
- `<WG_ENDPOINT_HOST>` — e.g. `xx-yyy.prod.surfshark.com`.
- Endpoint port is `51820`. Surfshark issues **no** preshared key.
- Surfshark DNS: `162.252.172.57` and `149.154.159.92`.

### 7.3 Configure the wg interface (split-tunnel model: route_allowed_ips 0)

```bash
uci set network.wgvpn='interface'
uci set network.wgvpn.proto='wireguard'
uci set network.wgvpn.private_key='<WG_PRIVATE_KEY>'
uci add_list network.wgvpn.addresses='<WG_ADDRESS>'   # MUST include the netmask
uci set network.wgvpn.mtu='1412'                       # avoid PMTU blackholes
# Split-tunnel: do NOT let the tunnel grab the default route; pbr steers traffic instead.
uci set network.wgvpn.route_allowed_ips='0'
# Start OFF by default.
uci set network.wgvpn.disabled='1'

# Peer
uci set network.wgpeer='wireguard_wgvpn'
uci set network.wgpeer.public_key='<WG_PEER_PUBLIC_KEY>'
uci set network.wgpeer.endpoint_host='<WG_ENDPOINT_HOST>'
uci set network.wgpeer.endpoint_port='51820'
uci set network.wgpeer.persistent_keepalive='25'
uci add_list network.wgpeer.allowed_ips='0.0.0.0/0'
uci add_list network.wgpeer.allowed_ips='::/0'
# Do NOT add a preshared_key — Surfshark does not issue one.

uci commit network
```

> **Pitfall:** omitting the netmask on `addresses` defaults to a `/32` host route and the
> tunnel silently fails to route. **Pitfall:** never set `route_allowed_ips 1` AND use pbr
> — pick exactly one model.

### 7.4 Firewall zone for the tunnel

```bash
uci add firewall zone
uci set firewall.@zone[-1].name='vpn'
uci set firewall.@zone[-1].network='wgvpn'
uci set firewall.@zone[-1].masq='1'
uci set firewall.@zone[-1].mtu_fix='1'
uci set firewall.@zone[-1].input='REJECT'
uci set firewall.@zone[-1].output='ACCEPT'
uci set firewall.@zone[-1].forward='REJECT'

uci add firewall forwarding
uci set firewall.@forwarding[-1].src='lan'
uci set firewall.@forwarding[-1].dest='vpn'

uci commit firewall
/etc/init.d/firewall restart
```

### 7.5 pbr policy (the VPN target) + mwan3 coexistence

```bash
# CRITICAL: pbr ip-rule priority must be BELOW mwan3's 2001-2254 band, or VPN policies
# never match. 900 is the documented mwan3 coexistence value.
# VPN is OFF by default (FR-V3): pbr config-level disabled AND per-policy disabled. This
# matches vpn.md §8 and architecture §9 — a reboot or `service pbr start` must NOT begin
# steering anything. The toggle script (7.7) is the single authority that flips both
# pbr.config.enabled and the per-policy enabled flag together.
uci set pbr.config.enabled='0'
uci set pbr.config.uplink_ip_rules_priority='900'
# Mark space MUST NOT overlap mwan3's 0x3F00 (NFR-S6, requirements §7, architecture §11).
# These are pbr's defaults; set fw_mask explicitly so the non-overlap is asserted, not
# left implicit. uplink_mark defaults to 0x00010000 (a different byte than mwan3's mask).
uci set pbr.config.fw_mask='0x00ff0000'
# (uplink_mark default 0x00010000 is left at default; uncomment to pin it explicitly.)
# uci set pbr.config.uplink_mark='0x00010000'
# For domain-based policies, point pbr's resolver at dnsmasq's nftset:
uci set pbr.config.resolver_set='dnsmasq.nftset'
# Kill switch: drop policy-matched traffic if the tunnel is down instead of leaking to WAN.
uci set pbr.config.strict_enforcement='1'
# If pbr does not auto-detect the wg interface, force it:
uci add_list pbr.config.supported_interface='wgvpn'

# Policy: route the whole LAN through the VPN. Disabled by default (VPN OFF).
# For selected clients use src_addr=<CLIENT_IP_OR_MAC>; for selected sites use dest_addr=<DOMAIN>.
uci add pbr policy
uci set pbr.@policy[-1].name='lan_via_vpn'
uci set pbr.@policy[-1].src_addr='192.168.1.0/24'   # or a single <CLIENT_IP>
uci set pbr.@policy[-1].interface='wgvpn'
uci set pbr.@policy[-1].enabled='0'                  # OFF by default

uci commit pbr
/etc/init.d/pbr stop      # default state: VPN off, pbr not steering anything
```

**Verify (mark/priority do not collide with mwan3):**

```bash
# pbr uses mask 0x00ff0000; mwan3 uses 0x3F00 — different bytes, no overlap.
nft list ruleset | grep -i 0x00ff0000   # pbr's mask present once VPN is toggled on
nft list ruleset | grep -i 0x3f00       # mwan3's mask, separate bits
ip rule | grep -E '900|2001|2254'       # pbr at 900, BELOW mwan3's 2001-2254 band
```

### 7.6 Flush wg conntrack on WAN transition (so the tunnel survives a WAN death)

WireGuard pins its client UDP source port, so its conntrack will not migrate cleanly on
mwan3 failover. Flush only the wg endpoint flow on transitions so it re-handshakes over
the surviving WAN.

```bash
opkg install conntrack
cat > /etc/mwan3.user <<'EOF'
# Flush the WireGuard endpoint conntrack on a WAN transition so the tunnel
# re-handshakes over the surviving WAN. Do NOT flush all conntrack (strands downloads).
# Action set (aligned with vpn.md §10, architecture §6, requirements FR-V11):
#   ifdown       - the most direct "WAN went down" signal; without it the failover
#                  trigger can be missed and the tunnel black-holes on the dead WAN.
#   disconnected - mwan3 declared the link down via tracking (track_ip failed).
#   connected    - link came back; re-pin the endpoint flow to the now-active WAN.
case "$ACTION" in
    ifdown|disconnected|connected)
        conntrack -D -p udp --dport 51820 2>/dev/null || true
        ;;
esac
EOF
```

### 7.7 On-demand toggle script

```bash
cat > /usr/bin/vpn-toggle.sh <<'EOF'
#!/bin/sh
# vpn-toggle.sh on|off|status - on-demand Surfshark WireGuard toggle.
# ON  : enable iface, ifup, wait for handshake, enable pbr policy, reload pbr.
# OFF : disable pbr policy, reload/stop pbr, ifdown, disable iface.
# Order matters: tunnel must be UP before pbr reload so pbr sees the live interface.

set -eu
POLICY_NAME="lan_via_vpn"

policy_index() {
    # Find the @policy[] index whose name matches POLICY_NAME.
    i=0
    while uci -q get pbr.@policy[$i] >/dev/null 2>&1; do
        if [ "$(uci -q get pbr.@policy[$i].name)" = "$POLICY_NAME" ]; then
            echo "$i"; return 0
        fi
        i=$((i+1))
    done
    return 1
}

case "${1:-status}" in
    on)
        uci set network.wgvpn.disabled='0'; uci commit network
        ifup wgvpn
        # Wait up to 15s for a handshake.
        n=0
        while [ $n -lt 15 ]; do
            wg show wgvpn latest-handshakes 2>/dev/null | awk '{exit ($2>0)?0:1}' && break
            n=$((n+1)); sleep 1
        done
        # Single authority for the OFF-by-default invariant: flip BOTH the config-level
        # enable and the per-policy enable together (7.5 ships both as '0').
        uci set pbr.config.enabled='1'
        idx="$(policy_index)" && { uci set pbr.@policy[$idx].enabled='1'; }
        uci commit pbr
        /etc/init.d/pbr enable
        /etc/init.d/pbr reload
        logger -t vpn-toggle "VPN ON"
        ;;
    off)
        # Restore the OFF-by-default invariant: disable both the per-policy and config-level
        # flags so the persisted config truthfully reads "OFF" (matches 7.5 / vpn.md §8).
        idx="$(policy_index)" && { uci set pbr.@policy[$idx].enabled='0'; }
        uci set pbr.config.enabled='0'
        uci commit pbr
        /etc/init.d/pbr reload 2>/dev/null || true
        ifdown wgvpn
        uci set network.wgvpn.disabled='1'; uci commit network
        /etc/init.d/pbr stop 2>/dev/null || true
        logger -t vpn-toggle "VPN OFF"
        ;;
    status)
        echo "iface disabled = $(uci -q get network.wgvpn.disabled)"
        wg show wgvpn 2>/dev/null || echo "wgvpn: down"
        /etc/init.d/pbr status 2>/dev/null || true
        ;;
    *) echo "usage: $0 on|off|status" >&2; exit 1 ;;
esac
EOF
chmod +x /usr/bin/vpn-toggle.sh
```

**Verify (toggle on, then off):**

```bash
/usr/bin/vpn-toggle.sh on
wg show wgvpn          # latest handshake non-zero; transfer counters climbing
# From a policied LAN client, confirm public IP is Surfshark's and DNS does not leak:
#   curl https://ipinfo.io/ip        -> a Surfshark egress IP
#   (use a DNS-leak test site)        -> resolver is Surfshark's, NOT your ISP
/usr/bin/vpn-toggle.sh off
wg show wgvpn          # "down"; LAN client public IP reverts to a WAN IP
```

> **Honesty note (Goal 3):** with the tunnel up and `AllowedIPs 0.0.0.0/0`, tunneled
> traffic egresses exactly ONE WAN — Surfshark/WireGuard cannot bond the two lines. The
> second WAN is failover only while the VPN is up. Non-VPN flows keep full mwan3 balancing.
> **DNS:** OpenWrt ignores the WireGuard peer's `DNS=` field, so set Surfshark DNS
> explicitly and route the resolver via the tunnel; keep adblock force-DNS ON so clients
> cannot leak around the router.

---

## Phase 8 — End-to-end verification

Run these after a full build (and after any reboot, to confirm persistence).

### 8.1 Hardware / arch

```bash
ubus call system board | grep -i bcm2712      # bcm27xx/bcm2712
cat /etc/openwrt_release | grep ARCH           # aarch64_cortex-a76
```

### 8.2 Interfaces pinned and up

```bash
ip link show wan2dev && ip link show landev    # USB NICs present by pinned name
ip addr show eth0                              # WAN1 has a lease
ubus call network.interface.wanb status | jsonfilter -e '@["ipv4-address"][0].address'
```

### 8.3 Multi-WAN

```bash
mwan3 interfaces        # wan + wanb online
mwan3 policies          # balanced split reflects current weights
# Per-flow stickiness check: two simultaneous large downloads should each pin to one WAN.
# Inspect conntrack marks:
conntrack -L 2>/dev/null | head
# Failover: unplug WAN1's uplink, confirm flows survive on wanb within ~30s:
mwan3 interfaces        # wan -> offline, wanb still online; re-plug to recover
```

### 8.4 Healthcheck

```bash
/usr/bin/wan-weight.sh && logread | grep wan-weight | tail
cat /etc/crontabs/root | grep wan-weight       # cron entry present
```

### 8.5 DNS filtering

```bash
nslookup doubleclick.net 192.168.1.1           # NXDOMAIN/0.0.0.0
nslookup openwrt.org 192.168.1.1               # resolves normally
/etc/init.d/adblock status                     # feeds loaded, domain count
# Anti-bypass: from a LAN client, a hard-coded resolver should still be filtered:
nslookup doubleclick.net 8.8.8.8               # still NXDOMAIN (DNAT force-dns)
```

### 8.6 VPN toggle

```bash
/usr/bin/vpn-toggle.sh on
wg show wgvpn                                  # handshake + traffic
# policied client: public IP = Surfshark, DNS-leak test = Surfshark resolver
/usr/bin/vpn-toggle.sh off
wg show wgvpn                                  # down; dual-WAN balancing resumes
```

### 8.7 Reboot persistence

```bash
reboot
# After it comes back:
ip link show wan2dev; ip link show landev      # pinned names survive
mwan3 interfaces                               # both WANs online
/etc/init.d/adblock status                     # adblock running
/usr/bin/vpn-toggle.sh status                  # VPN still OFF by default
```

---

## Quick reference — common operations

| Task | Command |
| --- | --- |
| Multi-WAN status | `mwan3 status` / `mwan3 interfaces` / `mwan3 policies` |
| Take a WAN out / in (no full restart) | `mwan3 ifdown wanb` / `mwan3 ifup wanb` |
| Re-download adblock feeds | `/etc/init.d/adblock reload` (NOT restart) |
| Pause/resume blocking (no download) | `/etc/init.d/adblock suspend` / `resume` |
| Check why a domain is blocked | `/etc/init.d/adblock search <domain>` |
| Run capacity probe now | `/usr/bin/wan-weight.sh` |
| VPN on / off / status | `/usr/bin/vpn-toggle.sh on|off|status` |
| Bind a one-off command to a WAN | `mwan3 use wan <cmd>` |
