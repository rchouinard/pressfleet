#!/usr/bin/env bash
# List post permalinks for the traffic script (#98): writes
# $STATE_DIR/urls.tsv with one "<slug>\t<path>" line per sampled post. The
# paths are what a visitor requests, so the page cache serves them after the
# first hit. Run as root on the host under test, after seed.sh.
#
# Environment:
#   STATE_DIR   where provision.sh left sites.tsv (default /root/capacity)
#   PER_SITE    posts to sample per site (default 20)
set -euo pipefail

STATE_DIR="${STATE_DIR:-/root/capacity}"
PER_SITE="${PER_SITE:-20}"
out="$STATE_DIR/urls.tsv"
: >"$out"
while IFS=$'\t' read -r id slug _; do
	docker exec -e HOME=/tmp "pf-$id-php-1" wp post list --post_type=post --post_status=publish \
		--posts_per_page="$PER_SITE" --field=url |
		sed -E 's#^https?://[^/]+##' | awk -v s="$slug" 'NF { printf "%s\t%s\n", s, $0 }' >>"$out"
done <"$STATE_DIR/sites.tsv"
echo "$(wc -l <"$out") URLs in $out"
