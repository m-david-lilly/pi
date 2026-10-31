#!/bin/sh
# Per-kid controls for the kids network: bedtimes, daily time limits per
# category, and usage history for the analytics page.
#
#   kids-schedule.sh apply      evaluate everything now (cron, every minute)
#   kids-schedule.sh status     machine-readable state for the dashboard
#   kids-schedule.sh flush      remove both nft tables and the dnsmasq hooks
#   kids-schedule.sh install    seed default categories/services, add cron, first apply
#
# /etc/config/kids_schedule (UCI):
#
#   config kid 'cfg0a1b2c'
#   	option name 'Alex'
#   	option enabled '1'
#   	list mac 'aa:bb:cc:dd:ee:ff'
#   	list block 'sun mon tue wed thu 21:00-07:00'   # bedtime windows
#   	list limit 'video 60'                          # minutes per day
#   	list limit 'social 0'                          # 0 = always blocked
#
#   config category 'video'
#   	option label 'Video'
#   	option min_bytes '50000'      # traffic in a minute that counts as "used"
#
#   config service 'youtube'
#   	option label 'YouTube'
#   	option category 'video'
#   	list domain 'googlevideo.com'
#
# BEDTIMES. A `block` entry is "<days> HH:MM-HH:MM"; the days are the days the
# window STARTS on, and an end earlier than the start runs past midnight. Times
# are router local time (system timezone). Enforced by table inet kids_schedule:
# every packet from a blocked MAC on br-kids is dropped at prerouting -150,
# before conntrack's established-accept and any redirect, so open connections
# stop too. DHCP is still answered.
#
# SERVICES, LIMITS, HISTORY. dnsmasq adds the IPv4 answers for each service's
# domains to `svc_<service>` (nftset=, set timeout 1d) in table inet
# kids_limits. A dynamic set `use_<service>` keeps a byte counter per MAC. Each
# minute, per MAC, a service is "active" if its byte delta reaches its
# category's min_bytes, and a category is active if the sum over its services
# does. Active minutes are added to /tmp/kids-stats/YYYYMMDD.tsv
# ("HH mac s|c id minutes"), copied to /etc/kids-stats twice an hour (kept
# HISTORY_DAYS) for the analytics page. A kid's category minute counts toward
# their limit if the category bytes summed over all their MACs reach
# min_bytes; once used >= limit (or limit is 0) the kid's MACs go into
# `blk_<category>` and their traffic to every service in it is dropped until
# midnight. Limit usage lives in /tmp/kids-limits and resets at midnight/reboot.
#
# (No `set -u`: /lib/functions.sh references unset variables.)

TABLE=kids_schedule
LTABLE=kids_limits
IFACE=br-kids
STATE=/tmp/kids-schedule.state
LDIR=/tmp/kids-limits
SDIR=/tmp/kids-stats
PERSIST=/etc/kids-stats
HISTORY_DAYS=90

. /lib/functions.sh

# HH:MM -> minutes since midnight; fails on malformed input.
to_min() {
	case "$1" in
		[0-2][0-9]:[0-5][0-9]) ;;
		*) return 1 ;;
	esac
	h=${1%%:*}; m=${1##*:}
	h=${h#0}; m=${m#0}
	[ "${h:-0}" -le 23 ] || return 1
	echo $(( ${h:-0} * 60 + ${m:-0} ))
}

# Is "<days> HH:MM-HH:MM" active at minute $now on day $today (yesterday $yday)?
window_active() {
	spec=$1
	range=${spec##* }
	days=" ${spec% *} "
	s=$(to_min "${range%-*}") || return 1
	e=$(to_min "${range#*-}") || return 1
	if [ "$s" -lt "$e" ]; then
		case "$days" in *" $today "*) [ "$now" -ge "$s" ] && [ "$now" -lt "$e" ] && return 0 ;; esac
	else
		case "$days" in *" $today "*) [ "$now" -ge "$s" ] && return 0 ;; esac
		case "$days" in *" $yday "*) [ "$now" -lt "$e" ] && return 0 ;; esac
	fi
	return 1
}

