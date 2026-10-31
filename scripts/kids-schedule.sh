#!/bin/sh
# Per-kid internet schedules for the kids network: bedtimes and daily time
# limits per category (video, social, games, ...).
#
#   kids-schedule.sh apply      evaluate everything now (cron, every minute)
#   kids-schedule.sh status     machine-readable state for the dashboard
#   kids-schedule.sh flush      remove both nft tables and the dnsmasq hooks
#   kids-schedule.sh install    seed default categories, add cron, first apply
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
#   	list domain 'googlevideo.com'
#
# BEDTIMES. A `block` entry is "<days> HH:MM-HH:MM"; the days are the days the
# window STARTS on, and an end earlier than the start runs past midnight. Times
# are router local time (system timezone). Enforced by table inet kids_schedule:
# every packet from a blocked MAC on br-kids is dropped at prerouting -150,
# before conntrack's established-accept and any redirect, so open connections
# stop too. DHCP is still answered.
#
# LIMITS. dnsmasq adds the IPv4 answers for each category's domains to
# `cat_<category>` (nftset=, set timeout 1d) in table inet kids_limits. A
# dynamic set `use_<category>` keeps a byte counter per kid MAC. Each minute a
# kid's total delta per category is compared with min_bytes; if exceeded the
# minute counts. Once used >= limit (or limit is 0) the kid's MACs go into
# `blk_<category>` and their traffic to that category is dropped until midnight.
# Usage lives in /tmp (RAM): it resets at midnight and on reboot.
#
# (No `set -u`: /lib/functions.sh references unset variables.)

TABLE=kids_schedule
LTABLE=kids_limits
IFACE=br-kids
STATE=/tmp/kids-schedule.state
LDIR=/tmp/kids-limits

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

# ── Daily limits ────────────────────────────────────────────────────

# Default categories, written only when the config has none.
seed_categories() {
	[ -n "$(uci -q show kids_schedule | grep '=category$')" ] && return
	touch /etc/config/kids_schedule
	uci -q batch <<-EOF
	set kids_schedule.video=category
	set kids_schedule.video.label='Video'
	set kids_schedule.video.min_bytes='50000'
	add_list kids_schedule.video.domain='googlevideo.com'
	add_list kids_schedule.video.domain='nflxvideo.net'
	add_list kids_schedule.video.domain='netflix.com'
	add_list kids_schedule.video.domain='twitch.tv'
	add_list kids_schedule.video.domain='ttvnw.net'
	add_list kids_schedule.video.domain='jtvnw.net'
	add_list kids_schedule.video.domain='disneyplus.com'
	add_list kids_schedule.video.domain='dssott.com'
	add_list kids_schedule.video.domain='bamgrid.com'
	add_list kids_schedule.video.domain='hulu.com'
	add_list kids_schedule.video.domain='hulustream.com'
	add_list kids_schedule.video.domain='vimeo.com'
	add_list kids_schedule.video.domain='vimeocdn.com'
	add_list kids_schedule.video.domain='aiv-cdn.net'
	add_list kids_schedule.video.domain='primevideo.com'
	set kids_schedule.social=category
	set kids_schedule.social.label='Social media'
	set kids_schedule.social.min_bytes='20000'
	add_list kids_schedule.social.domain='instagram.com'
	add_list kids_schedule.social.domain='cdninstagram.com'
	add_list kids_schedule.social.domain='facebook.com'
	add_list kids_schedule.social.domain='fbcdn.net'
	add_list kids_schedule.social.domain='tiktok.com'
	add_list kids_schedule.social.domain='tiktokv.com'
	add_list kids_schedule.social.domain='tiktokcdn.com'
	add_list kids_schedule.social.domain='tiktokcdn-us.com'
	add_list kids_schedule.social.domain='byteoversea.com'
	add_list kids_schedule.social.domain='ibytedtos.com'
	add_list kids_schedule.social.domain='snapchat.com'
	add_list kids_schedule.social.domain='sc-cdn.net'
	add_list kids_schedule.social.domain='snapkit.com'
	add_list kids_schedule.social.domain='twitter.com'
	add_list kids_schedule.social.domain='x.com'
	add_list kids_schedule.social.domain='twimg.com'
	add_list kids_schedule.social.domain='reddit.com'
	add_list kids_schedule.social.domain='redd.it'
	add_list kids_schedule.social.domain='redditmedia.com'
	add_list kids_schedule.social.domain='redditstatic.com'
	add_list kids_schedule.social.domain='discord.com'
	add_list kids_schedule.social.domain='discordapp.com'
	add_list kids_schedule.social.domain='discordapp.net'
	add_list kids_schedule.social.domain='discord.gg'
	add_list kids_schedule.social.domain='pinterest.com'
	add_list kids_schedule.social.domain='pinimg.com'
	add_list kids_schedule.social.domain='threads.net'
	add_list kids_schedule.social.domain='bsky.app'
	set kids_schedule.games=category
	set kids_schedule.games.label='Games'
	set kids_schedule.games.min_bytes='20000'
	add_list kids_schedule.games.domain='roblox.com'
	add_list kids_schedule.games.domain='rbxcdn.com'
	add_list kids_schedule.games.domain='rbx.com'
	add_list kids_schedule.games.domain='epicgames.com'
	add_list kids_schedule.games.domain='epicgames.dev'
	add_list kids_schedule.games.domain='fortnite.com'
	add_list kids_schedule.games.domain='minecraft.net'
	add_list kids_schedule.games.domain='mojang.com'
	add_list kids_schedule.games.domain='minecraftservices.com'
	EOF
	uci commit kids_schedule
	config_load kids_schedule
}

