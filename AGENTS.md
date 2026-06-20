# AGENTS.md — orientation for AI agents working on this project

This repo builds and documents an **OpenWrt dual-WAN router on a Raspberry Pi 5**:
two USB WAN uplinks, weighted per-flow load balancing + failover (mwan3),
DNS filtering (adblock + DoH), and a staged-but-off Surfshark WireGuard VPN.

The router was **built and proven on real hardware** (OpenWrt 25.12.4, 2026-06-20).
This file tells you what's actually deployed, the traps that cost real time, how to
reach the box, and what's left. **Read it before touching anything** — several
"obvious" moves here are wrong for reasons that aren't obvious.

---

## TL;DR for picking up

1. The router works end-to-end. Don't "fix" it blind.
2. **You cannot reach the Pi over IPv4 from the owner's Mac** — Zscaler eats it.
   Use **IPv6 link-local** (see [Accessing the Pi](#accessing-the-pi)).
3. The deployed config is captured in `config/` and applied by `scripts/bringup.sh`.
   The docs in `docs/reference/` + `docs/planning/requirements.md` are **as-built**
   and authoritative. `docs/runbooks/setup.md` is the *original design* with
   as-built correction callouts — where they disagree, **as-built wins**.
4. Before concluding the Pi is broken, **check the workstation** (Zscaler/routing)
   — we re-flashed the SD card twice on a misdiagnosis that was Zscaler all along.

---

## As-built architecture (what is actually running)

| Element | Reality |
|---|---|
| Board / OS | Raspberry Pi 5 (4GB), OpenWrt **25.12.4** (r32933), custom Firmware-Selector image, squashfs on a 32GB SD (testing media) |
| LAN / management | onboard `eth0` → `br-lan`, **192.168.1.1/24** (NOT renumbered) |
| WAN1 | USB3 RTL8153, device **`uwan1`** (MAC `44:ed:57:10:00:30`), iface `wan1`, metric 10 |
| WAN2 | USB3 RTL8153, device **`uwan2`** (MAC `00:e0:4c:68:01:1e`), iface `wan2`, metric 20 |
| Firewall zones | `lan` (masq 0) · `wan` (holds BOTH `wan1`+`wan2`, masq 1) · `vpn` (wgvpn) |
| DNS | dnsmasq + adblock (~463k domains) + **https-dns-proxy** DoH: Cloudflare `127.0.0.1#5053` + Google `#5054` |
| VPN | Surfshark WireGuard `wgvpn` + pbr — **staged DEFAULT-OFF**, placeholder creds |
| Downstream WiFi | NETGEAR Orbi **MR60** in NAT mode off the Pi LAN (its LAN = 10.0.0.0/24). The Pi's own radio is **disabled**. |

> This topology is the **inverse** of the original design (which put WAN1 on the
> onboard port). Management stability across USB re-enumeration won over a marginal
> throughput edge. Don't "restore" the original topology — it's a deliberate choice.

---

## The traps (each of these cost real time — do not re-derive)

### 1. Zscaler blocks IPv4 to the Pi from the owner's Mac
The owner's Mac runs **Zscaler** (corporate VPN, **cannot be disabled**). It
intercepts IPv4 to RFC1918 (`192.168.x`, `10.x`) at the packet-filter layer, so
`ping`/`ssh`/`scp` to `192.168.1.1` **time out even though the Pi is healthy**.
- **Tell:** ARP resolves the Pi's MAC, but unicast IPv4 (ping/ssh/port-22) is
  filtered; `ping -b en7 192.168.1.1` (interface-bound) works while plain `ping`
  fails. A `route add -host ... -interface` does NOT fix it (Zscaler is below
  routing).
- **DIAGNOSE BEFORE RE-FLASHING.** If IPv4 to the Pi dies, FIRST check the Mac:
  `netstat -rn -f inet | grep 192.168` (look for a `utun*`/VPN owning the subnet)
  and `pgrep -fl zscaler`. We bricked-by-misdiagnosis and re-flashed twice before
  realizing the Pi was fine. Confirm the Pi is *actually* down (LED amber = not
  booted; green = running) or unreachable from a non-Zscaler device first.

### 2. Device names MUST NOT end in `dev`
mwan3 2.12.0's `mwan3_route_line_dev()` extracts a route's device with a **greedy**
`sed -ne "s/.*dev \([^ ]*\).*/\1/p"`. A name like `wan1dev` is mis-parsed as the
next token (`proto`), so the per-WAN default route is silently dropped from tables
1/2 and marked LAN traffic gets "Network unreachable". Hence **`uwan1`/`uwan2`**,
never `wan1dev`/`wan2dev`/`landev`.

### 3. The NIC rename is a HOTPLUG rule, not `config device`
netifd does **NOT** honor the `/etc/config/network` `config device` MAC alias for
these RTL8153 USB NICs — the rename never fires on reload/restart/**cold-boot**,
and carrier presence is irrelevant (an earlier "rename fires on carrier" theory was
falsified). The working mechanism is `config/hotplug/05-rename-wan-by-mac` →
`/etc/hotplug.d/net/05-rename-wan-by-mac`, renaming by MAC at the hotplug `add`
event before netifd/mwan3 bind.

### 4. WANs need DISTINCT interface metrics
`network.wan1.metric=10`, `network.wan2.metric=20`. Without distinct metrics only
one DHCP default route lands in the main table and the other WAN flaps offline.
This is the **network-interface** metric layer — SEPARATE from mwan3 **member**
metrics (which stay EQUAL at `1` for active-active balancing). Two independent
layers; don't conflate them.

### 5. `mwan3 reload` does NOT apply weight changes
Proven on 2.12.0: change a weight + `mwan3 reload` → the balanced split stays at
its OLD ratio. Only **`mwan3 ifup <iface>`** (per changed interface) or a full
`mwan3 restart` applies it. `restart` is forbidden (FR-H13: blips the VPN WAN pin),
so `wan-weight.sh` reapplies via `mwan3 ifup`. (`ifup` cycles the interface and
flushes its conntrack — acceptable because the 15% threshold makes it rare.)

### 6. `coreutils-timeout` is required
busybox has **no** `timeout` applet. `wan-weight.sh` wraps every probe in
`timeout`, so without the `coreutils-timeout` package every capacity probe silently
fails and weighting never happens.

### 7. Cold-boot clock/DNS/TLS deadlock (no RTC)
The Pi 5 has no RTC. On cold boot the clock is stale → DoH over TLS fails cert
validation → NTP using pool *hostnames* needs DNS → which needs DoH → which needs
the clock. Circular deadlock. **Fix: IP-literal NTP servers FIRST** in
`system.ntp.server` (Cloudflare `162.159.200.123`/`.1`, Google `216.239.35.0`),
so busybox ntpd sets the clock over UDP/123 with no DNS/TLS. `fake-hwclock` is NOT
in the 25.12.4 repo; the IP-literal fix is sufficient.

### 8. adblock boot-race
adblock's default boot run fires before WAN/clock are ready and writes an EMPTY
blocklist (router unprotected until the 5am cron). Fix: `adb_trigger='wan1 wan2'`
+ `adb_triggerdelay='20'` — adblock skips the boot run and fires on WAN ifup.

### 9. LAN renumber is KNOWN-BAD — keep the Pi at 192.168.1.1
Renumbering `network.lan.ipaddr` to `192.168.10.1` *appeared* to strand the box
repeatedly (LAN input filtered) — but this was very likely the same Zscaler
artifact, never conclusively root-caused. **Don't renumber.** A downstream NAT
WiFi router uses its OWN non-192.168.1.x LAN instead (the MR60 uses 10.0.0.0/24).

### 10. adblock feed catalog gap
adblock 4.5.6 has NO `hagezi Pro/TIF` or `urlhaus` (the original FR-F2 targets).
Deployed feeds = `oisd_big certpl hagezi` (closest match). **Do NOT add
`doh_blocklist`** without first confirming it doesn't list `cloudflare-dns.com` /
`dns.google` — those are the router's own DoH upstreams; blocking them blackholes
its resolver.

---

## Accessing the Pi

**From the owner's Mac (Zscaler active): use IPv6 link-local, NOT 192.168.1.1.**

```sh
# Discover neighbors on the wired interface (replace en7 with the live one):
ping6 -c3 ff02::1%en7
# The Pi is the neighbor whose MAC matches br-lan: 2c:cf:67:6b:d0:d7
# (EUI-64 link-local, stable):
ssh -o StrictHostKeyChecking=no 'root@fe80::2ecf:67ff:fe6b:d0d7%en7'
```

- Only works while **wired to the same L2 segment** as the Pi.
- SSH **key auth** is installed (owner's `id_rsa.pub` in `/etc/dropbear/authorized_keys`).
- dropbear has **no sftp-server** → plain `scp` fails; use `scp -O` (legacy proto).
- From a **non-Zscaler device**, plain `ssh root@192.168.1.1` works normally.
- Pi 5 LED: **green = up**, **amber/off = not booted** (check this before assuming
  a software fault). Reboot ≈ 80s to SSH, +40s for carrier/DHCP/mwan3 to settle.

---

## Repo layout

```
config/
  etc-config/{network,dhcp,system,adblock,https-dns-proxy}  # live UCI, pulled from Pi
  hotplug/05-rename-wan-by-mac                               # the NIC rename rule
  mwan3                                                       # mwan3 UCI (commented)
scripts/
  bringup.sh        # ONE-SHOT idempotent post-flash bring-up (does NOT renumber)
  wan-weight.sh     # cron */30 capacity probe -> mwan3 weight (ifup reapply)
  mwan3.user        # WG conntrack flush on WAN transition (dynamic port)
  vpn-toggle.sh     # Surfshark on/off/status        (staged, not yet exercised)
  stage-vpn.sh      # idempotent VPN+pbr staging      (staged)
  apply-wg-secret.sh# inject WG private key from /etc/wireguard/wgvpn.secret
  README.md         # install + hardware-verified status
docs/
  reference/{architecture,hardware,load-balancing,vpn}.md    # AS-BUILT (authoritative)
  planning/requirements.md                                   # AS-BUILT (FR/NFR)
  runbooks/setup.md            # ORIGINAL design + as-built banner & callouts
```

**Secrets:** `.claude/.secrets/` is gitignored — real keys/passwords live there,
NEVER in tracked files. Tracked config uses placeholders (`<WG_PRIVATE_KEY>` etc.).
Always scan a diff for secrets before committing.

---

## Rebuilding from scratch

If the SD is re-flashed: flash the custom Firmware-Selector image (package set in
`docs/reference/hardware.md` §7 + `https-dns-proxy` + `coreutils-timeout`), then on
the Pi deploy `config/mwan3`, `config/hotplug/05-rename-wan-by-mac`, and `scripts/*`,
and run **`scripts/bringup.sh`** (idempotent; applies WANs+metrics, firewall zone,
IP-literal NTP, DoH, adblock+trigger, mwan3, crons; does NOT renumber). Reboot once
to fire the hotplug NIC rename. Then: `mwan3 restart; /etc/init.d/adblock reload;
/usr/bin/wan-weight.sh`.

---

## What's left (resume points)

- **VPN (Surfshark + pbr):** staged default-OFF. To enable: inject real creds
  (`apply-wg-secret.sh`), set peer public_key/endpoint_host, enable iface + pbr,
  then **Phase 8.6**: wgvpn device-bind on https-dns-proxy + toggle switching
  dnsmasq upstream + a DNS-leak test. **NFR-S2 (router DNS via tunnel) is NOT yet
  met** — known/accepted debt while VPN is off. `stage-vpn.sh` creates the pbr
  `lan_via_vpn` policy (a fresh image has only stock pbr example policies).
- **Capacity weighting** works but falls back to 50/50 when librespeed.org's
  server-list endpoint is down (upstream outage); self-corrects on the next cron.
- **Cosmetic:** Pi timezone is UTC (`GMT0`); set local zone if local-time logs wanted.
- **Permanent media:** move off the SD to NVMe/USB-SSD for an always-on appliance.

---

## Working conventions

- **Verify on the live box before asserting** — this project's history is full of
  plausible theories ("carrier", "reload applies weights", "the renumber bricked
  it") that hardware **falsified**. Prefer `mwan3 status`, `ip route show table N`,
  `logread`, `ubus call` over reasoning from docs.
- **OpenWrt 25.x uses `apk`, not `opkg`.**
- Commit messages on this repo end with the project's Co-Authored-By trailer; only
  commit/push when asked. Configs+scripts are real artifacts (pushable); avoid
  docs-only pushes per the owner's workflow — batch with code.
- When you change the live router, **pull the resulting `/etc/config/*` back into
  `config/`** so the repo stays the source of truth, and update the as-built docs.
