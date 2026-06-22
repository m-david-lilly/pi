# scripts/

Operational scripts for the OpenWrt Raspberry Pi 5 dual-WAN router. These are the
production forms of the snippets in [`../docs/runbooks/setup.md`](../docs/runbooks/setup.md);
the design rationale lives in the [reference docs](../docs/reference/).

| Script | Installs to | Purpose | Runbook phase |
|---|---|---|---|
| `bringup.sh` | `/usr/bin/bringup.sh` | One-shot, idempotent post-flash bring-up of the whole stack (WANs + distinct metrics, firewall wan-zone, IP-literal NTP, DoH, adblock+trigger, mwan3, crons). Does NOT renumber the LAN. | Rebuild |
| `wan-weight.sh` | `/usr/bin/wan-weight.sh` | Per-WAN capacity probe → mwan3 member `weight`. Cron `*/30`. | Phase 6 |
| `vpn-toggle.sh` | `/usr/bin/vpn-toggle.sh` | On-demand Surfshark WireGuard on/off/status. | Phase 7.7 |
| `mwan3.user` | `/etc/mwan3.user` | Re-handshake the tunnel over the surviving WAN on a WAN transition. | Phase 7.6 |
| `stage-vpn.sh` | `/usr/bin/stage-vpn.sh` | One-shot, idempotent: stage WireGuard + pbr split-tunnel (DEFAULT OFF), no secret, no internet. | Phase 7.1 |
| `apply-wg-secret.sh` | `/usr/bin/apply-wg-secret.sh` | Inject the Surfshark private key into UCI from `/etc/wireguard/wgvpn.secret` (never git-tracked). | Phase 7.2 |

### Admin dashboard (www/)

| File | Installs to | Purpose | Runbook phase |
|---|---|---|---|
| `www/admin.html` | `/www/admin.html` | Single-page admin dashboard (HTML/JS/CSS). Session-based login, auto-refresh, tooltips. | Phase 10 |
| `www/cgi-bin/admin` | `/www/cgi-bin/admin` | Shell CGI backend. Serves JSON status (system, WAN, mwan3, adblock, DNS, VPN), handles actions (adblock toggle, VPN server switch, WAN re-weight, reboot), and manages auth (login/logout/change-password). | Phase 10 |

The dashboard CGI dynamically loads VPN servers from `/etc/wireguard/servers/*.conf` —
drop a Surfshark WireGuard config file in and it appears in the UI. Auth credentials
are in `/etc/piadmin/credentials` (default `admin`/`admin`, change via the UI).

Target shell is OpenWrt's BusyBox **ash** (POSIX `sh`), not bash.

## Install

> **⚠️ Management-host caveat (Zscaler).** If the admin workstation runs Zscaler
> (or any always-on corporate VPN), IPv4 to `192.168.1.1` is intercepted at the
> packet-filter layer and `scp`/`ssh` to the Pi will TIME OUT even though the Pi is
> healthy. Manage the Pi over **IPv6 link-local** instead — discover with
> `ping6 -c3 ff02::1%<iface>`, then use the Pi's br-lan link-local, e.g.
> `scp -O scripts/wan-weight.sh 'root@[fe80::2ecf:67ff:fe6b:d0d7%en7]:/usr/bin/'`.
> The plain `192.168.1.1` form below is the no-VPN case. See the runbook's
> management-access gotcha for the full explanation.

```sh
# From a workstation, copy to the router (adjust host; see Zscaler caveat above).
# NOTE: OpenWrt dropbear ships no sftp-server, so plain `scp` fails with
# "/usr/libexec/sftp-server: not found". Use `scp -O` (legacy protocol).
scp -O scripts/bringup.sh scripts/wan-weight.sh scripts/vpn-toggle.sh root@192.168.1.1:/usr/bin/
scp -O scripts/stage-vpn.sh scripts/apply-wg-secret.sh root@192.168.1.1:/usr/bin/
scp -O scripts/mwan3.user root@192.168.1.1:/etc/mwan3.user

# On the router:
chmod +x /usr/bin/bringup.sh /usr/bin/wan-weight.sh /usr/bin/vpn-toggle.sh \
         /usr/bin/stage-vpn.sh /usr/bin/apply-wg-secret.sh
grep -q '/usr/bin/wan-weight.sh' /etc/crontabs/root \
  || echo '*/30 * * * * /usr/bin/wan-weight.sh' >> /etc/crontabs/root
/etc/init.d/cron enable && /etc/init.d/cron restart
```

Prerequisites (see the runbook for the full build): `mwan3`, `librespeed-cli`,
**`coreutils-timeout`** (busybox has NO `timeout` applet — `wan-weight.sh` wraps
every probe in `timeout`, so without this package every capacity probe silently
fails), `conntrack`, `wireguard-tools`, `pbr`, and the members/policies the
scripts reference (`wan1_m1_w3` / `wan2_m1_w3`, pbr policy `lan_via_vpn`).

## Known deploy-time gap — DNS through the tunnel

`vpn-toggle.sh` contains two `TODO(deploy)` markers where the router's **own**
upstream DNS must be switched into the tunnel when VPN is ON (and restored when
OFF). This is **designed but not implemented** — fail-closed DNS routing is
order- and hardware-sensitive and must be validated with a live DNS-leak test
rather than written blind. See [`../docs/reference/vpn.md`](../docs/reference/vpn.md)
§7 and §13. Until it is closed, with the VPN ON the router's upstream DNS
egresses a WAN (encrypted under DoH, but not tunnel-routed → NFR-S2 not yet met).

## Hardware-verified status (2026-06-20, OpenWrt 25.12.4, mwan3 2.12.0)

`bringup.sh`, `wan-weight.sh`, and `mwan3.user` were run on real hardware and the
full dual-WAN + DNS stack was proven working. Real Surfshark WireGuard credentials
have been injected and multiple servers configured in `/etc/wireguard/servers/`.
The VPN is default-OFF but ready to connect via the admin dashboard or
`vpn-toggle.sh on`.

Hard-won facts now baked into `wan-weight.sh`:

- **`mwan3 reload` does NOT re-evaluate member weights** on 2.12.0 — a weight
  change + `reload` leaves the live balanced split at its OLD ratio. Only
  `mwan3 ifup <iface>` (or `mwan3 restart`, which FR-H13 forbids — it blips the
  VPN WAN pin) applies a weight. The script reapplies via per-interface
  `mwan3 ifup`. Observe with `mwan3 ifup <iface>; mwan3 status` (NOT `reload`).
- **`coreutils-timeout` is required** — busybox lacks the `timeout` applet.
- **librespeed.org's server-list endpoint can be down upstream**, failing the
  probe; the script then keeps existing weights / falls back to 50/50 (graceful,
  self-corrects on the next cron tick).

Before relying on the VPN scripts (still unverified):

- `shellcheck -s sh scripts/*.sh` (clean for the `.sh` files; `mwan3.user` is a
  sourced fragment, not standalone).
- Confirm `librespeed-cli` flag names on the installed build (`librespeed-cli --help`).
- Confirm the pbr policy name matches `POLICY_NAME` in `vpn-toggle.sh`
  (`uci show pbr | grep lan_via_vpn`) — `vpn-toggle on` aborts if it can't resolve it.
  Note: a fresh image ships only stock pbr example policies until `stage-vpn.sh` runs.