# Sets CATS (category ids) from the loaded config.
load_categories() {
	CATS=""
	collect_cat() { CATS="$CATS $1"; }
	config_foreach collect_cat category
}

# Fingerprint of everything that shapes the limits table / dnsmasq hooks.
categories_sig() {
	uci -q show kids_schedule | grep -E "^kids_schedule\.[A-Za-z0-9_]+(=category|\.domain=)" | md5sum | cut -d' ' -f1
}

# (Re)create table inet kids_limits. Only runs when categories change or the
# table is missing: recreating it empties the dnsmasq-filled IP sets.
build_limits_table() {
	{
		echo "table inet $LTABLE"
		echo "delete table inet $LTABLE"
		echo "table inet $LTABLE {"
		for c in $CATS; do
			echo "	set cat_$c { type ipv4_addr; flags timeout; timeout 1d; }"
			echo "	set use_$c { type ether_addr; flags dynamic; counter; size 1024; }"
			echo "	set blk_$c { type ether_addr; }"
		done
		echo "	chain prerouting {"
		echo "		type filter hook prerouting priority -149; policy accept;"
		for c in $CATS; do
			echo "		iifname \"$IFACE\" ether saddr @blk_$c ip daddr @cat_$c counter drop comment \"kids-limits: $c over limit\""
			echo "		iifname \"$IFACE\" ip daddr @cat_$c update @use_$c { ether saddr } comment \"kids-limits: meter $c\""
		done
		echo "	}"
		echo "}"
	} | nft -f -
}

# Point dnsmasq at the category sets: one `config ipset 'kidslim_<cat>'` per
# category in /etc/config/dhcp (OpenWrt's dnsmasq init turns these into
# --nftset=/dom1/dom2/4#inet#kids_limits#cat_<cat>). Replaces our old sections.
sync_dnsmasq() {
	for s in $(uci -q show dhcp | sed -n 's/^dhcp\.\(kidslim_[A-Za-z0-9_]*\)=ipset$/\1/p'); do
		uci -q delete "dhcp.$s"
	done
	# Not `c`: config_list_foreach uses a local `c` as its loop counter, which
	# would shadow ours inside the callback.
	add_dom() { uci add_list "dhcp.kidslim_$cat_id.domain=$1"; }
	for cat_id in $CATS; do
		uci set "dhcp.kidslim_$cat_id=ipset"
		uci add_list "dhcp.kidslim_$cat_id.name=cat_$cat_id"
		uci set "dhcp.kidslim_$cat_id.table=$LTABLE"
		uci set "dhcp.kidslim_$cat_id.table_family=inet"
		uci set "dhcp.kidslim_$cat_id.family=4"
		config_list_foreach "$cat_id" domain add_dom
	done
	uci commit dhcp
	/etc/init.d/dnsmasq restart >/dev/null 2>&1
}

