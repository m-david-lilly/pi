# Requirements — Raspberry Pi 5 OpenWrt Multi-WAN Router

**Status:** Draft
**Date:** 2026-06-16
**Platform:** Raspberry Pi 5 (4GB) · OpenWrt `bcm27xx/bcm2712` · aarch64 (Cortex-A76)

---

## 1. Purpose & Scope

This document specifies the functional (FR) and non-functional (NFR) requirements for a home router built on a Raspberry Pi 5 running OpenWrt. The router provides three primary capabilities plus a network baseline:

1. **Multi-WAN** weighted per-flow load balancing with automatic failover, driven by a periodic speed-test/liveness healthcheck (Goal 1 / 1a).
2. **DNS-based filtering** of ads, trackers, spyware, malware, and known-bad-actor domains (Goal 2).
3. **On-demand Surfshark VPN** (WireGuard), OFF by default, with a toggle to route all or selected traffic through the tunnel (Goal 3).
4. A **LAN / DHCP / DNS baseline** that all three capabilities sit on top of.

Requirements use [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119) keywords (MUST / SHOULD / MAY / WON'T) and are prioritized with [MoSCoW](https://en.wikipedia.org/wiki/MoSCoW_method) (Must / Should / Could / Won't).

---

## 2. Hardware & Topology (Reference)

| Element | Value |
| --- | --- |
| SoC / arch | BCM2712, aarch64 ARMv8-A Cortex-A76, quad-core @ 2.4 GHz |
| OpenWrt target | `bcm27xx/bcm2712`, device profile `rpi-5` |
| Image | Stable release (25.12.x), **squashfs** factory/sysupgrade |
| Root storage | NVMe (PCIe Gen3 HAT+) or USB3 SSD preferred over SD card |
| Power | Official 27 W (5V/5A) USB-C PD PSU, or powered USB hub for NICs |
| **WAN1** | Onboard GbE (RP1, dedicated lane) — fastest, primary uplink |
| **WAN2** | USB3 GbE adapter (RTL8153 / `kmod-usb-net-rtl8152`, or AX88179 / `kmod-usb-net-asix-ax88179`) |
| **LAN** | USB3 GbE adapter → downstream switch |
| WiFi AP | Optional / secondary; dedicated AP device preferred over internal radio |

> Both USB3 NICs MUST be on USB3 (blue) ports. USB2 caps at ~300 Mbps real-world and MUST NOT carry a gigabit WAN.

---

## 3. Functional Requirements

### 3.1 LAN / DHCP / DNS Baseline (FR-B)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| FR-B1 | Must | The router MUST serve a single LAN subnet (e.g. `192.168.1.0/24`) with DHCP via `dnsmasq`/`odhcpd`, leasing addresses, gateway, and DNS to clients. |
| FR-B2 | Must | The router MUST act as the LAN's recursive/forwarding DNS resolver via `dnsmasq` (the backend for adblock). All LAN DNS MUST resolve through the router. |
| FR-B3 | Must | The two USB-Ethernet NICs MUST be pinned to stable names by MAC address in `/etc/config/network` (or a hotplug rule) so WAN/LAN role assignment survives reboots and enumeration reordering. |
| FR-B4 | Must | Each WAN MUST be a distinct OpenWrt interface in its own firewall zone with `option masq '1'` and `option mtu_fix '1'`; the LAN zone MUST have `masq '0'`. Forwardings `lan→wan1`, `lan→wan2` (and `lan→vpn` when present) MUST each be defined. |
| FR-B5 | Should | DNS upstream SHOULD egress through an encrypted forwarder (`https-dns-proxy` DoH or `stubby` DoT) with `dnsmasq` set to `noresolv` + `server=127.0.0.1#5053`. |
| FR-B6 | Should | `peerdns` SHOULD be disabled on the WAN interfaces so ISP-pushed resolvers do not override the router's resolver. |
| FR-B7 | Could | IPv6 (DHCPv6/SLAAC via `odhcpd`) COULD be provided on LAN if both WANs deliver usable IPv6. |
| FR-B8 | Won't | The router WON'T expose its management UI (LuCI/SSH) to any WAN interface. |

