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
| dnsmasq hooks | `config ipset 'kidslim_<category>'` in `/etc/config/dhcp` | generated |
| Usage counters | `/tmp/kids-limits/` (RAM) | — |

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

### Daily limits

1. **Categories** are domain lists in `/etc/config/kids_schedule` (`config category`).
   For each, `kids-schedule.sh` creates `cat_<category>` (IPv4, 1-day element
   timeout) in `table inet kids_limits` and a dnsmasq `config ipset` section, so
   every DNS answer for those domains adds its addresses to the set. Kids' DNS is
   forced to the Pi (firewall rule `Kids-Force-DNS`), so their lookups are seen.
2. **Metering.** A rule at prerouting **-149** updates a dynamic set
   `use_<category>` with a **byte counter per MAC** for traffic to those addresses.
   Each minute the script takes each kid's byte delta (all their devices) per
   category; if it is at least the category's `min_bytes` (video 50 KB, others
   20 KB), the minute counts as used.
3. **Enforcement.** When used ≥ limit (or the limit is 0), the kid's MACs go into
   `blk_<category>`, and a rule before the meter drops their traffic to that
   category — including connections already open.
4. Usage is kept per day in `/tmp/kids-limits/usage-YYYYMMDD` and resets at
   midnight. Bedtime-blocked traffic is dropped before metering, so it never counts.

Logged transitions: `logread -e kids-schedule` (bedtime start/end, "limit reached").

### Default categories

Seeded by `install` only when the config has none; edit freely.

| Category | Domains (suffix match) |
|---|---|
| `video` | `googlevideo.com`, Netflix (`netflix.com`, `nflxvideo.net`), Twitch (`twitch.tv`, `ttvnw.net`, `jtvnw.net`), Disney+ (`disneyplus.com`, `dssott.com`, `bamgrid.com`), Hulu, Vimeo, Prime Video (`primevideo.com`, `aiv-cdn.net`) |
| `social` | Instagram, Facebook (`facebook.com`, `fbcdn.net`), TikTok (`tiktok*`, `byteoversea.com`, `ibytedtos.com`), Snapchat, X/Twitter, Reddit, Discord, Pinterest, Threads, Bluesky |
| `games` | Roblox (`roblox.com`, `rbxcdn.com`, `rbx.com`), Epic/Fortnite, Minecraft |

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

`install` seeds the default categories (if none), adds the cron line, restarts
cron and runs a first `apply` (which creates the tables and the dnsmasq hooks,
restarting dnsmasq once). The dashboard's **Enable schedules** button runs the same
command. After installing, copy `/etc/config/dhcp` and `/etc/config/kids_schedule`
back into `config/etc-config/`.

```sh
kids-schedule.sh status   # <kid> blocked=… window=…, limit <kid> <cat> used= max= over=, category …
kids-schedule.sh flush    # remove both tables and the dnsmasq hooks (until the next apply)
```

To disable entirely: remove the cron line, then `kids-schedule.sh flush`.