lower() { echo "$1" | tr 'A-Z' 'a-z'; }

# ── Bedtimes ────────────────────────────────────────────────────────

# Sets: RESULT ("<kid> <0|1> <window>" lines), BLOCK_MACS, KIDS (enabled kid
# ids), and macs_<kid> (space-separated) for every enabled kid.
evaluate() {
	now=$(to_min "$(date +%H:%M)")
	today=$(lower "$(date +%a)")
	yday=$(lower "$(date -d "@$(( $(date +%s) - 86400 ))" +%a)")
	BLOCK_MACS=""
	RESULT=""
	KIDS=""

	check_window() {
		[ -n "$active" ] && return
		window_active "$1" && active=$1
	}
	add_mac() {
		kid_macs="$kid_macs $(lower "$1")"
	}
	each_kid() {
		local enabled active="" kid_macs=""
		config_get_bool enabled "$1" enabled 1
		[ "$enabled" = 1 ] || { RESULT="$RESULT$1 0 disabled
"; return; }
		config_list_foreach "$1" mac add_mac
		# A hand-edited `option mac` (not `list`) is invisible to list_foreach.
		if [ -z "$kid_macs" ]; then
			config_get one_mac "$1" mac
			[ -n "$one_mac" ] && add_mac "$one_mac"
		fi
		eval "macs_$1=\"\$kid_macs\""
		KIDS="$KIDS $1"
		config_list_foreach "$1" block check_window
		if [ -n "$active" ]; then
			BLOCK_MACS="$BLOCK_MACS $kid_macs"
			RESULT="$RESULT$1 1 ${active##* }
"
		else
			RESULT="$RESULT$1 0 -
"
		fi
	}
	config_load kids_schedule
	config_foreach each_kid kid
}

# Replace the bedtime table atomically with the current block set.
sync_table() {
	elems=$(echo $BLOCK_MACS | tr ' ' ',')
	elem_line=""
	[ -n "$elems" ] && elem_line="elements = { $elems }"
	nft -f - <<-EOF
	table inet $TABLE
	delete table inet $TABLE
	table inet $TABLE {
		set blocked {
			type ether_addr
			$elem_line
		}
		chain prerouting {
			type filter hook prerouting priority -150; policy accept;
			iifname "$IFACE" ether saddr @blocked udp dport 67 accept comment "kids-schedule: keep DHCP"
			iifname "$IFACE" ether saddr @blocked counter drop comment "kids-schedule: bedtime"
		}
	}
	EOF
}

# ── Categories and services ─────────────────────────────────────────

# Default categories and services, written only when the config has no
# services. Older configs kept domain lists on the categories themselves;
# those are dropped in favour of the per-service lists.
seed_defaults() {
	[ -n "$(uci -q show kids_schedule | grep '=service$')" ] && return
	touch /etc/config/kids_schedule
	for c in $(uci -q show kids_schedule | sed -n 's/^kids_schedule\.\([A-Za-z0-9_]*\)=category$/\1/p'); do
		uci -q delete "kids_schedule.$c.domain"
	done
	{
		cat_def() { echo "set kids_schedule.$1=category"; echo "set kids_schedule.$1.label='$2'"; echo "set kids_schedule.$1.min_bytes='$3'"; }
		svc_def() {
			id=$1 cat=$2 label=$3; shift 3
			echo "set kids_schedule.$id=service"
			echo "set kids_schedule.$id.label='$label'"
			echo "set kids_schedule.$id.category='$cat'"
			for d in "$@"; do echo "add_list kids_schedule.$id.domain='$d'"; done
		}
		cat_def video 'Video' 50000
		cat_def social 'Social media' 20000
		cat_def games 'Games' 20000
		svc_def youtube video 'YouTube' googlevideo.com
		svc_def netflix video 'Netflix' netflix.com nflxvideo.net nflxso.net nflximg.net
		svc_def twitch video 'Twitch' twitch.tv ttvnw.net jtvnw.net
		svc_def disney video 'Disney+' disneyplus.com dssott.com bamgrid.com
		svc_def hulu video 'Hulu' hulu.com hulustream.com
		svc_def prime video 'Prime Video' primevideo.com aiv-cdn.net aiv-delivery.net
		svc_def vimeo video 'Vimeo' vimeo.com vimeocdn.com
		svc_def tiktok social 'TikTok' tiktok.com tiktokv.com tiktokcdn.com tiktokcdn-us.com byteoversea.com ibytedtos.com
		svc_def instagram social 'Instagram' instagram.com cdninstagram.com
		svc_def snapchat social 'Snapchat' snapchat.com sc-cdn.net snapkit.com
		svc_def facebook social 'Facebook' facebook.com fbcdn.net
		svc_def discord social 'Discord' discord.com discordapp.com discordapp.net discord.gg
		svc_def reddit social 'Reddit' reddit.com redd.it redditmedia.com redditstatic.com
		svc_def x social 'X (Twitter)' twitter.com x.com twimg.com
		svc_def pinterest social 'Pinterest' pinterest.com pinimg.com
		svc_def threads social 'Threads' threads.net
		svc_def bluesky social 'Bluesky' bsky.app
		svc_def roblox games 'Roblox' roblox.com rbxcdn.com rbx.com
		svc_def fortnite games 'Fortnite / Epic' epicgames.com epicgames.dev fortnite.com
		svc_def minecraft games 'Minecraft' minecraft.net mojang.com minecraftservices.com
	} | uci -q batch
	uci commit kids_schedule
	config_load kids_schedule
}

# Sets CATS, SVCS, SVCMAP ("svc:cat ...") and MINMAP ("cat:bytes ...").
load_catalog() {
	CATS="" SVCS="" SVCMAP="" MINMAP=""
	collect_cat() {
		local mb
		config_get mb "$1" min_bytes 20000
		CATS="$CATS $1"; MINMAP="$MINMAP $1:$mb"
	}
	collect_svc() {
		local sc
		config_get sc "$1" category
		case " $CATS " in *" $sc "*) ;; *) return ;; esac
		SVCS="$SVCS $1"; SVCMAP="$SVCMAP $1:$sc"
	}
	config_foreach collect_cat category
	config_foreach collect_svc service
}

