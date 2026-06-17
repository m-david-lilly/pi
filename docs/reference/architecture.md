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
- **OpenWrt target:** `bcm27xx/bcm2712`, device profile `rpi-5`, **current
  stable** release (pin the exact version against the live OpenWrt download
  page — verify against live source), **squashfs** image. Use only the aarch64
  image for this target; an image for a different Raspberry Pi target will not boot.
- **NIC layout:**
  - Onboard gigabit Ethernet (wired to the RP1 I/O controller over a dedicated
    lane, **not** shared with the USB bus) → used as the primary WAN uplink.
  - 2× USB3 gigabit Ethernet adapters (RTL8153 via `kmod-usb-net-rtl8152`, or
    AX88179 via `kmod-usb-net-asix-ax88179`) on the two USB3 (blue) ports → one
    is the second WAN uplink, one is the LAN port to the switch.
- **Interface pinning:** USB NIC enumeration order is not guaranteed across
  reboots (eth1/eth2 can swap). **Both** USB NICs — the WAN2 uplink *and* the
  LAN port — are pinned by MAC address in `/etc/config/network`; the onboard
  GbE (eth0) is fixed and needs no pin. Pinning the WAN2 NIC keeps the mwan3
  member-to-WAN mapping stable (a correctness requirement for per-flow
  stickiness and weighting); pinning the LAN NIC keeps the trusted LAN zone from
  landing on a WAN slot after a reboot-time swap.

The Pi 5's two USB3 ports are **independent 5 Gbps xHCI controllers** in RP1 (not the
Pi 4's single shared 5 Gbps path), so two USB3 GbE NICs at ~1 Gbps each do not contend at
the controller level. The only shared resource is RP1's **PCIe 2.0 x4 uplink to the SoC**
(~16 Gbps), which has ample headroom for 1 GbE + 2×5 Gbps USB. See
[`./hardware.md`](./hardware.md) §4 for the full bandwidth model.

---

## 2. Network topology

```
                          INTERNET
                  ┌──────────┴───────────┐
            ISP / Uplink A          ISP / Uplink B
            (modem/ONT 1)           (modem/ONT 2)
                  │                       │
        onboard GbE (eth0)      USB3 GbE #1 (eth1)
        RP1 dedicated lane      RTL8153 / AX88179
                  │                       │
   ┌──────────────┼───────────────────────┼──────────────────────┐
   │  Raspberry Pi 5  —  OpenWrt (bcm2712 / aarch64)             │
   │                                                             │
   │   WAN1  (network: wan)         WAN2  (network: wanb)        │
   │   zone: wan   masq=1           zone: wanb  masq=1           │
   │      │                            │                         │
   │      └──────────┬─────────────────┘                         │
   │                 │  mwan3 (per-flow connmark balancer)       │
   │                 │  + dnsmasq/adblock (DNS plane)            │
   │                 │  + pbr (VPN policy routing, OFF default)  │
   │                 │                                           │
   │             wgvpn (proto wireguard)  ── Surfshark           │
   │             zone: vpn  masq=1 mtu_fix=1                     │
   │             route_allowed_ips=0 (split via pbr, always)     │
   │             interface disabled=1  (tunnel OFF by default)   │
   │                 │                                           │
   │              LAN (network: lan)                             │
   │              USB3 GbE #2 (eth2)  zone: lan  masq=0          │
   └─────────────────┬───────────────────────────────────────────┘
                     │
              ┌──────┴──────┐
              │  LAN switch │
              └──┬───┬───┬──┘
                 │   │   │
              client client client ...
              (DNS forced to the Pi resolver via force-dns)

  Optional / secondary: Pi 5 internal WiFi as a weak AP, OR a separate
  dedicated AP device hung off the LAN switch (recommended over the
  internal radio).
```

Logical mapping summary:

| Role | Physical NIC | OpenWrt iface (`network`) | Firewall zone | NAT (`masq`) |
|---|---|---|---|---|
| WAN1 (primary) | onboard GbE | `eth0` → `wan` | `wan` | 1 |
| WAN2 (secondary) | USB3 GbE #1 | `eth1` → `wanb` | `wanb` | 1 |
| LAN | USB3 GbE #2 | `eth2` → `lan` | `lan` | 0 |
| VPN tunnel | (virtual) | `wgvpn` (wireguard) | `vpn` | 1 |

