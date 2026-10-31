# Kids network: bedtimes and daily time limits (as-built)

Per-kid controls for devices on the kids network (`Home-Kids`, `br-kids`,
`192.168.30.0/24`), managed from the admin dashboard's **Bedtimes & Time Limits**
card:

- **Devices → kid.** The card lists the devices on the kids WiFi; assign each one to
  a kid (a kid can have several devices).
- **Bedtimes.** Per kid, any number of windows such as *Sun–Thu 9:00 PM → 7:00 AM*.
  During a window that kid's devices get no internet at all.
- **Daily limits.** Per kid, minutes per day for each category (Video, Social media,
  Games). **0 blocks the category**; blank means no limit. Once a kid reaches a
  limit, that category is blocked on all their devices until midnight.

Example: Kid A → phone + tablet, Sun–Thu 9 PM–7 AM, Video 60, Social 30.
Kid B → tablet, Sun–Thu 10 PM–7 AM, Video 120, Social 0.

Other kids and the adult LAN are unaffected. Nothing is decrypted: categories are
matched by the addresses their domains resolve to.

## How it works

| Piece | Path (on the Pi) | Repo path |
|---|---|---|
| Config (UCI) | `/etc/config/kids_schedule` | `config/etc-config/kids_schedule` |
| Enforcer | `/usr/bin/kids-schedule.sh` | `scripts/kids-schedule.sh` |
| Dashboard API | `GET /cgi-bin/admin?section=schedule`, POST `sched_*` | `www/cgi-bin/admin` |
| Dashboard card | *Bedtimes & Time Limits* | `www/admin.html` |
| Cron | `* * * * * /usr/bin/kids-schedule.sh apply` | added by `kids-schedule.sh install` |
| nft tables | `inet kids_schedule` (bedtimes), `inet kids_limits` (limits) | generated |
| dnsmasq hooks | `config ipset 'kidslim_<service>'` in `/etc/config/dhcp` | generated |
| Usage counters | `/tmp/kids-limits/` (RAM) | — |
| Usage history | `/tmp/kids-stats/` (today), `/etc/kids-stats/` (90 days) | — |
| Analytics page | `/analytics.html` | `www/analytics.html` |

### Bedtimes

Every minute `apply` works out, from the router's **local time**, which kids are
inside a window and atomically replaces `table inet kids_schedule` with a set of
their MACs. A chain at `prerouting` priority **-150** drops every packet from those
MACs on `br-kids`:

- It runs **before conntrack's established-accept and before any redirect**, so a
  video or game already connected stops at bedtime too.
- **DHCP (udp/67) is still answered**, so the device stays on the WiFi and shows
  "connected, no internet".
- A reboot or missed minute self-corrects within a minute. `kids-toggle.sh pause`
  does not lift a bedtime.

A window's days are the days it **starts**; if the end is earlier than the start it
runs past midnight (`sun 21:00-07:00` = Sunday 9 PM to Monday 7 AM).

### Services, categories and daily limits

1. **Services** (`config service`: label, category, domain list) are the unit of
   measurement; **categories** (`config category`: label, `min_bytes`) group them
   and carry the limits. For each service `kids-schedule.sh` creates `svc_<service>`
   (IPv4, 1-day element timeout) in `table inet kids_limits` and a dnsmasq
   `config ipset 'kidslim_<service>'` section, so every DNS answer for its domains
   adds the addresses to the set. Kids' DNS is forced to the Pi (firewall rule
   `Kids-Force-DNS`), so their lookups are seen.
2. **Metering.** A rule at prerouting **-149** updates a dynamic set
   `use_<service>` with a **byte counter per MAC**. Each minute, per MAC, a service
   is *active* if its byte delta reaches its category's `min_bytes` (video 50 KB,
   others 20 KB), and a category is active if the sum over its services does.
3. **Limits.** A kid's category minute counts toward their limit when the category
   bytes summed over all their devices reach `min_bytes`. When used ≥ limit (or the
   limit is 0), the kid's MACs go into `blk_<category>`, and a rule per service
   drops their traffic to every service in that category — including connections
   already open. Limit usage is kept in `/tmp/kids-limits/usage-YYYYMMDD` and resets
   at midnight (and on reboot). Bedtime-blocked traffic is dropped before metering,
   so it never counts.

Logged transitions: `logread -e kids-schedule` (bedtime start/end, "limit reached").

### Usage history, analytics page and CSV export

Every active minute is also recorded per **device, hour, service and category** in
`/tmp/kids-stats/YYYYMMDD.tsv` (RAM). Each line is `HH mac s|c id minutes`
(`s` = service, `c` = category). It is copied to **`/etc/kids-stats/`** (flash) at
:29 and :59 each hour, finalized there at midnight, restored into RAM after a
reboot, and kept **90 days** (`HISTORY_DAYS`). Only aggregate minutes are stored —
no URLs, no content.

