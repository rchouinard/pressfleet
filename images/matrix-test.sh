#!/usr/bin/env bash
# Tests for matrix.sh's EXTRA_WORDPRESS overlay. Needs only jq.
#
#   images/matrix-test.sh
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
matrix="$here/matrix.sh"
failed=0

check() { # name expected actual
	if [ "$2" = "$3" ]; then
		echo "ok   $1"
	else
		echo "FAIL $1: want '$2', got '$3'" >&2
		failed=1
	fi
}

latest="$("$matrix" latest-wp)"
base_wp="$("$matrix" wp | xargs)"
base_json="$("$matrix" github | jq -c .)"
base_php="$("$matrix" php | wc -l | tr -d ' ')"

# Empty: identical to no overlay.
check "empty overlay changes nothing" "$base_json" "$(EXTRA_WORDPRESS='' "$matrix" github | jq -c .)"
check "no extra flag set without the overlay" "0" "$("$matrix" github | jq '[.nginx[], .php[] | select(.extra)] | length')"

# Two extras: one nginx and one PHP image per branch each.
export EXTRA_WORDPRESS="7.1.2 7.1.3"
out="$("$matrix" github)"
check "wp lists the manifest's versions, then the extras" "$base_wp 7.1.2 7.1.3" "$("$matrix" wp | xargs)"
check "latest-wp is unchanged" "$latest" "$("$matrix" latest-wp)"
check "nginx images" "3" "$(jq '.nginx | length' <<<"$out")"
check "php images" "$((base_php * 3))" "$(jq '.php | length' <<<"$out")"
check "extra images are flagged" "$((base_php * 2 + 2))" "$(jq '[.nginx[], .php[] | select(.extra)] | length' <<<"$out")"
check "only the latest WordPress is smoke tested" "$latest" "$(jq -r '[.php[] | select(.smoke) | .wp] | unique | join(",")' <<<"$out")"
check "sha1 of 7.1.2" "761b8101538f0631a0bfc4fba7bc4abeea92f81c" "$("$matrix" sha1 7.1.2)"
check "dbversion of 7.1.3" "61833" "$("$matrix" dbversion 7.1.3)"

# Commas, repeats, and versions the manifest already builds.
check "commas, repeats, and manifest versions are folded" "$base_wp 7.1.2" \
	"$(EXTRA_WORDPRESS="7.1.2,7.1.2,$latest" "$matrix" wp | xargs)"

# Failures.
for bad in "7.1.2;rm" "7.1" "latest" "7.0.99"; do
	if EXTRA_WORDPRESS="$bad" "$matrix" wp >/dev/null 2>&1; then
		echo "FAIL '$bad' was accepted" >&2
		failed=1
	else
		echo "ok   '$bad' is rejected"
	fi
done
unset EXTRA_WORDPRESS
if "$matrix" sha1 7.1.2 >/dev/null 2>&1; then
	echo "FAIL 7.1.2 is in the manifest without the overlay" >&2
	failed=1
else
	echo "ok   extras are unknown without the overlay"
fi

exit "$failed"