# "mac bytes" per line from the use_<cat> counters.
read_use() {
	nft list set inet $LTABLE "use_$1" 2>/dev/null | tr ',{}' '\n\n\n' |
		sed -n 's/^[[:space:]]*\([0-9a-f:]\{17\}\) .*bytes \([0-9]*\).*/\1 \2/p'
}

# Meter one minute, update today's usage and the over-limit sets.
# Sets LIMITS ("<kid> <cat> <used> <max> <0|1>" lines) for status.
apply_limits() {
	mkdir -p "$LDIR"
	load_categories
	sig=$(categories_sig)
	if ! nft list table inet $LTABLE >/dev/null 2>&1 || [ "$sig" != "$(cat "$LDIR/sig" 2>/dev/null)" ]; then
		build_limits_table || { logger -t kids-schedule "failed to build limits table"; return 1; }
		sync_dnsmasq
		echo "$sig" > "$LDIR/sig"
		rm -f "$LDIR"/last_*
	fi

	day=$(date +%Y%m%d)
	usage="$LDIR/usage-$day"
	touch "$usage"
	for f in "$LDIR"/usage-*; do
		[ "$f" = "$usage" ] || rm -f "$f"
	done

	# Per-MAC byte delta since last minute, per category (counter reset -> use cur).
	for c in $CATS; do
		touch "$LDIR/last_$c"
		read_use "$c" > "$LDIR/cur_$c"
		# FILENAME, not NR==FNR: the latter misfires when the last_ file is empty.
		awk 'FILENAME == ARGV[1] { last[$1] = $2; next } { d = $2 - last[$1]; if (d < 0) d = $2; print $1, d }' \
			"$LDIR/last_$c" "$LDIR/cur_$c" > "$LDIR/delta_$c"
		mv "$LDIR/cur_$c" "$LDIR/last_$c"
	done

	LIMITS=""
	nft_cmds=""
	for c in $CATS; do eval "blk_$c=''"; done
	each_limit() {
		lc=${1% *}; max=${1#* }
		case " $CATS " in *" $lc "*) ;; *) return ;; esac
		case "$max" in ''|*[!0-9]*) return ;; esac
		config_get min_bytes "$lc" min_bytes 20000
		used=$(awk -v k="$kid" -v c="$lc" '$1 == k && $2 == c { print $3 }' "$usage")
		used=${used:-0}
		# Count this minute if the kid's devices moved enough bytes in this category.
		bytes=0
		for m in $kmacs; do
			b=$(awk -v m="$m" '$1 == m { print $2 }' "$LDIR/delta_$lc")
			bytes=$(( bytes + ${b:-0} ))
		done
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
	load_categories
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
	# kid <id> blocked=<0|1> window=<HH:MM-HH:MM|-|disabled>
	# limit <id> <category> used=<min> max=<min> over=<0|1>
	# category <id> <label>
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
	nft list table inet $TABLE >/dev/null 2>&1 && echo "table=applied" || echo "table=absent"
	;;
flush)
	nft delete table inet $TABLE 2>/dev/null
	nft delete table inet $LTABLE 2>/dev/null
	CATS=""
	sync_dnsmasq
	rm -rf "$STATE" "$LDIR"
	;;
install)
	config_load kids_schedule
	seed_categories
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
