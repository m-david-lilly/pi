# Architecture — Raspberry Pi 5 OpenWrt Multi-WAN Router

This document describes the system architecture of the Raspberry Pi 5 OpenWrt
router: physical and logical topology, the firewall zone / interface model, how
each OpenWrt package layers into a distinct functional plane, the data flow for
ordinary and VPN-routed traffic, the mwan3 connmark / sticky model, how the
speed-test healthcheck loop feeds member weights, and how the VPN toggle changes
routing.

For detailed per-subsystem configuration see the sibling reference docs:

- Hardware / image / NIC selection: [`./hardware.md`](./hardware.md)
- Multi-WAN (mwan3), speed-test healthcheck, and DNS filtering (adblock):
  [`./load-balancing.md`](./load-balancing.md)
- Surfshark WireGuard VPN and firewall / integration:
  [`./vpn.md`](./vpn.md)

> Path note: the filenames above are relative paths from `docs/reference/`.

---

## 1. Hardware platform

- **Board:** Raspberry Pi 5 (BCM2712), aarch64 ARMv8-A Cortex-A76, quad-core
  @ 2.4 GHz, 4 GB RAM.
- **OpenWrt target:** `bcm27xx/bcm2712`, device profile `rpi-5`. **As-built: a
  custom Firmware-Selector image of 25.12.4** (r32933) with the full package set
  pre-baked (see [`./hardware.md`](./hardware.md) §7). **squashfs** image. Use
  only the aarch64 image for this target; an image for a different Raspberry Pi
  target will not boot.
- **NIC layout (AS-BUILT — note this is the *inverse* of the original design,
  which put WAN1 on the onboard port for throughput):**
  - Onboard gigabit Ethernet (RP1, dedicated lane) → **LAN / management**
    (`br-lan`, `192.168.1.1/24`). Keeping management on the fixed onboard port
    is what keeps SSH alive across USB re-enumeration.
  - 2× USB3 gigabit Ethernet adapters (RTL8153 via `kmod-usb-net-rtl8152`) on the
    two USB3 (blue) ports → **both are WANs**: `uwan1` and `uwan2`. The original
    docs' "fastest uplink on onboard GbE" recommendation (NFR-P3) was deliberately
    NOT followed — management stability won over a marginal throughput edge on a
    home dual-WAN box.
- **Interface pinning (AS-BUILT — by HOTPLUG rule, NOT `config device`):** USB
  NIC enumeration order is not guaranteed across reboots (eth1/eth2 swap, and did
  swap on hardware). The two USB WAN NICs are pinned to stable names **by MAC at
  the hotplug `add` event** via `/etc/hotplug.d/net/05-rename-wan-by-mac`
  (in repo: `config/hotplug/05-rename-wan-by-mac`). **netifd does NOT honor the
  `/etc/config/network` `config device` MAC alias for these RTL8153 USB NICs** —
  verified on hardware: the rename never fired on reload/restart/cold-boot, and
  carrier presence is irrelevant (an earlier "rename fires on carrier" theory was
  falsified). The onboard GbE (`eth0` → `br-lan`) is fixed and needs no pin.
- **Device names MUST NOT end in `dev`.** mwan3 2.12.0's `mwan3_route_line_dev()`
  extracts a route's device with a greedy `sed -ne "s/.*dev \([^ ]*\).*/\1/p"`, so
  a name like `wan1dev` is mis-parsed as the next token (`proto`), and the per-WAN
  default route is silently dropped from tables 1/2 → marked LAN traffic gets
  "Network unreachable". Hence the names `uwan1`/`uwan2` (USB uplink 1/2), never
  `wan1dev`/`wan2dev`/`landev`.

The Pi 5's two USB3 ports are **independent 5 Gbps xHCI controllers** in RP1 (not the
Pi 4's single shared 5 Gbps path), so two USB3 GbE NICs at ~1 Gbps each do not contend at
the controller level. The only shared resource is RP1's **PCIe 2.0 x4 uplink to the SoC**
(~16 Gbps), which has ample headroom for 1 GbE + 2×5 Gbps USB. See
[`./hardware.md`](./hardware.md) §4 for the full bandwidth model.

