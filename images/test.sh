#!/usr/bin/env bash
# Smoke test for the site images (issues #17, #18, #40).
#
#   images/test.sh [php-version ...]     default: every PHP version images/matrix.sh builds
#
# Builds site-nginx and site-php, then for each PHP version starts MariaDB and
# both site containers on a throwaway network (read-only root filesystems, no
# capabilities, non-root site UID, named wp-content and cache volumes, a minimal read-only
# wp-config.php), installs WordPress with WP-CLI, and checks the runtime
# contract from images/README.md. Everything it creates is removed on exit;
# the built images are kept.
#
# Environment:
#   SKIP_BUILD=1     use existing images instead of building
#   IMAGE_REPO       image repository (default: "registry" from manifest.json)
#   WP_VERSION       WordPress version to test (default: the manifest's latest;
#                    must be in the manifest, e.g. a pinned version)
#   SITE_UID=10001   UID:GID the site containers run as
#   MARIADB_IMAGE    database image (default mariadb:11.8)
#   VERBOSE=1        print the tail of the site container logs
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
matrix="$here/matrix.sh"
WP_VERSION="${WP_VERSION:-$("$matrix" latest-wp)}"
WP_SHA1="$("$matrix" sha1 "$WP_VERSION")"

registry="${IMAGE_REPO:-$("$matrix" registry)}"
site_uid="${SITE_UID:-10001}"
mariadb_image="${MARIADB_IMAGE:-mariadb:11.8}"
run_id="pf-imgtest-$$"
label="io.pressfleet.imgtest=$run_id"
nginx_image="$registry/site-nginx:wp$WP_VERSION"

