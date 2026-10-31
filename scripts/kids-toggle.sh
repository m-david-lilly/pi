#!/bin/sh
# Turn the kids network on/off, or temporarily pause filtering.
#
#   kids-toggle.sh on            SSID up, proxy running, interception applied
#   kids-toggle.sh off           SSID down, proxy stopped, interception flushed
#   kids-toggle.sh pause [mins]  keep the SSID up but stop filtering (default 30)
#   kids-toggle.sh resume         end a pause early
#   kids-toggle.sh status        key=value output for the dashboard
#   kids-toggle.sh resume-if-due cron helper; restores filtering when pause ends
set -u

PAUSE_FILE=/etc/kidproxy/pause_until

wifi_iface_state() {
	[ "$(uci -q get wireless.kids.disabled)" = "0" ] && echo 1 || echo 0
}

case "${1:-status}" in
on)
	uci set wireless.kids.disabled='0'
	uci commit wireless
	wifi reload
	/etc/init.d/kidproxy enable
	/etc/init.d/kidproxy restart
	/etc/init.d/kidproxy-nft enable
	/etc/init.d/kidproxy-nft start
	rm -f "$PAUSE_FILE"
	echo "kids network ON"
	;;
off)
	uci set wireless.kids.disabled='1'
	uci commit wireless
	wifi reload
	/usr/bin/kids-nft.sh flush
	/etc/init.d/kidproxy stop
	rm -f "$PAUSE_FILE"
	echo "kids network OFF"
	;;
pause)
	mins="${2:-30}"
	case "$mins" in ''|*[!0-9]*) echo "minutes must be a number" >&2; exit 2;; esac
	echo $(( $(date +%s) + mins * 60 )) > "$PAUSE_FILE"
	/usr/bin/kids-nft.sh flush
	logger -t kids "filtering paused for ${mins}m"
	echo "filtering paused for ${mins} min"
	;;
resume)
	rm -f "$PAUSE_FILE"
	/usr/bin/kids-nft.sh apply
	logger -t kids "filtering resumed"
	echo "filtering resumed"
	;;
resume-if-due)
	[ -f "$PAUSE_FILE" ] || exit 0
	now=$(date +%s)
	until_ts=$(cat "$PAUSE_FILE" 2>/dev/null || echo 0)
	[ "$until_ts" -gt "$now" ] 2>/dev/null && exit 0
	rm -f "$PAUSE_FILE"
	/usr/bin/kids-nft.sh apply
	logger -t kids "filtering resumed after pause"
	;;
status)
	echo "enabled=$(wifi_iface_state)"
	if pidof kidproxy >/dev/null 2>&1; then
		echo "proxy=running"
	else
		echo "proxy=stopped"
	fi
	echo "nft=$(/usr/bin/kids-nft.sh status)"
	pause_until=$(cat "$PAUSE_FILE" 2>/dev/null || echo 0)
	echo "pause_until=$pause_until"
	;;
*)
	echo "usage: $0 on|off|pause [mins]|resume|status|resume-if-due" >&2
	exit 2
	;;
esac
