# pi — OpenWrt dual-WAN router on a Raspberry Pi 5

A home router built on a Raspberry Pi 5 running OpenWrt: two USB WAN uplinks with
weighted per-flow load balancing and automatic failover, network-wide DNS ad/tracker
filtering, and an on-demand (default-off) Surfshark WireGuard VPN.

**Status:** built and working on hardware (OpenWrt 25.12.4, June 2026). Dual-WAN,
DNS filtering, and capacity-based weighting are live; the VPN is staged but off.

## What it does

- **Dual-WAN load balancing + failover** (`mwan3`) — two USB3 gigabit uplinks,
  per-flow sticky balancing weighted by measured capacity, automatic failover when
  a link dies. 4-hour HTTPS sticky timeout prevents streaming (Netflix, etc.)
  from breaking mid-session. (Not bonding — a single connection rides one WAN.)
- **DNS filtering** — `dnsmasq` + `adblock` (~561k blocked domains, 9 feeds
  including hagezi multi-pro, adguard, stevenblack, and device-specific
  trackers) with encrypted DoH upstreams (`https-dns-proxy` → Cloudflare +
  Google), plus force-DNS and DoT blocking so clients can't bypass it.
- **Capacity weighting** — a cron'd speed-test probe (`librespeed-cli`) measures
  each WAN and adjusts the load-balance ratio.
- **On-demand VPN** — Surfshark WireGuard + policy-based routing, split-tunnel,
  **off by default**. Multiple server locations (drop a `.conf` file in
  `/etc/wireguard/servers/` and it appears in the UI).
- **Admin dashboard** — lightweight single-page web UI at `/admin.html` with
  session-based auth (SHA-256 hashed credentials, brute-force protection,
  security headers). Per-WAN toggle switches, VPN server selection, adblock
  controls, package update management with selective install, reboot, and
  change password. HTTPS with a local CA certificate.

## Topology

```
   ISP A ── USB3 NIC (uwan1) ─┐
                              ├─ Raspberry Pi 5 (OpenWrt) ── onboard eth0 / br-lan ── downstream WiFi router ── clients
   ISP B ── USB3 NIC (uwan2) ─┘        192.168.1.1/24
```

Both USB3 adapters are the WANs; the onboard Ethernet port is the LAN/management
interface. A separate WiFi router hangs off the LAN port (NAT mode) to serve
wireless clients — the Pi's own radio is disabled.

## Quick start

This repo holds the **deployed configuration and operational scripts**, not a
turnkey installer. To reproduce the router:

1. Flash a custom [OpenWrt Firmware Selector](https://firmware-selector.openwrt.org/)
   image for `bcm27xx/bcm2712` (`rpi-5`) with the package set in
   [`docs/reference/hardware.md`](docs/reference/hardware.md) §7.
2. Copy `config/` and `scripts/` to the Pi.
3. Run [`scripts/bringup.sh`](scripts/bringup.sh) (idempotent: applies WANs,
   DNS, adblock, weighting, NTP, firewall) and reboot once.

Full step-by-step build guide: [`docs/runbooks/setup.md`](docs/runbooks/setup.md).

## Documentation

| Doc | Contents |
|---|---|
| [`AGENTS.md`](AGENTS.md) | Onboarding + the hard-won gotchas (read this before working on it) |
| [`docs/reference/architecture.md`](docs/reference/architecture.md) | System design, planes, data flow, zone model (as-built) |
| [`docs/reference/hardware.md`](docs/reference/hardware.md) | Bill of materials, NIC selection, throughput, packages |
| [`docs/reference/load-balancing.md`](docs/reference/load-balancing.md) | mwan3 config + the speed-test healthcheck |
| [`docs/reference/vpn.md`](docs/reference/vpn.md) | Surfshark WireGuard + pbr split-tunnel design |
| [`docs/planning/requirements.md`](docs/planning/requirements.md) | Functional / non-functional requirements (as-built) |
| [`docs/runbooks/setup.md`](docs/runbooks/setup.md) | End-to-end build runbook |
| [`scripts/README.md`](scripts/README.md) | Operational scripts: install + status |

> The reference docs and `requirements.md` are **as-built** (reconciled to the
> deployed hardware). `setup.md` is the original build guide with as-built
> correction callouts.

## Repository layout

```
config/      Live UCI config pulled from the router + the hotplug NIC-rename rule
scripts/     bringup.sh (one-shot rebuild), wan-weight.sh (capacity weighting), VPN scripts
www/         admin.html (dashboard UI) + cgi-bin/admin (CGI backend)
docs/        reference/ · planning/ · runbooks/
```

Secrets (WireGuard keys, router admin password) live in a gitignored
`.claude/.secrets/` directory and are never committed — tracked config uses
placeholders.

## Hardware

Raspberry Pi 5 (4GB) · 2× RTL8153 USB3 gigabit Ethernet adapters · official 27W
USB-C PD supply. See [`docs/reference/hardware.md`](docs/reference/hardware.md)
for the full bill of materials and selection rationale.

## License

[MIT](LICENSE).