if [ $# -gt 0 ]; then
	versions=("$@")
else
	mapfile -t versions < <("$matrix" php)
	[ "${#versions[@]}" -gt 0 ] || exit 1
fi

tmp="$(mktemp -d)"
cleanup() {
	local ids
	ids="$(docker ps -aq --filter "label=$label")"
	[ -z "$ids" ] || docker rm -f $ids >/dev/null
	ids="$(docker network ls -q --filter "label=$label")"
	[ -z "$ids" ] || docker network rm $ids >/dev/null
	ids="$(docker volume ls -q --filter "label=$label")"
	[ -z "$ids" ] || docker volume rm $ids >/dev/null
	rm -rf "$tmp"
}
trap cleanup EXIT

failures=0
pass() { printf '  ok    %s\n' "$1"; }
fail() {
	printf '  FAIL  %s\n' "$1"
	[ -z "${2:-}" ] || printf '        %s\n' "$2"
	failures=$((failures + 1))
}
# check DESCRIPTION COMMAND...: passes when the command succeeds.
check() {
	local desc="$1" out
	shift
	if out="$("$@" 2>&1)"; then pass "$desc"; else fail "$desc" "$(tail -n 3 <<<"$out")"; fi
}
# check_not DESCRIPTION COMMAND...: passes when the command fails.
check_not() {
	local desc="$1"
	shift
	if "$@" >/dev/null 2>&1; then fail "$desc" "command unexpectedly succeeded: $*"; else pass "$desc"; fi
}
# check_eq DESCRIPTION EXPECTED ACTUAL
check_eq() {
	if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "want '$2', got '$3'"; fi
}
# check_contains DESCRIPTION NEEDLE HAYSTACK
check_contains() {
	if grep -qF -- "$2" <<<"$3"; then pass "$1"; else fail "$1" "missing '$2' in: $(head -c 300 <<<"$3")"; fi
}

# x_cache CURL-ARGS...: the response's X-Cache header, or nothing.
x_cache() {
	curl -s -o /dev/null -D - "$@" | tr -d '\r' | awk -F': ' 'tolower($1) == "x-cache" { print $2 }'
}

wait_healthy() {
	local name="$1" status
	for _ in $(seq 90); do
		status="$(docker inspect -f '{{.State.Health.Status}}' "$name")"
		[ "$status" = healthy ] && return 0
		[ "$(docker inspect -f '{{.State.Running}}' "$name")" = true ] || break
		sleep 1
	done
	docker logs --tail 30 "$name" >&2 || true
	return 1
}

build() {
	local dockerfile="$1" tag="$2"
	shift 2
	echo "==> building $tag"
	docker build -q -f "$here/$dockerfile" -t "$tag" \
		--build-arg "WP_VERSION=$WP_VERSION" --build-arg "WP_SHA1=$WP_SHA1" "$@" "$here" >/dev/null
}

if [ "${SKIP_BUILD:-0}" != 1 ]; then
	build site-nginx/Dockerfile "$nginx_image"
	for v in "${versions[@]}"; do
		build site-php/Dockerfile "$registry/site-php:php$v-wp$WP_VERSION" --build-arg "PHP_VERSION=$v"
	done
fi

db_password="$(head -c 18 /dev/urandom | base64 | tr -d '/+=')"
admin_password="$(head -c 18 /dev/urandom | base64 | tr -d '/+=')"

# Minimal stand-in for the control plane's generated wp-config.php (#21),
# including the constants the image contract requires.
cat >"$tmp/wp-config.php" <<EOF
<?php
define( 'DB_NAME', 'wordpress' );
define( 'DB_USER', 'wordpress' );
define( 'DB_PASSWORD', '$db_password' );
define( 'DB_HOST', 'db' );
define( 'DB_CHARSET', 'utf8mb4' );
define( 'DB_COLLATE', '' );
\$table_prefix = 'wp_';
foreach ( array( 'AUTH_KEY', 'SECURE_AUTH_KEY', 'LOGGED_IN_KEY', 'NONCE_KEY', 'AUTH_SALT', 'SECURE_AUTH_SALT', 'LOGGED_IN_SALT', 'NONCE_SALT' ) as \$k ) {
	define( \$k, 'smoke-test-' . \$k );
}
define( 'WPMU_PLUGIN_DIR', '/opt/pressfleet/mu-plugins' );
define( 'WPMU_PLUGIN_URL', '/_pressfleet/mu-plugins' );
define( 'FS_METHOD', 'direct' );
define( 'WP_AUTO_UPDATE_CORE', false );
define( 'DISABLE_WP_CRON', true );
if ( ! defined( 'ABSPATH' ) ) {
	define( 'ABSPATH', __DIR__ . '/' );
}
require_once ABSPATH . 'wp-settings.php';
EOF
chmod 0644 "$tmp/wp-config.php"

cat >"$tmp/probe.php" <<'EOF'
<?php
if ( isset( $_GET['sleep'] ) ) {
	sleep( (int) $_GET['sleep'] );
}
echo 'open_basedir=', ini_get( 'open_basedir' ), "\n";
echo 'passwd=', @file_get_contents( '/etc/passwd' ) === false ? 'blocked' : 'readable', "\n";
echo 'https=', $_SERVER['HTTPS'] ?? '', "\n";
echo 'remote_addr=', $_SERVER['REMOTE_ADDR'] ?? '', "\n";
EOF

test_stack() {
	local php_version="$1"
	local php_image="$registry/site-php:php$php_version-wp$WP_VERSION"
	local p="$run_id-php${php_version//./}"
	local net="$p" php="$p-php" nginx="$p-nginx" db="$p-db"
	local wp_content="$p-wp-content" cache="$p-cache"
	local user="$site_uid:$site_uid"
	# The hardened settings the Compose templates are expected to use.
	local hardening=(--read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges:true)

	echo "==> PHP $php_version / WordPress $WP_VERSION ($php_image)"

	docker network create --label "$label" "$net" >/dev/null
	docker volume create --label "$label" "$wp_content" >/dev/null
	docker volume create --label "$label" "$cache" >/dev/null

	docker run -d --label "$label" --name "$db" --network "$net" --network-alias db \
		-e MARIADB_RANDOM_ROOT_PASSWORD=1 -e MARIADB_DATABASE=wordpress \
		-e MARIADB_USER=wordpress -e "MARIADB_PASSWORD=$db_password" \
		--health-cmd 'healthcheck.sh --connect --innodb_initialized' --health-interval 2s \
		"$mariadb_image" >/dev/null

	docker run -d --label "$label" --name "$php" --network "$net" --network-alias php \
		--user "$user" "${hardening[@]}" \
		-v "$wp_content:/var/www/wordpress/wp-content" \
		-v "$cache:/var/cache/pressfleet/fastcgi" \
		-v "$tmp/wp-config.php:/var/www/wordpress/wp-config.php:ro" \
		-e PHP_FPM_SLOWLOG_TIMEOUT=1s --health-interval 2s "$php_image" >/dev/null

	docker run -d --label "$label" --name "$nginx" --network "$net" \
		--user "$user" "${hardening[@]}" \
		-v "$wp_content:/var/www/wordpress/wp-content:ro" \
		-v "$cache:/var/cache/pressfleet/fastcgi" \
		-p 127.0.0.1::8080 --health-interval 2s "$nginx_image" >/dev/null

	check "mariadb healthy" wait_healthy "$db"
	check "php healthcheck passes" wait_healthy "$php"
	check "nginx healthcheck passes" wait_healthy "$nginx"

	local port base
	port="$(docker port "$nginx" 8080/tcp | head -n 1 | sed 's/.*://')"
	base="http://127.0.0.1:$port"
	local cexec=(docker exec "$php")

	# --- Runtime identity and read-only core ---------------------------------
	check_eq "php runs as site UID" "$site_uid" "$("${cexec[@]}" id -u)"
	check_eq "php-fpm master runs as site UID" "$site_uid" \
		"$("${cexec[@]}" awk '/^Uid:/ { print $2 }' /proc/1/status)"
	check_eq "nginx master runs as site UID" "$site_uid" \
		"$(docker exec "$nginx" awk '/^Uid:/ { print $2 }' /proc/1/status)"
	check_eq "core is root-owned" "root" "$("${cexec[@]}" stat -c %U /var/www/wordpress/wp-login.php)"
	check_not "core file is not writable" "${cexec[@]}" sh -c 'echo x >> /var/www/wordpress/wp-login.php'
	check_not "core directory is not writable" "${cexec[@]}" touch /var/www/wordpress/new.php
	check_not "mu-plugins are not writable" "${cexec[@]}" touch /opt/pressfleet/mu-plugins/new.php
	check "wp-content is writable" "${cexec[@]}" sh -c 'touch wp-content/.w && rm wp-content/.w'
	check "wp-content was seeded" "${cexec[@]}" test -f wp-content/themes/index.php
	check "cache volume is writable by php" "${cexec[@]}" sh -c 'touch /var/cache/pressfleet/fastcgi/.w && rm /var/cache/pressfleet/fastcgi/.w'
	check_not "wp-content is read-only in nginx" docker exec "$nginx" touch /var/www/wordpress/wp-content/new

	# --- PHP and WP-CLI -------------------------------------------------------
	check "wp --info" "${cexec[@]}" wp --info
	local modules
	modules="$("${cexec[@]}" php -m)"
	for ext in bcmath curl exif gd igbinary imagick intl mbstring mysqli redis sodium zip "Zend OPcache"; do
		check_contains "extension $ext" "$ext" "$modules"
	done
	check "gd supports WebP and AVIF" "${cexec[@]}" php -r \
		'$i = gd_info(); exit(($i["WebP Support"] && $i["AVIF Support"]) ? 0 : 1);'
	check "phpredis built with igbinary" "${cexec[@]}" php -r 'exit(defined("Redis::SERIALIZER_IGBINARY") ? 0 : 1);'
	check "wp core install" "${cexec[@]}" wp core install --url="$base" --title="PressFleet smoke test" \
		--admin_user=admin --admin_password="$admin_password" --admin_email=admin@example.com --skip-email

	# --- Platform mu-plugin ---------------------------------------------------
	check_contains "mu-plugin listed as must-use" "pressfleet" \
		"$("${cexec[@]}" wp plugin list --status=must-use --field=name)"
	check_eq "mu-plugin loaded" "yes" \
		"$("${cexec[@]}" wp eval 'echo defined("PRESSFLEET_PLATFORM") ? "yes" : "no";')"
	check_eq "core updates disabled for admins" "no" \
		"$("${cexec[@]}" wp eval --user=admin 'echo current_user_can("update_core") ? "yes" : "no";')"
	check_eq "filesystem method is direct" "direct" \
		"$("${cexec[@]}" wp eval 'require_once ABSPATH . "wp-admin/includes/file.php"; echo get_filesystem_method();')"

	# --- FastCGI page cache (#40) --------------------------------------------
	# The first request for a page misses and stores it; bypass rules keep
	# per-user and dynamic requests away from the cache.
	check_eq "anonymous GET misses the cache first" "MISS" "$(x_cache "$base/")"
	check_eq "anonymous GET then hits the cache" "HIT" "$(x_cache "$base/")"
	check_eq "HEAD shares the GET cache entry" "HIT" "$(x_cache -I "$base/")"
	check_eq "wordpress_test_cookie alone still hits" "HIT" \
		"$(x_cache -H 'Cookie: wordpress_test_cookie=WP%20Cookie%20check' "$base/")"
	check_eq "logged-in cookie bypasses the cache" "BYPASS" \
		"$(x_cache -H 'Cookie: wordpress_test_cookie=x; wordpress_logged_in_0123456789abcdef0123456789abcdef=admin' "$base/")"
	check_eq "comment author cookie bypasses the cache" "BYPASS" \
		"$(x_cache -H 'Cookie: comment_author_0123456789abcdef0123456789abcdef=someone' "$base/")"
	check_eq "WooCommerce cart cookie bypasses the cache" "BYPASS" \
		"$(x_cache -H 'Cookie: woocommerce_items_in_cart=1' "$base/")"
	# nginx doesn't look up methods outside fastcgi_cache_methods at all.
	check_eq "POST is never served from the cache" "" "$(x_cache -X POST -d x=1 "$base/")"
	check_eq "query string bypasses the cache" "BYPASS" "$(x_cache "$base/?s=hello")"
	check_eq "Authorization header bypasses the cache" "BYPASS" "$(x_cache -u admin:secret "$base/")"
	check_eq "wp-login.php bypasses the cache" "BYPASS" "$(x_cache "$base/wp-login.php")"
	check_eq "wp-admin bypasses the cache" "BYPASS" "$(x_cache "$base/wp-admin/")"
	check_eq "REST API bypasses the cache" "BYPASS" "$(x_cache "$base/wp-json/")"
	check_eq "feed bypasses the cache" "BYPASS" "$(x_cache "$base/feed/")"
	check_eq "WooCommerce cart page bypasses the cache" "BYPASS" "$(x_cache "$base/cart/")"
	check_eq "static files have no X-Cache" "" "$(x_cache "$base/wp-includes/css/dashicons.min.css")"
	check_contains "cache keeps X-Content-Type-Options" "x-content-type-options: nosniff" \
		"$(curl -s -o /dev/null -D - "$base/" | tr -d '\r' | tr '[:upper:]' '[:lower:]')"
	# Nginx Helper's file purge (#41) deletes md5(scheme . "GET" . host . path)
	# under levels=1:2, with the visitor's scheme (https from a trusted proxy).
	check_eq "proxied HTTPS request has its own cache entry" "MISS" \
		"$(x_cache -H 'X-Forwarded-Proto: https' "$base/")"
	local key_hash cache_file
	key_hash="$(printf '%s' "httpsGET127.0.0.1/" | md5sum | cut -c1-32)"
	cache_file="/var/cache/pressfleet/fastcgi/${key_hash:31:1}/${key_hash:29:2}/$key_hash"
	check "cache file at the Nginx Helper path" "${cexec[@]}" test -f "$cache_file"
	check_eq "cache file owned by the site UID" "$site_uid" "$("${cexec[@]}" stat -c %u "$cache_file" 2>&1)"
	check "php can purge a cache file" "${cexec[@]}" rm "$cache_file"
	check_eq "a purged page misses again" "MISS" "$(x_cache -H 'X-Forwarded-Proto: https' "$base/")"
	check "nginx -t with the cache off" docker run --rm --label "$label" --user "$user" "${hardening[@]}" \
		-e NGINX_FASTCGI_CACHE=off "$nginx_image" nginx -e stderr -c /tmp/nginx/nginx.conf -t
	check_not "invalid NGINX_FASTCGI_CACHE is rejected" docker run --rm --label "$label" --user "$user" \
		"${hardening[@]}" -e NGINX_FASTCGI_CACHE=maybe "$nginx_image" true

	# --- HTTP through nginx ---------------------------------------------------
	local body code
	body="$(curl -fsS "$base/")" && pass "home page returns 200" || fail "home page returns 200"
	check_contains "home page is WordPress" "PressFleet smoke test" "$body"
	code="$(curl -s -o /dev/null -w '%{http_code} %{content_type}' "$base/wp-includes/css/dashicons.min.css")"
	check_eq "core static asset served" "200 text/css" "$code"
	local theme
	theme="$("${cexec[@]}" wp theme list --status=active --field=name)"
	check_eq "wp-content static asset served" "200" \
		"$(curl -s -o /dev/null -w '%{http_code}' "$base/wp-content/themes/$theme/style.css")"
	check_eq "nginx redirects are relative" "location: /wp-admin/" \
		"$(curl -sI "$base/wp-admin" | tr -d '\r' | grep -i '^location:' | tr '[:upper:]' '[:lower:]')"

	"${cexec[@]}" sh -c 'mkdir -p wp-content/uploads/2026 && printf "<?php echo \"pwned\";" > wp-content/uploads/2026/evil.php && echo secret > wp-content/.env'
	code="$(curl -s -o "$tmp/out" -w '%{http_code}' "$base/wp-content/uploads/2026/evil.php")"
	check_eq "PHP in uploads is denied" "403" "$code"
	check_not "PHP in uploads is not executed" grep -q pwned "$tmp/out"
	check_eq "hidden files are denied" "403" "$(curl -s -o /dev/null -w '%{http_code}' "$base/wp-content/.env")"
	check_eq "xmlrpc.php denied by default" "403" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$base/xmlrpc.php")"
	check_eq "wp-config.php not served" "404" "$(curl -s -o /dev/null -w '%{http_code}' "$base/wp-config.php")"
	check_eq "readme.html not served" "404" "$(curl -s -o /dev/null -w '%{http_code}' "$base/readme.html")"
	check_eq "wp-includes PHP denied" "403" "$(curl -s -o /dev/null -w '%{http_code}' "$base/wp-includes/version.php")"
	check_eq "mu-plugin PHP not served" "403" \
		"$(curl -s -o /dev/null -w '%{http_code}' "$base/_pressfleet/mu-plugins/pressfleet.php")"

	docker exec -i "$php" sh -c 'cat > wp-content/pf-probe.php' <"$tmp/probe.php"
	body="$(curl -fsS -H 'X-Forwarded-For: 203.0.113.7' -H 'X-Forwarded-Proto: https' "$base/wp-content/pf-probe.php")" || true
	check_contains "open_basedir restricts PHP" \
		"open_basedir=/var/www/wordpress:/opt/pressfleet/mu-plugins:/var/cache/pressfleet/fastcgi:/tmp" "$body"
	check_contains "files outside open_basedir unreadable" "passwd=blocked" "$body"
	check_contains "HTTPS detected from trusted proxy" "https=on" "$body"
	check_contains "real client IP from trusted proxy" "remote_addr=203.0.113.7" "$body"
	curl -s -o /dev/null "$base/wp-content/pf-probe.php?sleep=2"
	check_contains "FPM slow log written to stderr" "pf-probe.php" \
		"$(docker logs "$php" 2>&1 | grep -A 3 'executing too slow' || true)"
	"${cexec[@]}" rm -f wp-content/pf-probe.php

	local logs
	logs="$(docker logs "$nginx" 2>/dev/null)"
	check_contains "access log has client IP" '"remote_addr":"203.0.113.7"' "$logs"
	check_contains "access log has cache status" '"upstream_cache_status":' "$logs"
	check_contains "access log has request time" '"request_time":' "$logs"

	# --- Internal status port -------------------------------------------------
	check_contains "nginx stub_status" "Active connections" \
		"$(docker exec "$nginx" wget -qO- http://127.0.0.1:8081/nginx-status)"
	check_contains "FPM status via nginx" "process manager:      ondemand" \
		"$(docker exec "$nginx" wget -qO- http://127.0.0.1:8081/fpm-status)"
	check_eq "FPM ping via nginx" "pong" "$(docker exec "$nginx" wget -qO- http://127.0.0.1:8081/fpm-ping)"
	local nginx_ip
	nginx_ip="$(docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$nginx")"
	check_not "status port denies non-allowed clients" docker exec "$nginx" wget -qO- "http://$nginx_ip:8081/nginx-status"
	check "nginx -t" docker exec "$nginx" nginx -e stderr -c /tmp/nginx/nginx.conf -t

	# --- Static assets without PHP -------------------------------------------
	docker stop "$php" >/dev/null
	check_eq "core static asset served with php stopped" "200" \
		"$(curl -s -o /dev/null -w '%{http_code}' "$base/wp-includes/js/jquery/jquery.min.js")"
	code="$(curl -s -o /dev/null -w '%{http_code}' "$base/?uncached=1")"
	case "$code" in 502 | 504) pass "PHP request fails with php stopped" ;; *) fail "PHP request fails with php stopped" "got $code" ;; esac
	check_eq "cached page still served with php stopped" "HIT" "$(x_cache "$base/")"

	# A recreated php container (new IP, as on a PHP version change) is picked
	# up by nginx without a restart.
	docker rm -f "$php" >/dev/null
	docker run -d --label "$label" --name "$php" --network "$net" --network-alias php \
		--user "$user" "${hardening[@]}" \
		-v "$wp_content:/var/www/wordpress/wp-content" \
		-v "$cache:/var/cache/pressfleet/fastcgi" \
		-v "$tmp/wp-config.php:/var/www/wordpress/wp-config.php:ro" \
		--health-interval 2s "$php_image" >/dev/null
	wait_healthy "$php" || true
	code=""
	for _ in $(seq 20); do
		code="$(curl -s -o /dev/null -w '%{http_code}' "$base/")"
		[ "$code" = 200 ] && break
		sleep 1
	done
	check_eq "nginx follows a recreated php container" "200" "$code"

	if [ "${VERBOSE:-0}" = 1 ]; then
		echo "--- php logs"
		docker logs "$php" 2>&1 | tail -n 20
		echo "--- nginx logs"
		docker logs "$nginx" 2>&1 | tail -n 20
	fi

	docker rm -f "$nginx" "$php" "$db" >/dev/null
	docker network rm "$net" >/dev/null
	docker volume rm "$wp_content" "$cache" >/dev/null
}

for v in "${versions[@]}"; do
	test_stack "$v"
done

if [ "$failures" -gt 0 ]; then
	echo "FAILED: $failures check(s)"
	exit 1
fi
echo "PASSED: PHP ${versions[*]} / WordPress $WP_VERSION"