### 3.2 Goal 1 — Multi-WAN Load Balancing & Failover (FR-W)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| FR-W1 | Must | Multi-WAN MUST be implemented with `mwan3` (+ `luci-app-mwan3`) on the fw4/nftables stack, including the `iptables-nft` / `kmod-nft-*` compatibility shims mwan3 still requires for its mangle/connmark rules. |
| FR-W2 | Must | Both WANs MUST be active-active `config member` entries with the **same** `metric '1'`; the relative `weight` (1–1000) MUST control the share of new flows between them. |
| FR-W3 | Must | Balancing MUST be **per-flow (per-connection)**, not per-packet. Every packet of a given TCP/UDP flow MUST exit the same WAN for that flow's lifetime. This is satisfied intrinsically by mwan3's connmark + conntrack mechanism — no per-packet mode is permitted. |
| FR-W4 | Must | A single `policy` (e.g. `balanced`) MUST list both members, and a final catch-all `rule` (`dest_ip 0.0.0.0/0`) MUST route LAN traffic through that policy. |
| FR-W5 | Must | On the death of one WAN, all flows MUST automatically fail over to the surviving WAN; on recovery the dead WAN MUST rejoin the balancing pool. `last_resort 'unreachable'` MUST apply when both are down. |
| FR-W6 | Must | `flush_conntrack` MUST be set on `ifdown`/`disconnected` so flows pinned to a dead WAN are flushed and re-balanced to the survivor rather than black-holed. |
| FR-W7 | Should | mwan3 `mmx_mask` (default `0x3F00`) SHOULD be left at default and documented as reserved, so it never overlaps pbr's mark space (`0x00ff0000`) or any future SQM marks. |
| FR-W8 | Could | A rule-level `sticky '1'` with `timeout '600'` COULD be added so a client's *successive* connections pin to one WAN (helps banking/HTTPS sites sensitive to mid-session IP changes). Off by default. |
| FR-W9 | Won't | The router **WON'T** bond/aggregate the two WANs into one fat pipe. A single TCP stream rides one WAN; "100+100" yields ~100 Mbps single-stream, not 200. (See §6 Out of Scope.) |