> **No RTC → cold-boot clock/DNS/TLS deadlock (real, fixed).** The Pi 5 has no
> battery-backed real-time clock, so on cold boot the clock is stale. The DNS
> plane (DoH over TLS) fails cert validation with a wrong clock, and NTP using
> pool *hostnames* needs DNS — which needs DoH — which needs a correct clock: a
> hard circular deadlock that leaves the box permanently off by years. **Fix:
> list IP-literal NTP servers FIRST** in `system.ntp.server` (Cloudflare
> `162.159.200.123`/`.1`, Google `216.239.35.0`, ahead of the pool hostnames), so
> busybox `ntpd` sets the clock over UDP/123 with no DNS and no TLS, breaking the
> loop. (`fake-hwclock` is NOT in the 25.12.4 repo; the IP-literal-NTP fix is the
> durable mitigation and is independently sufficient — verified by booting with a
> deliberately stale 2024 clock and watching it self-correct.)

---

## 2. Network topology

```
                          INTERNET
                  ┌──────────┴───────────┐
            ISP / Uplink A          ISP / Uplink B
            (modem/ONT 1)           (modem/ONT 2)
                  │                       │
        USB3 GbE  (uwan1)        USB3 GbE  (uwan2)
        RTL8153, MAC-pinned      RTL8153, MAC-pinned
                  │                       │
   ┌──────────────┼───────────────────────┼──────────────────────┐
   │  Raspberry Pi 5  —  OpenWrt 25.12.4 (bcm2712 / aarch64)     │
   │                                                             │
   │   WAN1 (network: wan1)        WAN2 (network: wan2)          │
   │   metric 10                   metric 20                     │
   │   both in firewall zone: wan   (masq=1)                     │
   │      │                            │                         │
   │      └──────────┬─────────────────┘                         │
   │                 │  mwan3 (per-flow connmark balancer)       │
   │                 │  + dnsmasq/adblock + https-dns-proxy (DNS)│
   │                 │  + pbr (VPN policy routing, OFF default)  │
   │                 │                                           │
   │             wgvpn (proto wireguard)  ── Surfshark           │
   │             zone: vpn  masq=1 mtu_fix=1                     │
   │             route_allowed_ips=0 (split via pbr, always)     │
   │             interface disabled=1  (tunnel OFF by default)   │
   │                 │                                           │
   │              LAN / management (network: lan)                │
   │              onboard GbE → br-lan (eth0) 192.168.1.1/24     │
   │              zone: lan  masq=0   (Pi WiFi radio DISABLED)   │
   └─────────────────┬───────────────────────────────────────────┘
                     │  (single onboard LAN port)
              ┌──────┴───────────-────┐
              │  downstream WiFi      │   NETGEAR Orbi MR60, NAT mode
              │  router (MR60)        │   WAN=DHCP from Pi (192.168.1.x)
              │  LAN 10.0.0.0/24      │   LAN/WiFi = 10.0.0.x to clients
              └──┬───┬───┬────────────┘
                 │   │   │
              client client client ...
              (non-Zscaler clients get the Pi's adblock + dual-WAN)
```

Logical mapping summary (as-built):

| Role | Physical NIC | Device | OpenWrt iface (`network`) | Firewall zone | NAT (`masq`) |
|---|---|---|---|---|---|
| LAN / management | onboard GbE | `eth0` → `br-lan` | `lan` (192.168.1.1/24) | `lan` | 0 |
| WAN1 | USB3 RTL8153 #1 | `uwan1` (MAC-pinned) | `wan1` (dhcp, metric 10) | `wan` | 1 |
| WAN2 | USB3 RTL8153 #2 | `uwan2` (MAC-pinned) | `wan2` (dhcp, metric 20) | `wan` | 1 |
| VPN tunnel | (virtual) | `wgvpn` | `wgvpn` (wireguard) | `vpn` | 1 |

