#!/bin/sh
# wan-weight.sh — per-WAN capacity probe -> mwan3 member weight.
#
# Liveness/failover is owned by mwan3track (Phase 4 of docs/runbooks/setup.md);
# this script ONLY measures per-WAN capacity and rewrites the member `weight`.
# It MUST NEVER mark an interface up/down — that is mwan3track's job, and two
# writers of link state fight each other (see docs/reference/load-balancing.md §6).
#
# Algorithm (docs/reference/load-balancing.md §5.4, requirements FR-H8):
#   1. Probe each live WAN, bound to its egress via `mwan3 use` (real guarantee;
#      a bare `--source` only sets the socket source addr and can egress the
#      wrong WAN — §5.1).
#   2. Smooth each measurement into an EWMA of Mbps (persisted per member).
#   3. weight = clamp(round(WEIGHT_MAX * ewma / peak), 1, WEIGHT_MAX), where
#      `peak` is the fastest live WAN's EWMA. The fastest link gets WEIGHT_MAX;
#      slower links get their proportional capacity share, floored at 1. This is
#      the spec's proportional map, NOT raw clamped Mbps (which skews the ratio
#      once a link exceeds WEIGHT_MAX).
#   4. Commit + reapply only when a member's weight moves beyond the threshold.
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
readonly WAN_MEMBERS="wan1:wan1_m1_w3 wan2:wan2_m1_w3"

readonly EWMA_ALPHA_NEW=40      # integer percent weight of the new sample (0.4)
readonly EWMA_ALPHA_OLD=60      # integer percent weight of the prior value (0.6)
readonly REWEIGHT_THRESHOLD=15  # percent change required before commit + reload
readonly WEIGHT_MAX=1000        # self-imposed clamp; mwan3 documents no hard max
readonly PROBE_DURATION=8       # seconds of download per probe
readonly PROBE_CONCURRENT=2     # parallel streams within a single probe
readonly PROBE_TIMEOUT=30       # hard kill for a hung librespeed run (seconds)
readonly PROBE_SERIALIZE_SLEEP=5  # gap between probes (RP1 uplink + softirq contention)

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

# probe_json <iface> — bound capacity probe, download-only; prints raw JSON.
# Egress is pinned by `mwan3 use` (real guarantee); `timeout` kills a hung run so
# the lock is released and the cron does not stack. Returns the raw document so
# the caller can try both the array and bare-object download keys WITHOUT
# re-running the (8-second, data-heavy) probe.
probe_json() {
    timeout "${PROBE_TIMEOUT}" mwan3 use "${1}" \
        librespeed-cli --no-upload \
            --duration "${PROBE_DURATION}" \
            --concurrent "${PROBE_CONCURRENT}" \
            --json 2>/dev/null \
        || true
}

# --- Pass 1: probe each live WAN, smooth to an EWMA of Mbps, track the peak. ---
# We persist smoothed *Mbps* (not the final weight) so pass 2's proportional map
# always divides by a comparable peak. `live` accumulates "iface:member:ewma"
# tokens for WANs that probed OK (no spaces inside a token, so `for` splits it).
live=""
peak=0

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

    # Probe ONCE, capture the raw JSON, then try both download keys against it.
    # Some librespeed-cli builds emit a bare object (`@.download`), others an
    # array (`@[0].download`) — both parse the same captured document, so a
    # parse miss never triggers a second 8-second probe.
    probe_out="$(probe_json "${wan_if}")"
    mbps="$(printf '%s' "${probe_out}" | jsonfilter -e '@[0].download' 2>/dev/null || true)"
    if [ -z "${mbps}" ]; then
        mbps="$(printf '%s' "${probe_out}" | jsonfilter -e '@.download' 2>/dev/null || true)"
    fi
    if [ -z "${mbps}" ]; then
        logger -t "${LOG_TAG}" "probe failed/unparseable on ${wan_if} (${src_ip}); keeping weight"
        continue
    fi

    # EWMA-smooth the Mbps against the persisted value (one bad sample must not
    # slam the weights). First run (no state) seeds with the raw sample.
    state_file="${STATE_DIR}/${member}.mbps"
    old_ewma="${mbps}"
    [ -f "${state_file}" ] && read -r old_ewma < "${state_file}"
    ewma="$(awk -v m="${mbps}" -v o="${old_ewma}" \
                -v ao="${EWMA_ALPHA_OLD}" -v an="${EWMA_ALPHA_NEW}" \
        'BEGIN {
             v = int(m + 0.5); if (v < 1) v = 1
             e = int((o * ao + v * an) / 100 + 0.5); if (e < 1) e = 1
             print e
         }')"
    echo "${ewma}" > "${state_file}"

    live="${live} ${wan_if}:${member}:${ewma}"
    [ "${ewma}" -gt "${peak}" ] && peak="${ewma}"
    logger -t "${LOG_TAG}" "${wan_if}/${member}: ${mbps}Mbps ewma=${ewma}Mbps"

    sleep "${PROBE_SERIALIZE_SLEEP}"