### 3.3 Goal 1a — Speed-test / Liveness Healthcheck (FR-H)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| FR-H1 | Must | **Liveness/failover** MUST be owned by mwan3's built-in `mwan3track`: per-interface `track_ip` (e.g. `1.1.1.1`, `8.8.8.8`), `track_method 'ping'`, `reliability '2'`, `interval '10'`, `down '3'`, `up '3'`. The healthcheck script MUST NOT mark interfaces up/down itself (single source of truth). |
| FR-H2 | Must | `track_ip` targets MUST be reachable **only** via the WAN under test, and `reliability` MUST be ≤ the number of `track_ip` entries (otherwise the interface never comes up). |
| FR-H3 | Must | **Capacity weighting** MUST be a separate cron-driven shell script that runs a per-WAN capacity probe **bound to that WAN's L3 device / source IP**, computes a weight, and applies it via `uci set mwan3.<member>.weight=<N>; uci commit mwan3`. |
| FR-H4 | Must | The probe MUST verify it actually used the intended WAN (e.g. assert the source/`local_ip` matches that WAN's address) before trusting the result — an unbound probe silently measures whichever WAN mwan3 picked and corrupts both weights. |
| FR-H5 | Must | The script MUST skip probing a WAN that `mwan3 status` reports as down (do not probe a dead link), and MUST floor every computed weight at `1` (never `0`, which removes a member from balancing). |
| FR-H6 | Should | The capacity probe SHOULD use `librespeed-cli --interface <l3dev> --no-upload --duration 8 --concurrent 2 --json` (predictable interface binding). A `curl --interface if!<dev> --limit-rate` against a known large CDN file SHOULD be the lighter fallback. The L3 device SHOULD be resolved via `ubus call network.interface.<wan> status | jsonfilter -e '@.l3_device'`. |
| FR-H7 | Should | Probes SHOULD run on a sparse cadence (every 30 min default; cron `*/30 * * * *`) and SHOULD be **serialized** (probe A, parse, then probe B) so the two USB-bus NICs don't throttle each other or contend for the shared ~5 Gbps RP1/USB3 budget. |
| FR-H8 | Should | Weights SHOULD be smoothed with an EWMA (e.g. `new = 0.6·old + 0.4·measured`) persisted to a state file, and `mwan3` SHOULD only be reloaded when a weight changes beyond a threshold (e.g. >15%) to avoid churning routing tables every tick. Weight mapping: `weight = clamp(round(mbps), 1, 1000)` or proportional to the fastest link. |
| FR-H9 | Should | Reweighting SHOULD prefer `mwan3 ifdown/ifup <iface>` over a full `mwan3 restart` where possible, since `restart` briefly tears down all rules and can blip in-flight flows. |
| FR-H10 | Should | Every run SHOULD log `mbps`, computed weight, and applied weight via `logger -t wan-weight` for tuning. |
| FR-H11 | Could | A metered-link mode COULD downgrade a capped uplink to liveness-only (or a tiny byte-count `curl` probe) to avoid burning a data cap — a full librespeed run can move hundreds of MB per WAN per run. |
| FR-H12 | Won't | The healthcheck **WON'T** use the Ookla `speedtest` CLI as the default probe (not in opkg feeds; interactive EULA prompt can hang a cron job). librespeed-cli is the chosen tool. |

### 3.4 Goal 2 — DNS Filtering (FR-F)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| FR-F1 | Must | Filtering MUST use the OpenWrt `adblock` package (+ `luci-app-adblock`) with `adb_dns='dnsmasq'`. (`adblock-fast` is rejected — the 4GB Pi removes its memory rationale and we want the daemon's force-DNS/report/jail features.) |
| FR-F2 | Must | Blocked domains MUST resolve to NXDOMAIN. The feed mix MUST cover ads/trackers/telemetry **and** malware/phishing/known-bad-actors: **hagezi Pro** (or Pro++) + **oisd Big** as baseline, plus threat feeds **hagezi TIF** (or TIF-Medium), **certpl**, and **urlhaus**. |
| FR-F3 | Must | Feed refresh MUST use `/etc/init.d/adblock reload` on a daily cron (e.g. `0 5 * * *`). `start`/`restart` MUST NOT be used for refresh — they only restore the cached backup, not re-download. |
| FR-F4 | Must | A maintainable allowlist (`/etc/adblock/adblock.allowlist`) and manual blocklist (`/etc/adblock/adblock.blocklist`) MUST exist for false-positive overrides; `/etc/init.d/adblock search <domain>` MUST be used to confirm the offending feed before allowlisting. |
| FR-F5 | Should | Anti-bypass SHOULD be enabled: adblock force-DNS (`adb_nftforce`) to DNAT all LAN port-53 to the local resolver, **plus** an fw4 rule rejecting outbound DoT (TCP/UDP 853) from LAN, plus blocking of known DoH hostnames (hagezi's DoH/bypass entries help). Port-53 hijack alone does not stop DoH on 443. |
| FR-F6 | Could | Reporting (`adb_report=1`, tcpdump-based top/blocked domains per client) and the optional GeoIP map (`adb_map=1`) COULD be enabled for observability. |
| FR-F7 | Could | `adb_tld=1` (TLD compression) COULD be enabled to cut memory; not required on 4GB. |
| FR-F8 | Won't | Jail/allowlist-only mode (`adb_jail=1`) **WON'T** be enabled on the general LAN — it black-holes all normal browsing. FireHOL-style IP lists **WON'T** be loaded into adblock (they are IP-based; that is a separate `banip`/nftset layer). |

### 3.5 Goal 3 — Surfshark VPN Toggle (FR-V)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| FR-V1 | Must | The VPN MUST be a single WireGuard client interface `wgvpn` (`wireguard-tools` + `kmod-wireguard`), configured from Surfshark's *Manual setup → Router → WireGuard* output (PrivateKey, Address, peer PublicKey, Endpoint UDP 51820). Surfshark issues **no** preshared key — a bogus `preshared_key` MUST NOT be added (breaks the handshake). |
| FR-V2 | Must | `list addresses` MUST include the netmask (e.g. `10.14.0.2/16`). A bare address defaults to a `/32` host route and the tunnel silently fails to route. |
| FR-V3 | Must | The VPN MUST be **OFF by default** (`uci set network.wgvpn.disabled 1`) so the full active-active dual-WAN setup is the steady state. |
| FR-V4 | Must | Split-tunnel routing MUST be done with `pbr` (+ `luci-app-pbr`) on fw4/nftables, with `route_allowed_ips 0` on the wg interface (pbr owns routing; WireGuard MUST NOT grab the default route). Exactly one model is used — pbr split-tunnel, **not** full-tunnel via `route_allowed_ips 1`. |
| FR-V5 | Must | pbr's `uplink_ip_rules_priority` MUST be set to **900** (below mwan3's 2001–2254 band) so VPN-policy flows are evaluated before, and fall through to, mwan3's balancer. pbr's default mark (`0x00010000` / mask `0x00ff0000`) MUST be kept non-overlapping with mwan3's `0x3F00`. |
| FR-V6 | Must | An on-demand **toggle** MUST exist. **ON:** `ifup wgvpn` → wait for handshake/route → enable the relevant pbr policy → `service pbr reload`. **OFF:** disable policy → `service pbr reload` → `ifdown wgvpn`. Order matters: tunnel must be up before pbr reload so pbr sees the live interface. |
| FR-V7 | Must | The toggle MUST support both "all LAN via VPN" (pbr policy `src_addr` = LAN subnet) and "selected clients/destinations" (per-IP/-MAC `src_addr`, or domain `dest_addr` via `dnsmasq-full` + `resolver_set dnsmasq.nftset`). Domain policies require `dnsmasq-full`, not stock dnsmasq. |
| FR-V8 | Must | The wg interface MUST be in its own firewall zone (not folded into the WAN zone, which would make mwan3 try to balance the tunnel) with `masq '1'` and `mtu_fix '1'`; MTU SHOULD be ~1412 to avoid PMTU blackholes. |
| FR-V9 | Should | **Kill switch:** pbr `strict_enforcement 1` SHOULD be enabled so policy-matched traffic is DROPPED when the tunnel is down rather than leaking out a WAN. (Limitation: this protects forwarded LAN traffic only — it is not a router-egress kill switch.) |
| FR-V10 | Should | When the tunnel is up, the router's own DNS path SHOULD egress through the tunnel and use Surfshark's resolvers (`162.252.172.57` / `149.154.159.92`), since OpenWrt ignores the WireGuard peer's `DNS=` field. A DNS-leak test SHOULD confirm policied clients show only Surfshark. |
| FR-V11 | Should | A `/etc/mwan3.user` hook SHOULD flush the WireGuard UDP/51820 conntrack on mwan3 connect/disconnect so the tunnel re-handshakes over the surviving WAN; WireGuard's sticky client source port otherwise breaks tunnel failover. Toggling the VPN MUST NOT flush *all* conntrack (would reset every active download). |
| FR-V12 | Won't | The design **WON'T** claim VPN bandwidth aggregation. With the tunnel up and `AllowedIPs 0.0.0.0/0`, tunneled traffic rides exactly one WAN; dual-WAN degrades to **failover-only** for VPN traffic. This honesty note MUST be surfaced in user-facing docs. |

---

## 4. Non-Functional Requirements

### 4.1 Performance (NFR-P)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| NFR-P1 | Must | The router MUST sustain line-rate NAT/routing for two ~1 Gbps WANs without the Cortex-A76 CPU being the bottleneck. The design constraint is the shared ~5 Gbps RP1/USB3 budget, not CPU. |
| NFR-P2 | Must | The capacity probe MUST be bounded (`--duration 8 --concurrent 2`, or `--limit-rate` on curl) so it does not saturate the link or materially degrade live user traffic during the probe window. |
| NFR-P3 | Should | The fastest uplink SHOULD be on the onboard GbE (RP1 dedicated lane) to keep its throughput off the contended USB3 path. |
| NFR-P4 | Should | adblock SHOULD comfortably run XL/XXL feed tiers (500K+ domains) given 4GB RAM; memory MUST NOT be the limiting factor for feed selection. |

### 4.2 Reliability & Failover (NFR-R)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| NFR-R1 | Must | A single WAN failure MUST NOT take the LAN offline; surviving-WAN failover MUST be automatic via mwan3track. |
| NFR-R2 | Must | Failover detection MUST be reliable: `track_ip` reachable only via the tested WAN, `reliability ≤ track_ip count`, and conntrack flushed for the dead WAN (FR-W6). |
| NFR-R3 | Must | USB NIC enumeration order MUST NOT break WAN/LAN role mapping — interfaces pinned by MAC (FR-B3). |
| NFR-R4 | Should | The Pi SHOULD use the 27 W PSU (or powered USB hub) so a brownout-induced USB link flap is not misdiagnosed as a WAN-down event. |
| NFR-R5 | Should | Root storage SHOULD be NVMe/SSD, not SD, to avoid rootfs corruption from adblock list churn and logging over months. |
| NFR-R6 | Could | VPN server failover (rotating Surfshark endpoint/public-key on server death) COULD be scripted; it is distinct from WAN failover and not required for v1. |

### 4.3 Security (NFR-S)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| NFR-S1 | Must | **No plaintext secrets in version control.** The WireGuard PrivateKey and any auth keys (e.g. URLhaus abuse.ch Auth-Key) MUST be injected via UCI / a gitignored file and represented in docs and repo as placeholders (e.g. `<WG_PRIVATE_KEY>`, `<URLHAUS_AUTH_KEY>`). |
| NFR-S2 | Must | **DNS-leak prevention.** When the tunnel is up, DNS MUST NOT egress the non-tunnel WAN or via client DoH. Force-DNS (FR-F5) MUST stay on regardless of VPN state, and the upstream resolver MUST be routed through the tunnel (FR-V10). |
| NFR-S3 | Must | **Kill switch.** pbr `strict_enforcement 1` MUST be available so policy-matched traffic does not leak to a WAN when the tunnel drops (FR-V9), with its router-egress limitation documented. |
| NFR-S4 | Must | LAN inputs/queries MUST be validated where the healthcheck/toggle scripts parse external output (jsonfilter/JSON parsing MUST handle empty/malformed probe results without crashing or producing weight `0`). |
| NFR-S5 | Should | Management surfaces (LuCI, SSH) SHOULD be LAN-only (FR-B8) with key-based SSH; outbound DoT/known-DoH SHOULD be blocked to enforce filtering (FR-F5). |
| NFR-S6 | Should | The firewall mark allocation (mwan3 `0x3F00`, pbr `0x00ff0000`) MUST be documented and masks MUST NOT be widened to overlap. |

### 4.4 Maintainability (NFR-M)

| ID | MoSCoW | Requirement |
| --- | --- | --- |
| NFR-M1 | Must | Configuration MUST use stock OpenWrt UCI files (`/etc/config/{network,firewall,mwan3,dhcp,pbr}`, `/etc/adblock/*`) and standard package CLIs — no out-of-tree forks. |
| NFR-M2 | Must | Shell scripts (healthcheck, VPN toggle) MUST use descriptive variable names, type/format-checked parsing, and explicit error handling (no silent failures, no generic catch-alls); comments MUST explain "why" not "what". |
| NFR-M3 | Should | All packages SHOULD be pre-baked via the OpenWrt Firmware Selector (`mwan3`, `luci-app-mwan3`, `adblock`, `luci-app-adblock`, `wireguard-tools`, `kmod-wireguard`, `luci-app-wireguard`, `pbr`, `luci-app-pbr`, `dnsmasq-full`, `librespeed-cli`, `kmod-usb-net-rtl8152`/`-asix-ax88179`, `iptables-nft`) so USB NICs and services come up on first boot. |
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

- **A1.** Two usable WAN uplinks are available (onboard GbE + at least one USB3 GbE), each on a separate physical ISP/handoff. WAN1 is the faster link.
- **A2.** The OpenWrt `bcm27xx/bcm2712` stable release (25.12.x) supports all required packages for aarch64 (confirmed: `librespeed-cli` v1.0.10 is available for the arch).
- **A3.** The operator has a valid Surfshark subscription and can generate a WireGuard key pair / per-server config from the dashboard.
- **A4.** USB-Ethernet adapters are genuine RTL8153 or AX88179 (verified via `lsusb`/`dmesg`), not counterfeit RTL8157/2.5G variants lacking a kmod.
- **A5.** The Pi runs from the official 27 W PSU (or powered hub for the NICs) and from NVMe/SSD rather than SD for an always-on appliance.
- **A6.** Modern OpenWrt fw4/nftables is the firewall stack; mwan3's iptables-nft compatibility shims are installed.
- **A7.** Track-IP targets (`1.1.1.1`, `8.8.8.8`, etc.) are routable only via their respective WAN once source-routing is configured.

---

## 6. Out of Scope (Won't)

| ID | Item | Rationale |
| --- | --- | --- |
| OOS-1 | **True line bonding / bandwidth aggregation** of the two WANs into one logical pipe. | mwan3 is per-flow, not bonding. A single TCP stream uses one WAN. Aggregation needs MPTCP/bonding — far more complex and not a project goal. |
| OOS-2 | **Per-packet load balancing.** | Breaks NAT/TLS/conntrack; per-flow sticky is the explicit, correct model. |
| OOS-3 | **VPN bandwidth aggregation across both WANs.** | WireGuard rides one WAN at a time; VPN-up = failover-only for tunneled traffic. |
| OOS-4 | **Ookla `speedtest` CLI** as the capacity probe. | Not in opkg feeds; interactive EULA can hang cron. librespeed-cli used instead. |
| OOS-5 | **FireHOL IP blocklists in adblock.** | IP-based, not DNS — belongs in a separate `banip`/nftset firewall layer. |
| OOS-6 | **Internal Pi 5 radio as primary WiFi AP.** | Weak; a dedicated AP device is recommended. Secondary AP only if needed. |
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

1. **Upstream-DNS-through-tunnel mechanism (FR-V10):** bind the `https-dns-proxy`/`stubby` forwarder to `wgvpn`, or add a pbr policy for the router's own port-53 egress? Both work; needs a decision and a test that the router's own DNS does not leak when the tunnel is up. OpenWrt ignoring the WireGuard `DNS=` field is the constraint forcing this.
2. **URLhaus Auth-Key:** abuse.ch now requires a free Auth-Key for the hostfile export. Confirm whether adblock's bundled `urlhaus` feed entry accepts the key inline and where to store it (secret — placeholder only).
3. **Metered-link detection (FR-H11):** no signal yet on whether either uplink is metered/capped. If one is LTE/5G, its probe should drop to liveness-only — needs operator input.
4. **WAN count:** research references a possible third WAN (onboard + 2× USB = wan1/wan2/wan3 with LAN on a 4th port via the switch). This doc assumes **2 WAN + 1 LAN**. Confirm whether a third uplink is in play; if so, FR-W2/FR-W4 extend to three same-metric members.
5. **Rule-level sticky (FR-W8) default:** leave off, or enable for known IP-sensitive sites? Needs an operator preference.
