#!/bin/sh
# Watchdog: if the kids SSID is enabled but kidproxy is not running, restart it
# and email a warning if it stays down. Also restores a time-boxed pause.
set -u

[ "$(uci -q get wireless.kids.disabled)" = "0" ] || exit 0

if ! pidof kidproxy >/dev/null 2>&1; then
	logger -t kids-watchdog "kidproxy down; restarting"
	/etc/init.d/kidproxy restart
	sleep 5
	if ! pidof kidproxy >/dev/null 2>&1; then
		logger -t kids-watchdog "kidproxy still down"
		[ -x /usr/bin/kidproxy ] && /usr/bin/kidproxy notify-test --kind down \
			--detail "kidproxy is not running; the kids network has no web access" 2>/dev/null
	fi
fi

/usr/bin/kids-toggle.sh resume-if-due
