#!/bin/sh
# Downloads the PECL extensions the site-php image builds, so `pecl install`
# runs from local tarballs and needs no network access (and no CA for
# pecl.php.net) in the PHP stage.
#
# Usage: fetch-pecl.sh <pecl.json> <output-dir>
#
# pecl.json pins each extension's name, version, and the SHA-256 of its
# release tarball (pecl.php.net/get/<name>-<version>.tgz); update a pin with
# images/update-pecl.sh, which rewrites both. A download that doesn't match
# its checksum fails the build. The tarballs are written unchanged to
# <output-dir>/<name>.tgz.
set -eu

lock="$1"
out="$2"
mkdir -p "$out"

count="$(jq -er '.extensions | length' "$lock")"
i=0
while [ "$i" -lt "$count" ]; do
	name="$(jq -er --argjson i "$i" '.extensions[$i].name | select(test("^[a-z0-9_]+$"))' "$lock")"
	version="$(jq -er --argjson i "$i" '.extensions[$i].version | select(test("^[0-9A-Za-z.]+$"))' "$lock")"
	sha256="$(jq -er --argjson i "$i" '.extensions[$i].sha256 | select(test("^[0-9a-f]{64}$"))' "$lock")"
	echo "fetch-pecl: $name $version" >&2
	curl -fsSL --retry 3 --retry-all-errors -o "$out/$name.tgz" "https://pecl.php.net/get/$name-$version.tgz"
	echo "$sha256  $out/$name.tgz" | sha256sum -c - >&2
	i=$((i + 1))
done
