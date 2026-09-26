#!/bin/sh
# Downloads a WordPress release, verifies it, and unpacks it for the site
# images. Used by the build stage of both site-php and site-nginx so the two
# images always carry byte-identical core files.
#
# Usage: fetch-wordpress.sh <version> <expected-sha1|""> <output-dir>
#
# Verification is two-step:
#   1. The release tarball's SHA-1 must match. The expected value is the
#      WP_SHA1 build arg when given (pinned in images/versions.env); otherwise
#      it is fetched from wordpress.org over HTTPS.
#   2. Every extracted file must match the per-file MD5 published by the
#      WordPress checksums API, and no file may be missing.
#
# Output layout:
#   <output-dir>/wordpress         core, with an empty wp-content mount point
#   <output-dir>/wp-content-skel   the release's bundled wp-content (themes etc.)
set -eu

version="$1"
expected_sha1="$2"
out="$3"

if [ -z "$version" ]; then
	echo "fetch-wordpress: WP_VERSION is required" >&2
	exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

base="https://wordpress.org/wordpress-${version}.tar.gz"
curl -fsSL --retry 3 -o "$tmp/wordpress.tar.gz" "$base"

if [ -z "$expected_sha1" ]; then
	expected_sha1="$(curl -fsSL --retry 3 "${base}.sha1" | tr -d '[:space:]')"
fi
echo "${expected_sha1}  $tmp/wordpress.tar.gz" | sha1sum -c -

mkdir -p "$out"
tar -xzf "$tmp/wordpress.tar.gz" -C "$out"

curl -fsSL --retry 3 -o "$tmp/checksums.json" \
	"https://api.wordpress.org/core/checksums/1.0/?version=${version}&locale=en_US"
jq -er '.checksums | if type == "object" and length > 0 then . else error("no checksums published") end
	| to_entries[] | "\(.value)  \(.key)"' "$tmp/checksums.json" >"$tmp/md5sums"
(cd "$out/wordpress" && md5sum --quiet --strict -c "$tmp/md5sums")
echo "fetch-wordpress: verified $(wc -l <"$tmp/md5sums") files for WordPress ${version}"

mv "$out/wordpress/wp-content" "$out/wp-content-skel"
mkdir "$out/wordpress/wp-content"
