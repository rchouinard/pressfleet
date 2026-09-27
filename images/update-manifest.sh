#!/usr/bin/env bash
# Refreshes images/manifest.json from upstream (issue #20):
#
#   - PHP branches and support dates from php.net. Every branch php.net lists
#     as supported is added; dates of branches already in the manifest are
#     updated. Branches that reached end of life stay (the control plane
#     reports them as EOL; images/matrix.sh stops building them) until they
#     are removed by hand.
#   - The latest WordPress release and its tarball SHA-1 from wordpress.org.
#     The previous release is not kept automatically; add it to
#     wordpress.pinned by hand if its images should keep being rebuilt.
#   - The default plugins the site-php image bundles (shared/plugins.json):
#     each moves to its latest WordPress.org release, pinned by the SHA-256
#     of the downloaded zip.
#
# Writes the manifest and plugins.json in place and a Markdown summary of the
# changes to stdout (nothing when up to date). The scheduled Image updates
# workflow turns the change into a pull request.
#
# Environment:
#   MANIFEST        manifest path (default: manifest.json next to this script)
#   PLUGINS         plugin list path (default: shared/plugins.json next to this script)
#   MATRIX_DATE     "today" as YYYY-MM-DD, UTC (default: the current date)
#   PHP_BRANCHES_URL, WP_VERSION_CHECK_URL, WP_PLUGIN_INFO_URL,
#   WP_PLUGIN_DOWNLOAD_URL   upstream endpoints (for testing)
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
manifest="${MANIFEST:-$here/manifest.json}"
plugins="${PLUGINS:-$here/shared/plugins.json}"
today="${MATRIX_DATE:-$(date -u +%F)}"
php_url="${PHP_BRANCHES_URL:-https://www.php.net/releases/branches.php}"
wp_url="${WP_VERSION_CHECK_URL:-https://api.wordpress.org/core/version-check/1.7/}"
plugin_url="${WP_PLUGIN_INFO_URL:-https://api.wordpress.org/plugins/info/1.2/}"
plugin_download_url="${WP_PLUGIN_DOWNLOAD_URL:-https://downloads.wordpress.org/plugin}"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fetch() {
	curl -fsSL --retry 3 --max-time 60 "$1"
}

# --- PHP ---------------------------------------------------------------------
fetch "$php_url" >"$tmp/branches.json"
jq -e 'type == "array" and length > 0' "$tmp/branches.json" >/dev/null || {
	echo "update-manifest: unexpected response from $php_url" >&2
	exit 1
}

# --- WordPress ---------------------------------------------------------------
fetch "$wp_url" >"$tmp/wp.json"
current_wp="$(jq -er '.wordpress.latest.version' "$manifest")"
latest_wp="$(jq -er '.offers[0].version | select(test("^[0-9]+\\.[0-9]+(\\.[0-9]+)?$"))' "$tmp/wp.json")" || {
	echo "update-manifest: no stable WordPress version in $wp_url" >&2
	exit 1
}
wp_sha1="$(jq -er '.wordpress.latest.sha1' "$manifest")"
if [ "$latest_wp" != "$current_wp" ]; then
	# Never move backwards (e.g. a stale API cache node).
	newest="$(printf '%s\n%s\n' "$current_wp" "$latest_wp" | sort -V | tail -n 1)"
	if [ "$newest" = "$latest_wp" ]; then
		wp_sha1="$(fetch "https://wordpress.org/wordpress-${latest_wp}.tar.gz.sha1" | tr -d '[:space:]')"
		[[ "$wp_sha1" =~ ^[0-9a-f]{40}$ ]] || {
			echo "update-manifest: bad SHA-1 for WordPress $latest_wp: $wp_sha1" >&2
			exit 1
		}
	else
		latest_wp="$current_wp"
	fi
fi

