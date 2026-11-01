#!/usr/bin/env bash
# shellcheck disable=SC2016 # jq programs are single-quoted on purpose
# Reads the site image build matrix from images/manifest.json (issue #20).
# The Makefile, test.sh, and the GitHub workflows all go through this script so
# there is one definition of "what gets built".
#
#   images/matrix.sh php          PHP branches to build (not past end of life), newest first
#   images/matrix.sh wp           WordPress versions to build: latest, pinned, then extra
#   images/matrix.sh latest-wp    the latest WordPress version
#   images/matrix.sh sha1 <wp>    SHA-1 of wordpress-<wp>.tar.gz
#   images/matrix.sh dbversion <wp>  database schema version ($wp_db_version) of <wp>
#   images/matrix.sh registry     image repository prefix (ghcr.io/rchouinard/pressfleet)
#   images/matrix.sh github       JSON for GitHub Actions matrices (see below)
#
# Extra WordPress versions (a one-time rebuild of superseded tags, e.g. to give
# them a newly added mu-plugin): set EXTRA_WORDPRESS to versions that are in
# extra-wordpress.json, and they are built in addition to the manifest's, with
# "extra": true in the github output. Nothing in the manifest changes, so the
# control plane's catalog and update-manifest.sh never see them.
#
# A PHP branch is built through the last day of security support. Branches
# past it stay in the manifest (so the control plane can still report them as
# end of life) but are no longer built.
#
# Environment:
#   MANIFEST     manifest path (default: manifest.json next to this script)
#   MATRIX_DATE  "today" as YYYY-MM-DD, UTC (default: the current date)
#   EXTRA_WORDPRESS       extra WordPress versions to build, space or comma
#                         separated (default: none)
#   EXTRA_WORDPRESS_FILE  their SHA-1s and dbVersions (default:
#                         extra-wordpress.json next to this script)
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
manifest="${MANIFEST:-$here/manifest.json}"
today="${MATRIX_DATE:-$(date -u +%F)}"

# Entries for the extra WordPress versions, from extra_file. A version already
# in the manifest is left out (it is built anyway); one with no recorded
# checksum is an error, so nothing is ever built unverified.
extra_file="${EXTRA_WORDPRESS_FILE:-$here/extra-wordpress.json}"
extra='[]'
if [ -n "${EXTRA_WORDPRESS:-}" ]; then
	in_manifest="$(jq -c '[.wordpress.latest.version] + [.wordpress.pinned[].version]' "$manifest")"
	for v in ${EXTRA_WORDPRESS//,/ }; do
		[[ "$v" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || {
			echo "matrix.sh: invalid WordPress version '$v' in EXTRA_WORDPRESS (want e.g. 7.1.2)" >&2
			exit 1
		}
		extra="$(jq -ce --arg v "$v" --argjson have "$in_manifest" --slurpfile known "$extra_file" '
			if ($have | index($v)) or (map(.version) | index($v)) then .
			elif ($known[0][$v].sha1 // "") == "" or ($known[0][$v].dbVersion // "") == "" then
				error("no SHA-1 and dbVersion")
			else . + [{version: $v, sha1: $known[0][$v].sha1, dbVersion: $known[0][$v].dbVersion, extra: true}] end' \
			<<<"$extra" 2>/dev/null)" || {
			echo "matrix.sh: WordPress $v has no SHA-1 and dbVersion in $extra_file; add them there first" >&2
			exit 1
		}
	done
fi

q() {
	jq -er --arg today "$today" --argjson extra "$extra" "$@" "$manifest"
}

# jq definitions shared by the queries below.
defs='
def php_versions: [.php[] | select(.securitySupportEnds >= $today)]
	| sort_by(.version | split(".") | map(tonumber)) | reverse | map(.version)
	| if length == 0 then error("no PHP branch in the manifest is supported on \($today)") else . end;
def wp_versions: [.wordpress.latest] + .wordpress.pinned + $extra;
'

case "${1:-}" in
php)
	q "$defs"' php_versions[]'
	;;
wp)
	q "$defs"' wp_versions[] | .version'
	;;
latest-wp)
	q '.wordpress.latest.version'
	;;
sha1)
	[ -n "${2:-}" ] || {
		echo "usage: $0 sha1 <wordpress-version>" >&2
		exit 2
	}
	q --arg wp "$2" "$defs"' first(wp_versions[] | select(.version == $wp) | .sha1)
		// error("WordPress \($wp) is not in the manifest")'
	;;
dbversion)
	[ -n "${2:-}" ] || {
		echo "usage: $0 dbversion <wordpress-version>" >&2
		exit 2
	}
	q --arg wp "$2" "$defs"' first(wp_versions[] | select(.version == $wp) | .dbVersion)
		// error("WordPress \($wp) has no dbVersion in the manifest")'
	;;
registry)
	q '.registry'
	;;
github)
	# {"nginx": [{"wp", "sha1", "extra"}...],
	#  "php":   [{"php", "wp", "sha1", "smoke", "extra"}...]}
	# extra marks the EXTRA_WORDPRESS versions. smoke is true for the latest WordPress on the newest and oldest PHP
	# branch: the combinations the pull request smoke test covers.
	q -c "$defs"'
		php_versions as $php
		| .wordpress.latest.version as $latest
		| {
			nginx: [wp_versions[] | {wp: .version, sha1, extra: (.extra // false)}],
			php: [wp_versions[] as $w | $php[] | {
				php: .,
				wp: $w.version,
				sha1: $w.sha1,
					extra: ($w.extra // false),
				smoke: ($w.version == $latest and (. == $php[0] or . == $php[-1]))
			}]
		}'
	;;
*)
	sed -n '3,/^set -euo/{/^#/s/^# \{0,1\}//p}' "$0" >&2
	exit 2
	;;
esac
