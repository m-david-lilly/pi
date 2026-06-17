# VPN Design — Surfshark WireGuard on OpenWrt (Pi 5 Router)

> Scope: Surfshark VPN delivered as a WireGuard client on the Raspberry Pi 5
> OpenWrt router (bcm2712 / aarch64). VPN is **OFF by default**: full dual-WAN
> per-flow balancing + failover runs normally. An on-demand toggle routes
> all-or-selected traffic through the tunnel. Honest constraint: while the
> tunnel is up it rides exactly **one** WAN at a time — no bonding, dual-WAN
> degrades to failover for tunneled traffic.

---

## 1. Goals and non-goals

| | |
|---|---|
| **Goal** | Surfshark via WireGuard, default OFF, on-demand on/off toggle. |
| **Goal** | Selective routing: all LAN, specific clients, or specific destinations through the tunnel. |
| **Goal** | Kill-switch + DNS-leak prevention when the tunnel is up. |
| **Goal** | Keep DNS filtering (adblock) working correctly with the VPN on or off. |
| **Goal** | Coexist cleanly with mwan3 dual-WAN and adblock (no fwmark / ip-rule collisions). |
| **Non-goal** | Bandwidth aggregation / bonding across both WANs through the VPN. WireGuard rides one WAN at a time. |
| **Non-goal** | VPN-server-side failover (rotating Surfshark servers is a separate concern from WAN failover). |

---

## 2. Packages

Install on the bcm2712 / aarch64 build (NOT ARMv7):

```sh
opkg update
opkg install wireguard-tools kmod-wireguard \
             pbr luci-app-pbr \
             dnsmasq-full
```

- `wireguard-tools` + `kmod-wireguard` — the tunnel itself.
- `pbr` + `luci-app-pbr` — policy-based routing for on-demand split-tunnel.
  pbr ≥ 1.1.8 uses the **nftables/fw4** backend only (the iptables/ipset
  backend was dropped); the Pi 5 stable build is fw4-native, so this is fine.
- `dnsmasq-full` — **required** for domain-based VPN policies. Stock `dnsmasq`
  cannot populate the `dnsmasq.nftset` that pbr reads for `dest_addr` domains.
  Removing stock dnsmasq and installing `dnsmasq-full` is a one-time swap; do
  it before configuring adblock so both share the same resolver.

Depends pulled in automatically by pbr: `resolveip`, `ip-full`.

---

## 3. Obtaining Surfshark WireGuard credentials

1. Log in to the Surfshark dashboard → **VPN** → **Manual setup** → **Router**
   → **WireGuard**.
2. Generate / register a key pair. Surfshark uploads your **public** key to its
   service and shows you the **private** key once — the private key **never
   leaves your control** and is the secret you must protect.
3. Pick a server location and download (or copy) the per-server `.conf`. It
   contains:

| Field | Example | Notes |
|---|---|---|
| `PrivateKey` | `<WG_PRIVATE_KEY>` | **Secret.** Your local private key. |
| `Address` | `10.14.0.2/16` | Tunnel address. **Keep the netmask.** |
| `DNS` | `162.252.172.57`, `149.154.159.92` | Surfshark resolvers. OpenWrt **ignores** the WG `DNS=` line — configure DNS separately (§7). |
| `[Peer] PublicKey` | `<SERVER_PUBLIC_KEY>` | The chosen server's public key. |
| `Endpoint` | `xx-yyy.prod.surfshark.com:51820` | UDP. Host varies per server. |
| `AllowedIPs` | `0.0.0.0/0`, `::/0` | Full-tunnel allowed-IPs. |
| **PresharedKey** | *(none)* | Surfshark issues **no** preshared key. Do **not** add a bogus `preshared_key` — it breaks the handshake. |

> **Per-server caveat:** Surfshark gives one config per server. Rotating servers
> means updating `endpoint_host`, `endpoint_port`, and the peer `public_key`.
> Hardcoding one server gives no VPN-side failover if that server dies.

### 3.1 Secrets handling — NO plaintext keys in git

The private key is the one true secret in this design. Rules:

- **Never** commit `PrivateKey` (or any real key) to the repo. Config files
  checked into git use the placeholder `<WG_PRIVATE_KEY>`.
- Keep the real key out of `/etc/config/network` if that file is tracked. Two
  workable patterns:

**Pattern A — UCI injection from an untracked secrets file (recommended).**
Store the secret in a root-only file on the router, never in git:

