# Requirements — Raspberry Pi 5 OpenWrt Multi-WAN Router

**Status:** As-built (reconciled to deployed hardware 2026-06-20)
**Date:** 2026-06-16 (original) · 2026-06-20 (as-built reconciliation)
**Platform:** Raspberry Pi 5 (4GB) · OpenWrt `bcm27xx/bcm2712` 25.12.4 · aarch64 (Cortex-A76)

> **As-built deviations from the original design (all proven on hardware):**
> - **Topology inverted:** onboard `eth0` = LAN/management (`br-lan`, 192.168.1.1/24);
>   **both** USB RTL8153 NICs = the WANs (`uwan1`, `uwan2`). The original "fastest
>   uplink on onboard GbE" (NFR-P3) was deliberately dropped for management stability.
> - **Device names `uwan1`/`uwan2`** (never `*dev` — breaks mwan3 2.12.0's greedy
>   route-device regex), pinned by a **hotplug rule**, not `config device` (netifd
>   ignores that alias for these NICs).
> - **Distinct interface metrics** (wan1=10, wan2=20) so both default routes coexist.
> - **DNS:** https-dns-proxy with TWO DoH upstreams (Cloudflare #5053 + Google #5054),
>   auto-wiring dnsmasq + force-DNS + DoT-block.
> - **Cold-boot:** IP-literal NTP servers first (no-RTC deadlock fix).
> - **Weights:** reapplied via `mwan3 ifup`, NOT `reload` (reload is a no-op on 2.12.0).
> - **VPN:** staged default-OFF; NFR-S2 (router DNS via tunnel) NOT yet met — deferred.
> - **WiFi:** downstream NETGEAR Orbi MR60 in NAT mode; the Pi's own radio is disabled.

---

## 1. Purpose & Scope

This document specifies the functional (FR) and non-functional (NFR) requirements for a home router built on a Raspberry Pi 5 running OpenWrt. The router provides three primary capabilities plus a network baseline:

1. **Multi-WAN** weighted per-flow load balancing with automatic failover, driven by a periodic speed-test/liveness healthcheck (Goal 1 / 1a).
2. **DNS-based filtering** of ads, trackers, spyware, malware, and known-bad-actor domains (Goal 2).
3. **On-demand Surfshark VPN** (WireGuard), OFF by default, with a toggle to route all or selected traffic through the tunnel (Goal 3).
4. A **LAN / DHCP / DNS baseline** that all three capabilities sit on top of.

Requirements use [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119) keywords (MUST / MUST NOT / SHOULD / MAY) and are prioritized with [MoSCoW](https://en.wikipedia.org/wiki/MoSCoW_method) (Must / Should / Could / Won't). "Won't" is a MoSCoW priority, not an RFC 2119 keyword.

---

## 2. Hardware & Topology (Reference)

| Element | Value |
| --- | --- |
| SoC / arch | BCM2712, aarch64 ARMv8-A Cortex-A76, quad-core @ 2.4 GHz |
| OpenWrt target | `bcm27xx/bcm2712`, device profile `rpi-5` |
| Image | OpenWrt 25.12.4 (r32933), custom Firmware-Selector build with the full package set pre-baked, **squashfs** factory |
| Root storage | NVMe (PCIe Gen3 HAT+) or USB3 SSD preferred over SD card (as-built: 32GB SD, testing media) |
| Power | Official 27 W (5V/5A) USB-C PD PSU, or powered USB hub for NICs |
| **LAN / management** | **Onboard GbE (RP1, dedicated lane)** → `eth0`/`br-lan`, 192.168.1.1/24. Fixed, no MAC pin. |
| **WAN1** | USB3 GbE adapter #1 (RTL8153 / `kmod-usb-net-rtl8152`) → device `uwan1`, MAC-pinned |
| **WAN2** | USB3 GbE adapter #2 (RTL8153 / `kmod-usb-net-rtl8152`) → device `uwan2`, MAC-pinned |
| WiFi AP | Downstream NETGEAR Orbi MR60 in NAT mode off the Pi LAN; the Pi's own radio is **disabled** |

> Both USB3 NICs MUST be on USB3 (blue) ports. USB2 caps at ~300 Mbps real-world and MUST NOT carry a gigabit WAN. **Since BOTH WANs are USB, this constraint applies to both uplinks.**
>
> **Device names MUST NOT end in `dev`** — mwan3 2.12.0's greedy `s/.*dev \([^ ]*\).*/\1/` route-device regex mis-parses such names and drops the per-WAN default route. Hence `uwan1`/`uwan2`.

---

## 3. Functional Requirements

### 3.1 LAN / DHCP / DNS Baseline (FR-B)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| FR-B1 | Must | The router MUST serve a single LAN subnet (e.g. `192.168.1.0/24`) with DHCP via `dnsmasq`/`odhcpd`, leasing addresses, gateway, and DNS to clients. |
| FR-B2 | Must | The router MUST act as the LAN's recursive/forwarding DNS resolver via `dnsmasq` (the backend for adblock). All LAN DNS MUST resolve through the router. |
| FR-B3 | Must | The two USB-Ethernet WAN NICs MUST be pinned to stable names by MAC so WAN role assignment survives reboots and enumeration reordering. **As-built: a hotplug rule (`/etc/hotplug.d/net/05-rename-wan-by-mac`) renaming to `uwan1`/`uwan2` is the REQUIRED mechanism — the `/etc/config/network` `config device` MAC alias does NOT work for these RTL8153 USB NICs (netifd never applies it; verified across reload/restart/cold-boot, carrier-independent).** Names MUST NOT end in `dev` (mwan3 regex). The onboard LAN NIC (`eth0`/`br-lan`) is fixed and needs no pin. |
| FR-B4 | Must | Each WAN MUST be a distinct OpenWrt interface (`wan1`, `wan2`) with a **distinct route metric** (as-built: `wan1.metric=10`, `wan2.metric=20`) so both DHCP default routes coexist in the main table — without distinct metrics only one lands and the other WAN flaps offline (proven on hardware). **As-built both WAN interfaces share a single firewall zone named `wan`** (zone `network` list = `wan1 wan2`) with `masq '1'` + `mtu_fix '1'`; the LAN zone has `masq '0'`. Forwardings `lan→wan` and (when present) `lan→vpn` MUST be defined. (A single zone holding both WANs keeps every `src/dest='wan'` rule valid; mwan3 balances across the interfaces within it.) |
| FR-B5 | Should | DNS upstream SHOULD egress through an encrypted forwarder. **As-built: `https-dns-proxy` with TWO DoH upstream instances — Cloudflare `127.0.0.1#5053` and Google `127.0.0.1#5054`.** It auto-wires `dnsmasq` (`noresolv` + both `server=` entries) AND auto-installs the force-DNS port-53 redirect + DoT (853) block (so FR-F5's anti-bypass is largely satisfied by https-dns-proxy, not just adblock). This is the steady-state (tunnel-down) DNS upstream; when the tunnel is up, FR-V10 governs. |
| FR-B6 | Should | `peerdns` SHOULD be disabled on the WAN interfaces so ISP-pushed resolvers do not override the router's resolver. |
| FR-B7 | Could | IPv6 (DHCPv6/SLAAC via `odhcpd`) COULD be provided on LAN if both WANs deliver usable IPv6. |
| FR-B8 | Won't | The router WON'T expose its management UI (LuCI/SSH) to any WAN interface. |
| FR-B9 | Must | The NTP server list MUST lead with **IP-literal** servers (as-built: Cloudflare `162.159.200.123`/`162.159.200.1`, Google `216.239.35.0`, ahead of the pool hostnames). The Pi 5 has no battery-backed RTC, so on cold boot the clock is stale; the DoH resolver (FR-B5) fails TLS cert validation with a wrong clock, and NTP using pool *hostnames* needs DNS — which needs DoH — which needs the clock: a hard circular **deadlock** that leaves the box permanently off by years. IP-literal NTP servers let `busybox ntpd` set the clock over UDP/123 with no DNS and no TLS, breaking the loop. (Verified on hardware: deliberately set clock to 2024, reboot, it self-corrects. `fake-hwclock` is NOT in the 25.12.4 repo; IP-literal NTP is the durable, sufficient fix.) Promoted Should→Must: without it a cold boot can wedge DNS/adblock indefinitely. |

### 3.2 Goal 1 — Multi-WAN Load Balancing & Failover (FR-W)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| FR-W1 | Must | Multi-WAN MUST be implemented with `mwan3` (+ `luci-app-mwan3`) on the fw4/nftables stack, including the `iptables-nft` / `kmod-nft-*` compatibility shims mwan3 still requires for its mangle/connmark rules. |
| FR-W2 | Must | Both WANs MUST be active-active mwan3 `config member` entries with the **same member `metric '1'`**; the relative `weight` (1–1000) MUST control the share of new flows between them. **Do not conflate with the network-interface metric (FR-B4): interface metrics are DISTINCT (10/20) for default-route coexistence; mwan3 member metrics are EQUAL for active-active balancing. These are orthogonal layers.** |
| FR-W3 | Must | Balancing MUST be **per-flow (per-connection)**, not per-packet. Every packet of a given TCP/UDP flow MUST exit the same WAN for that flow's lifetime. This is satisfied intrinsically by mwan3's connmark + conntrack mechanism — no per-packet mode is permitted. |
| FR-W4 | Must | A single `policy` (e.g. `balanced`) MUST list both members, and a final catch-all `rule` (`dest_ip 0.0.0.0/0`) MUST route LAN traffic through that policy. |
| FR-W5 | Must | On the death of one WAN, all flows MUST automatically fail over to the surviving WAN; on recovery the dead WAN MUST rejoin the balancing pool. `last_resort 'unreachable'` MUST apply when both are down. |
| FR-W6 | Must | `flush_conntrack` MUST be set on `ifdown`/`disconnected` so flows pinned to a dead WAN are flushed and re-balanced to the survivor rather than black-holed. |
| FR-W7 | Should | mwan3 `mmx_mask` (default `0x3F00`) SHOULD be left at default and documented as reserved, so it never overlaps pbr's mark space (`0x00ff0000`) or any future SQM marks. |
| FR-W8 | Must | **As-built: HTTPS rule has `sticky '1'` with `timeout '14400'` (4 hours).** Source-IP affinity across separate HTTPS connections. Prevents streaming services (Netflix, Disney+, etc.) from dropping mid-session when mwan3 assigns new connections to a different WAN — these services tie DRM sessions to the public IP. Each device independently pinned; both WANs still carry streaming load. |
| FR-W9 | Won't | The router **WON'T** bond/aggregate the two WANs into one fat pipe. A single TCP stream rides one WAN; "100+100" yields ~100 Mbps single-stream, not 200. (See §6 Out of Scope.) |

### 3.3 Goal 1a — Speed-test / Liveness Healthcheck (FR-H)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| FR-H1 | Must | **Liveness/failover** MUST be owned by mwan3's built-in `mwan3track`: per-interface `track_ip` (at least three, e.g. `1.1.1.1`, `8.8.8.8`, `9.9.9.9`), `track_method 'ping'`, `interval '10'`, `down '3'`, `up '3'`. `reliability` MUST be set strictly **below** the `track_ip` count (e.g. 3 track_ips with `reliability '2'`) so that loss of a single target does not flap the WAN down; setting `reliability` equal to the track_ip count (e.g. 2 targets, `reliability '2'`) makes one packet-loss event to either target trip a failover and is prohibited. The healthcheck script MUST NOT mark interfaces up/down itself (single source of truth). |
| FR-H2 | Must | `track_ip` targets MUST be reachable **only** via the WAN under test. `reliability` MUST be ≤ the number of `track_ip` entries (otherwise the interface never comes up) and, per FR-H1, SHOULD be strictly below that count to tolerate single-target loss. |
| FR-H3 | Must | **Capacity weighting** MUST be a separate cron-driven shell script that runs a per-WAN capacity probe **bound to that WAN's L3 device / source IP**, computes a weight, and applies it via `uci set mwan3.<member>.weight=<N>; uci commit mwan3`. |
| FR-H4 | Must | The probe MUST verify it actually used the intended WAN (e.g. assert the source/`local_ip` matches that WAN's address) before trusting the result — an unbound probe silently measures whichever WAN mwan3 picked and corrupts both weights. |
| FR-H5 | Must | The script MUST skip probing a WAN that `mwan3 status` reports as down (do not probe a dead link), and MUST floor every computed weight at `1` (never `0`, which removes a member from balancing) and ceiling at `1000` (per FR-W2). |
| FR-H6 | Should | The capacity probe SHOULD use `librespeed-cli --interface <l3dev> --no-upload --duration 8 --concurrent 2 --json` (predictable interface binding). A `curl --interface if!<dev> --limit-rate` against a known large CDN file SHOULD be the lighter fallback. The L3 device SHOULD be resolved via `ubus call network.interface.<wan> status | jsonfilter -e '@.l3_device'`. If `jsonfilter` returns an empty L3 device (interface down or renamed), the probe MUST skip that WAN (treat as down) rather than running an unbound probe. |
| FR-H7 | Should | Probes SHOULD run on a sparse cadence (every 30 min default; cron `*/30 * * * *`) and SHOULD be **serialized** (probe A, parse, then probe B). The two WANs (`uwan1`/`uwan2`) are on independent 5 Gbps USB3 controllers, but both — plus the onboard LAN — share RP1's PCIe x4 uplink to the SoC and the CPU softirq budget; serializing avoids two simultaneous probes contending at that uplink / on softirqs and skewing each other's measured throughput. |
| FR-H8 | Should | Weights SHOULD be smoothed with an EWMA (`new = 0.6·old + 0.4·measured`) persisted to a state file, and reapplied only when a weight changes beyond a threshold (>15%) to avoid churning routing tables every tick. **As-built: reapply via `mwan3 ifup <iface>` per changed interface — `mwan3 reload` does NOT re-evaluate member weights on 2.12.0 (PROVEN: a weight change + reload leaves the live balanced split at its OLD ratio; only `ifup` or a full `restart` applies it). FR-H13 forbids `restart`, so `ifup` is the mechanism.** Weight mapping: each link's weight proportional to its share of total measured throughput, scaled 1–1000. Members MUST ship with a sane static default `weight` in `/etc/config/mwan3` so balancing works from boot until the first probe runs. **Capacity weighting also REQUIRES the `coreutils-timeout` package** — busybox has no `timeout` applet and the probe is wrapped in `timeout`, so without it every probe silently fails. |
| FR-H9 | Should | Reweighting MUST use `mwan3 ifup <iface>` (per changed interface), NOT a full `mwan3 restart` (which blips all ip rules incl. the VPN WAN pin) and NOT `mwan3 reload` (a no-op for weights on 2.12.0 — see FR-H8). Note: `ifup` cycles the interface and, with `flush_conntrack` set (FR-W6), resets that WAN's in-flight flows — acceptable given the >15% threshold gate keeps it rare. |
| FR-H10 | Should | Every run SHOULD log `mbps`, computed weight, and applied weight via `logger -t wan-weight` for tuning. |
| FR-H11 | Could | A metered-link mode COULD downgrade a capped uplink to liveness-only (or a tiny byte-count `curl` probe) to avoid burning a data cap — a full librespeed run can move hundreds of MB per WAN per run. |
| FR-H12 | Won't | The healthcheck **WON'T** use the Ookla `speedtest` CLI as the default probe (not in the package feeds; interactive EULA prompt can hang a cron job). librespeed-cli is the chosen tool. |
| FR-H13 | Should | Capacity reweighting SHOULD continue while the VPN is up: probes bind to the raw WAN L3 device (FR-H4) and so measure the underlying WAN, unaffected by the tunnel. A weight change MUST NOT trigger a full `mwan3 restart` that would blip the tunnel's WAN pin — use `mwan3 ifdown/ifup` (FR-H9) and re-handshake via the mwan3.user hook (FR-V11) if the tunnel's WAN changes. |

### 3.4 Goal 2 — DNS Filtering (FR-F)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| FR-F1 | Must | Filtering MUST use the OpenWrt `adblock` package (+ `luci-app-adblock`) with `adb_dns='dnsmasq'`. (`adblock-fast` is rejected — the 4GB Pi removes its memory rationale and we want the daemon's force-DNS/report/jail features.) |
| FR-F2 | Must | Blocked domains MUST resolve to NXDOMAIN. **As-built feed mix (9 feeds, ~561k domains): `oisd_big`, `certpl`, `hagezi` (multi-pro variant), `adguard`, `adguard_tracking`, `stevenblack`, `firetv_tracking`, `smarttv_tracking`, `android_tracking`.** The hagezi multi-pro category is set via `adb_categories='hag;multi-pro'`. (Do NOT add `doh_blocklist` without first confirming it does not list `cloudflare-dns.com`/`dns.google` — those are this router's own DoH upstreams; blocking them blackholes its resolver.) |
| FR-F3 | Must | Feed refresh MUST use `/etc/init.d/adblock reload` on a daily cron (`0 5 * * *`). `start`/`restart` MUST NOT be used for refresh — they only restore the cached backup, not re-download. **As-built also sets `adb_trigger='wan1 wan2'` + `adb_triggerdelay='20'`** so adblock SKIPS the premature boot run (which would write an empty blocklist before DNS/clock are ready) and instead fires on WAN ifup once connectivity + NTP-corrected clock are available. Without this gate the router is unprotected from boot until the 5am cron. |
| FR-F4 | Must | A maintainable allowlist (`/etc/adblock/adblock.allowlist`) and manual blocklist (`/etc/adblock/adblock.blocklist`) MUST exist for false-positive overrides; `/etc/init.d/adblock search <domain>` MUST be used to confirm the offending feed before allowlisting. |
| FR-F5 | Should | Anti-bypass SHOULD be enabled. **As-built: `https-dns-proxy` auto-installs BOTH the force-DNS port-53 redirect AND the DoT (853) reject** when enabled (so this is largely satisfied by https-dns-proxy, not adblock); adblock's `adb_nftforce` is also on. Known-DoH-host blocking remains best-effort. Port-53 hijack alone does not stop DoH on 443 — a documented residual (NFR-S2). |
| FR-F6 | Could | Reporting (`adb_report=1`, tcpdump-based top/blocked domains per client) and the optional GeoIP map (`adb_map=1`) COULD be enabled for observability. |
| FR-F7 | Could | `adb_tld=1` (TLD compression) COULD be enabled to cut memory; not required on 4GB. |
| FR-F8 | Won't | Jail/allowlist-only mode (`adb_jail=1`) **WON'T** be enabled on the general LAN — it black-holes all normal browsing. FireHOL-style IP lists **WON'T** be loaded into adblock (they are IP-based; that is a separate `banip`/nftset layer). |

### 3.5 Goal 3 — Surfshark VPN Toggle (FR-V)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| FR-V1 | Must | The VPN MUST be a single WireGuard client interface `wgvpn` (`wireguard-tools` + `kmod-wireguard`), configured from Surfshark's *Manual setup → Router → WireGuard* output (PrivateKey, Address, peer PublicKey, Endpoint UDP 51820). Surfshark issues **no** preshared key — a bogus `preshared_key` MUST NOT be added (breaks the handshake). |
| FR-V2 | Must | `list addresses` MUST include the netmask (e.g. `10.14.0.2/16` — use the exact netmask from your Surfshark config; do not copy this example value). A bare address defaults to a `/32` host route and the tunnel silently fails to route. |
| FR-V3 | Must | The VPN MUST be **OFF in the shipped/initial configuration** (`uci set network.wgvpn.disabled='1'; uci commit network`) so the full active-active dual-WAN setup is the steady state on a freshly-provisioned router. "Off by default" here means the as-delivered default, not a reboot-forced reset: because the FR-V6 toggle commits the `disabled` flag, the last toggled VPN state persists across reboot (an operator who turns the VPN on and reboots stays on until they toggle off). Note: because OpenWrt's `ifup` refuses to bring up an interface marked `disabled '1'`, the toggle (FR-V6) MUST clear this flag before `ifup` — see FR-V6. |
| FR-V4 | Must | Split-tunnel routing MUST be done with `pbr` (+ `luci-app-pbr`) on fw4/nftables, with `route_allowed_ips 0` on the wg interface (pbr owns routing; WireGuard MUST NOT grab the default route). Exactly one model is used — pbr split-tunnel, **not** full-tunnel via `route_allowed_ips 1`. |
| FR-V5 | Must | pbr's `uplink_ip_rules_priority` MUST be set to **900** (a lower number than the smallest mwan3 ip-rule priority, 1001, and below all mwan3 priorities 1001–2254 — see §7) so VPN-policy flows are evaluated first and unmatched traffic falls through to mwan3's balancer. Lower ip-rule priority number = consulted first. pbr's default mark (`0x00010000` / mask `0x00ff0000`) MUST be kept non-overlapping with mwan3's `0x3F00`. |
| FR-V6 | Must | An on-demand **toggle** MUST exist. **ON:** clear the disabled flag (`uci set network.wgvpn.disabled='0'; uci commit network`) → `ifup wgvpn` → wait for handshake/route → enable the relevant pbr policy → `service pbr reload`. **OFF:** disable policy → `service pbr reload` → `ifdown wgvpn` → restore the steady-state disabled flag (`uci set network.wgvpn.disabled='1'; uci commit network`). Order matters: the disabled flag MUST be cleared before `ifup` (OpenWrt's `ifup` refuses a `disabled '1'` interface — see FR-V3), and the tunnel must be up before pbr reload so pbr sees the live interface. Handshake completion MUST be detected by polling `wg show wgvpn latest-handshakes` for a recent (non-zero, within-the-last-few-seconds) timestamp, with a concrete bounded wait (e.g. poll every 1s up to a 15s ceiling). If no WAN is up (or the handshake does not complete within that bound), the ON path MUST fail cleanly — log, leave pbr policies disabled, and **roll back the interface** (`ifdown wgvpn`; restore `disabled='1'` and commit) — so a failed ON attempt does not leave a half-enabled interface pointing at a dead tunnel that would also try to start on the next reboot. |
| FR-V7 | Must | The toggle MUST support both "all LAN via VPN" (pbr policy `src_addr` = LAN subnet) and "selected clients/destinations" (per-IP/-MAC `src_addr`, or domain `dest_addr` via `dnsmasq-full` + `resolver_set dnsmasq.nftset`). Domain policies require `dnsmasq-full`, not stock dnsmasq. |
| FR-V8 | Must | The wg interface MUST be in its own firewall zone (not folded into the WAN zone, which would make mwan3 try to balance the tunnel) with `masq '1'` and `mtu_fix '1'`. The wg MTU MUST be sized for the **smallest-MTU WAN path the tunnel can ride**, not assumed-1500: ~1412 on a 1500-byte WAN, but if either WAN is PPPoE/1492 (or otherwise smaller) the wg MTU MUST be reduced to that smaller path minus WireGuard's ~80-byte overhead, because on failover the single wg MTU must remain valid over whichever WAN carries the tunnel. `mtu_fix '1'` clamps TCP MSS but does not protect non-clamped UDP, so an over-large wg MTU still blackholes UDP after failover to a smaller-MTU WAN. |
| FR-V9 | Should | **Kill switch:** pbr `strict_enforcement 1` SHOULD be enabled so policy-matched traffic is DROPPED when the tunnel is down rather than leaking out a WAN. (Limitation: this protects forwarded LAN traffic only — it is not a router-egress kill switch.) |
| FR-V10 | Should | When the tunnel is up, the router's own DNS path SHOULD egress through the tunnel, since OpenWrt ignores the WireGuard peer's `DNS=` field. Two mutually exclusive regimes satisfy this and MUST NOT both be active: **(a)** keep the FR-B5 DoH forwarder as dnsmasq's upstream but route the forwarder's traffic through the tunnel, or **(b)** switch dnsmasq's upstream to Surfshark's plain resolvers (e.g. `162.252.172.57` / `149.154.159.92` — verify current IPs) reached through the tunnel. A DNS-leak test SHOULD confirm policied clients show only the chosen upstream. **⚠️ STATUS: NOT YET IMPLEMENTED (deferred).** The chosen mechanism (wgvpn device-bind on https-dns-proxy) is staged but not wired, since the VPN is default-OFF. Implement + leak-test at Phase 8.6 before relying on the VPN for privacy. |
| FR-V11 | Should | A `/etc/mwan3.user` hook SHOULD flush the WireGuard tunnel's conntrack — keyed on the **actual Endpoint UDP port from the Surfshark config** (commonly 51820, but the hook MUST read it from the live wg peer config rather than hardcoding 51820, since a mismatched port flushes nothing and tunnel failover silently breaks) — on mwan3 connect/disconnect so the tunnel re-handshakes over the surviving WAN; WireGuard's sticky client source port otherwise breaks tunnel failover. Toggling the VPN MUST NOT flush *all* conntrack (would reset every active download). **Consequence (MUST be documented):** because toggling the VPN ON does not flush existing conntrack, LAN flows already established out a WAN before the toggle keep egressing that WAN until they close — only *new* connections enter the tunnel. The toggle therefore does not retroactively pull in-flight flows into the tunnel; operators wanting a clean cutover must accept this window or restart the affected client connections. |
| FR-V12 | Won't | The design **WON'T** claim VPN bandwidth aggregation. With the tunnel up and the wg peer's `AllowedIPs 0.0.0.0/0` (crypto reach — note this is *not* a contradiction of FR-V4: `AllowedIPs 0.0.0.0/0` defines what the tunnel can encrypt to, while `route_allowed_ips 0` stops WireGuard from installing a system default route, leaving pbr to do the selective routing), tunneled traffic rides exactly one WAN; dual-WAN degrades to **failover-only** for VPN traffic. This honesty note MUST be surfaced in user-facing docs. |

---

## 4. Non-Functional Requirements

### 4.1 Performance (NFR-P)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| NFR-P1 | Must | The router MUST sustain line-rate NAT/routing for two ~1 Gbps WANs without the Cortex-A76 CPU being the bottleneck. **As-built both WANs (`uwan1`/`uwan2`) sit on the two independent 5 Gbps USB3 controllers** (one NIC each, well within its controller); the LAN is onboard GbE. All share RP1's ~16 Gbps PCIe x4 uplink to the SoC, far from saturated at these rates. The binding design constraint is CPU softirq / per-flow throughput, not the USB controllers. |
| NFR-P2 | Must | The capacity probe MUST be bounded (`--duration 8 --concurrent 2`, or `--limit-rate` on curl) so it does not saturate the link or materially degrade live user traffic during the probe window. |
| NFR-P3 | ~~Should~~ | **SUPERSEDED by the as-built deviation.** The original intent put the fastest uplink on the onboard GbE. As-built, the onboard port is LAN/management and **both** WANs are on USB3 — management stability across USB re-enumeration won over a marginal throughput edge on a home dual-WAN box. The two USB3 controllers are independent 5 Gbps lanes, so each gigabit WAN has ample headroom regardless. |
| NFR-P4 | Should | adblock SHOULD comfortably run XL/XXL feed tiers (500K+ domains) given 4GB RAM; memory SHOULD NOT be the limiting factor for feed selection. |

### 4.2 Reliability & Failover (NFR-R)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| NFR-R1 | Must | A single WAN failure MUST NOT take the LAN offline; surviving-WAN failover MUST be automatic via mwan3track. |
| NFR-R2 | Must | Failover detection MUST be reliable: `track_ip` reachable only via the tested WAN, `reliability` strictly below the `track_ip` count to avoid single-loss flapping (FR-H1/FR-H2), and conntrack flushed for the dead WAN (FR-W6). |
| NFR-R3 | Must | USB NIC enumeration order MUST NOT break WAN/LAN role mapping — interfaces pinned by MAC (FR-B3). |
| NFR-R4 | Should | The Pi SHOULD use the 27 W PSU (or powered USB hub) so a brownout-induced USB link flap is not misdiagnosed as a WAN-down event. |
| NFR-R5 | Should | Root storage SHOULD be NVMe/SSD, not SD, to avoid rootfs corruption from adblock list churn and logging over months. |
| NFR-R6 | Could | VPN server failover (rotating Surfshark endpoint/public-key on server death) COULD be scripted; it is distinct from WAN failover and not required for v1. |

### 4.3 Security (NFR-S)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| NFR-S1 | Must | **No plaintext secrets in version control.** The WireGuard PrivateKey and any auth keys (e.g. URLhaus abuse.ch Auth-Key) MUST be injected via UCI / a gitignored file and represented in docs and repo as placeholders (e.g. `<WG_PRIVATE_KEY>`, `<URLHAUS_AUTH_KEY>`). |
| NFR-S2 | Must | **DNS-leak prevention.** When the tunnel is up, the router's own DNS egress MUST route through the tunnel (FR-V10) and MUST NOT exit the non-tunnel WAN. **⚠️ As-built status: NOT YET MET — deferred (accepted debt).** The wgvpn device-bind on https-dns-proxy is staged but not wired (VPN is default-OFF); if the VPN is brought up today the router's own upstream DNS egresses a physical WAN (DoH-encrypted but not tunnel-routed). Close at Phase 8.6 before using the VPN for privacy. Client DoH suppression is **best-effort** (force-DNS + DoT/853 block + known-DoH-host blocking, FR-F5); arbitrary DoH on 443 to unlisted hosts cannot be fully prevented — a documented residual. Force-DNS MUST stay on regardless of VPN state. |
| NFR-S3 | Must | **Kill switch.** pbr `strict_enforcement 1` MUST be available so policy-matched traffic does not leak to a WAN when the tunnel drops (FR-V9), with its router-egress limitation documented. |
| NFR-S4 | Must | Untrusted command/probe output MUST be validated where the healthcheck/toggle scripts parse it (`ubus`/`jsonfilter`/`mwan3 status`/librespeed JSON) — parsing MUST handle empty, malformed, or unexpected results without crashing or producing weight `0`. (The scripts are root-run from cron/SSH and take no direct LAN-client input; the threat is malformed *output*, not user-supplied input.) |
| NFR-S5 | Should | Management surfaces (LuCI, SSH) SHOULD be LAN-only (FR-B8) with key-based SSH; outbound DoT/known-DoH SHOULD be blocked to enforce filtering (FR-F5). |
| NFR-S6 | Must | The firewall mark allocation (mwan3 mask `0x3F00`; pbr mark `0x00010000` / mask `0x00ff0000` — see §7) MUST be documented and masks MUST NOT be widened to overlap. (Promoted to Must: a silent mask overlap corrupts both mwan3 connmark balancing and pbr policy routing with no error — it is a hard invariant, not a best-effort.) |

### 4.4 Maintainability (NFR-M)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| NFR-M1 | Must | Configuration MUST use stock OpenWrt UCI files (`/etc/config/{network,firewall,mwan3,dhcp,pbr}`, `/etc/adblock/*`) and standard package CLIs — no out-of-tree forks. |
| NFR-M2 | Must | Shell scripts (healthcheck, VPN toggle) MUST use descriptive variable names, type/format-checked parsing, and explicit error handling (no silent failures, no generic catch-alls); comments MUST explain "why" not "what". |
| NFR-M3 | Should | All packages SHOULD be pre-baked via the OpenWrt Firmware Selector so USB NICs and services come up on first boot. **As-built package set:** `mwan3`, `luci-app-mwan3`, `adblock`, `luci-app-adblock`, `wireguard-tools`, `kmod-wireguard`, `luci-proto-wireguard`, `pbr`, `luci-app-pbr`, `dnsmasq-full`, `librespeed-cli`, `conntrack`, `kmod-usb-net-rtl8152`, `luci`, **`https-dns-proxy`** (DoH resolver), and **`coreutils-timeout`** (busybox lacks the `timeout` applet that `wan-weight.sh` needs — without it every capacity probe silently fails). (Package names verified against the live 25.12.4 feed: WireGuard LuCI integration is `luci-proto-wireguard`, not `luci-app-wireguard`; conntrack tooling is `conntrack`, not `conntrack-tools`.) |
| NFR-M4 | Should | Scripts SHOULD be idempotent and safe to re-run; cron entries SHOULD live in `/etc/crontabs/root`. |
| NFR-M5 | Could | A single documented toggle wrapper (SSH alias or LuCI custom-command button) COULD expose VPN on/off to the operator. |

### 4.5 Observability (NFR-O)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| NFR-O1 | Must | The healthcheck MUST emit per-run capacity, computed weight, and applied weight to syslog via `logger -t wan-weight`. |
| NFR-O2 | Must | mwan3 state MUST be inspectable via `mwan3 status` / `mwan3 interfaces` / `mwan3 policies` (showing computed % distribution). |
| NFR-O3 | Should | adblock SHOULD expose block status per domain (`/etc/init.d/adblock search <domain>`) and a report (`/etc/init.d/adblock report`). |
| NFR-O4 | Could | A periodic DNS-leak / IP-leak self-test COULD run with the tunnel up and log a pass/fail. |

---

## 5. Assumptions

- **A1.** Two usable WAN uplinks are available, each on a separate physical ISP/handoff. As-built both are USB3 GbE (`uwan1`/`uwan2`); the onboard GbE is LAN/management. (The original "WAN1 is the faster link on onboard" assumption no longer holds — see superseded NFR-P3.)
- **A2.** The current OpenWrt `bcm27xx/bcm2712` stable release supports all required packages for aarch64. Verify exact OpenWrt version and `librespeed-cli` availability for aarch64 against the live Firmware Selector / package feed before relying on either — do not assume a specific version.
- **A3.** The operator has a valid Surfshark subscription and can generate a WireGuard key pair / per-server config from the dashboard.
- **A4.** USB-Ethernet adapters are genuine RTL8153 or AX88179 (verified via `lsusb`/`dmesg`), not counterfeit RTL8157/2.5G variants lacking a kmod.
- **A5.** The Pi runs from the official 27 W PSU (or powered hub for the NICs) and from NVMe/SSD rather than SD for an always-on appliance.
- **A6.** Modern OpenWrt fw4/nftables is the firewall stack; mwan3's iptables-nft compatibility shims are installed.
- **A7.** Track-IP targets (`1.1.1.1`, `8.8.8.8`, etc.) are routable only via their respective WAN once source-routing is configured.
- **A8.** Package-internal behaviors asserted in this document as load-bearing facts — pbr's default mark `0x00010000` / mask `0x00ff0000` and `uplink_ip_rules_priority`, mwan3's `mmx_mask` default `0x3F00` and ip-rule priority ranges (§7), adblock's `reload` re-downloading vs `start`/`restart` restoring only the cached backup (FR-F3), Surfshark issuing no preshared key (FR-V1), OpenWrt ignoring the WireGuard peer's `DNS=` field (FR-V10), and OpenWrt's `ifup` refusing to start an interface marked `disabled '1'` (FR-V3/FR-V6) — MUST be verified against the live package source/documentation for the pinned OpenWrt release before relying on them. They are version-sensitive and could change between releases; do not treat them as guaranteed.

---

## 6. Out of Scope (Won't)

| ID | Item | Rationale |
| --- | --- | --- |
| OOS-1 | **True line bonding / bandwidth aggregation** of the two WANs into one logical pipe. | mwan3 is per-flow, not bonding. A single TCP stream uses one WAN. Aggregation needs MPTCP/bonding — far more complex and not a project goal. |
| OOS-2 | **Per-packet load balancing.** | Breaks NAT/TLS/conntrack; per-flow sticky is the explicit, correct model. |
| OOS-3 | **VPN bandwidth aggregation across both WANs.** | WireGuard rides one WAN at a time; VPN-up = failover-only for tunneled traffic. |
| OOS-4 | **Ookla `speedtest` CLI** as the capacity probe. | Not in the package feeds; interactive EULA can hang cron. librespeed-cli used instead. |
| OOS-5 | **FireHOL IP blocklists in adblock.** | IP-based, not DNS — belongs in a separate `banip`/nftset firewall layer. |
| OOS-6 | **Internal Pi 5 radio as a WiFi AP.** | The Pi's own radio is **disabled**. As-built, a downstream NETGEAR Orbi MR60 in NAT mode (off the Pi's single LAN port) is the client AP — its WAN pulls a 192.168.1.x lease from the Pi, its own LAN serves 10.0.0.0/24. (Double-NAT: the Pi sees only the MR60's WAN IP for per-client adblock reporting.) |
| OOS-7 | **A router-egress kill switch.** | pbr `strict_enforcement` only protects forwarded LAN traffic; the router itself can still reach the internet directly. |
| OOS-8 | **Full-tunnel VPN via `route_allowed_ips 1`.** | We commit to the pbr split-tunnel model; mixing both makes WireGuard fight pbr for the default route. |

---

## 7. Mark / Priority Allocation (Reference)

| Plane | Package | fwmark / mask | ip-rule priority |
| --- | --- | --- | --- |
| WAN balancing | mwan3 | mask `0x3F00` | 1001–1250 (in), 2001–2250 (out), 2253/2254 (last-resort) |
| VPN policy routing | pbr | mark `0x00010000`, mask `0x00ff0000` | **900** (set explicitly; below mwan3) |
| DNS filtering | adblock | n/a (DNS layer + nft DNS force) | n/a |

Masks are non-overlapping by design and MUST NOT be widened.

---

## 8. Open Questions

1. **Upstream-DNS-through-tunnel mechanism (FR-V10):** decide between the two FR-V10 regimes. **Regime (a)** keeps the `https-dns-proxy`/`stubby` forwarder as upstream and routes *its* egress through the tunnel — implemented either by binding the forwarder to `wgvpn` or by a pbr policy for the router's own port-53/forwarder egress. **Regime (b)** switches dnsmasq's upstream to Surfshark's plain resolvers reached through the tunnel while the tunnel is up. Both satisfy "no leak"; (a) preserves encryption-to-public-resolver, (b) matches Surfshark's own DNS. Needs a decision and a test that the router's own DNS does not exit the non-tunnel WAN. OpenWrt ignoring the WireGuard `DNS=` field is the constraint forcing this.
2. **URLhaus Auth-Key:** abuse.ch now requires a free Auth-Key for the hostfile export. Confirm whether adblock's bundled `urlhaus` feed entry accepts the key inline and where to store it (secret — placeholder only).
3. **Metered-link detection (FR-H11):** no signal yet on whether either uplink is metered/capped. If one is LTE/5G, its probe should drop to liveness-only — needs operator input.
4. **WAN count:** ~~research referenced a possible third WAN~~ **RESOLVED — as-built is 2 WAN + 1 LAN.** Both WANs are USB3 (`uwan1`/`uwan2`); LAN is onboard. A third uplink is not in play; if ever added, FR-W2/FR-W4 would extend to three same-metric members, but the Pi has only one onboard + two USB3 ports, so a 3rd WAN would require a fourth NIC and re-home the LAN behind the downstream router.
5. **Rule-level sticky (FR-W8) default:** leave off, or enable for known IP-sensitive sites? Needs an operator preference.
