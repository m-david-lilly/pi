# Kids network with TLS interception (as-built)

An **optional, isolated WiFi network** whose web traffic is decrypted, filtered,
logged and alerted on, so a parent can see what a child is doing and be told when
something looks like grooming or self-harm. The adult LAN's traffic is not
proxied, logged or intercepted; the only thing shared with it is the `Pi Home CA`
that signs both the dashboard certificate and the kids' intercepted leaves.

**Status:** deployed and verified on the Pi (OpenWrt 25.12.4, October 2026).
SSID `Home-Kids` is up, DHCP hands out `192.168.30.100–199`, the proxy intercepts
HTTP/HTTPS, and the dashboard has a Kids panel.

---

## 1. Why a real proxy, not just DNS filtering

`adblock` (see [`architecture.md`](architecture.md)) only sees hostnames. That is
not enough for this job:

- a page title or chat message never appears in a DNS query;
- `https://www.youtube.com/watch?v=…` is one hostname for every video;
- DNS filtering cannot alert on *content* ("don't tell your parents").

So the kids network gets a **signing CA** (`Pi Home CA`, the router's own root)
and a **decrypting proxy** (`kidproxy`). The proxy terminates the device's TLS,
reads the request, opens its own TLS connection to the real site, and returns a
CA-signed certificate to the device. Everything the device sends is visible;
everything it receives is filtered, logged, and scanned for alert keywords.

This is a **monitoring** tool, not a security boundary against a determined
teenager with their own data plan. See [§10 Limits](#10-limits-and-known-gaps).

---

## 2. Topology and addresses

```
 Home-Kids (WPA2/WPA3, 5 GHz, onboard radio0)
        │
        │  br-kids  192.168.30.1/24   (DHCP 192.168.30.100-199, 12h)
        ▼
 ┌───────────────────────────────────────────────┐
 │ nft table inet kidproxy  (own table, not fw4) │
 │   80/443 ──redirect──▶ kidproxy :8080         │
 │   DNS 53 ──redirect──▶ dnsmasq :53            │
 │   QUIC/DoT/VPN/Tor/SMTP ──reject──▶           │
 │   IPv6 ──drop──▶   lateral to LAN ──reject──▶ │
 └───────────────────────────────────────────────┘
        │  kidproxy: CONNECT-less transparent MITM
        ▼
    WAN (mwan3, unchanged)
```

| Item | Value |
|---|---|
| SSID / band | `Home-Kids` · onboard `radio0`, 5 GHz ch36, VHT80, `sae-mixed` |
| Bridge / interface | `br-kids` · UCI `network.kids` (static, `force_link`) |
| Gateway | `192.168.30.1` (dnsmasq DNS + DHCP + CA install page) |
| DHCP range | `192.168.30.100–199`, 12 h lease, options 3/6 → `192.168.30.1` |
| Proxy listeners | transparent `:8080` · explicit CONNECT `127.0.0.1:3128` (loopback only) |
| Firewall zone | `kids`: input REJECT, forward REJECT, masq 1; only DHCP/DNS/8080/ICMP allowed in |
| CA | **`Pi Home CA`** (RSA 4096, SHA-256, 10 y) — the same root that signs the dashboard cert; leaf certs are ECDSA P-256, 7 d |
| Data | `/var/lib/kidproxy/events.db` (SQLite, WAL), retention 30 days |

One root covers both jobs: `Pi Home CA` signs `https://192.168.1.1` (uhttpd) and
the leaves the proxy mints for intercepted sites. Devices therefore trust the
*same* CA for the dashboard and for filtering — no second root to install or
rotate. The CA lives in `.claude/.secrets/ca/` (gitignored) and is copied to
`/etc/kidproxy/`. **The adult LAN's traffic is untouched** — nothing in the
kidproxy rules references `br-lan`, and the proxy refuses to dial private
addresses (`denyPrivate`), so a kids device cannot reach the dashboard through it.

### Choosing a different access point

The kids SSID defaults to the Pi's onboard 5 GHz radio because it was unused.
To move it to another AP, point the UCI wifi-iface at that AP's SSID and leave
`network.kids`/`br-kids` alone (the proxy only cares about the subnet):

```sh
uci set wireless.kids.disabled='1'      # stop the Pi's own SSID
uci commit wireless && wifi reload
# ...then bridge the kids VLAN/SSID on the external AP onto 192.168.30.0/24
```

The dashboard's **Kids WiFi** card manages the passphrase only while the Pi's own
radio serves the network. It treats the network as external — and hides the
password controls — when `wireless.kids.disabled='1'` **and** `network.brkids` has a
port (the uplink to the other AP). A Pi SSID that is merely turned off still counts
as the Pi's; a new password applies when it is turned back on. Changing it updates
`wireless.kids.key` and `/etc/kidproxy/wifi.key` (so a `kids-setup.sh` re-run keeps
it) and reloads only the kids radio — every kids device must rejoin.

Everything else (nft redirect, DHCP, DNS, proxy) keys off `br-kids` and
`192.168.30.0/24`, so a different AP only has to deliver those clients into that
subnet.

---

## 3. Components

| Path (on the Pi) | Repo path | Purpose |
|---|---|---|
| `/usr/bin/kidproxy` | `proxy/` (built to `proxy/dist/kidproxy`) | The proxy: MITM, capture, rules, alerts, CLI |
| `/etc/kidproxy/config.yaml` | `config/kidproxy/config.yaml` | Block/allow lists, keywords, capture limits, email |
| `/etc/kidproxy/kids.nft` | `config/kidproxy/kids.nft` | Interception + isolation rules |
| `/etc/kidproxy/ca.crt`, `ca.key` | *(not in repo — copy from `.claude/.secrets/ca/`)* | Signing CA = `Pi Home CA`; cert 0644, key 0600 |
| `/etc/kidproxy/ca.crt.bak`, `ca.key.bak` | *(not in repo)* | Previous signing CA (the generated `Kids Filter CA`), kept when the CA is replaced |
| `/etc/kidproxy/wifi.key` | *(not in repo)* | `Home-Kids` passphrase |
| `/etc/init.d/kidproxy` | `config/kidproxy/kidproxy.init` | procd service for the binary |
| `/etc/init.d/kidproxy-nft` | `config/kidproxy/kidproxy-nft.init` | Loads `kids.nft` at boot |
| `/usr/bin/kids-nft.sh` | `scripts/kids-nft.sh` | `apply` / `flush` / `status` the nft table |
| `/usr/bin/kids-toggle.sh` | `scripts/kids-toggle.sh` | `on` / `off` / `pause` / `resume` / `status` |
| `/usr/bin/kids-watchdog.sh` | `scripts/kids-watchdog.sh` | Cron: restart a dead proxy, warn if it stays down |
| `/etc/msmtprc` | *(not in repo)* | SMTP credentials for alert email |

Cron (installed by `kids-setup.sh`):

```
*/5 * * * *  /usr/bin/kids-watchdog.sh
*   * * * *  /usr/bin/kids-toggle.sh resume-if-due
```

`kids-setup.sh` is the idempotent installer: binaries, packages (`msmtp`), CA,
WiFi key, UCI for network/DHCP/wireless/firewall, services, cron. It does not
touch the WANs, mwan3, or uhttpd's certificate.

The CA step is `--ca <dir>`: point it at a copy of `.claude/.secrets/ca/` and it
installs that cert/key (keeping the previous pair as `*.bak`). Without `--ca` it
generates a fresh `Kids Filter CA` **only if none exists** — after a re-flash,
pass `--ca` or every device's trust breaks.

---

## 4. How a request is intercepted

Transparent mode (the only path a kids device can use):

1. nft redirects the device's TCP 80/443 to `:8080`; the proxy recovers the
   original destination with `SO_ORIGINAL_DST` (`proxy/origdst_linux.go`).
2. The proxy peeks the first byte:
   - **`0x16`** → TLS record. It parses the ClientHello for **SNI**, mints a
     leaf for that name, finishes the handshake, and serves the request through
     `net/http` (HTTP/1.1 and HTTP/2 both work).
   - **anything else** → plain HTTP. The connection is handed to the HTTP server
     **with the peeked bytes replayed** (`prefixConn`), because `bufio.Peek`
     already pulled the request out of the socket.
3. The request is checked against the blocklist; a hit returns a `403` block page
   instead of contacting the site.
4. Otherwise the proxy dials the real host, reads the response, captures a
   snippet of text bodies, and streams the **full** body to the client.
5. Hosts in `bypass:` (Signal/WhatsApp/Telegram API hosts) are **blind-tunnelled**:
   raw bytes, no decryption, no content logging.
6. Requests to the gateway names (`192.168.30.1`, `setup.kids`,
   `kidproxy.lan`) are answered locally with the **CA install page**.

The explicit listener (`127.0.0.1:3128`) exists only for testing from the Pi and
accepts **CONNECT only**. A plain `http://` request sent *through* it returns
`405 Method Not Allowed` — that is by design, not a bug (the transparent path
handles plain HTTP).

---

## 5. Trusting the CA on a device

Without the CA in the device's trust store, every HTTPS page shows a certificate
warning and the device is effectively offline (fail-closed). The install page at
**`http://192.168.30.1/`** (also `http://setup.kids/`) serves:

| Link | Use |
|---|---|
| `/ca.crt` | The CA in PEM — Android, Windows, Firefox, curl |
| `/ca.mobileconfig` | iOS/iPadOS/macOS profile (installs as a *root* cert) |
| on-page steps | Per-platform instructions |

- **Android**: Settings → Security → Encryption & credentials → Install a
  certificate → CA certificate. (Chrome/WebView honour it; most *apps* do not —
  see §10.)
- **iPhone/iPad**: open `/ca.mobileconfig`, then Settings → General → VPN & Device
  Management → install the profile, then General → About → Certificate Trust
  Settings → enable full trust for `Pi Home CA` (the profile and page show the
  CA's own name, so it always matches).
- **Windows**: double-click `ca.crt` → Install Certificate → Local Machine →
  Trusted Root Certification Authorities.
- **macOS**: Keychain Access → System → import `ca.crt` → Always Trust.
- **Firefox**: has its own store — import `ca.crt` under Privacy & Security →
  Certificates → Authorities.

The CA private key is the skeleton key for every trusting device. It lives at
`/etc/kidproxy/ca.key` (0600) and is never committed. **It is the Pi Home CA key**,
so it also signs the dashboard certificate — the copy on the router is the only
online copy; keep the offline original in `.claude/.secrets/ca/` backed up and
treat the router's filesystem as sensitive. Rotating it means re-issuing the
uhttpd cert *and* re-installing the CA on every device.

---

## 6. What gets recorded

One SQLite database, `/var/lib/kidproxy/events.db`, WAL mode, pruned every 6 h to
`retention_days` (30). Tables:

| Table | Contents |
|---|---|
| `events` | Every request: ts, src IP, device name (from dnsmasq leases), method, scheme, host, **full URL with query string**, status, MIME, bytes, action (`bump`/`block`/`tunnel`/`error`), reason, **text snippet** |
| `searches` | Search-engine queries extracted from the URL (`q=`, `p=`, `query=`, …) |
| `youtube` | Video id, **title**, channel, duration — parsed from watch URLs and from the InnerTube `player` JSON response, so titles appear without downloading video |
| `alerts` | Rule matches and notable events (see §7) |
| `rules` | Dashboard-added `block`/`allow` domains |
| `devices` | IP → MAC → friendly name |

Capture limits (`capture:` in the config): bodies are inspected up to
`max_body_bytes` (256 KiB) and only `text/html`, `text/plain`, JSON and XHTML are
scanned; snippets are truncated to `snippet_bytes` (4 KiB). Media/CDN hosts
(`googlevideo.com`, `ytimg.com`, `fbcdn.net`, …) are skipped so video playback is
not buffered through the proxy. The client always receives the full body
regardless of what was inspected.

Read it with the CLI (JSON out, safe to run while the proxy writes):

```sh
kidproxy stats                                  # 24h counters + db size
kidproxy query --kind events --since 24h --limit 50
kidproxy query --kind events --q "roblox"       # substring on URL/snippet/title
kidproxy query --kind alerts --since 7d
kidproxy query --kind youtube --since 7d
kidproxy query --kind searches --since 7d
kidproxy query --kind devices
kidproxy query --kind rules
```

---

## 7. Rules and alerts

**Blocklist** — `block.domains` in the config (suffix match) plus `rules` rows
with `action=block`. Currently the config list is the public DoH resolvers
(`dns.google`, `cloudflare-dns.com`, …), which stops apps that hard-code their own
encrypted resolver; DNS is already forced to the Pi.

**Allow rules win.** A dashboard `allow` rule for a host overrides both the config
list and any `block` rule, so a false positive can be fixed without editing the
config or restarting: rules reload every 60 s.

**Keywords** — `block.keywords` are matched case-insensitively against the URL,
the page snippet, YouTube titles/channels, and search queries. They are
**alert-only** and never block; the shipped list is grooming/self-harm phrases
(`send nudes`, `dont tell your parents`, `kik me`, `kill myself`, …). Expect
false positives — tune it to your family. Keep it short: alerts are a prompt to
look, not evidence.

Alert kinds (`alerts.kind`):

| Kind | Severity | Fires when |
|---|---|---|
| `blocked` | warn | A request hit the blocklist |
| `keyword` | high | A keyword matched URL / page text / video title / search |
| `new-site` | info | First visit to a host that served HTML (`alert.first_seen`) |
| `late-night` | warn | HTML navigation between `late_night_from` (23) and `late_night_to` (6), in `alert.timezone` (set it: the router clock is UTC) |
| `tls-pin` | info | TLS handshake failed — usually a pinned app, sometimes a broken cert |

**Alert noise control:** repeats of the same `kind|host|device` are suppressed for
one hour inside the proxy (`Store.allowAlert`). This matters: a single pinned app
produced **1083** identical `tls-pin` rows before the dedupe existed. Clear
history with:

```sh
kidproxy alerts clear --kind tls-pin   # one noisy kind
kidproxy alerts clear                  # everything
```

**Email** — `notify.email` in the config (`enabled`, `to`, `min_severity`,
`cooldown_seconds`, `max_per_hour`), sent with `msmtp` from `/etc/msmtprc` (0600).
Verify end to end with `kidproxy notify-test`. The watchdog uses the same path to
warn when the proxy will not stay up.

---

## 8. Operating it

The whole feature is **off by default** and can be switched off completely without
touching the adult network.

```sh
kids-toggle.sh status       # enabled=1 proxy=running nft=applied pause_until=0
kids-toggle.sh off          # SSID down, proxy stopped, nft flushed
kids-toggle.sh on           # back to normal
kids-toggle.sh pause 30     # SSID stays up, filtering off for 30 min
kids-toggle.sh resume       # end a pause early
```

- **`off`** removes the SSID, stops the proxy and flushes the interception table.
  A device still associated would lose web access entirely (fail-closed).
- **`pause`** keeps the network usable but **unfiltered and unlogged** — the
  redirect table is flushed, so traffic bypasses the proxy. A cron job
  (`resume-if-due`) re-applies it when the timer expires; a reboot also clears it.
- **Fail-closed** is deliberate: if the proxy is down, kids have no web. The
  watchdog restarts it within 5 minutes and emails if it cannot.

### Dashboard

`https://192.168.1.1/admin.html` → **Kids Network** card (full width):

- badge: `Active` / `Paused` / `Off` / `Not filtering`;
- 24 h counters (requests, alerts, devices), database size, SSID;
- **Pause filtering** (15/30/60/120 min), **Turn off**, **Turn on**, **Resume now**;
- **Alerts** and **Recent activity** lists (device, time, host, block/status);
- **Domain rules**: add `block`/`allow`, remove existing;
- **Search** box: substring search over URLs and page text, sticky across the 10 s
  auto-refresh until cleared.

Backed by `GET /cgi-bin/admin?section=kids[&q=text]` (also embedded in
`section=all`) and the POST actions `kids_on`, `kids_off`, `kids_pause`,
`kids_resume`, `kids_rule`, `kids_rule_rm`.

---

## 9. Disk, SD wear, and retention

The database is on the SD card. At a few thousand requests/day with snippets
enabled, expect roughly 50–200 MB per 30-day window; `db_bytes` is shown in the
dashboard. Retention pruning runs every 6 h, so the database stays bounded.

To cut wear on a card you intend to keep:

- lower `retention_days` (7 is plenty for "what did they do this week");
- set `capture.store_snippets: false` (URLs still logged, page text is not);
- or move `/var/lib/kidproxy` to NVMe/USB-SSD (the same recommendation as the rest
  of the router).

---

## 10. Limits and known gaps

- **Certificate pinning.** Apps that pin their certificate (banking, some Google
  and social apps) reject the forged cert. They appear as `tls-pin` alerts and
  simply fail. This is expected; interception cannot beat pinning.
- **Android 7+ ignores user-installed CAs for app traffic.** Chrome and WebView
  honour the CA; most apps only trust the system store (root required to change
  that). iOS is similar for apps that opt out of ATS exceptions.
- **End-to-end encrypted messengers are unreadable.** Signal/WhatsApp/Telegram
  API hosts are blind-tunnelled so the apps keep working; message contents are
  not logged. Their metadata (which host, when, how much) still is.
- **QUIC is blocked, so browsers fall back to TCP.** This costs a little
  throughput and latency on Google properties. Do not "fix" it by allowing
  UDP/443 — that traffic would bypass the proxy entirely.
- **IPv6 is dropped on `br-kids`** and DHCPv6/RA are disabled, so devices cannot
  sidestep the IPv4 redirect. A dual-stack kids network would need the same
  interception rules for IPv6.
- **Hard-coded DoH** is handled by blocking the well-known resolvers, not by
  protocol inspection. A custom resolver IP in a custom app is a gap.
- **Throughput** is roughly 40–80 Mbps per device on the Pi 5 (TLS termination is
  CPU-bound and single-process). Fine for browsing and video; not a benchmark.
- **The CA key is a skeleton key.** Anyone who reads `/etc/kidproxy/ca.key` can
  impersonate any site to every trusting device *and* mint a valid dashboard
  certificate (it is the Pi Home CA key). Keep it 0600, keep the offline copy in
  `.claude/.secrets/ca/`, never commit it, and rotate both uses together if it leaks.
- **`off`/`pause` are honour-system controls** on the router. A device with mobile
  data, a VPN profile, or its own hotspot is outside this design.
- **Snapchat/Instagram/TikTok are not bypassed by default.** If they break, add
  their API hosts to `bypass:` knowingly — those hosts then stop being inspected.

---

## 11. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| Kids device shows `169.254.x` | DHCP did not complete. Check `logread \| grep dnsmasq` for `DHCPACK(br-kids)`; re-associate. This is usually transient during `wifi reload`. |
| Every HTTPS page warns about the certificate | CA not installed (or not *trusted* on iOS). Open `http://192.168.30.1/`. |
| Pages started warning right after a CA change | The signing CA was replaced, so devices still trust the old root. Re-install from `http://192.168.30.1/` (the page and profile name the current CA). |
| Plain `http://` requests hang | Regression in transparent plain-HTTP handling — `bufio.Peek` buffers the request, so the proxy must replay it (`prefixConn`). Covered by `TestPlainHTTPReplaysPeekedBytes`. |
| `curl -x 127.0.0.1:3128 http://…` → `405` | Expected: the explicit listener is CONNECT-only. Test plain HTTP through `:8080` instead. |
| A site is blocked that should not be | Add an **allow** rule in the dashboard; it wins over the blocklist and applies within 60 s. |
| Dashboard shows `Not filtering` | `kids-toggle.sh status`; usually `nft=absent` after a manual `fw4 reload` or a pause. `kids-nft.sh apply` restores it. |
| Hundreds of `tls-pin` alerts | A pinned app. `kidproxy alerts clear --kind tls-pin`; the proxy now dedupes per hour. |
| CGI returns 500 / `syntax error` | `www/cgi-bin/admin` was saved with CRLF. `.gitattributes` forces LF; check `git ls-files --eol www/`. |
| A POST returns two JSON bodies, or an invalid value is still applied | A `break` in a `case` branch — a silent no-op in BusyBox ash outside a loop. Must be `return`; see `AGENTS.md` trap #14. |
| Proxy won't start | `kidproxy check --config /etc/kidproxy/config.yaml`, then `logread \| grep kidproxy`. |

Useful checks (from the Pi):

```sh
kids-toggle.sh status
nft list table inet kidproxy            # counters show traffic actually redirected
curl -s http://192.168.30.1:8080/ | head -3          # bootstrap page
curl -sx 127.0.0.1:3128 --cacert /etc/kidproxy/ca.crt https://example.com/ -o /dev/null -w '%{http_code}\n'
kidproxy query --kind events --since 10m --limit 5
```

---

## 12. Rebuild checklist

1. Build the binary: `powershell -File scripts/build-kidproxy.ps1` (static
   `linux/arm64`, pure-Go SQLite — no packages needed at runtime).
2. Copy `config/kidproxy/`, `scripts/kids-*.sh`, `proxy/dist/kidproxy` **and the
   CA pair** (`.claude/.secrets/ca/ca.crt` + `ca.key`) to the Pi.
3. Run `scripts/kids-setup.sh --src /tmp/pi --ca /tmp/pi/ca [--email you@example.com]`.
   `--ca` installs the Pi Home CA; omit it and a fresh `Kids Filter CA` is minted,
   which no device trusts.
4. If `br-kids` has no address, reboot once so netifd settles.
5. Install `http://192.168.30.1/`'s CA on each device (it is `Pi Home CA`).
6. Configure `/etc/msmtprc`, set `notify.email.enabled: true` and `to:`, then
   `kidproxy notify-test`.
7. Verify: `kids-toggle.sh status`, the block page, a YouTube title in
   `kidproxy query --kind youtube`, and the dashboard Kids panel.

`gofmt`, `go vet` and `go test ./...` (34 tests, including regressions for the
plain-HTTP replay bug, ECDSA **and RSA** CA leaf signing, CA cert/key mismatch,
allow-overrides-block, alert dedupe, and
the stats/rule helpers) must pass before deploying.
