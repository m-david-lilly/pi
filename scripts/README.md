# scripts/

Operational scripts for the OpenWrt Raspberry Pi 5 dual-WAN router. These are the
production forms of the snippets in [`../docs/runbooks/setup.md`](../docs/runbooks/setup.md);
the design rationale lives in the [reference docs](../docs/reference/).

| Script | Installs to | Purpose | Runbook phase |
|---|---|---|---|
| `wan-weight.sh` | `/usr/bin/wan-weight.sh` | Per-WAN capacity probe → mwan3 member `weight`. Cron `*/30`. | Phase 6 |
| `vpn-toggle.sh` | `/usr/bin/vpn-toggle.sh` | On-demand Surfshark WireGuard on/off/status. | Phase 7.7 |
| `mwan3.user` | `/etc/mwan3.user` | Re-handshake the tunnel over the surviving WAN on a WAN transition. | Phase 7.6 |
| `stage-vpn.sh` | `/usr/bin/stage-vpn.sh` | One-shot, idempotent: stage WireGuard + pbr split-tunnel (DEFAULT OFF), no secret, no internet. | Phase 7.1 |
| `apply-wg-secret.sh` | `/usr/bin/apply-wg-secret.sh` | Inject the Surfshark private key into UCI from `/etc/wireguard/wgvpn.secret` (never git-tracked). | Phase 7.2 |

Target shell is OpenWrt's BusyBox **ash** (POSIX `sh`), not bash.

## Install

```sh
# From a workstation, copy to the router (adjust host).
# NOTE: OpenWrt dropbear ships no sftp-server, so plain `scp` fails with
# "/usr/libexec/sftp-server: not found". Use `scp -O` (legacy protocol).
scp -O scripts/wan-weight.sh scripts/vpn-toggle.sh root@192.168.1.1:/usr/bin/
scp -O scripts/stage-vpn.sh scripts/apply-wg-secret.sh root@192.168.1.1:/usr/bin/
scp -O scripts/mwan3.user root@192.168.1.1:/etc/mwan3.user

# On the router:
chmod +x /usr/bin/wan-weight.sh /usr/bin/vpn-toggle.sh \
         /usr/bin/stage-vpn.sh /usr/bin/apply-wg-secret.sh
grep -q '/usr/bin/wan-weight.sh' /etc/crontabs/root \
  || echo '*/30 * * * * /usr/bin/wan-weight.sh' >> /etc/crontabs/root
/etc/init.d/cron enable && /etc/init.d/cron restart
```

Prerequisites (see the runbook for the full build): `mwan3`, `librespeed-cli`,
`conntrack`, `wireguard-tools`, `pbr`, and the members/policies the scripts
reference (`wan1_m1_w3` / `wan2_m1_w3`, pbr policy `lan_via_vpn`).

## Known deploy-time gap — DNS through the tunnel

`vpn-toggle.sh` contains two `TODO(deploy)` markers where the router's **own**
upstream DNS must be switched into the tunnel when VPN is ON (and restored when
OFF). This is **designed but not implemented** — fail-closed DNS routing is
order- and hardware-sensitive and must be validated with a live DNS-leak test
rather than written blind. See [`../docs/reference/vpn.md`](../docs/reference/vpn.md)
§7 and §13. Until it is closed, with the VPN ON the router's upstream DNS
egresses a WAN (encrypted under DoH, but not tunnel-routed → NFR-S2 not yet met).

## Verify before trusting

These have not been run on hardware. Before relying on them:

- `shellcheck -s sh scripts/*.sh` (clean for the `.sh` files; `mwan3.user` is a
  sourced fragment, not standalone).
- Confirm `librespeed-cli` flag names on the installed build (`librespeed-cli --help`).
- Confirm `mwan3 use <iface>` exists (mwan3 ≥ 2.10), and that `mwan3 reload`
  re-evaluates member weights on the installed build (`mwan3 reload; mwan3 policies`).
- Confirm the pbr policy name matches `POLICY_NAME` in `vpn-toggle.sh`
  (`uci show pbr | grep lan_via_vpn`) — `vpn-toggle on` aborts if it can't resolve it.
- Run `wan-weight.sh` by hand and confirm each probe egressed the intended WAN
  (runbook Phase 6.3), then watch `logread -e wan-weight`.
