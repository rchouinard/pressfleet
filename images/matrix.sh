#!/usr/bin/env bash
# shellcheck disable=SC2016 # jq programs are single-quoted on purpose
# Reads the site image build matrix from images/manifest.json (issue #20).
# The Makefile, test.sh, and the GitHub workflows all go through this script so
# there is one definition of "what gets built".
#
#   images/matrix.sh php          PHP branches to build (not past end of life), newest first
#   images/matrix.sh wp           WordPress versions to build: latest, then pinned
#   images/matrix.sh latest-wp    the latest WordPress version
#   images/matrix.sh sha1 <wp>    SHA-1 of wordpress-<wp>.tar.gz
#   images/matrix.sh registry     image repository prefix (ghcr.io/rchouinard/pressfleet)
#   images/matrix.sh github       JSON for GitHub Actions matrices (see below)
#
# A PHP branch is built through the last day of security support. Branches
# past it stay in the manifest (so the control plane can still report them as
# end of life) but are no longer built.
#
# Environment:
#   MANIFEST     manifest path (default: manifest.json next to this script)
#   MATRIX_DATE  "today" as YYYY-MM-DD, UTC (default: the current date)
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
manifest="${MANIFEST:-$here/manifest.json}"
today="${MATRIX_DATE:-$(date -u +%F)}"

q() {
	jq -er --arg today "$today" "$@" "$manifest"
}

# jq definitions shared by the queries below.
defs='
def php_versions: [.php[] | select(.securitySupportEnds >= $today)]
	| sort_by(.version | split(".") | map(tonumber)) | reverse | map(.version)
	| if length == 0 then error("no PHP branch in the manifest is supported on \($today)") else . end;
def wp_versions: [.wordpress.latest] + .wordpress.pinned;
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
registry)
	q '.registry'
	;;
github)
	# {"nginx": [{"wp", "sha1"}...],
	#  "php":   [{"php", "wp", "sha1", "smoke"}...]}
	# smoke is true for the latest WordPress on the newest and oldest PHP
	# branch: the combinations the pull request smoke test covers.
	q -c "$defs"'
		php_versions as $php
		| .wordpress.latest.version as $latest
		| {
			nginx: [wp_versions[] | {wp: .version, sha1}],
			php: [wp_versions[] as $w | $php[] | {
				php: .,
				wp: $w.version,
				sha1: $w.sha1,
				smoke: ($w.version == $latest and (. == $php[0] or . == $php[-1]))
			}]
		}'
	;;
*)
	sed -n '3,/^set -euo/{/^#/s/^# \{0,1\}//p}' "$0" >&2
	exit 2
	;;
esac
