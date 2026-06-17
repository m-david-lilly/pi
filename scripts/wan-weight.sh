#!/bin/sh
# wan-weight.sh — per-WAN capacity probe -> mwan3 member weight.
#
# Liveness/failover is owned by mwan3track (Phase 4 of docs/runbooks/setup.md);
# this script ONLY measures per-WAN capacity and rewrites the member `weight`.
# It MUST NEVER mark an interface up/down — that is mwan3track's job, and two
# writers of link state fight each other (see docs/reference/load-balancing.md §6).
#
# Binding the probe to the correct WAN is the whole game: with mwan3 active there
# is a load-balanced default route, so an UNBOUND probe measures "whichever WAN
# got picked" and corrupts both weights. We bind via `mwan3 use <iface>`, which
# runs the probe inside that WAN's routing table — a real egress guarantee. A
# bare `--source <ip>` only sets the socket source address and can still egress
# the wrong WAN (docs/reference/load-balancing.md §5.1).
#
# Install: /usr/bin/wan-weight.sh   Cron: */30 * * * * /usr/bin/wan-weight.sh
#
# NOTE on `set -e`: the org Bash standard discourages `set -e`. We keep `set -eu`
# here deliberately, paired with an explicit `|| true` on every probe/parse step,
# so an unset-variable bug aborts loudly while a single WAN's probe failure does
# NOT abort the loop and leave the other WAN unweighted. Do not remove the guards.
set -eu

readonly LOG_TAG="wan-weight"
# Persist EWMA state under /etc (survives reboot), NOT /tmp (tmpfs, wiped on
# reboot). A wiped state file is non-fatal: the first run reseeds from the raw
# sample. /etc lives on the overlay; 48 small writes/day is negligible wear.
readonly STATE_DIR="/etc/wan-weight"
readonly LOCK_FILE="/var/lock/wan-weight.lock"

# logical mwan3 interface : member section name.
# Edit if you renamed members in setup.md Phase 4.
readonly WAN_MEMBERS="wan:wan_m1_w1 wanb:wanb_m1_w1"

readonly EWMA_ALPHA_NEW=40      # integer percent weight of the new sample (0.4)
readonly EWMA_ALPHA_OLD=60      # integer percent weight of the prior value (0.6)
readonly REWEIGHT_THRESHOLD=15  # percent change required before commit + reload
readonly WEIGHT_MAX=1000        # self-imposed clamp; mwan3 documents no hard max
readonly PROBE_DURATION=8       # seconds of download per probe
readonly PROBE_CONCURRENT=2     # parallel streams within a single probe
readonly PROBE_TIMEOUT=30       # hard kill for a hung librespeed run (seconds)

# Serialize: a hung probe + the */30 cron must not stack two writers racing on
# `uci`. Non-blocking lock — if a prior run is still going, skip this tick.
exec 9>"${LOCK_FILE}" || exit 1
if ! flock -n 9; then
    logger -t "${LOG_TAG}" "another run holds the lock; skipping this tick"
    exit 0
fi

mkdir -p "${STATE_DIR}"

# wan_is_online <iface> — read mwan3's authoritative state; never set it.
# mwan3 prints status on ONE line ("interface wan is online ..."); match it on a
# single line. An -A1 multi-line match grabs the next line and falsely skips.
wan_is_online() {
    mwan3 interfaces 2>/dev/null | grep -qiE "interface ${1} is online"
}

# wan_src_ip <iface> — the WAN's own IPv4 address (netifd-assigned).
# Used for logging and the egress cross-check; NOT needed to bind the probe when
# using `mwan3 use`.
wan_src_ip() {
    ubus call "network.interface.${1}" status 2>/dev/null \
        | jsonfilter -e '@["ipv4-address"][0].address' || true
}

# probe_mbps <iface> — bound capacity probe, download-only, JSON. Egress is
# pinned by `mwan3 use` (real guarantee); `timeout` kills a hung run so the lock
# is released and the cron does not stack.
probe_mbps() {
    timeout "${PROBE_TIMEOUT}" mwan3 use "${1}" \
        librespeed-cli --no-upload \
            --duration "${PROBE_DURATION}" \
            --concurrent "${PROBE_CONCURRENT}" \
            --json 2>/dev/null \
        | jsonfilter -e '@[0].download' 2>/dev/null \
        || true
}

changed=0

for pair in ${WAN_MEMBERS}; do
    wan_if="${pair%%:*}"
    member="${pair##*:}"

    if ! wan_is_online "${wan_if}"; then
        logger -t "${LOG_TAG}" "skip ${wan_if}: not online per mwan3"
        continue
    fi

    src_ip="$(wan_src_ip "${wan_if}")"
    if [ -z "${src_ip}" ]; then
        logger -t "${LOG_TAG}" "skip ${wan_if}: no ipv4 source address"
        continue
    fi

    mbps="$(probe_mbps "${wan_if}")"
    # Some librespeed-cli builds emit a bare object instead of an array; fall
    # back to the non-indexed key. Keep this expression identical to the runbook.
    if [ -z "${mbps}" ]; then
        mbps="$(probe_mbps "${wan_if}" | jsonfilter -e '@.download' 2>/dev/null || true)"
    fi
    if [ -z "${mbps}" ]; then
        logger -t "${LOG_TAG}" "probe failed/unparseable on ${wan_if} (${src_ip}); keeping weight"
        continue
    fi

    # Round measured Mbps to an integer floor-1 sample.
    meas="$(awk -v m="${mbps}" 'BEGIN { v = int(m + 0.5); if (v < 1) v = 1; print v }')"

    # EWMA against the last persisted value so one bad sample does not slam the
    # routing tables. First run (no state) seeds with the raw sample.
    state_file="${STATE_DIR}/${member}.ewma"
    old_ewma="$(cat "${state_file}" 2>/dev/null || echo "${meas}")"
    new_ewma="$(awk -v o="${old_ewma}" -v n="${meas}" \
                    -v ao="${EWMA_ALPHA_OLD}" -v an="${EWMA_ALPHA_NEW}" \
                    'BEGIN { printf "%d", (o * ao + n * an) / 100 + 0.5 }')"
    [ "${new_ewma}" -lt 1 ] && new_ewma=1
    [ "${new_ewma}" -gt "${WEIGHT_MAX}" ] && new_ewma="${WEIGHT_MAX}"

    cur_weight="$(uci -q get "mwan3.${member}.weight" || echo 1)"

    # Only reweight when the change clears the threshold (avoid churning routes).
    delta_pct="$(awk -v a="${cur_weight}" -v b="${new_ewma}" \
                     'BEGIN { d = (a > b ? a - b : b - a); base = (a > 0 ? a : 1); \
                              printf "%d", (d * 100) / base }')"
    if [ "${delta_pct}" -ge "${REWEIGHT_THRESHOLD}" ]; then
        uci set "mwan3.${member}.weight=${new_ewma}"
        changed=1
        logger -t "${LOG_TAG}" \
            "${wan_if}/${member}: ${mbps}Mbps meas=${meas} ewma=${new_ewma} (was ${cur_weight}) APPLIED"
    else
        logger -t "${LOG_TAG}" \
            "${wan_if}/${member}: ${mbps}Mbps meas=${meas} ewma=${new_ewma} (was ${cur_weight}) below threshold, holding"
    fi
    echo "${new_ewma}" > "${state_file}"

    sleep 5   # serialize: never probe both WANs at once (RP1 uplink + softirq contention)
done

if [ "${changed}" = 1 ]; then
    uci commit mwan3
    mwan3 restart
    logger -t "${LOG_TAG}" "weights committed; mwan3 restarted"
fi