done

# Degenerate guards (§5.4): nothing probed this run, or every live link measured
# 0 (peak <= 0 would divide-by-zero the proportional map). Leave weights as-is.
if [ -z "${live}" ] || [ "${peak}" -le 0 ]; then
    logger -t "${LOG_TAG}" "no usable probe this run (peak=${peak}Mbps); weights unchanged"
    exit 0
fi

# --- Pass 2: map each live WAN's EWMA to a weight proportional to the peak. ---
changed=0
for entry in ${live}; do
    wan_if="${entry%%:*}"
    rest="${entry#*:}"
    member="${rest%%:*}"
    ewma="${rest##*:}"

    new_w="$(awk -v e="${ewma}" -v p="${peak}" -v wmax="${WEIGHT_MAX}" \
        'BEGIN {
             w = int(wmax * e / p + 0.5); if (w < 1) w = 1; if (w > wmax) w = wmax
             print w
         }')"
    cur_w="$(uci -q get "mwan3.${member}.weight" || echo 1)"
    delta_pct="$(awk -v a="${cur_w}" -v b="${new_w}" \
        'BEGIN { d = (a > b ? a - b : b - a); print int((d * 100) / (a > 0 ? a : 1)) }')"

    if [ "${delta_pct}" -ge "${REWEIGHT_THRESHOLD}" ]; then
        uci set "mwan3.${member}.weight=${new_w}"
        changed=1
        logger -t "${LOG_TAG}" \
            "${wan_if}/${member}: weight ${cur_w} -> ${new_w} (ewma=${ewma}Mbps peak=${peak}Mbps) APPLIED"
    else
        logger -t "${LOG_TAG}" \
            "${wan_if}/${member}: weight ${cur_w} held (new=${new_w}, ${delta_pct}% < ${REWEIGHT_THRESHOLD}%)"
    fi
done

if [ "${changed}" = 1 ]; then
    uci commit mwan3
    # Reapply WITHOUT `mwan3 restart`: FR-H13 forbids a full restart for a weight
    # change because it tears down every ip rule including the tunnel's WAN pin,
    # blipping the VPN. `mwan3 ifdown/ifup` (FR-H9's literal suggestion) is also
    # unsuitable here — our mwan3 config sets `flush_conntrack` on ifdown, so
    # cycling an interface for a mere weight change would strand that WAN's
    # in-flight flows. `mwan3 reload` re-reads config and re-applies rules with
    # neither a full teardown nor a tracking transition (no conntrack flush),
    # satisfying FR-H13's MUST-NOT. Verify on the installed build that `reload`
    # re-evaluates member weights (`mwan3 reload; mwan3 policies`); if it does
    # not, fall back to per-changed-interface ifdown/ifup.
    mwan3 reload
    logger -t "${LOG_TAG}" "weights committed; mwan3 reloaded"
fi