svc_cat() { echo " $SVCMAP " | sed -n "s/.* $1:\([^ ]*\) .*/\1/p"; }

# Fingerprint of everything that shapes the limits table / dnsmasq hooks.
catalog_sig() {
	uci -q show kids_schedule | grep -E "^kids_schedule\.[A-Za-z0-9_]+(=category|=service|\.domain=|\.category=)" | md5sum | cut -d' ' -f1
}

# (Re)create table inet kids_limits. Only runs when the catalog changes or the
# table is missing: recreating it empties the dnsmasq-filled IP sets.
build_limits_table() {
	{
		echo "table inet $LTABLE"
		echo "delete table inet $LTABLE"
		echo "table inet $LTABLE {"
		for c in $CATS; do
			echo "	set blk_$c { type ether_addr; }"
		done
		for s in $SVCS; do
			echo "	set svc_$s { type ipv4_addr; flags timeout; timeout 1d; }"
			echo "	set use_$s { type ether_addr; flags dynamic; counter; size 1024; }"
		done
		echo "	chain prerouting {"
		echo "		type filter hook prerouting priority -149; policy accept;"
		for s in $SVCS; do
			c=$(svc_cat "$s")
			echo "		iifname \"$IFACE\" ether saddr @blk_$c ip daddr @svc_$s counter drop comment \"kids-limits: $c over limit ($s)\""
			echo "		iifname \"$IFACE\" ip daddr @svc_$s update @use_$s { ether saddr } comment \"kids-limits: meter $s\""
		done
		echo "	}"
		echo "}"
	} | nft -f -
}

