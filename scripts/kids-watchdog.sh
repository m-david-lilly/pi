#!/bin/sh
# Watchdog: if the kids SSID is enabled but kidproxy is not running, restart it
# and email a warning if it stays down. Also restores a time-boxed pause.
set -u

MAIL_STAMP=/tmp/kids-watchdog.mailed

[ "$(uci -q get wireless.kids.disabled)" = "0" ] || exit 0

if ! pidof kidproxy >/dev/null 2>&1; then
	logger -t kids-watchdog "kidproxy down; restarting"
	/etc/init.d/kidproxy restart
	sleep 5
	if ! pidof kidproxy >/dev/null 2>&1; then
		logger -t kids-watchdog "kidproxy still down"
		# notify-test skips the proxy's cooldown and max_per_hour, so limit it
		# here: one email per hour while the proxy stays down, not one per run.
		now=$(date +%s)
		last=$(cat "$MAIL_STAMP" 2>/dev/null)
		case "$last" in ''|*[!0-9]*) last=0 ;; esac
		if [ $((now - last)) -ge 3600 ] && [ -x /usr/bin/kidproxy ] &&
			/usr/bin/kidproxy notify-test --kind down \
				--detail "kidproxy is not running; the kids network has no web access" 2>/dev/null; then
			echo "$now" > "$MAIL_STAMP"
		fi
	fi
fi

/usr/bin/kids-toggle.sh resume-if-due
