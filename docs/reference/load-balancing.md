# Multi-WAN Load Balancing + Speed-Test Healthcheck

Design reference for the Raspberry Pi 5 OpenWrt router (target `bcm27xx/bcm2712`,
aarch64 Cortex-A76). This document covers **Goal 1** (weighted, per-flow,
auto-failover dual-WAN) and **Goal 1a** (periodic per-WAN speed-test + liveness
healthcheck that drives mwan3 member weights).

> **Scope note.** This is the load-balancing + healthcheck plane only. DNS
> filtering (adblock), the Surfshark WireGuard VPN, and the firewall-zone /
> `pbr` integration are covered in their own reference docs. Mark-space and
> ip-rule-priority coordination with those subsystems is summarized in
> [§8 Coexistence](#8-coexistence-with-pbrwireguard-and-adblock).

---

## 1. What mwan3 does (and does not) do

`mwan3` is the OpenWrt package for weighted, **per-flow** multi-WAN with
automatic failover. It is the correct tool for this project.

How it works:

1. For each **new** connection, mwan3 selects a member (WAN) and stamps the
   connection with an `fwmark` (connmark) in the mangle table.
2. `ip rule`s route packets to a per-WAN routing table based on that mark.
3. `conntrack` carries the mark for the connection's entire lifetime, so **every
   packet of one TCP/UDP flow exits the same WAN**.

Two consequences that must be stated plainly:

- **Per-flow stickiness is intrinsic, not optional.** The "a TCP connection
  stays pinned to one WAN for its lifetime" requirement is satisfied by the
  default connmark/conntrack mechanism. No extra option is required.
- **mwan3 does NOT bond bandwidth.** A single TCP download uses exactly **one**
  WAN. A `100 + 100` Mbps setup yields ~100 Mbps single-stream, **not** 200.
  Aggregate throughput only appears across **many concurrent** flows.
  Bandwidth bonding requires MPTCP/link bonding, which is out of scope.

### 1.1 Why per-flow is correct and per-packet is wrong

Per-packet (round-robin) balancing sprays packets of a single connection across
both WANs. That breaks real traffic:

- **NAT breaks.** The two WANs have different public source IPs. A remote server
  sees packets for one TCP session arriving from two different addresses and
  drops them.
- **TLS/HTTPS breaks.** A mid-session source-IP change looks like session
  hijacking; handshakes and long-lived sessions fail.
- **Reordering and PMTU.** Two paths have different latency/MTU, producing heavy
  reordering, spurious retransmits, and throughput collapse — the opposite of
  the intended benefit.

Per-flow balancing distributes **connections** across WANs while keeping each
connection coherent on one path. This is exactly what mwan3 does by design, and
it is the right model for this router. **We do not attempt per-packet
balancing.**

---

## 2. Interface naming and the MAC-pinning prerequisite

USB-Ethernet adapter enumeration order is **not** guaranteed across reboots —
`eth1`/`eth2` can swap. If that happens, mwan3 members track the wrong physical
uplink, silently corrupting weighting and failover. **Pin every interface by MAC
address in `/etc/config/network` before configuring mwan3.**

Topology used in this doc (matches the hardware research recommendation):

| Logical | Physical | OpenWrt iface | netifd L3 device | Role |
|---|---|---|---|---|
| WAN1 | Onboard GbE (RP1, dedicated lane) | `wan` | `eth0` | Primary uplink |
| WAN2 | USB3 GbE adapter (RTL8153) | `wanb` | `eth1` | Second uplink |
| LAN | USB3 GbE adapter (RTL8153) → switch | `lan` | `eth2` | LAN |

Example MAC pin (replace placeholder MACs with the real adapter addresses from
`ip link` / `dmesg`):

```text
config device
        option name 'eth1'
        option macaddr 'AA:BB:CC:DD:EE:01'   # WAN2 USB adapter — PLACEHOLDER

config device
        option name 'eth2'
        option macaddr 'AA:BB:CC:DD:EE:02'   # LAN  USB adapter — PLACEHOLDER
```

Resolve the live L3 device for a logical WAN at runtime with:

```sh
ubus call network.interface.wan  status | jsonfilter -e '@.l3_device'    # -> eth0
ubus call network.interface.wanb status | jsonfilter -e '@.l3_device'    # -> eth1
```

The healthcheck script (§5) resolves devices this way rather than hard-coding
`ethN`, so it survives any reordering that slips past the MAC pin.

---

## 3. mwan3 configuration model — 2 weighted WANs, active-active

Design choice: **both members share the same `metric '1'`** so they
load-balance, with `weight` driving the split. Equal metric also gives
**mutual failover** for free — when one member is marked down, mwan3 removes it
from the live pool and all new flows go to the survivor.

> If instead you want WAN2 to be a pure cold standby (no traffic until WAN1
> dies), give it a **higher** metric (e.g. WAN1 metric 1, WAN2 metric 2). Then
> `weight` is ignored — the lower-metric member takes **all** traffic until it
> fails. We use **equal metric** here because Goal 1 wants both WANs carrying
> weighted traffic.

### 3.1 `/etc/config/mwan3` (full example)

```text
config globals 'globals'
        option mmx_mask '0x3F00'         # mark bits reserved for mwan3 (default)
        # Do NOT widen this mask. pbr uses 0x00ff0000 (non-overlapping). See §8.

#### Interfaces (tracking / liveness) ####

config interface 'wan'
        option enabled '1'
        option family 'ipv4'
        list track_ip '1.1.1.1'          # Cloudflare anycast
        list track_ip '8.8.8.8'          # Google anycast
        option track_method 'ping'
        option reliability '2'           # need >=2 of the track_ip to reply
        option count '1'
        option timeout '4'
        option interval '10'
        option failure_interval '5'      # probe faster while transitioning
        option recovery_interval '5'
        option down '3'                  # 3 consecutive fails => offline
        option up '3'                    # 3 consecutive oks   => online
        option flush_conntrack 'ifdown'
        list flush_conntrack 'disconnected'
        option initial_state 'online'

config interface 'wanb'
        option enabled '1'
        option family 'ipv4'
        list track_ip '9.9.9.9'          # Quad9 anycast
        list track_ip '8.8.4.4'          # Google anycast (secondary)
        option track_method 'ping'
        option reliability '2'
        option count '1'
        option timeout '4'
        option interval '10'
        option failure_interval '5'
        option recovery_interval '5'
        option down '3'
        option up '3'
        option flush_conntrack 'ifdown'
        list flush_conntrack 'disconnected'
        option initial_state 'online'

#### Members (metric + weight) ####
# Naming convention: <iface>_m<metric>_w<weight>. Equal metric => balance.
# Weights below are SEEDS; the healthcheck overwrites them at runtime.

config member 'wan_m1_w5'
        option interface 'wan'
        option metric '1'
        option weight '5'

config member 'wanb_m1_w5'
        option interface 'wanb'
        option metric '1'
        option weight '5'

#### Policy ####

config policy 'balanced'
        list use_member 'wan_m1_w5'
        list use_member 'wanb_m1_w5'
        option last_resort 'unreachable'   # if BOTH down: reject, don't leak

#### Rules (top-to-bottom, first match wins) ####

config rule 'balanced_rule'
        option dest_ip '0.0.0.0/0'
        option proto 'all'
        option use_policy 'balanced'
        option sticky '0'                  # see §3.3 re: source-IP affinity
```

Key option reference:

- **`track_ip`** — probe targets that mark a WAN up/down. These must be
  reachable **only** via the WAN being tested (mwan3 source-routes the probe out
  that interface). Use distinct public anycast IPs per WAN as shown so a single
  resolver outage cannot flap both links simultaneously.
- **`reliability`** — minimum successful replies to count a test as up. **Must
  be `<=` the number of `track_ip` entries**, or the interface *never* comes up.
  We set `2` with two `track_ip`s.
- **`down` / `up`** — consecutive failed/good tests before state flips. `3` with
  `interval 10` ⇒ ~30 s to declare a WAN dead, ~30 s to bring it back.
- **`flush_conntrack ifdown` / `disconnected`** — when a WAN drops, flush its
  pinned flows so they re-balance to the survivor instead of black-holing.
- **`last_resort 'unreachable'`** — when *both* members are down, reject traffic
  rather than leaking it out an unintended path.

### 3.2 Weight semantics

- `weight` is meaningful **only among members sharing the same metric**.
- Among equal-metric members, the per-WAN share of **new flows** ≈
  `weight_i / Σ weight`. Example: weights `6` and `4` ⇒ ~60 % / 40 % of new
  connections to WAN1 / WAN2.
- mwan3 documents no upper bound on `weight` (default 1, no published max); we
  constrain the healthcheck to the range **1–1000** by convention.
- **Never set weight `0` or leave it unset** on a live member — that removes it
  from balancing (effectively "down"). The healthcheck floors every computed
  weight at `1` so a slow-but-alive WAN still carries a sliver of traffic.

### 3.3 Stickiness — what is already guaranteed vs. the optional layer

- **Per-connection stickiness (the Goal 1 requirement)** is guaranteed by
  connmark/conntrack with **no option set**. `option sticky '0'` above does not
  weaken this — a single TCP flow still rides one WAN for its life.
- **`option sticky '1'` (rule-level)** is a *different* feature:
  **source-IP affinity across separate connections**. It pins a client's
  *subsequent* new connections to the same WAN within `option timeout` (default
  600 s). This helps sites that dislike a client's IP changing between requests
  (some banking/HTTPS portals). It is **optional** for this build. To enable:

  ```text
  config rule 'balanced_rule'
          option dest_ip '0.0.0.0/0'
          option proto 'all'
          option use_policy 'balanced'
          option sticky '1'
          option timeout '600'
  ```

---

## 4. Runtime weight changes and liveness commands

### 4.1 Applying a new weight (what the healthcheck does)

```sh
uci set mwan3.wan_m1_w5.weight='6'
uci set mwan3.wanb_m1_w5.weight='4'
uci commit mwan3
mwan3 restart           # rebuilds rules/routes from config
```

> **`mwan3 restart` is disruptive.** It tears down and rebuilds all ip rules and
> routing tables; in-flight connections can hiccup at the moment of reload. We
> therefore only `commit + restart` when a weight has **actually changed beyond
> a threshold** (§5.4), not on every cron tick.

### 4.2 Liveness marking (prefer over full restart)

To take a WAN out of / back into rotation without rebuilding everything:

```sh
mwan3 ifdown wanb       # remove wanb from the live pool
mwan3 ifup   wanb       # restore it
```

These are less disruptive than `restart`. In normal operation **mwan3track owns
up/down** (via `track_ip`); the script should not call `ifdown`/`ifup` itself
(see §6).

### 4.3 Observability commands

| Command | Shows |
|---|---|
| `mwan3 status` | Everything (interfaces, policies, rules, connected) |
| `mwan3 interfaces` | Per-iface online/offline + tracking state |
| `mwan3 policies` | Active policies with computed % distribution |
| `mwan3 rules` | Active rules in priority order |
| `mwan3 use <iface> <cmd>` | Run `<cmd>` bound to one iface, bypassing mwan3 rules |

`mwan3 use <iface> <cmd>` is the clean way to force a probe out a specific WAN
(§5.1, alternative binding method).

---

## 5. Periodic speed-test healthcheck (Goal 1a)

The healthcheck is a **separate plane** from liveness. Two cooperating layers:

| Layer | Owner | Job | Cadence |
|---|---|---|---|
| **Liveness / failover** | `mwan3track` (built-in) | Marks WANs up/down via `track_ip` | every 10 s |
| **Capacity weighting** | cron shell script | Measures per-WAN Mbps, writes `weight` | every 30 min |

**Liveness must never depend on the speed test running.** mwan3track decides
up/down on its own short interval; the script only *reads* that status and
adjusts weight.

### 5.1 Binding the probe to a specific WAN — the whole game

With mwan3 active there is a **load-balanced default route**. An **unbound**
speed test exits "whatever WAN got picked", so you measure one link twice and
never the other, corrupting both weights. **Every probe must be explicitly bound
to its WAN, and the bind must be verified.**

**Primary tool: `librespeed-cli`** (OpenWrt package `librespeed-cli`, available
for aarch64). It supports source binding and JSON output and is more
predictable than curl's interface binding.

```sh
# Bind by source IP (the WAN's own IPv4 address):
librespeed-cli --source "$src_ip" --no-upload --duration 8 --concurrent 2 --json
# (--source and --interface are mutually exclusive; we use --source so the
#  verify step can assert directly on the source IP netifd already gives us)
```

**Fallback: `curl`** (install `opkg install curl`; the stock `uclient-fetch`
won't do this). Modern curl uses prefix syntax and reports the source IP used:

```sh
curl --interface 'if!eth1' -o /dev/null \
     --limit-rate 80M \
     -w '%{speed_download} %{local_ip}\n' \
     https://<fast-cdn>/<known-large-file>
```

**Verify the bind** before trusting any number: assert the source IP the probe
actually used matches that WAN's IP. Because we bind with `--source "$WAN_IP"`,
the verify is the same IP we passed in — the probe cannot egress any other WAN
without the source address being invalid for that link.

```sh
WAN_IP=$(ubus call network.interface.wanb status | jsonfilter -e '@["ipv4-address"][0].address')
librespeed-cli --source "$WAN_IP" --no-upload --duration 8 --concurrent 2 --json
# Cross-check: if librespeed (or a parallel curl --interface "if!$dev" probe)
# reports a local IP != WAN_IP, DISCARD the sample — the probe was mis-bound.
```

> **Alternative bind via mwan3:** `mwan3 use wanb librespeed-cli --json ...`
> runs the command bound to `wanb`, bypassing mwan3's balancing rules. Either
> approach is acceptable; pick one and verify the source IP regardless.

### 5.2 Tool choice rationale

- **`librespeed-cli`** — recommended. In opkg for aarch64; `--source` (canonical
  bind here), `--interface`, `--json`, `--duration`, `--concurrent`,
  `--no-upload/--no-download`.
- **`curl`** — light built-in-ish fallback; `--limit-rate` caps the burst.
  Interface-binding syntax varies across curl builds — test `if!ethN` first.
- **Ookla `speedtest`** — **avoid**. Not in opkg feeds; needs a hand-placed
  static binary and an interactive EULA (`--accept-license --accept-gdpr`), which
  easily hangs a cron job on the prompt.
- **Python `speedtest-cli` (sivel)** — **avoid**. Binds only by `--source` (like
  our canonical path) but pulls in full CPython, heavy for the router.

### 5.3 Cadence and guardrails (don't saturate links / burn data)

- **Cadence: every 30 minutes** via cron. Balances freshness against the data a
  full-rate test burns. (A 15 s bidirectional run can move **hundreds of MB per
  WAN per run**.)
- **Download-only** (`--no-upload`) unless upload weighting matters.
- **Short, throttled burst:** `--duration 8 --concurrent 2` (librespeed) so the
  probe itself doesn't skew live traffic; or `--limit-rate ~70-80%` of the known
  ceiling with curl.
- **Serialize the two WAN probes** — never run both concurrently. The two USB3
  NICs share the RP1 5 Gbps upstream and CPU softirq budget; concurrent probes
  throttle each other and report falsely low capacity. Probe WAN1, parse, then
  probe WAN2.
- **Skip dead links.** If `mwan3 interfaces` reports a WAN offline, don't probe
  it — leave its weight as-is (mwan3 has already removed it from the pool).
- **Metered uplinks:** stretch the cadence (2–4 h) or downgrade that WAN to a
  tiny `curl` byte-count liveness probe with `--limit-rate`, never a full-rate
  test. (Not needed if both WANs are unmetered wired links — documented for
  completeness.)
- **EWMA smoothing:** smooth measurements so one bad sample doesn't slam the
  weights: `new = 0.6*old + 0.4*measured`, persisted in a state file.
- **Threshold the reload:** only `commit + mwan3 restart` when a smoothed weight
  changed beyond ~15 %, to avoid churning routing tables every tick (§4.1).
- **Don't measure through the tunnel.** Bind to the **physical** WAN device/IP,
  never the `wg` interface, or you'll measure tunnel throughput instead of raw
  uplink capacity.

### 5.4 Mbps → weight mapping algorithm

Goal: turn measured per-WAN download Mbps into integer `weight`s in `[1, 1000]`,
proportional to capacity, floored at 1, smoothed across runs.

```text
INPUTS:  measured[wan]  = download Mbps from a verified, bound probe (or NULL if down/skipped)
STATE:   ewma[wan]      = persisted smoothed Mbps from prior runs (state file)
PARAMS:  ALPHA = 0.4        # weight of the new sample in the EWMA
         WMAX  = 1000        # self-imposed clamp; mwan3 documents no hard max
         RELOAD_DELTA = 0.15 # 15% smoothed-weight change before reloading mwan3

for each wan in {wan, wanb}:
    if measured[wan] is NULL:          # mwan3 says down, or bind failed -> skip
        continue                       # leave existing weight untouched
    if ewma[wan] exists:
        ewma[wan] = ALPHA*measured[wan] + (1-ALPHA)*ewma[wan]
    else:
        ewma[wan] = measured[wan]      # first run: seed with the raw sample

# Proportional mapping against the fastest live WAN, so the slow WAN keeps a share.
peak = max(ewma[w] for w in live_wans)
for each live wan:
    raw      = round(WMAX * ewma[wan] / peak)
    new_w[wan] = clamp(raw, 1, WMAX)   # FLOOR AT 1 — never 0

# Apply only meaningful changes.
changed = false
for each live wan:
    old_w = uci_get(mwan3.<member>.weight)
    if abs(new_w[wan] - old_w) / max(old_w,1) >= RELOAD_DELTA:
        uci_set(mwan3.<member>.weight = new_w[wan])
        changed = true
    log("wan=%s mbps=%.1f ewma=%.1f old_w=%d new_w=%d" ...)

if changed:
    uci commit mwan3
    mwan3 restart
persist ewma to state file
```

Worked example: WAN1 EWMA 940 Mbps, WAN2 EWMA 470 Mbps ⇒ peak = 940 ⇒
WAN1 weight `1000`, WAN2 weight `round(1000*470/940)=500`. New flows split
~67 % / 33 %, matching the ~2:1 measured capacity.

### 5.5 Shell sketch (`/usr/bin/wan-weight.sh`)

Skeleton only — production version adds locking (`flock`), the EWMA state file,
and the verified-bind check from §5.1.

```sh
#!/bin/sh
# Per-WAN capacity probe -> mwan3 weight. Cron: */30 * * * *
# Liveness is owned by mwan3track; this script only adjusts weight.

set -eu
TAG=wan-weight
WMAX=1000

# logical mwan3 iface  ->  member section name
WANS="wan:wan_m1_w5 wanb:wanb_m1_w5"

is_up() {                      # read mwan3's authoritative state; never set it
        mwan3 interfaces | grep -qE "interface $1 is online"
}

src_ip() {                     # the WAN's own IPv4 address (netifd-assigned)
        ubus call "network.interface.$1" status \
        | jsonfilter -e '@["ipv4-address"][0].address'
}

probe_mbps() {                 # bound by source IP, throttled, download-only, JSON
        ip=$(src_ip "$1")
        [ -n "$ip" ] || return 1
        librespeed-cli --source "$ip" --no-upload \
                       --duration 8 --concurrent 2 --json 2>/dev/null \
        | jsonfilter -e '@.download'    # Mbps; see Open questions re: @.download vs @[0].download
}

changed=0
for pair in $WANS; do
        iface=${pair%%:*}
        member=${pair##*:}

        if ! is_up "$iface"; then
                logger -t "$TAG" "skip $iface (mwan3 reports down)"
                continue
        fi

        mbps=$(probe_mbps "$iface" || echo "")
        [ -n "$mbps" ] || { logger -t "$TAG" "probe failed on $iface"; continue; }

        # --- EWMA smoothing + proportional map elided for brevity (see §5.4) ---
        new_w=$(awk -v m="$mbps" -v cap="$WMAX" \
                'BEGIN{w=int(m+0.5); if(w<1)w=1; if(w>cap)w=cap; print w}')

        old_w=$(uci -q get "mwan3.$member.weight" || echo 1)
        logger -t "$TAG" "$iface mbps=$mbps old_w=$old_w new_w=$new_w"

        # threshold check (>=15%) before marking a reload as needed
        if awk -v o="$old_w" -v n="$new_w" \
               'BEGIN{d=(o>0?(n>o?n-o:o-n)/o:1); exit !(d>=0.15)}'; then
                uci set "mwan3.$member.weight=$new_w"
                changed=1
        fi

        sleep 5     # serialize: never probe both WANs at once
done

if [ "$changed" = 1 ]; then
        uci commit mwan3
        mwan3 restart
        logger -t "$TAG" "weights committed; mwan3 restarted"
fi
```

Cron entry (`/etc/crontabs/root`):

```text
*/30 * * * * /usr/bin/wan-weight.sh
```

(Enable cron with `/etc/init.d/cron enable && /etc/init.d/cron start`.)

---

## 6. Two sources of truth — keep them separate

| Decision | Owner | Never done by |
|---|---|---|
| Is a WAN up or down? | `mwan3track` (`track_ip` probing) | the weight script |
| How much weight does a live WAN get? | weight script (`uci ... weight`) | mwan3track |

If the script also implemented its own liveness, it would fight mwan3track and
create contradictory state. The script **reads** `mwan3 interfaces`/`mwan3
status` and only ever writes `weight`. It must never call `mwan3 ifup/ifdown` to
"fix" a link.

---

## 7. Required packages and platform notes

- `mwan3`, `luci-app-mwan3`
- `librespeed-cli` (primary probe), optionally `curl` (fallback probe)
- `jsonfilter` (in base), `conntrack-tools` (for conntrack flushing/inspection)
- USB NIC driver baked into the image: `kmod-usb-net-rtl8152` + `r8152-firmware`
  (RTL8153 adapters) — pre-select in Firmware Selector so the USB WANs come up
  on first boot.
- **fw4/nftables note:** modern OpenWrt (22.03+/24.10/25.12) defaults to fw4
  (nftables), but mwan3 is still connmark/iptables-based and pulls
  `iptables-nft` compatibility shims. If those are missing, **mwan3 is silently
  non-functional.** Verify with `mwan3 status` and `iptables -t mangle -S`
  (or `nft list ruleset`) after install.
- **Power:** use the official 27 W (5V/5A) PSU or a powered USB3 hub. An
  underpowered supply browns out bus-powered USB NICs, causing link flaps that
  masquerade as "mwan3 marking WAN down" bugs.

---

## 8. Coexistence with pbr/WireGuard and adblock

The three subsystems live in **non-overlapping planes**, so they coexist
cleanly. Summarized here; full detail is in the VPN/firewall docs.

- **Mark spaces don't overlap.** mwan3 uses `mmx_mask 0x3F00`; `pbr` uses
  `fw_mask 0x00ff0000`. **Do not widen `mmx_mask`** — that would clobber pbr's
  marks and break VPN routing silently.
- **ip-rule ordering is the one real conflict.** mwan3's marked-outbound rules
  sit in the 2001–2254 priority band. `pbr` defaults to priority 30000, which
  would lose to mwan3. The documented fix: set pbr
  `uplink_ip_rules_priority = 900` so VPN-matched flows are steered first and
  everything else falls through to mwan3's balancer.
- **adblock is orthogonal.** It filters at DNS-resolution time, before routing —
  unaffected by which WAN a flow later uses.
- **VPN-up degrades dual-WAN to failover.** When the Surfshark WireGuard tunnel
  is up with `AllowedIPs 0.0.0.0/0`, tunneled traffic egresses **exactly one
  WAN at a time** — mwan3 cannot bond the two lines for VPN traffic, so for that
  traffic dual-WAN becomes **failover only**. mwan3 still load-balances all
  non-tunneled flows and still tracks/fails over both physical WANs. Note also
  that WireGuard pins its client UDP source port, so its conntrack does not
  migrate cleanly on WAN failover; the fix (flush the wg conntrack from
  `/etc/mwan3.user` on WAN transition so the tunnel re-handshakes over the
  survivor) is documented in the VPN reference.

---

## 9. Validation checklist

1. `ip link` / `dmesg` confirm the USB NIC chipset is RTL8153 (not a counterfeit
   2.5G variant needing a different kmod), and MAC pins hold across a reboot.
2. `mwan3 interfaces` shows both `wan` and `wanb` **online**.
3. `mwan3 policies` shows the `balanced` policy with a non-zero % to each member.
4. Unplug WAN1; within ~30 s `mwan3 interfaces` shows it offline, conntrack is
   flushed, and traffic continues over WAN2. Re-plug; it returns within ~30 s.
5. Run `wan-weight.sh` by hand; `logger`/`logread -e wan-weight` shows a probe
   that **used the correct source IP** for each WAN (bind verified), the
   computed weights, and whether a reload fired.
6. Confirm a single large download saturates **one** WAN (per-flow), and that
   many parallel downloads spread across both roughly in the weight ratio.
7. `iptables -t mangle -S` (or `nft list ruleset`) shows mwan3's mark rules
   present — proves the iptables-nft shim is working.

---

## Open questions / to resolve at build time

- **Real adapter MACs and link rates.** The MAC pins and seed weights in §3.1
  are placeholders; capture the actual addresses and a baseline speed-test before
  finalizing.
- **Fast known-large-file URL for the curl fallback.** §5.1 references
  `https://<fast-cdn>/<known-large-file>` — pick a stable, high-bandwidth CDN
  object (or stand up a local one) if curl is used instead of librespeed.
- **librespeed JSON field name for download.** §5.5 parses `@.download`; some
  builds emit an array, so the runbook (Phase 6.2) falls back to
  `@[0].download`. Confirm the exact key emitted by the installed
  `librespeed-cli` build and use the **same** `jsonfilter` expression in both
  §5.5 and the runbook.
- **Metered-link policy.** This design assumes two unmetered wired uplinks. If
  either WAN is ever a metered LTE/5G link, switch it to the liveness-only /
  rate-limited probe path described in §5.3 before enabling the full probe.
- **EWMA tuning.** `ALPHA = 0.4` and `RELOAD_DELTA = 0.15` are starting points;
  tune against observed link stability to trade responsiveness vs. reload churn.