- **`/analytics.html`** (linked from the admin header; same login): time per day
  stacked by category, click a category (segment, legend or table row) to see its
  services; click a day to see that day's hours; time-of-day and day-of-week
  charts; filter by kid (or *Unassigned devices*) and 7/14/30/90 days; a
  breakdown table that is also the accessible view of the charts.
- **Download CSV** on that page (`GET /cgi-bin/admin?section=statscsv&days=N`):
  `date,weekday,hour,kid,device_mac,device_name,type,category,service,minutes`,
  one row per device/hour/service and per device/hour/category (`type` says which).
  Kid names come from the current device assignments; unassigned devices say
  `unassigned`. JSON for the page: `section=stats&days=N`.

A kid's time is the sum over their devices (two devices in the same minute count
twice). A category counts a minute once even when several of its services were
active, so its services can add up to slightly more than the category.

### Default services

Seeded by `install` only when the config has no services (an older config's
category-level domain lists are dropped then); edit freely — the next `apply`
rebuilds the sets and restarts dnsmasq once.

| Category | Services (domains, suffix match) |
|---|---|
| `video` | YouTube (`googlevideo.com`), Netflix (`netflix.com`, `nflxvideo.net`, `nflxso.net`, `nflximg.net`), Twitch (`twitch.tv`, `ttvnw.net`, `jtvnw.net`), Disney+ (`disneyplus.com`, `dssott.com`, `bamgrid.com`), Hulu, Prime Video (`primevideo.com`, `aiv-cdn.net`, `aiv-delivery.net`), Vimeo |
| `social` | TikTok (`tiktok*`, `byteoversea.com`, `ibytedtos.com`), Instagram, Snapchat, Facebook (`facebook.com`, `fbcdn.net`), Discord, Reddit, X/Twitter, Pinterest, Threads, Bluesky |
| `games` | Roblox (`roblox.com`, `rbxcdn.com`, `rbx.com`), Fortnite/Epic, Minecraft |

The analytics page colors up to 7 services per category in config order and folds
the rest into *Other*, so keep the most-used services first.

> **YouTube is metered and blocked on `googlevideo.com` (its video servers), not
> `youtube.com`.** `youtube.com` and `ytimg.com` resolve to Google front-end
> addresses shared with Search, Gmail and Drive; putting them in a set would meter
> and block those too. Over the limit, the YouTube site/app still opens but videos
> will not play.

After changing a category's domains, the next `apply` rebuilds `kids_limits` and
restarts dnsmasq (the sets refill as devices look names up again).

## Accuracy and limits

- **Usage is approximate, to the minute.** Players that buffer ahead can be
  under-counted slightly; apps refreshing in the background can add a few minutes.
- **Usage resets on reboot** (it lives in RAM to avoid SD-card writes every minute).
- **Shared infrastructure.** A category address set can contain an address another
  service also uses (e.g. a CDN). If something unrelated breaks only when a kid is
  over a limit, remove the overly broad domain from that category.
- **Encrypted DNS on the device** (a browser's own DoH) bypasses the Pi's DNS, so
  its lookups never reach the sets. DoT (853) is already blocked for the kids
  network; turn off "secure DNS" in the kids' browsers for full coverage.
- **Games**: limits gate logins, lobbies and downloads. Live gameplay often talks
  to game-server IPs that are not looked up by name, so it may not be metered or
  cut mid-match — the block applies at the next join.
- Only **devices on the kids WiFi** with a **known MAC** are covered. Set each kid
  device's private WiFi address for `Home-Kids` to **Fixed** (iOS: *Private Wi-Fi
  Address → Fixed*; Android: keep the per-network randomized MAC, not
  "non-persistent"). The card lists unassigned devices so new ones stand out.

## Time zone

Windows and the midnight reset use the router's local time. As built the Pi is
**America/Phoenix** (`MST7`, no DST):

```sh
uci set system.@system[0].zonename='America/Phoenix'
uci set system.@system[0].timezone='MST7'
uci commit system && /etc/init.d/system reload && /etc/init.d/cron restart
```

## Install / operate

```sh
scp -O scripts/kids-schedule.sh root@192.168.1.1:/usr/bin/
scp -O www/cgi-bin/admin root@192.168.1.1:/www/cgi-bin/admin
scp -O www/admin.html root@192.168.1.1:/www/admin.html
ssh root@192.168.1.1 'chmod 755 /usr/bin/kids-schedule.sh /www/cgi-bin/admin && kids-schedule.sh install'
```

`install` seeds the default categories and services (if none), adds the cron line, restarts
cron and runs a first `apply` (which creates the tables and the dnsmasq hooks,
restarting dnsmasq once). The dashboard's **Enable schedules** button runs the same
command. After installing, copy `/etc/config/dhcp` and `/etc/config/kids_schedule`
back into `config/etc-config/`.

```sh
kids-schedule.sh status   # <kid> blocked=… window=…, limit <kid> <cat> used= max= over=, category …
kids-schedule.sh flush    # remove both tables and the dnsmasq hooks (until the next apply)
```

To disable entirely: remove the cron line, then `kids-schedule.sh flush`.