> **Zone vs interface:** both WAN *interfaces* (`wan1`, `wan2`) sit in the single
> firewall *zone* named `wan` (the zone's `network` list = `wan1 wan2`). Keeping
> the zone named `wan` means every `src/dest='wan'` firewall rule stays valid;
> mwan3 balances across the two *interfaces*, not zones.
>
> **Distinct interface metrics (10/20) are load-bearing:** without distinct
> metrics only one DHCP default route lands in the main table and the other WAN
> flaps offline. This is the *network-interface* metric layer — separate from
> mwan3 *member* metrics (§7), which stay equal for active-active balancing.
>
> The `vpn` zone also carries `mtu_fix '1'` (a separate zone option, not a value
> of `masq`) to MSS-clamp tunneled TCP. See §4 for the full per-zone options.
>
> **Downstream WiFi:** the Pi's own WiFi radio is disabled. A NETGEAR Orbi MR60
> runs in NAT mode off the Pi's single onboard LAN port (its WAN pulls a
> `192.168.1.x` DHCP lease from the Pi; its own LAN serves `10.0.0.0/24`). Note
> this double-NATs WiFi clients behind the Pi, so the Pi's per-client adblock
> reporting sees only the MR60's WAN IP, not individual clients.

---

## 3. Component / package model

The system is built from OpenWrt packages that operate in **three
non-overlapping planes** plus a management UI. They only interact at well-defined
seams (the ip-rule priority band and the fwmark masks), which keeps them
composable.

```
┌──────────────────────────────────────────────────────────────────┐
│  MANAGEMENT                                                      │
│  luci + luci-app-mwan3 + luci-app-adblock + luci-app-pbr +       │
│  luci-proto-wireguard   (web UI; read/write of /etc/config/*)    │
└──────────────────────────────────────────────────────────────────┘
        │ reads/writes UCI config; does not sit in the data path
        ▼
┌──────────────────────────────────────────────────────────────────┐
│  PLANE 1 — DNS / FILTERING                                       │
│  dnsmasq + adblock + https-dns-proxy (DoH)  (all + luci apps)    │
│  - adblock returns NXDOMAIN for blocked domains                  │
│  - feeds (adblock 4.5.6 catalog): oisd_big, certpl, hagezi       │
│    (FR-F2's hagezi Pro/TIF + urlhaus do NOT exist in 4.5.6;      │
│     these three are the closest available match — open gap)      │
│  - https-dns-proxy DoH upstreams: Cloudflare 127.0.0.1#5053 +    │
│    Google 127.0.0.1#5054. It AUTO-WIRES dnsmasq (noresolv +      │
│    server=#5053/#5054) AND auto-installs force-DNS (port-53      │
│    redirect) + DoT block (port-853 reject). WANs peerdns=0.      │
│  Acts at DNS resolution time, BEFORE routing. VPN state never    │
│  breaks it (VPN-OFF steady state).                               │
└──────────────────────────────────────────────────────────────────┘
        │ resolved IP handed to the routing decision
        ▼
┌──────────────────────────────────────────────────────────────────┐
│  PLANE 2 — POLICY ROUTING (VPN)                                  │
│  pbr (+ luci-app-pbr)         OFF by default                     │
│  - fwmark 0x00010000, mask 0x00ff0000  (distinct from mwan3)     │
│  - uplink_ip_rules_priority = 900  (numerically lower than mwan3 │
│    2001-2254 ⇒ evaluated FIRST ⇒ higher precedence)              │
│  - matched flows → wgvpn routing table; unmatched fall through   │
│  - strict_enforcement=1 = kill switch (drop instead of leak)     │
└──────────────────────────────────────────────────────────────────┘
        │ unmatched (non-VPN) flows fall through to:
        ▼
┌──────────────────────────────────────────────────────────────────┐
│  PLANE 3 — WAN BALANCING / FAILOVER                              │
│  mwan3 (+ luci-app-mwan3)  + iptables-nft conntrack shims        │
│  - connmark in mangle, mask 0x3F00, ip rules 2001-2254           │
│  - per-flow (connmark + conntrack) sticky balancing              │
│  - members same metric, weights from healthcheck → ratio split   │
│  - mwan3track liveness (track_ip) → mark WAN up/down             │
└──────────────────────────────────────────────────────────────────┘
        │ flow assigned to a WAN routing table
        ▼
┌──────────────────────────────────────────────────────────────────┐
│  NAT / EGRESS                                                    │
│  fw4 (nftables) per-zone masq: wan (wan1+wan2), vpn → masq=1     │
└──────────────────────────────────────────────────────────────────┘

  SUPPORTING:
  wireguard-tools + kmod-wireguard  → the wgvpn interface itself
  librespeed-cli (or curl)          → interface-bound capacity probe
  iperf3 / curl                     → diagnostics
  cron (/etc/crontabs/root)         → drives wan-weight.sh + feed refresh
```

