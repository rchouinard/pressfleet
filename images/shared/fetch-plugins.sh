#!/bin/sh
# Downloads the platform's default WordPress plugins (design doc §5.1 step 7)
# for the site-php image, so provisioning installs them from the image rather
# than from the network.
#
# Usage: fetch-plugins.sh <plugins.json> <output-dir>
#
# plugins.json pins each plugin's WordPress.org slug, version, and the
# SHA-256 of its release zip (downloads.wordpress.org/plugin/<slug>.<version>.zip).
# A download that doesn't match its checksum fails the build. The zips are
# written unchanged to <output-dir>/<slug>.zip, next to a copy of plugins.json
# that records what the image carries.
set -eu

lock="$1"
out="$2"
mkdir -p "$out"

count="$(jq -er '.plugins | length' "$lock")"
i=0
while [ "$i" -lt "$count" ]; do
	slug="$(jq -er --argjson i "$i" '.plugins[$i].slug | select(test("^[a-z0-9-]+$"))' "$lock")"
	version="$(jq -er --argjson i "$i" '.plugins[$i].version | select(test("^[0-9A-Za-z.-]+$"))' "$lock")"
	sha256="$(jq -er --argjson i "$i" '.plugins[$i].sha256 | select(test("^[0-9a-f]{64}$"))' "$lock")"
	echo "fetch-plugins: $slug $version" >&2
	curl -fsSL --retry 3 -o "$out/$slug.zip" "https://downloads.wordpress.org/plugin/$slug.$version.zip"
	echo "$sha256  $out/$slug.zip" | sha256sum -c - >&2
	i=$((i + 1))
done
cp "$lock" "$out/plugins.json"