# Point dnsmasq at the service sets: one `config ipset 'kidslim_<service>'`
# per service in /etc/config/dhcp (OpenWrt's dnsmasq init turns these into
# --nftset=/dom1/dom2/4#inet#kids_limits#svc_<service>). Replaces all of our
# previous kidslim_* sections.
sync_dnsmasq() {
	for sec in $(uci -q show dhcp | sed -n 's/^dhcp\.\(kidslim_[A-Za-z0-9_]*\)=ipset$/\1/p'); do
		uci -q delete "dhcp.$sec"
	done
	# Not `c`/`s`: config_list_foreach uses a local `c` as its loop counter,
	# which would shadow ours inside the callback.
	add_dom() { uci add_list "dhcp.kidslim_$svc_id.domain=$1"; }
	for svc_id in $SVCS; do
		uci set "dhcp.kidslim_$svc_id=ipset"
		uci add_list "dhcp.kidslim_$svc_id.name=svc_$svc_id"
		uci set "dhcp.kidslim_$svc_id.table=$LTABLE"
		uci set "dhcp.kidslim_$svc_id.table_family=inet"
		uci set "dhcp.kidslim_$svc_id.family=4"
		config_list_foreach "$svc_id" domain add_dom
	done
	uci commit dhcp
	/etc/init.d/dnsmasq restart >/dev/null 2>&1
}

# "svc mac bytes" for every use_<service> element, from one nft listing.
read_use() {
	nft list table inet $LTABLE 2>/dev/null | awk '
		/^[[:space:]]*set use_/ { svc = $2; sub(/^use_/, "", svc); inset = 1; next }
		/^[[:space:]]*set / { inset = 0; next }
		/^[[:space:]]*chain / { inset = 0; next }
		inset {
			n = split($0, parts, ",")
			for (i = 1; i <= n; i++)
				# Spelled out: BusyBox awk regexes may not support {n} intervals.
				if (match(parts[i], /[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f]:[0-9a-f][0-9a-f] counter packets [0-9]+ bytes [0-9]+/)) {
					split(substr(parts[i], RSTART, RLENGTH), f, " ")
					print svc, f[1], f[6]
				}
		}'
}

# Persist today's stats to flash and retire old days.
persist_stats() {
	mkdir -p "$PERSIST"
	cp "$SDIR/$day.tsv" "$PERSIST/$day.tsv.tmp" && mv "$PERSIST/$day.tsv.tmp" "$PERSIST/$day.tsv"
	cutoff=$(date -d "@$(( $(date +%s) - HISTORY_DAYS * 86400 ))" +%Y%m%d)
	for f in "$PERSIST"/*.tsv; do
		[ -e "$f" ] || continue
		b=${f##*/}; b=${b%.tsv}
		[ "$b" -lt "$cutoff" ] 2>/dev/null && rm -f "$f"
	done
}