```sh
# /etc/wireguard/wgvpn.secret  (chmod 600, root:root, .gitignore'd / never tracked)
WG_PRIVATE_KEY='<paste-real-private-key-here>'
```

Apply it at provisioning time with a small script (also not committing the key):

```sh
# scripts/apply-wg-secret.sh — run once on the router after flashing config
. /etc/wireguard/wgvpn.secret
uci set network.wgvpn.private_key="$WG_PRIVATE_KEY"
uci commit network
# Do NOT echo the key; do NOT write it to syslog.
```

Then your tracked `/etc/config/network` only ever contains the placeholder, and
the real value is injected on the device.

**Pattern B — `.gitignore` the secret-bearing file entirely.** If the whole
`/etc/config/network` lives in git, split the WireGuard `private_key` into an
untracked include/overlay and add it to `.gitignore`. Verify before every
commit:

```sh
git grep -nE 'private_key|PrivateKey' -- ':!*.example' && echo "STOP: secret in tree"
```

- Set file perms tight: `chmod 600 /etc/wireguard/wgvpn.secret`.
- The Surfshark **public** server key and endpoint are not secrets and may be
  committed, but treat the private key as the crown jewel.

---

## 4. WireGuard interface + peer (uci network)

The design uses a **single** interface named `wgvpn` with
`route_allowed_ips '0'` so WireGuard does **not** grab the default route — pbr
owns routing decisions. (Picking `route_allowed_ips '0'` + pbr is the
split-tunnel model; `route_allowed_ips '1'` is full-tunnel-via-WG and would
fight pbr. Choose exactly one — here we choose pbr.)

```sh
# /etc/config/network  (placeholders only — real key injected via §3.1)

config interface 'wgvpn'
    option proto 'wireguard'
    option private_key '<WG_PRIVATE_KEY>'      # injected on device, never committed
    list addresses '10.14.0.2/16'              # MUST include netmask, not /32
    option mtu '1412'                          # avoid PMTU blackhole; see §8
    option disabled '1'                        # DEFAULT OFF — tunnel down at boot
    # No 'option peerdns' here; DNS is forced explicitly (§7)

config wireguard_wgvpn
    option public_key '<SERVER_PUBLIC_KEY>'
    option endpoint_host 'xx-yyy.prod.surfshark.com'
    option endpoint_port '51820'
    option persistent_keepalive '25'
    option route_allowed_ips '0'               # pbr owns routing, not WG
    list allowed_ips '0.0.0.0/0'
    list allowed_ips '::/0'
    # NO preshared_key — Surfshark does not issue one
```

> **Top mistake (per OpenWrt WireGuard wiki):** omitting the netmask on
> `list addresses` defaults to a `/32` host route and the tunnel silently fails
> to route. Always include the mask Surfshark gives (e.g. `/16`).

---

## 5. Firewall (uci firewall)

Put `wgvpn` in its **own** zone — do **not** add it to a WAN zone. Adding it to
a WAN zone would let mwan3 try to treat the tunnel as a balanceable uplink and
double-count it.

```sh
# /etc/config/firewall

config zone
    option name 'vpn'
    list network 'wgvpn'
    option input 'REJECT'
    option output 'ACCEPT'
    option forward 'REJECT'
    option masq '1'          # NAT LAN clients to the tunnel address
    option mtu_fix '1'       # MSS clamp — prevents bulk-transfer/HTTPS hangs

config forwarding
    option src 'lan'
    option dest 'vpn'        # allow LAN -> tunnel
```

- `masq '1'` is required so forwarded LAN traffic is NAT'd to the tunnel IP.
- `mtu_fix '1'` clamps MSS; without it short requests work but large
  TCP/HTTPS transfers blackhole.
- The existing `lan -> wan1/wan2` forwardings stay as-is (non-VPN traffic).
- mwan3 has **no** interface stanza for `wgvpn` — the tunnel is a pbr *target*,
  not an mwan3 member.

---

## 6. On-demand toggle design

**Default state: OFF.** `network.wgvpn.disabled = '1'`, pbr service stopped or
all VPN policies `enabled '0'`. All flows fall through to mwan3 → full weighted
dual-WAN per-flow balancing + failover. adblock unaffected.

**What the toggle changes (routing-wise):**

- **OFF →** no `wgvpn` interface up, no pbr priority-900 ip-rules installed.
  Every flow is marked by mwan3's connmark and routed across both WANs.
