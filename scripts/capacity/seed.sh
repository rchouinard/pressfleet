#!/usr/bin/env bash
# Give each capacity test site realistic content (#98): a few hundred posts,
# some media, a common plugin, and a user for wp-admin logins. Backups and the
# page cache need something to work on. Run as root on the host under test,
# after provision.sh:
#
#   scripts/capacity/seed.sh
#
# WP-CLI runs inside each site's php container (docker exec), a few sites at
# a time. Sites already seeded (option pf98_seeded) are skipped.
#
# Environment:
#   STATE_DIR   where provision.sh left sites.tsv (default /root/capacity)
#   POSTS       posts per site (default 300)
#   IMAGES      media library images per site (default 12)
#   PLUGINS     plugins to install and activate (default "classic-editor")
#   PARALLEL    sites to seed at once (default 4)
#   LOGIN_USER  user to create for wp-admin logins (default loadtest)
#   LOGIN_PASS_FILE  file the shared login password is written to
#                    (default $STATE_DIR/login-password)
set -euo pipefail

STATE_DIR="${STATE_DIR:-/root/capacity}"
export POSTS="${POSTS:-300}" IMAGES="${IMAGES:-12}" PLUGINS="${PLUGINS:-classic-editor}"
export LOGIN_USER="${LOGIN_USER:-loadtest}"
LOGIN_PASS_FILE="${LOGIN_PASS_FILE:-$STATE_DIR/login-password}"
PARALLEL="${PARALLEL:-4}"

if [[ ! -s "$LOGIN_PASS_FILE" ]]; then
	(umask 077; openssl rand -base64 18 | tr -d '/+=' >"$LOGIN_PASS_FILE")
fi
LOGIN_PASS="$(<"$LOGIN_PASS_FILE")"
export LOGIN_PASS

seed_site() {
	# bash -c doesn't inherit set -e from this script.
	set -euo pipefail
	local id="$1" slug="$2" c="pf-$1-php-1"
	wp() { docker exec -e HOME=/tmp "$c" wp "$@"; }
	if [[ "$(wp option get pf98_seeded 2>/dev/null || true)" == 1 ]]; then
		echo "$slug: already seeded"
		return 0
	fi
	wp post generate --count="$POSTS" --post_content --quiet
	# Images made in the container, so it needs no network for them. GD
	# noise doesn't compress, which makes the uploads (and backups) realistic.
	docker exec -e IMAGES "$c" php -r '
		for ($i = 0; $i < (int) getenv("IMAGES"); $i++) {
			$im = imagecreatetruecolor(1600, 1000);
			for ($y = 0; $y < 1000; $y += 4) for ($x = 0; $x < 1600; $x += 4)
				imagefilledrectangle($im, $x, $y, $x + 3, $y + 3, imagecolorallocate($im, mt_rand(0, 255), mt_rand(0, 255), mt_rand(0, 255)));
			imagejpeg($im, "/tmp/seed-$i.jpg", 85);
		}'
	for i in $(seq 0 $((IMAGES - 1))); do
		wp media import "/tmp/seed-$i.jpg" --quiet
	done
	docker exec "$c" sh -c 'rm -f /tmp/seed-*.jpg'
	for p in $PLUGINS; do
		wp plugin install "$p" --activate --quiet
	done
	wp user create "$LOGIN_USER" "$LOGIN_USER@example.test" --role=editor --user_pass="$LOGIN_PASS" --quiet 2>/dev/null ||
		wp user update "$LOGIN_USER" --user_pass="$LOGIN_PASS" --quiet
	wp option update pf98_seeded 1 --quiet
	echo "$slug: seeded ($(wp post list --post_type=post --format=count) posts, $(wp post list --post_type=attachment --format=count) media)"
}
export -f seed_site

cut -f1,2 "$STATE_DIR/sites.tsv" | xargs -P "$PARALLEL" -L1 bash -c 'seed_site "$0" "$1"'