# Meter one minute: record history, update today's limit usage and the
# over-limit sets. Sets LIMITS ("<kid> <cat> <used> <max> <0|1>" lines).
apply_limits() {
	mkdir -p "$LDIR" "$SDIR"
	load_catalog
	sig=$(catalog_sig)
	if ! nft list table inet $LTABLE >/dev/null 2>&1 || [ "$sig" != "$(cat "$LDIR/sig" 2>/dev/null)" ]; then
		build_limits_table || { logger -t kids-schedule "failed to build limits table"; return 1; }
		sync_dnsmasq
		echo "$sig" > "$LDIR/sig"
		rm -f "$LDIR/last"
	fi

	day=$(date +%Y%m%d)
	hour=$(date +%H)
	minute=$(date +%M)

	# Day rollover: finalize earlier /tmp stats files to flash, then drop them.
	for f in "$SDIR"/*.tsv; do
		[ -e "$f" ] || continue
		[ "$f" = "$SDIR/$day.tsv" ] && continue
		mkdir -p "$PERSIST" && cp "$f" "$PERSIST/${f##*/}" && rm -f "$f"
	done
	# After a reboot, continue today's history from the last flash copy.
	[ -f "$SDIR/$day.tsv" ] || { [ -f "$PERSIST/$day.tsv" ] && cp "$PERSIST/$day.tsv" "$SDIR/$day.tsv"; }
	touch "$SDIR/$day.tsv"

	usage="$LDIR/usage-$day"
	touch "$usage"
	for f in "$LDIR"/usage-*; do
		[ "$f" = "$usage" ] || rm -f "$f"
	done

	# Byte deltas per (service, MAC) since last minute (counter reset -> use cur).
	touch "$LDIR/last"
	read_use > "$LDIR/cur"
	awk 'FILENAME == ARGV[1] { last[$1 " " $2] = $3; next }
	     { d = $3 - last[$1 " " $2]; if (d < 0) d = $3; if (d > 0) print $1, $2, d }' \
		"$LDIR/last" "$LDIR/cur" > "$LDIR/delta"
	mv "$LDIR/cur" "$LDIR/last"

	# This minute's activity: active services and categories per MAC (history),
	# and category bytes per MAC (for limits).
	awk -v H="$hour" -v svcmap="$SVCMAP" -v minmap="$MINMAP" -v catbytes="$LDIR/catbytes" '
		BEGIN {
			n = split(svcmap, a, " "); for (i = 1; i <= n; i++) { split(a[i], p, ":"); cat[p[1]] = p[2] }
			n = split(minmap, a, " "); for (i = 1; i <= n; i++) { split(a[i], p, ":"); minb[p[1]] = p[2] }
		}
		{
			c = cat[$1]; if (c == "") next
			if ($3 >= minb[c]) print H, $2, "s", $1, 1
			cb[$2 " " c] += $3
		}
		END {
			printf "" > catbytes
			for (k in cb) {
				split(k, p, " ")
				print p[1], p[2], cb[k] > catbytes
				if (cb[k] >= minb[p[2]]) print H, p[1], "c", p[2], 1
			}
		}' "$LDIR/delta" > "$LDIR/minute"
	if [ -s "$LDIR/minute" ]; then
		awk '{ t[$1 " " $2 " " $3 " " $4] += $5 } END { for (k in t) print k, t[k] }' \
			"$SDIR/$day.tsv" "$LDIR/minute" > "$SDIR/$day.tsv.new" && mv "$SDIR/$day.tsv.new" "$SDIR/$day.tsv"
	fi
	case "$minute" in 29|59) persist_stats ;; esac

	LIMITS=""
	for c in $CATS; do eval "blk_$c=''"; done
	each_limit() {
		lc=${1% *}; max=${1#* }
		case " $CATS " in *" $lc "*) ;; *) return ;; esac
		case "$max" in ''|*[!0-9]*) return ;; esac
		config_get min_bytes "$lc" min_bytes 20000
		used=$(awk -v k="$kid" -v c="$lc" '$1 == k && $2 == c { print $3 }' "$usage")
		used=${used:-0}
		bytes=$(awk -v macs="$kmacs" -v c="$lc" '
			BEGIN { n = split(macs, a, " "); for (i = 1; i <= n; i++) want[a[i]] = 1 }
			want[$1] && $2 == c { s += $3 } END { print s + 0 }' "$LDIR/catbytes")
		if [ "$max" -gt 0 ] && [ "$used" -lt "$max" ] && [ "$bytes" -ge "$min_bytes" ]; then
			used=$(( used + 1 ))
			[ "$used" -ge "$max" ] && logger -t kids-schedule "$kid: $lc limit reached ($max min)"
		fi
		over=0
		{ [ "$max" -eq 0 ] || [ "$used" -ge "$max" ]; } && over=1
		[ "$over" = 1 ] && eval "blk_$lc=\"\$blk_$lc $kmacs\""
		echo "$kid $lc $used" >> "$usage.new"
		LIMITS="$LIMITS$kid $lc $used $max $over
"
	}
	: > "$usage.new"
	for kid in $KIDS; do
		eval "kmacs=\$macs_$kid"
		config_list_foreach "$kid" limit each_limit
	done
	mv "$usage.new" "$usage"

	nft_cmds=""
	for c in $CATS; do
		eval "elems=\$blk_$c"
		elems=$(echo $elems | tr ' ' ',')
		nft_cmds="${nft_cmds}flush set inet $LTABLE blk_$c
