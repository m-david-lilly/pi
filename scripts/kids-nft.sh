#!/bin/sh
# Apply/flush the kids interception table. The table is standalone (not fw4) so
# pausing is atomic and an fw4 reload cannot drop the redirects.
set -u

NFT_FILE=/etc/kidproxy/kids.nft

case "${1:-status}" in
apply)
	[ -f "$NFT_FILE" ] || { echo "missing $NFT_FILE" >&2; exit 1; }
	# One transaction: declare (no-op if present), delete, re-create. nft
	# applies a file atomically, so a load error leaves the previous table in
	# place instead of a window -- or a lasting gap -- with no interception.
	tmp=$(mktemp /tmp/kids-nft.XXXXXX) || exit 1
	{
		echo 'table inet kidproxy'
		echo 'delete table inet kidproxy'
		cat "$NFT_FILE"
	} > "$tmp"
	nft -f "$tmp"
	rc=$?
	rm -f "$tmp"
	[ "$rc" -eq 0 ] || { echo "nft load failed" >&2; exit 1; }
	logger -t kids-nft "interception applied"
	;;
flush)
	nft delete table inet kidproxy 2>/dev/null
	logger -t kids-nft "interception flushed"
	;;
status)
	if nft list table inet kidproxy >/dev/null 2>&1; then
		echo applied
	else
		echo absent
	fi
	;;
*)
	echo "usage: $0 apply|flush|status" >&2
	exit 2
	;;
esac
