#!/usr/bin/env bash
# Pins a PECL extension the site-php image builds to a version, with the
# SHA-256 of its release tarball, in images/shared/pecl.json:
#
#   images/update-pecl.sh <name> <version>     e.g. images/update-pecl.sh redis 6.3.1
#
# The extension must already be in pecl.json; a new one also needs its
# `pecl install` and `docker-php-ext-enable` lines in site-php/Dockerfile.
# Then list the new version in site-php/README.md.
#
# Environment:
#   PECL         pin file path (default: shared/pecl.json next to this script)
#   PECL_GET_URL download base URL (default: https://pecl.php.net/get, for testing)
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
pecl="${PECL:-$here/shared/pecl.json}"
get_url="${PECL_GET_URL:-https://pecl.php.net/get}"

[ $# -eq 2 ] || {
	sed -n '2,/^set -euo/{/^#/s/^# \{0,1\}//p}' "$0" >&2
	exit 2
}
name="$1"
version="$2"
[[ "$name" =~ ^[a-z0-9_]+$ && "$version" =~ ^[0-9A-Za-z.]+$ ]] || {
	echo "update-pecl: bad name or version: $name $version" >&2
	exit 2
}
jq -e --arg n "$name" 'any(.extensions[]; .name == $n)' "$pecl" >/dev/null || {
	echo "update-pecl: $name is not in $pecl" >&2
	exit 1
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -fsSL --retry 3 --max-time 120 -o "$tmp/ext.tgz" "$get_url/$name-$version.tgz"
gzip -t "$tmp/ext.tgz"
sha256="$(sha256sum "$tmp/ext.tgz" | cut -d ' ' -f 1)"
jq --arg n "$name" --arg v "$version" --arg h "$sha256" \
	'(.extensions[] | select(.name == $n)) |= {name: $n, version: $v, sha256: $h}' \
	"$pecl" >"$tmp/pecl.json"
cp "$tmp/pecl.json" "$pecl"
echo "update-pecl: $name $version sha256:$sha256"