"
		[ -n "$elems" ] && nft_cmds="${nft_cmds}add element inet $LTABLE blk_$c { $elems }
"
	done
	printf '%s' "$nft_cmds" | nft -f - || { logger -t kids-schedule "failed to update limit sets"; return 1; }
}

# Current usage without metering (for status): reads today's file only.
read_limits() {
	load_catalog
	usage="$LDIR/usage-$(date +%Y%m%d)"
	LIMITS=""
	each_limit_ro() {
		lc=${1% *}; max=${1#* }
		case " $CATS " in *" $lc "*) ;; *) return ;; esac
		case "$max" in ''|*[!0-9]*) return ;; esac
		used=$(awk -v k="$kid" -v c="$lc" '$1 == k && $2 == c { print $3 }' "$usage" 2>/dev/null)
		used=${used:-0}
		over=0
		{ [ "$max" -eq 0 ] || [ "$used" -ge "$max" ]; } && over=1
		LIMITS="$LIMITS$kid $lc $used $max $over
"
	}
	for kid in $KIDS; do
		config_list_foreach "$kid" limit each_limit_ro
	done
}

case "${1:-status}" in
apply)
	evaluate
	sync_table || { logger -t kids-schedule "failed to apply block table"; exit 1; }
	# Log only bedtime transitions, not every minute. $(...) strips trailing
	# newlines on both sides, so compare the stripped forms.
	cur=$(printf '%s' "$RESULT")
	if [ "$cur" != "$(cat "$STATE" 2>/dev/null)" ]; then
		echo "$cur" | while read -r kid blocked win; do
			[ -n "$kid" ] || continue
			case "$blocked:$win" in
				1:*) msg="blocked ($win)" ;;
				*:disabled) msg="schedule disabled" ;;
				*) msg="allowed" ;;
			esac
			logger -t kids-schedule "$kid: $msg"
		done
		echo "$cur" > "$STATE"
	fi
	apply_limits
	;;
status)
	# <kid> blocked=<0|1> window=<HH:MM-HH:MM|-|disabled>
	# limit <kid> <category> used=<min> max=<min> over=<0|1>
	# category <id> <label>
	# service <id> <category> <label>
	evaluate
	echo "$RESULT" | while read -r kid blocked win; do
		[ -n "$kid" ] && echo "$kid blocked=$blocked window=$win"
	done
	read_limits
	echo "$LIMITS" | while read -r kid c used max over; do
		[ -n "$kid" ] && echo "limit $kid $c used=$used max=$max over=$over"
	done
	for c in $CATS; do
		config_get label "$c" label "$c"
		echo "category $c $label"
	done
	for s in $SVCS; do
		config_get label "$s" label "$s"
		echo "service $s $(svc_cat "$s") $label"
	done
	nft list table inet $TABLE >/dev/null 2>&1 && echo "table=applied" || echo "table=absent"
	;;
flush)
	nft delete table inet $TABLE 2>/dev/null
	nft delete table inet $LTABLE 2>/dev/null
	SVCS=""
	sync_dnsmasq
	# Keep today's history: persist it before clearing RAM state.
	day=$(date +%Y%m%d)
	[ -f "$SDIR/$day.tsv" ] && persist_stats
	rm -rf "$STATE" "$LDIR" "$SDIR"
	;;
install)
	config_load kids_schedule
	seed_defaults
	grep -q 'kids-schedule.sh apply' /etc/crontabs/root 2>/dev/null || \
		echo '* * * * * /usr/bin/kids-schedule.sh apply' >> /etc/crontabs/root
	/etc/init.d/cron enable
	/etc/init.d/cron restart
	"$0" apply
	;;
*)
	echo "usage: $0 apply|status|flush|install" >&2
	exit 2
	;;
esac