- **ON →** `wgvpn` comes up, pbr installs its ip-rules at priority **900**
  (below mwan3's 2001–2254 band, so pbr is evaluated first). Policy-matched
  **new** connections route into the `wgvpn` table; everything unmatched falls
  through to mwan3 and still balances across WANs.

**Order matters:** bring the tunnel UP *before* `service pbr reload`, so pbr
sees the live interface. On the OFF path, disable the policy and reload pbr
*before* taking the tunnel down.

**Why connmark, not conntrack-flush:** pbr (like mwan3) keys on a connmark set
at connection start. Only **new** connections migrate to the tunnel when you
toggle ON; in-flight downloads finish on their current path. Do **not** flush
all conntrack on toggle — that resets every active TCP session.

### 6.1 Toggle script (illustrative)

```sh
#!/bin/sh
# /usr/bin/vpn-toggle  —  usage: vpn-toggle on | off | status

case "$1" in
  on)
    uci set network.wgvpn.disabled='0'
    uci commit network
    ifup wgvpn
    # Wait for a handshake / route before steering traffic
    i=0
    while [ "$i" -lt 15 ]; do
      wg show wgvpn latest-handshakes 2>/dev/null | grep -qv '	0$' && break
      sleep 1; i=$((i+1))
    done
    uci set pbr.config.enabled='1'
    uci commit pbr
    service pbr reload
    logger -t vpn-toggle "VPN ON (pbr policies active)"
    ;;
  off)
    # Tear down routing first, then the interface
    uci set pbr.config.enabled='0'
    uci commit pbr
    service pbr reload
    ifdown wgvpn
    uci set network.wgvpn.disabled='1'
    uci commit network
    logger -t vpn-toggle "VPN OFF (full dual-WAN restored)"
    ;;
  status)
    wg show wgvpn 2>/dev/null && service pbr status
    ;;
  *)
    echo "usage: $0 on|off|status" >&2; exit 1 ;;
esac
```

Expose this via an SSH alias or a LuCI custom-command button. The script never
prints or logs the private key.

---

## 7. DNS-leak prevention + adblock interaction

This is the **#1 VPN gotcha**. OpenWrt ignores the WireGuard peer's `DNS=`
line, so DNS routing must be configured explicitly. The router's local resolver
(dnsmasq + adblock) stays the single point of DNS for LAN clients in both
states — adblock filters at *resolution time*, before routing, so the same
blocklists apply whether the answer's traffic later egresses a WAN or the
tunnel. **VPN state never breaks adblock.**

What to enforce:

1. **Clients always use the router resolver.** Keep adblock's force-DNS on
   (`adb_nftforce` / DNAT of LAN port 53 to the router). This stops a client
   from hard-coding `8.8.8.8` and leaking around the filter.
2. **Block encrypted client DNS bypass.** Force-DNS on port 53 does **not**
   stop DoT/DoH. Add fw4 rules to REJECT outbound **TCP/UDP 853** (DoT) from
   LAN, and rely on hagezi/oisd DoH-bypass entries (and/or block known DoH
   hostnames) so Chrome/Android/iOS can't skip filtering on 443.
3. **Route the router's own upstream DNS through the tunnel when VPN is up.**
   When `wgvpn` is up, dnsmasq's upstream forwarder must egress via the tunnel,
   not the non-tunnel WAN — otherwise queries reveal browsing to the ISP even
   though traffic rides the VPN. This is a **Must** requirement (NFR-S2: DNS
   MUST NOT egress the non-tunnel WAN), so it needs a committed mechanism, not a
   choice left to install time.
   **Chosen mechanism:** bind the local encrypted forwarder
   (`https-dns-proxy`) to the `wgvpn` interface so every router-originated
   upstream query traverses the tunnel. dnsmasq forwards only to the local
   proxy (`127.0.0.1#5053`); the proxy is the sole egress path and is pinned to
   `wgvpn`, so when the tunnel is down it cannot fall back to a WAN (fail-closed
   for the router's own resolver, matching the §9 kill-switch posture for LAN
   traffic). This is preferred over a pbr policy on the router's own port-53
   egress because the bound forwarder fails closed without an extra rule and
   does not depend on pbr's `uplink_ip_rules_priority` ordering. The concrete
   config lands in **setup.md Phase 7**; the leak self-test (NFR-O4) is in
   **Phase 8.6** (see §13).
4. **Disable peerdns on the WANs** so the ISP-pushed resolver is never adopted
   as upstream.

> When measuring WAN capacity (the healthcheck) or bringing the tunnel up,
> **bind probes to the physical WAN device/IP, not `wgvpn`** — an unbound probe
> may traverse the tunnel and measure tunnel throughput, not raw uplink.

**Validate:** with the tunnel up, run a DNS-leak test from a LAN client and
confirm the egress resolver is Surfshark's, not the ISP's on either WAN.
`nslookup doubleclick.net` should still return NXDOMAIN (adblock intact).

---

## 8. Policy-based routing (pbr) — selective VPN

pbr coexists with mwan3 by living in a separate plane: its own fwmark
(`uplink_mark 0x00010000`, mask `0x00ff0000`) which does **not** overlap mwan3's
`0x3F00` mask, plus a tunable ip-rule priority.

**The one mandatory knob:** set pbr's `uplink_ip_rules_priority` to **900** so
pbr's rules are evaluated **before** mwan3's 2001–2254 band. Left at the
default (30000), mwan3 wins and your VPN policies never match.

```sh
# /etc/config/pbr

config pbr 'config'
    option enabled '0'                       # DEFAULT OFF
    option uplink_ip_rules_priority '900'    # MUST be below mwan3's 2001-2254
    option resolver_set 'dnsmasq.nftset'     # enables domain-based dest_addr
    list supported_interface 'wgvpn'         # force-add WG if auto-detect misses it

# --- All LAN through the VPN (full-tunnel-by-policy) ---
config policy
    option name 'all_lan_via_vpn'
    option src_addr '192.168.1.0/24'
    option interface 'wgvpn'
    option enabled '0'                       # toggle flips this to '1'

# --- A single client by IP ---
config policy
    option name 'tv_via_vpn'
    option src_addr '192.168.1.50'
    option interface 'wgvpn'
    option enabled '0'

# --- A single client by MAC ---
config policy
    option name 'laptop_via_vpn'
    option src_addr 'AA:BB:CC:DD:EE:FF'
    option interface 'wgvpn'
    option enabled '0'

# --- Selected destinations (domains; needs dnsmasq-full + resolver_set) ---
config policy
    option name 'streaming_via_vpn'
    option dest_addr 'example-streaming.com'
    option interface 'wgvpn'
    option enabled '0'
```

- pbr auto-detects WireGuard interfaces; `supported_interface 'wgvpn'` is a
  safety net.
- Per-policy toggle: flip `option enabled '0'` → `'1'` then `service pbr reload`.
  The §6 toggle script flips the service-level `enabled` instead, but you can
  scope to specific policies the same way.
- Domain policies (`dest_addr` with a hostname) **silently do nothing** without
  `dnsmasq-full` + `resolver_set 'dnsmasq.nftset'`; clients must also flush
  their DNS cache after enabling.

---

## 9. Kill-switch

```sh
config pbr 'config'
    ...
    option strict_enforcement '1'
```

`strict_enforcement '1'` **drops** policy-matched traffic when `wgvpn` is down
instead of leaking it out a WAN. So if the tunnel dies, a client routed
"via VPN" loses internet rather than silently falling back to the cleartext
WAN.

> **Honest limitation:** `strict_enforcement` only protects **forwarded LAN**
> traffic. The router itself can still reach the internet directly — it is not
> a router-egress kill switch. Router-originated traffic (including DNS, see §7)
> must be steered separately.

Verify the kill-switch: enable a VPN policy for a test client, run an IP-leak
test (`curl ifconfig.co` should show the Surfshark egress IP), then `ifdown
wgvpn` and confirm that client loses connectivity rather than reverting to the
WAN IP.

---

## 10. Dual-WAN interaction — the honest treatment

**WireGuard rides exactly one WAN at a time. There is no bonding.**

- With `AllowedIPs 0.0.0.0/0`, tunneled traffic egresses **one** physical WAN.
  A single download through the VPN uses one uplink — you do **not** get the
  sum of both WANs' bandwidth.
- For **tunneled** traffic, dual-WAN therefore degrades to **failover only**.
  Non-VPN flows (everything not matched by a pbr policy) keep full mwan3
  weighted per-flow balancing across both WANs.
- mwan3 still tracks and fails over the underlying WANs while the VPN is up —
  it just can't spread one tunnel across both.

**WireGuard + mwan3 failover caveat (and the fix):** WireGuard pins its client
UDP source port regardless of mwan3 WAN state, so the tunnel's conntrack entry
is sticky and will **not** migrate cleanly when the active WAN dies. The
documented fix is to flush the WireGuard UDP flow's conntrack on mwan3
WAN transitions so the tunnel re-handshakes over the surviving WAN. Add a hook
in `/etc/mwan3.user` (requires the `conntrack` package):

```sh
# /etc/mwan3.user  — re-handshake WG over the surviving WAN on failover
# $ACTION is set by mwan3 (ifup/ifdown/connected/disconnected); $INTERFACE the WAN
case "$ACTION" in
  ifdown|disconnected|connected)
    # Flush only the WireGuard UDP 51820 endpoint flow, not all conntrack
    conntrack -D -p udp --dport 51820 2>/dev/null
    conntrack -D -p udp --sport 51820 2>/dev/null
    logger -t mwan3.user "flushed WG conntrack on $ACTION ($INTERFACE) for re-handshake"
    ;;
esac
```

This flushes only the tunnel's UDP flow (not every connection), letting
WireGuard re-handshake out the live WAN instead of black-holing on the dead one.

> The WG **endpoint host route** must stay on the physical WAN. With pbr (not
> `route_allowed_ips 1`) the tunnel's own endpoint is reached over the real
> WAN, so the tunnel can connect — verify this after any routing change.

---

## 11. Layering summary (how it all coexists)

Three non-overlapping planes; only the ip-rule priority needs tuning:

| Plane | Owner | Mechanism | Mark / priority |
|---|---|---|---|
| DNS filtering | adblock | NXDOMAIN at resolve time + force-DNS DNAT | (DNS layer, no marks) |
| VPN policy routing | pbr | fwmark + ip-rules into `wgvpn` table | mark `0x00010000`, mask `0x00ff0000`, **priority 900** |
| WAN balancing/failover | mwan3 | connmark + ip-rules into per-WAN tables | mask `0x3F00`, priority 2001–2254 |
| NAT | fw4 zones | per-zone `masq` | — |

Do **not** widen either fwmask (e.g. mwan3 `mmx_mask` to `0xffffffff`) or the
two services clobber each other's connmarks.

---

## 12. Validation checklist

- [ ] `git grep -nE 'private_key|PrivateKey'` returns only placeholders / examples.
- [ ] Boot state: `wgvpn` down (`option disabled '1'`), pbr `enabled '0'`.
- [ ] `vpn-toggle on` → `wg show wgvpn` shows a recent handshake.
- [ ] With VPN on for a test client: `curl ifconfig.co` shows Surfshark egress IP.
- [ ] DNS-leak test (tunnel up) shows Surfshark resolver, not ISP, on either WAN.
- [ ] `nslookup doubleclick.net` returns NXDOMAIN with VPN on **and** off.
- [ ] Kill-switch: `ifdown wgvpn` drops the policied client (no WAN fallback).
- [ ] Large file download over the tunnel completes (MTU/MSS OK, no blackhole).
- [ ] WAN failover with VPN up: pull the active WAN, confirm WG re-handshakes
      over the survivor (mwan3.user conntrack flush fired).
- [ ] `vpn-toggle off` → full dual-WAN balancing restored, adblock still active.

---

## 13. Open questions / to confirm on hardware

- **DoH blocking completeness.** Blocking DoT/853 + hagezi DoH-bypass entries
  reduces but may not fully eliminate client encrypted-DNS bypass (apps with
  hardcoded DoH IPs). Confirm coverage against the specific client devices on
  the LAN; a stricter "reject all 443 to known DoH IPs" nftset may be needed.
- **Router-egress DNS routing under VPN — RESOLVED.** Mechanism is chosen
  (not deferred): bind `https-dns-proxy` to the `wgvpn` interface so the
  router's *own* upstream DNS traverses the tunnel and fails closed when it is
  down (§7 item 3). The pbr-policy-on-port-53 alternative is rejected. Config
  lands in setup.md Phase 7; the DNS-leak self-test (NFR-O4) is added to
  Phase 8.6 so the NFR-S2 Must requirement is verifiable on hardware rather
  than deferred. Still confirm the egress resolver is Surfshark's (not the ISP)
  via the live leak test when first brought up.
- **Surfshark MTU.** `1412` is a safe starting point; the optimal value depends
  on the active WAN's path MTU. Tune if bulk transfers still stall after
  `mtu_fix`.
- **Server rotation / VPN-side failover.** This design hardcodes one Surfshark
  endpoint. If VPN-server resilience is wanted, scripting the community Surfshark
  WireGuard API (register pubkey, pull server list + public keys) to refresh
  `endpoint_host`/`public_key` is a follow-up, out of scope here.
- **LAN subnet placeholder.** Examples assume `192.168.1.0/24`; confirm the
  actual LAN subnet before applying pbr `src_addr` policies.
```