Plane summary:

| Plane | Package(s) | Mechanism | Mark / priority | Default state |
|---|---|---|---|---|
| DNS filtering | dnsmasq + adblock + https-dns-proxy | NXDOMAIN + port-53 DNAT + DoH upstream | n/a (NAT-layer DNS redirect) | ON |
| VPN policy routing | pbr | fwmark + ip rule | mark `0x00010000`, mask `0x00ff0000`, prio `900` | **OFF** |
| WAN balancing | mwan3 | connmark + ip rule | mask `0x3F00`, prio `2001-2254` | ON |
| NAT | fw4 (nftables) | per-zone `masq` | n/a | ON |

The masks (`0x3F00` for mwan3, `0x00ff0000` for pbr) are non-overlapping by
design — **do not widen either mask** or they will clobber each other's
connmarks.

---

## 4. Firewall / zone model (fw4 / nftables)

Modern OpenWrt (22.03 and later — verify the exact release against the live
OpenWrt source) uses **fw4 (nftables)**. Zones:

- **`lan`** — `option network 'lan'`, `masq '0'`, `input/output/forward ACCEPT`
  for LAN-side trust. Onboard `eth0`/`br-lan`, 192.168.1.1/24. Source of all
  client traffic (and the downstream MR60's WAN).
- **`wan`** — `list network 'wan1'` + `list network 'wan2'` (BOTH USB WANs in
  one zone), `masq '1'`, `mtu_fix '1'`, `input REJECT`, `forward DROP`. A single
  zone named `wan` holds both WAN interfaces — so every `src/dest='wan'` rule
  stays valid and mwan3 balances across the two interfaces within it. (There is
  no separate `wanb` zone in the as-built config.)
- **`vpn`** — `option network 'wgvpn'`, `masq '1'`, `mtu_fix '1'`,
  `forward REJECT`. WireGuard's per-packet overhead is **60 bytes over IPv4**
  (20 IP + 8 UDP + 32 WireGuard) ⇒ a 1500-byte path yields MTU **1440**; over
  IPv6 the outer header is 40 bytes ⇒ 80 bytes total ⇒ MTU 1420. WireGuard's
  own default MTU is 1420 (the IPv6-safe figure, also fine for IPv4). A
  conservative ~1412 (see §11) leaves extra headroom for PPPoE or
  double-encapsulated uplinks. `mtu_fix '1'` MSS-clamps so bulk TCP / HTTPS does
  not blackhole regardless of which value is set.

Forwardings (each direction is its own stanza):

- `lan → wan`  (the single `wan` zone covers both `wan1` and `wan2`)
- `lan → vpn`

Ownership notes:

- **mwan3 owns** the choice between `wan1` and `wan2` for each flow (it balances
  across the two *network* interfaces inside the single `wan` zone). The zone
  exists for firewall/NAT, not for mwan3.
- **`wgvpn` is NOT an mwan3 interface.** It lives in its own `vpn` zone and is a
  **pbr target**, not an mwan3 member. Adding `wgvpn` to a WAN zone (the minimal
  wiki approach) would make mwan3 try to balance the tunnel as if it were an
  uplink — do not do this.
- **adblock owns** the DNS plane only (NXDOMAIN + force-dns DNAT of port 53). To
  stop client DoH/DoT bypass: keep force-dns ON, REJECT outbound TCP/UDP 853
  (DoT) from LAN, and block known DoH hostnames (hagezi's bypass entries help).

---

## 5. Data flow — normal (non-VPN) flow

VPN is OFF by default. A new outbound TCP connection from a LAN client:

1. **DNS resolution.** Client's DNS query (port 53) is force-DNS DNAT'd to the
   Pi's dnsmasq resolver (https-dns-proxy auto-installs this redirect). adblock
   has loaded the blocklists into dnsmasq; if the domain is on a feed, dnsmasq
   returns **NXDOMAIN** and the connection never starts. Otherwise dnsmasq
   forwards to its DoH upstream — `https-dns-proxy` on `127.0.0.1#5053`
   (Cloudflare) / `#5054` (Google) — and returns the real IP. (Caveat: a client
   behind Zscaler or another always-on VPN does its own in-tunnel DNS and bypasses
   this entirely — the Pi's filtering only reaches non-VPN clients.)
2. **First packet → mwan3.** The connection is NEW, so no connmark yet. pbr is
   OFF (no priority-900 rules match), so the packet falls through to mwan3's
   `2001-2254` ip-rule band.
3. **WAN selection + connmark.** mwan3 picks a member among the equal-metric
   members in the balanced policy, weighted by the members' `weight` values
   (e.g. weights 3:2 ⇒ ~60/40 of *new flows*). It stamps the connection with an
   fwmark selecting that WAN.
4. **Sticky for the connection's lifetime.** conntrack carries the connmark, so
   **every subsequent packet of that flow exits the same WAN**. This is the
   intrinsic per-flow stickiness — it satisfies "a TCP connection stays on one
   WAN for its lifetime" with no extra option.
5. **Routing + NAT.** The ip rule routes the flow into that WAN's routing table
   (table 1 for `wan1`, table 2 for `wan2`); the `wan` zone applies `masq` and
   egresses.
6. **Failover.** mwan3track independently probes `track_ip` hosts per WAN. If a
   WAN fails enough consecutive checks it is marked down and removed from the
   live pool; with `flush_conntrack` set on `ifdown`/`disconnected`, stale flows
   pinned to the dead WAN are flushed and re-balanced onto the survivor.

**Honest limitation:** mwan3 does NOT bond bandwidth. A single TCP download uses
ONE WAN. A "100 + 100" setup gives ~100 Mbps single-stream, not 200. Aggregate
gains come only from many concurrent flows.

---

## 6. Data flow — VPN flow (tunnel up)

When the VPN toggle brings `wgvpn` up and enables the relevant pbr policy:

1. **DNS resolution** — same as above, dnsmasq + adblock still filter. To avoid
   a DNS leak, dnsmasq's upstream must egress through the tunnel (bind the
   forwarder to `wgvpn` or policy-route the router's own DNS via `wgvpn`), and
   Surfshark DNS (`<SURFSHARK_DNS_1>` / `<SURFSHARK_DNS_2>`) is set on the wg
   interface. OpenWrt **ignores** the WireGuard peer's `DNS =` line, so resolver
   routing is configured explicitly; `peerdns` is disabled on the WANs. **Note
   the OFF-state coupling:** the `wgvpn`-bound forwarder fails closed only while
   it is the active upstream, so the toggle must switch dnsmasq's upstream to
   this proxy on VPN-ON and back to a non-tunnel forwarder on VPN-OFF — pinning
   it as the *sole* upstream unconditionally would break DNS in the default-OFF
   state whenever the tunnel is down. See [`./vpn.md`](./vpn.md) §7 item 3.
   > **STATUS — NOT YET IMPLEMENTED (deferred, accepted debt).** The `wgvpn`
   > device-bind on `https-dns-proxy` and the toggle's upstream-switch are
   > deliberately deferred until the VPN is actually enabled (it is staged
   > default-OFF). **Consequence:** if the VPN is brought up today, the router's
   > own upstream DNS egresses a physical WAN (encrypted under DoH, but not
   > tunnel-routed), so **NFR-S2 is not yet met**. Close this during the Phase 8.6
   > leak test before relying on the VPN for privacy.
2. **First packet → pbr.** pbr's ip rules sit at priority **900**, *above*
   mwan3's `2001-2254` band, so they are evaluated first. If the flow matches a
   pbr policy (`src_addr` = a LAN subnet/IP/MAC, and/or `dest_addr` = IP or
   domain via `dnsmasq.nftset`), pbr marks it (`0x00010000`) and routes it into
   the `wgvpn` routing table.
3. **Tunnel egress.** Traffic is encapsulated by WireGuard, NAT'd by the `vpn`
   zone (`masq=1`, `mtu_fix=1`), and the encrypted UDP rides **one** physical
   WAN (the one the WireGuard endpoint route resolves to). The WireGuard
   endpoint host keeps a route on the physical WAN so the tunnel itself can
   connect (avoiding a route loop with `AllowedIPs 0.0.0.0/0`).
4. **Unmatched flows still balance.** Any flow that does NOT match a pbr policy
   falls through to mwan3 and is weighted/failed-over across both WANs as
   normal. So split-tunnel keeps full dual-WAN for non-VPN traffic.
5. **Kill switch.** With pbr `strict_enforcement '1'`, policy-matched traffic is
   **dropped** (not leaked out a WAN) whenever `wgvpn` is down. Caveat: this
   protects forwarded LAN traffic only — the router's own egress is not killed.
   This directly affects the DNS-leak story in step 1: because the router's own
   resolver egress is router-originated (not forwarded), a tunnel-down event can
   leak DNS out a physical WAN even with strict_enforcement on. If that leak
   matters, also policy-route or firewall the router's own DNS egress so it
   cannot fall back to a WAN when `wgvpn` is down.

**Honest limitation (GOAL 3):** with the tunnel up and `AllowedIPs 0.0.0.0/0`,
tunneled traffic egresses exactly one WAN at a time. Surfshark/WireGuard cannot
bond the two lines, so for VPN-routed traffic **dual-WAN degrades to
failover-only**. mwan3 still tracks and fails over the underlying physical WANs.

WireGuard's client UDP source port is sticky, so the tunnel's conntrack will not
migrate cleanly when the underlying WAN dies. The documented fix is an
`/etc/mwan3.user` hook (using the `conntrack` package) that flushes the wg
UDP 51820 flow on mwan3 connect/disconnect, forcing a re-handshake over the
surviving WAN.

---

## 7. mwan3 connmark / sticky model

```
NEW connection
     │
     ▼
[ mwan3 mangle rules ]  pick member by metric+weight
     │  stamp fwmark (mask 0x3F00) selecting WAN
     ▼
[ conntrack ]  remembers the connmark for this flow
     │
     ▼
EVERY subsequent packet of the flow ──► same WAN  (per-flow sticky)
```

Two distinct, layered stickiness mechanisms:

1. **Intrinsic per-connection stickiness (always on).** connmark + conntrack
   pin every packet of a single TCP/UDP flow to the WAN chosen at connection
   start. This alone satisfies the project's "a TCP connection stays on one WAN
   for its lifetime" requirement. **Per-packet balancing is wrong and does not
   occur** — it would break NAT/TLS.

2. **Rule-level `sticky '1'` (optional).** A `config rule` with `sticky '1'` and
   `timeout '600'` pins a *source IP's successive (different) connections* to
   the same WAN within the timeout window. This is source-IP affinity *across*
   connections, useful for sites that dislike a mid-session IP change (some
   banking/HTTPS). It is OPTIONAL for this build.

Member / metric / weight semantics:

- Members on the **same metric** load-balance; `weight` is the ratio of *new
  flows* among them. `weight` is only meaningful among equal-metric members.
- If member metrics **differ**, the lower-metric member takes ALL traffic and
  weight is ignored (the higher-metric member becomes pure standby).
- This build uses **active-active**: both balanced members on member metric `1`,
  weights driven by the healthcheck. One `balanced` policy lists both members;
  one catch-all rule (`dest_ip 0.0.0.0/0`) uses it. Dead members drop out of the
  live pool automatically (mutual failover).

> **Two independent "metric" layers — do not conflate:**
> - **Network-interface metric** (`network.wan1.metric=10`, `network.wan2.metric=20`)
>   — DISTINCT values. This is the kernel default-route metric in the *main*
>   table; distinct values let both DHCP default routes coexist (without it, only
>   one lands and the other WAN flaps offline — proven on hardware).
> - **mwan3 member metric** (`mwan3.wanN_m1_*.metric=1`) — EQUAL values for the
>   balanced members. This is mwan3's failover tier; equal = active-active.
>
> These are orthogonal: distinct *interface* metrics + equal *member* metrics is
> the correct, working combination.

mwan3 is connmark-based even on nftables (fw4) systems, so it pulls the
`iptables-nft` / `kmod-nft-*` compatibility shims. Verify with `mwan3 status`
and `iptables -t mangle -S` (or `nft list ruleset`); a missing shim leaves
mwan3 silently non-functional.

---

## 8. Healthcheck loop — feeding weights

The healthcheck is **two cooperating layers** with separate jobs. Full detail in
[`./load-balancing.md`](./load-balancing.md).

```
┌─ Layer 1: LIVENESS (authoritative up/down) ───────────────────────┐
│  mwan3track per WAN (built-in, already interface-bound)           │
│  3 DISTINCT track_ip per WAN (wan1: 1.1.1.1/8.8.8.8/9.9.9.9;      │
│  wan2: 1.0.0.1/8.8.4.4/149.112.112.112) ; reliability 2 (2-of-3); │
│  interval 10 ; down 3 ; up 3                                      │
│  → marks WAN up/down, triggers failover. The script NEVER does    │
│    up/down itself (avoid two sources of truth).                   │
└───────────────────────────────────────────────────────────────────┘

┌─ Layer 2: CAPACITY → WEIGHT (cron, every ~30 min) ────────────────┐
│  /usr/bin/wan-weight.sh                                           │
│  for each WAN (serialized, not concurrent):                       │
│    1. read mwan3 status; if WAN down → skip (don't probe a dead   │
│       link)                                                       │
│    2. resolve L3 device:                                          │
│         ubus call network.interface.<wan> status \                │
│           | jsonfilter -e '@.l3_device'                           │
│    3. BIND the probe to that WAN:                                 │
│         librespeed-cli --interface <dev> --no-upload \            │
│           --duration 8 --concurrent 2 --json                      │
│       (or curl --interface <dev> against a known large file)      │
│    4. VERIFY it used the right WAN (assert local_ip == WAN IP)    │
│    5. EWMA smooth: new = 0.6*old + 0.4*measured (state file)      │
│    6. weight = clamp(round(mbps), 1, 1000)   ← floor at 1         │
│  after all WANs, only if a weight changed > ~15%:                 │
│    uci set mwan3.<member>.weight=<N>; uci commit mwan3;           │
│    mwan3 ifup <iface>  (per changed iface — see "Why this shape") │
└───────────────────────────────────────────────────────────────────┘

  REQUIRES coreutils-timeout (busybox has no `timeout` applet; the
  probe is wrapped in `timeout` — without it every probe silently fails).
```

Why this shape:

- **Binding is the whole game.** With mwan3 active there is a load-balanced
  default route. An unbound speed test goes out "whatever WAN got picked", so it
  measures one link twice and never the other, corrupting both weights. Always
  bind and then verify the source IP used.
- **Liveness must not depend on the speed test.** mwan3track owns up/down so
  failover is fast and independent of the 30-minute capacity cadence.
- **Floor weight at 1.** `weight=0`/unset removes a member from balancing
  (effectively down); a slow-but-alive WAN must still get a sliver of traffic.
- **Reapply with `mwan3 ifup <iface>`, NOT `mwan3 reload` (verified on 2.12.0).**
  `mwan3 reload` does **not** re-evaluate member weights — a weight change +
  reload leaves the live balanced split at its OLD ratio. Only `mwan3 ifup
  <iface>` (per changed interface) or a full `mwan3 restart` applies a new weight;
  FR-H13 forbids `restart` (it tears down all ip rules incl. the VPN WAN pin). So
  the script commits then `ifup`s each changed interface. Only do this beyond the
  ~15% EWMA threshold — `ifup` cycles the interface and (with `flush_conntrack`
  set) resets that WAN's in-flight flows, so it must be rare and material.
- **Mind data burn.** A bidirectional librespeed run can move hundreds of MB per
  WAN per run; serialize the two probes, run download-only, keep the cadence at
  ~30 min, and downgrade any metered uplink to a small curl `--limit-rate`
  liveness-only probe.
- **Don't probe through the tunnel.** When the VPN is up, bind to the *physical*
  WAN device/IP, never `wgvpn`, or you measure tunnel throughput.

---

## 9. VPN toggle — how routing changes

VPN is **OFF by default** so the full active-active dual-WAN setup is intact.
The toggle is a single shell script. Full detail in [`./vpn.md`](./vpn.md).

State the system is in when VPN is OFF (default):

- `network.wgvpn.disabled = 1`, `route_allowed_ips = 0` (tunnel never owns the
  default route).
- pbr service stopped, or every pbr policy `enabled '0'`.
- All flows fall through to mwan3 → full weighted per-flow balancing + failover.
- adblock unaffected (it is in the DNS plane).

**Turning VPN ON** (order matters — tunnel up *before* pbr reload so pbr sees
the live interface):

```
1. uci set network.wgvpn.disabled='0'; uci commit network
2. ifup wgvpn                       # bring up the tunnel
3. (wait for handshake / route)
4. uci set pbr.config.enabled='1'   # and/or set target policies enabled '1'
5. service pbr reload               # installs priority-900 ip rules
```

pbr then steers matched traffic (all LAN, or selected `src_addr`/`dest_addr`)
into the `wgvpn` table; unmatched flows keep using mwan3. If pbr does not
auto-detect the tunnel, list `wgvpn` under pbr `supported_interface`.

**Turning VPN OFF** reverses it:

```
1. disable the pbr policies (enabled '0'); service pbr reload
2. ifdown wgvpn
3. uci set network.wgvpn.disabled='1'; uci commit network
```

Routing-model rules that the toggle must respect:

- **Pick exactly one tunnel model.** Either full-tunnel via `route_allowed_ips 1`
  OR split via pbr with `route_allowed_ips 0`. This build uses **split via pbr**
  so dual-WAN stays intact when the VPN is off. Setting `route_allowed_ips 1`
  while also using pbr makes the tunnel grab the default route and fight pbr.
- **Do NOT flush all conntrack on the flip.** That would reset every active
  download/TCP session. Rely on connmark-on-new-connection so in-flight flows
  finish on their current path and only NEW connections migrate into the tunnel.
- **No netmask = silent failure.** The `list addresses` on the wg interface MUST
  carry a netmask (e.g. `10.x.x.x/16`); omitting it defaults to a `/32` host
  route and the tunnel silently fails to route.
- **No preshared key.** Surfshark issues no `preshared_key` for WireGuard;
  adding a bogus one breaks the handshake.

---

## 10. Secrets

No real keys appear in this repo. The WireGuard private key, Surfshark server
public key, endpoint host, and any abuse.ch URLhaus auth-key are injected via
UCI / config at deploy time and are represented in docs as placeholders such as
`<WG_PRIVATE_KEY>`, `<SURFSHARK_SERVER_PUBKEY>`, `<SURFSHARK_ENDPOINT_HOST>`,
`<SURFSHARK_DNS_1>`, `<SURFSHARK_DNS_2>`, and `<URLHAUS_AUTH_KEY>`. The
WireGuard private key is treated as a secret and is never committed.

---

## 11. Design seams (the few places the planes touch)

| Seam | Risk if misconfigured | Resolution |
|---|---|---|
| ip-rule priority | lower priority number = evaluated first; pbr's documented default (≈`30000` — verify against the running build with `ip rule show`) is evaluated *after* mwan3's `2001-2254`, so mwan3 routes the flow first and VPN policies never match | set pbr `uplink_ip_rules_priority = 900` (numerically below the mwan3 band ⇒ evaluated first) |
| fwmark masks | widening either mask clobbers the other's connmark | keep mwan3 `0x3F00` and pbr `0x00ff0000` distinct; never widen |
| tunnel as uplink | adding `wgvpn` to a WAN zone makes mwan3 balance the tunnel | keep `wgvpn` in its own `vpn` zone; it is a pbr target, not an mwan3 member |
| DNS leak on VPN | dnsmasq upstream egresses non-tunnel WAN, or clients use DoH | route resolver via `wgvpn`, disable WAN `peerdns`, keep force-dns + block DoT/DoH |
| MTU/MSS | bulk TCP/HTTPS blackholes over the tunnel | `mtu_fix '1'` on `vpn` (and WAN) zones; consider wg MTU ~1412 |
| tunnel failover | sticky wg UDP port breaks mwan3 failover of the tunnel | flush wg conntrack from `/etc/mwan3.user` on WAN transitions |