# --- Rewrite -----------------------------------------------------------------
jq --slurpfile branches "$tmp/branches.json" --arg today "$today" \
	--arg wp "$latest_wp" --arg sha1 "$wp_sha1" '
	def date: .[0:10];
	def upstream: [$branches[0][]
		| select(.branch | test("^[0-9]+\\.[0-9]+$"))
		| {version: .branch, activeSupportEnds: (.active_support_end | date), securitySupportEnds: (.security_support_end | date), state}];
	def supported: [upstream[] | select((.state == "stable" or .state == "security") and .securitySupportEnds >= $today)];
	(upstream | map({key: .version, value: .}) | from_entries) as $up
	| .php = ([.php[] | if $up[.version] then $up[.version] | del(.state) else . end]
		+ [supported[] | del(.state)]
		| unique_by(.version)
		| sort_by(.version | split(".") | map(tonumber)) | reverse)
	| .wordpress.latest = {version: $wp, sha1: $sha1}
	| .wordpress.pinned |= map(select(.version != $wp))
	' "$manifest" >"$tmp/manifest.json"

# --- Summary -----------------------------------------------------------------
jq -r --slurpfile new "$tmp/manifest.json" --arg today "$today" '
	def byver: map({key: .version, value: .}) | from_entries;
	(.php | byver) as $old | ($new[0].php | byver) as $cur
	| ($cur | keys_unsorted[] | select($old[.] == null)
		| "- Add PHP \(.) (security support until \($cur[.].securitySupportEnds))"),
	  ($cur | keys_unsorted[] | select($old[.] != null and $old[.] != $cur[.])
		| "- Update PHP \(.) support dates: active until \($cur[.].activeSupportEnds), security until \($cur[.].securitySupportEnds)"),
	  ($cur | keys_unsorted[] | select($cur[.].securitySupportEnds < $today)
		| "- Note: PHP \(.) is past end of life (\($cur[.].securitySupportEnds)); it is no longer built. Remove it from the manifest once no site uses it."),
	  (if .wordpress.latest.version != $new[0].wordpress.latest.version
		then "- Update WordPress \(.wordpress.latest.version) -> \($new[0].wordpress.latest.version). Images for \(.wordpress.latest.version) stay published but are no longer rebuilt unless it is added to `wordpress.pinned`."
		else empty end)
	' "$manifest" >"$tmp/summary.md"

if ! cmp -s "$manifest" "$tmp/manifest.json"; then
	cp "$tmp/manifest.json" "$manifest"
	cat "$tmp/summary.md"
fi

# --- Default plugins ---------------------------------------------------------
# Each plugin moves to its latest WordPress.org release (never backwards),
# pinned by the SHA-256 of the zip downloaded here. The site-php build
# verifies the zip against it.
cp "$plugins" "$tmp/plugins.json"
: >"$tmp/plugins-summary.md"
for slug in $(jq -r '.plugins[].slug' "$plugins"); do
	[[ "$slug" =~ ^[a-z0-9-]+$ ]] || {
		echo "update-manifest: bad plugin slug in $plugins: $slug" >&2
		exit 1
	}
	current="$(jq -er --arg s "$slug" '.plugins[] | select(.slug == $s) | .version' "$plugins")"
	fetch "${plugin_url}?action=plugin_information&request%5Bslug%5D=${slug}&request%5Bfields%5D%5Bsections%5D=0&request%5Bfields%5D%5Bversions%5D=0" >"$tmp/plugin.json"
	latest="$(jq -er '.version | select(test("^[0-9]+(\\.[0-9]+)*$"))' "$tmp/plugin.json")" || {
		echo "update-manifest: no stable version of plugin $slug in $plugin_url" >&2
		exit 1
	}
	newest="$(printf '%s\n%s\n' "$current" "$latest" | sort -V | tail -n 1)"
	if [ "$latest" = "$current" ] || [ "$newest" != "$latest" ]; then
		continue
	fi
	fetch "${plugin_download_url}/${slug}.${latest}.zip" >"$tmp/plugin.zip"
	sha256="$(sha256sum "$tmp/plugin.zip" | cut -d ' ' -f 1)"
	jq --arg s "$slug" --arg v "$latest" --arg h "$sha256" \
		'(.plugins[] | select(.slug == $s)) |= {slug: $s, version: $v, sha256: $h}' \
		"$tmp/plugins.json" >"$tmp/plugins.next"
	mv "$tmp/plugins.next" "$tmp/plugins.json"
	echo "- Update plugin $slug $current -> $latest (sites that already have it update it through WordPress; new sites get this version)" >>"$tmp/plugins-summary.md"
done
if ! cmp -s "$plugins" "$tmp/plugins.json"; then
	cp "$tmp/plugins.json" "$plugins"
	cat "$tmp/plugins-summary.md"
fi