> The `vpn` zone also carries `mtu_fix '1'` (a separate zone option, not a value
> of `masq`) to MSS-clamp tunneled TCP. See §4 for the full per-zone options.
>
> Device names `eth0/eth1/eth2` are illustrative. The actual L3 device for each
> network is resolved at runtime via
> `ubus call network.interface.<name> status | jsonfilter -e '@.l3_device'` and
> the NICs are MAC-pinned in `/etc/config/network`.

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
│  luci-app-wireguard   (web UI; read/write of /etc/config/*)      │
└──────────────────────────────────────────────────────────────────┘
        │ reads/writes UCI config; does not sit in the data path
        ▼
┌──────────────────────────────────────────────────────────────────┐
│  PLANE 1 — DNS / FILTERING                                       │
│  dnsmasq  +  adblock (+ luci-app-adblock)                        │
│  - adblock returns NXDOMAIN for blocked domains                  │
│  - feeds: hagezi Pro/Pro++, oisd Big, hagezi TIF, certpl, urlhaus│
│  - force-dns (adb_nftforce): DNAT LAN port-53 → local resolver   │
│  - optional encrypted upstream: https-dns-proxy (DoH) or stubby  │
│  Acts at DNS resolution time, BEFORE routing. VPN state never    │
│  breaks it.                                                      │
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
│  fw4 (nftables) per-zone masq: wan, wanb, vpn → masq=1           │
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
| DNS filtering | dnsmasq + adblock | NXDOMAIN + port-53 DNAT | n/a (NAT-layer DNS redirect) | ON |
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
  for LAN-side trust. Source of all client traffic.
- **`wan`** — `option network 'wan'` (onboard GbE), `masq '1'`, `mtu_fix '1'`,
  `input REJECT`, `forward REJECT`.
- **`wanb`** — `option network 'wanb'` (USB3 #1), same options as `wan`.
- **`vpn`** — `option network 'wgvpn'`, `masq '1'`, `mtu_fix '1'`,
  `forward REJECT`. WireGuard's per-packet overhead is **60 bytes over IPv4**
  (20 IP + 8 UDP + 32 WireGuard) ⇒ a 1500-byte path yields MTU **1440**; over
  IPv6 the outer header is 40 bytes ⇒ 80 bytes total ⇒ MTU 1420. WireGuard's
  own default MTU is 1420 (the IPv6-safe figure, also fine for IPv4). A
  conservative ~1412 (see §11) leaves extra headroom for PPPoE or
  double-encapsulated uplinks. `mtu_fix '1'` MSS-clamps so bulk TCP / HTTPS does
  not blackhole regardless of which value is set.

Forwardings (each direction is its own stanza):

- `lan → wan`
- `lan → wanb`
- `lan → vpn`

Ownership notes:

- **mwan3 owns** the choice between `wan` and `wanb` for each flow (it balances
  across the `wan`/`wanb` *network* interfaces). The zone split exists for
  firewall/NAT, not for mwan3.
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
   Pi's dnsmasq resolver. adblock has loaded the blocklists into dnsmasq; if the
   domain is on a feed, dnsmasq returns **NXDOMAIN** and the connection never
   starts. Otherwise dnsmasq resolves (optionally via an encrypted upstream:
   `https-dns-proxy`/`stubby`) and returns the real IP.
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
5. **Routing + NAT.** The ip rule routes the flow into that WAN's routing table;
   the matching zone (`wan` or `wanb`) applies `masq` and egresses.
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
- If metrics **differ**, the lower-metric member takes ALL traffic and weight is
  ignored (the higher-metric member becomes pure standby).
- This build uses **active-active**: both members on metric `1`, weights driven
  by the healthcheck. One `balanced` policy lists both members; one catch-all
  rule (`dest_ip 0.0.0.0/0`) uses it. Dead members drop out of the live pool
  automatically (mutual failover).

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
│  track_ip = 1.1.1.1, 8.8.8.8 ; reliability 2 ; interval 10 ;      │
│  down 3 ; up 3                                                    │
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
│    mwan3 restart   (or mwan3 ifdown/ifup to avoid a full reload)  │
└───────────────────────────────────────────────────────────────────┘
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
- **Don't churn the routing tables.** Only commit + reload when a weight changes
  beyond a threshold (after EWMA smoothing), because `mwan3 restart` briefly
  tears down and rebuilds ip rules and can hiccup in-flight flows.
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
