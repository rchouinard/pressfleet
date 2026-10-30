#!/usr/bin/env bash
# Smoke test for the site images (issues #17, #18, #39, #40, #41, #43, #47, #108, #109, #127, #201).
#
#   images/test.sh [php-version ...]     default: every PHP version images/matrix.sh builds
#
# Builds site-nginx and site-php, then for each PHP version starts MariaDB and
# both site containers on a throwaway network (read-only root filesystems, no
# capabilities, non-root site UID, named wp-content and cache volumes, a minimal read-only
# wp-config.php), installs WordPress with WP-CLI, and checks the runtime
# contract from images/README.md, with a Mailpit container standing in for the
# platform's SMTP relay. Everything it creates is removed on exit;
# the built images are kept.
#
# Environment:
#   SKIP_BUILD=1     use existing images instead of building
#   PRESSFLEET_BUILD_EXTRA_CA, PRESSFLEET_BUILD_NETWORK, proxy variables:
#                    builds behind a TLS-intercepting proxy (build-flags.sh)
#   IMAGE_REPO       image repository (default: "registry" from manifest.json)
#   WP_VERSION       WordPress version to test (default: the manifest's latest;
#                    must be in the manifest, e.g. a pinned version)
#   SITE_UID=10001   UID:GID the site containers run as
#   MARIADB_IMAGE    database image (default mariadb:11.8)
#   MAILPIT_IMAGE    SMTP relay stand-in (default axllent/mailpit:v1.31)
#   VERBOSE=1        print the tail of the site container logs
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
matrix="$here/matrix.sh"
WP_VERSION="${WP_VERSION:-$("$matrix" latest-wp)}"
WP_SHA1="$("$matrix" sha1 "$WP_VERSION")"

registry="${IMAGE_REPO:-$("$matrix" registry)}"
site_uid="${SITE_UID:-10001}"
mariadb_image="${MARIADB_IMAGE:-mariadb:11.8}"
mailpit_image="${MAILPIT_IMAGE:-axllent/mailpit:v1.31}"
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
	docker build -q "${build_flags[@]}" -f "$here/$dockerfile" -t "$tag" \
		--build-arg "WP_VERSION=$WP_VERSION" --build-arg "WP_SHA1=$WP_SHA1" "$@" "$here" >/dev/null
}

if [ "${SKIP_BUILD:-0}" != 1 ]; then
	# Options for a TLS-intercepting proxy (PRESSFLEET_BUILD_*; build-flags.sh).
	mapfile -t build_flags < <("$here/build-flags.sh")
	build site-nginx/Dockerfile "$nginx_image"
	for v in "${versions[@]}"; do
		build site-php/Dockerfile "$registry/site-php:php$v-wp$WP_VERSION" --build-arg "PHP_VERSION=$v"
	done
fi

db_password="$(head -c 18 /dev/urandom | base64 | tr -d '/+=')"
smtp_password="$(head -c 18 /dev/urandom | base64 | tr -d '/+=')"
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
define( 'RT_WP_NGINX_HELPER_CACHE_PATH', '/var/cache/pressfleet/fastcgi' );
define( 'PRESSFLEET_SMTP_HOST', 'mailpit' );
define( 'PRESSFLEET_SMTP_PORT', 1025 );
define( 'PRESSFLEET_SMTP_USERNAME', 'relay' );
define( 'PRESSFLEET_SMTP_PASSWORD', '$smtp_password' );
define( 'PRESSFLEET_SMTP_ENCRYPTION', '' );
define( 'PRESSFLEET_SMTP_FROM_EMAIL', 'sites@pressfleet.test' );
define( 'PRESSFLEET_SMTP_FROM_NAME', 'PressFleet Sites' );
// Security set constants (§6.1), written by the security checks below.
if ( file_exists( __DIR__ . '/wp-content/pf-smoke-security.php' ) ) {
	require __DIR__ . '/wp-content/pf-smoke-security.php';
}
// Written by the two-factor checks; a file of its own, since PHP-FPM's
// OPcache would keep serving an earlier pf-smoke-security.php.
if ( file_exists( __DIR__ . '/wp-content/pf-smoke-two-factor.php' ) ) {
	require __DIR__ . '/wp-content/pf-smoke-two-factor.php';
}
// Site ID and public key for the one-click login checks (#127).
if ( file_exists( __DIR__ . '/wp-content/pf-smoke-login.php' ) ) {
	require __DIR__ . '/wp-content/pf-smoke-login.php';
}
// The outbound mail switch (#201).
if ( file_exists( __DIR__ . '/wp-content/pf-smoke-mail.php' ) ) {
	require __DIR__ . '/wp-content/pf-smoke-mail.php';
}
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
	local net="$p" php="$p-php" nginx="$p-nginx" db="$p-db" mailpit="$p-mailpit"
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

	# The SMTP relay stand-in, requiring the credentials from wp-config.php.
	docker run -d --label "$label" --name "$mailpit" --network "$net" --network-alias mailpit \
		-e "MP_SMTP_AUTH=relay:$smtp_password" -e MP_SMTP_AUTH_ALLOW_INSECURE=1 \
		-p 127.0.0.1::8025 "$mailpit_image" >/dev/null

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
	check "sodium verifies Ed25519 signatures (one-click login, #127)" "${cexec[@]}" php -r \
		'$k = sodium_crypto_sign_keypair(); $m = "x"; exit(sodium_crypto_sign_verify_detached(sodium_crypto_sign_detached($m, sodium_crypto_sign_secretkey($k)), $m, sodium_crypto_sign_publickey($k)) ? 0 : 1);'
	check "gd supports WebP and AVIF" "${cexec[@]}" php -r \
		'$i = gd_info(); exit(($i["WebP Support"] && $i["AVIF Support"]) ? 0 : 1);'
	check "phpredis built with igbinary" "${cexec[@]}" php -r 'exit(defined("Redis::SERIALIZER_IGBINARY") ? 0 : 1);'
	check "wp core install" "${cexec[@]}" wp core install --url="$base" --title="PressFleet smoke test" \
		--admin_user=admin --admin_password="$admin_password" --admin_email=admin@example.com --skip-email
	# Site imports compare dumps against the manifest's dbVersion (#112).
	check_eq "core database version matches the manifest" "$("$matrix" dbversion "$WP_VERSION")" \
		"$("${cexec[@]}" wp option get db_version 2>&1)"

	# --- Default plugins (#39) ------------------------------------------------
	# The image bundles the versions shared/plugins.json pins, and they install
	# from the image, as provisioning does, without WordPress.org.
	local slug version
	while read -r slug version; do
		check "bundled plugin $slug installs from the image" "${cexec[@]}" wp plugin install "/opt/pressfleet/plugins/$slug.zip"
		check_eq "bundled plugin $slug is $version" "$version" "$("${cexec[@]}" wp plugin get "$slug" --field=version 2>&1)"
	done < <(jq -r '.plugins[] | "\(.slug) \(.version)"' "$here/shared/plugins.json")

	# --- Platform mu-plugin ---------------------------------------------------
	check_contains "mu-plugin listed as must-use" "pressfleet" \
		"$("${cexec[@]}" wp plugin list --status=must-use --field=name)"
	check_eq "mu-plugin loaded" "yes" \
		"$("${cexec[@]}" wp eval 'echo defined("PRESSFLEET_PLATFORM") ? "yes" : "no";')"
	check_eq "core updates disabled for admins" "no" \
		"$("${cexec[@]}" wp eval --user=admin 'echo current_user_can("update_core") ? "yes" : "no";')"
	check_eq "filesystem method is direct" "direct" \
		"$("${cexec[@]}" wp eval 'require_once ABSPATH . "wp-admin/includes/file.php"; echo get_filesystem_method();')"
	# Background (plugin and theme) updates still start from WP-Cron's
	# version-check event, which no longer checks core.
	check_eq "plugin and theme auto-updates still run" "started" \
		"$("${cexec[@]}" wp eval 'remove_action( "wp_maybe_auto_update", "wp_maybe_auto_update" ); add_action( "wp_maybe_auto_update", function () { echo "started"; } ); define( "DOING_CRON", true ); do_action( "wp_version_check" );')"

	# --- FastCGI page cache (#40) --------------------------------------------
	# The first request for a page misses and stores it; bypass rules keep
	# per-user and dynamic requests away from the cache.
	check_eq "anonymous GET misses the cache first" "MISS" "$(x_cache "$base/")"
	check_eq "anonymous GET then hits the cache" "HIT" "$(x_cache "$base/")"
	# nginx adds its headers to pages served from the cache too (#109).
	local hit_headers
	hit_headers="$(curl -s -o /dev/null -D - "$base/" | tr -d '\r' | tr '[:upper:]' '[:lower:]')"
	check_contains "cache HIT response" "x-cache: hit" "$hit_headers"
	check_contains "cache HIT has Referrer-Policy" "referrer-policy: strict-origin-when-cross-origin" "$hit_headers"
	check_contains "cache HIT has X-Frame-Options" "x-frame-options: sameorigin" "$hit_headers"
	check_contains "cache HIT has Permissions-Policy" "permissions-policy: camera=(), microphone=(), usb=()" "$hit_headers"
	check_contains "cache HIT has X-Content-Type-Options" "x-content-type-options: nosniff" "$hit_headers"
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
	# The bundled Nginx Helper, configured as the control plane configures
	# it, purges the home page when a post is published (#41).
	"${cexec[@]}" wp plugin activate nginx-helper >/dev/null
	"${cexec[@]}" wp option update rt_wp_nginx_helper_options \
		'{"enable_purge":1,"cache_method":"enable_fastcgi","purge_method":"unlink_files"}' --format=json >/dev/null
	x_cache "$base/" >/dev/null
	check_eq "home page cached before publishing" "HIT" "$(x_cache "$base/")"
	check "publish a post" "${cexec[@]}" wp post create --post_title="Cache purge test" --post_status=publish
	check_eq "publishing purges the home page (Nginx Helper)" "MISS" "$(x_cache "$base/")"
	"${cexec[@]}" wp plugin deactivate nginx-helper >/dev/null
	check "nginx -t with the cache off" docker run --rm --label "$label" --user "$user" "${hardening[@]}" \
		-e NGINX_FASTCGI_CACHE=off "$nginx_image" nginx -e stderr -c /tmp/nginx/nginx.conf -t
	check_not "invalid NGINX_FASTCGI_CACHE is rejected" docker run --rm --label "$label" --user "$user" \
		"${hardening[@]}" -e NGINX_FASTCGI_CACHE=maybe "$nginx_image" true

	# --- Outbound email through the SMTP relay (#47) ------------------------
	# A password reset requested over HTTP arrives at the relay, from the
	# platform's sender address.
	check_eq "SMTP mu-plugin hooks PHPMailer" "yes" \
		"$("${cexec[@]}" wp eval 'echo has_action("phpmailer_init") ? "yes" : "no";')"
	local mailpit_api messages
	mailpit_api="http://127.0.0.1:$(docker port "$mailpit" 8025/tcp | head -n 1 | sed 's/.*://')/api/v1"
	check_eq "password reset request accepted" "302" \
		"$(curl -s -o /dev/null -w '%{http_code}' -d 'user_login=admin&wp-submit=Get+New+Password' "$base/wp-login.php?action=lostpassword")"
	messages=""
	for _ in $(seq 20); do
		messages="$(curl -fsS "$mailpit_api/messages" 2>/dev/null || true)"
		[ "$(jq -r '.total // 0' <<<"$messages" 2>/dev/null)" -gt 0 ] 2>/dev/null && break
		sleep 0.5
	done
	check_contains "password reset email delivered via the relay" "Password Reset" \
		"$(jq -r '.messages[].Subject' <<<"$messages" 2>/dev/null)"
	check_eq "reset email sent to the admin" "admin@example.com" \
		"$(jq -r '.messages[0].To[0].Address' <<<"$messages" 2>/dev/null)"
	check_eq "reset email from the platform sender" "PressFleet Sites <sites@pressfleet.test>" \
		"$(jq -r '.messages[0].From | "\(.Name) <\(.Address)>"' <<<"$messages" 2>/dev/null)"

	# --- Outbound mail switch and environment badge (#201) -------------------
	# Same reset request as above, now with PRESSFLEET_DISABLE_MAIL defined
	# (and the environment set to staging): the relay gets nothing, the log
	# gets one line without the address, and the request still succeeds.
	local mail_before mail_after mail_log
	mail_before="$(curl -fsS "$mailpit_api/messages" | jq -r '.total')"
	docker exec -i "$php" sh -c 'cat > wp-content/pf-smoke-mail.php' <<-'PHP'
		<?php
		define( 'PRESSFLEET_DISABLE_MAIL', true );
		define( 'WP_ENVIRONMENT_TYPE', 'staging' );
	PHP
	check_eq "mail switch: wp_mail() reports success" "yes" \
		"$("${cexec[@]}" wp eval 'echo wp_mail( "nobody@example.com", "Secret subject", "x" ) ? "yes" : "no";' 2>/dev/null)"
	check_eq "mail switch: password reset request still accepted" "302" \
		"$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Forwarded-For: 198.51.100.77' -d 'user_login=admin&wp-submit=Get+New+Password' "$base/wp-login.php?action=lostpassword")"
	sleep 2
	mail_after="$(curl -fsS "$mailpit_api/messages" | jq -r '.total')"
	check_eq "mail switch: the relay receives nothing" "$mail_before" "$mail_after"
	mail_log="$(docker logs "$php" 2>&1)"
	check_contains "mail switch: suppressed message logged with a recipient count" \
		"pressfleet: outbound email is disabled (PRESSFLEET_DISABLE_MAIL); suppressed a message to 1 recipient(s)" "$mail_log"
	check_not "mail switch: log has no addresses or subjects" grep -qE 'admin@example\.com|nobody@example\.com|Secret subject' \
		<(grep 'outbound email is disabled' <<<"$mail_log")
	check_contains "environment badge: admin bar names the environment" "STAGING | email off" \
		"$("${cexec[@]}" wp eval --user=admin 'show_admin_bar( true ); _wp_admin_bar_init(); global $wp_admin_bar; do_action_ref_array( "admin_bar_menu", array( &$wp_admin_bar ) ); echo $wp_admin_bar->get_node( "pressfleet-environment" )->title;' 2>/dev/null)"
	docker exec "$php" rm -f wp-content/pf-smoke-mail.php
	check_eq "mail switch: unhooked again without the constant" "none" \
		"$("${cexec[@]}" wp eval 'echo has_action("pre_wp_mail") ? "hooked" : "none";')"

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
	# A peer on a network nginx joined after starting is not a trusted proxy
	# (NGINX_TRUSTED_PROXIES=local; see the trusted proxy checks below).
	docker network create --label "$label" "$net-other" >/dev/null
	docker network connect --alias nginx-other "$net-other" "$nginx"
	body="$(docker run --rm --label "$label" --network "$net-other" --entrypoint curl "$php_image" \
		-fsS -H 'X-Forwarded-For: 203.0.113.7' -H 'X-Forwarded-Proto: https' \
		http://nginx-other:8080/wp-content/pf-probe.php)" || true
	check_contains "untrusted peer reaches nginx" "remote_addr=" "$body"
	check_not "untrusted peer's X-Forwarded-Proto is ignored" grep -qF "https=on" <<<"$body"
	check_not "untrusted peer's X-Forwarded-For is ignored" grep -qF "remote_addr=203.0.113.7" <<<"$body"
	docker network disconnect "$net-other" "$nginx"
	curl -s -o /dev/null "$base/wp-content/pf-probe.php?sleep=2"
	check_contains "FPM slow log written to stderr" "pf-probe.php" \
		"$(docker logs "$php" 2>&1 | grep -A 3 'executing too slow' || true)"
	"${cexec[@]}" rm -f wp-content/pf-probe.php

	local logs
	logs="$(docker logs "$nginx" 2>/dev/null)"
	check_contains "access log has client IP" '"remote_addr":"203.0.113.7"' "$logs"
	check_contains "access log has cache status" '"upstream_cache_status":' "$logs"
	check_contains "access log has request time" '"request_time":' "$logs"

	# --- Trusted proxies: the container's own network only ------------------
	# NGINX_TRUSTED_PROXIES=local (the default) trusts the network nginx was
	# on when it started, where the platform's Traefik is the only proxy. The
	# test's curl reaches nginx through the network's gateway, which is on
	# it; a peer on a network joined later is not trusted (checked above).
	local subnet
	subnet="$(docker network inspect -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' "$net" | awk '{ print $1 }')"
	check_eq "trusted proxies are the site network" "set_real_ip_from $subnet;" \
		"$(docker exec "$nginx" cat /tmp/nginx/realip.conf)"

	# --- WordPress security set (§6.1), PHP modules -------------------------
	# The modules are off unless wp-config.php defines their constants; the
	# smoke test's wp-config.php includes wp-content/pf-smoke-security.php
	# for them, standing in for the control plane's generated constants.
	"${cexec[@]}" wp rewrite structure '/%postname%/' >/dev/null
	local weak_pw="Qwerty123456" strong_pw argon_pw hello_url="$base/hello-world/" route rp_key
	strong_pw="$(head -c 18 /dev/urandom | base64 | tr -d '/+=')"
	argon_pw="$(head -c 18 /dev/urandom | base64 | tr -d '/+=')"
	login_post() { curl -s -d "log=$1&pwd=$2&wp-submit=Log+In" "$base/wp-login.php"; }

	check_eq "off: ?author=N redirects to the author archive" "301 $base/author/admin/" \
		"$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' "$base/?author=1")"
	check_contains "off: REST lists users" '"slug":"admin"' "$(curl -s "$base/wp-json/wp/v2/users")"
	check_eq "off: users sitemap served" "200" "$(curl -s -o /dev/null -w '%{http_code}' "$base/wp-sitemap-users-1.xml")"
	check_contains "off: oEmbed names the author" '"author_name":"admin"' \
		"$(curl -s "$base/wp-json/oembed/1.0/embed?url=$hello_url")"
	check_contains "off: login error reveals unknown users" "is not registered" "$(login_post nosuchuser x)"
	check "off: weak password accepted" "${cexec[@]}" wp user create weakling weakling@example.com --user_pass="$weak_pw"
	"${cexec[@]}" wp user delete weakling --yes >/dev/null
	check_eq "off: file editor available" "yes" \
		"$("${cexec[@]}" wp eval --user=admin 'echo current_user_can("edit_themes") ? "yes" : "no";')"
	check_eq "off: application passwords available" "yes" \
		"$("${cexec[@]}" wp eval '$_SERVER["HTTPS"] = "on"; echo wp_is_application_passwords_available() ? "yes" : "no";')"
	check "bcrypt user created" "${cexec[@]}" wp user create hasher hasher@example.com --user_pass="$strong_pw"
	check_contains "off: new hashes are bcrypt" '$wp$2y$' "$("${cexec[@]}" wp user get hasher --field=user_pass)"

	docker exec -i "$php" sh -c 'cat > wp-content/pf-smoke-security.php' <<-'PHP'
		<?php
		define( 'DISALLOW_FILE_EDIT', true );
		define( 'PRESSFLEET_BLOCK_USER_ENUMERATION', true );
		define( 'PRESSFLEET_PASSWORD_POLICY_MIN_LENGTH', 12 );
		define( 'PRESSFLEET_DISABLE_APPLICATION_PASSWORDS', true );
	PHP

	check_eq "on: ?author=N is a 404" "404" "$(curl -s -o /dev/null -w '%{http_code}' "$base/?author=1")"
	for route in "/wp-json/wp/v2/users" "/wp-json/wp/v2/users/1" "/wp-json/wp/v2/USERS" "/?rest_route=/wp/v2/users"; do
		check_eq "on: anonymous $route refused" "401" "$(curl -s -o /dev/null -w '%{http_code}' "$base$route")"
	done
	check_eq "on: logged-in users still list users" "200" \
		"$("${cexec[@]}" wp eval --user=admin 'echo rest_do_request( new WP_REST_Request( "GET", "/wp/v2/users" ) )->get_status();')"
	check_eq "on: users sitemap gone" "404" "$(curl -s -o /dev/null -w '%{http_code}' "$base/wp-sitemap-users-1.xml")"
	body="$(curl -s "$base/wp-json/oembed/1.0/embed?url=$hello_url")"
	check_contains "on: oEmbed still works" '"title":"Hello world!"' "$body"
	check_not "on: oEmbed omits the author" grep -q author_name <<<"$body"
	body="$(login_post nosuchuser x)"
	check_contains "on: unknown user gets the generic login error" "username, email address, or password is incorrect" "$body"
	check_not "on: login error doesn't reveal unknown users" grep -q "is not registered" <<<"$body"
	check_contains "on: wrong password gets the same error" "username, email address, or password is incorrect" \
		"$(login_post admin wrong-password)"
	check_eq "on: lost password form doesn't reveal unknown users" "302 $base/wp-login.php?checkemail=confirm" \
		"$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' -d 'user_login=nobody@example.com' "$base/wp-login.php?action=lostpassword")"

	check_contains "on: short password rejected (WP-CLI create)" "at least 12 characters" \
		"$("${cexec[@]}" wp user create weakling weakling@example.com --user_pass=short-pw1 2>&1 || true)"
	check_contains "on: common password rejected (WP-CLI create)" "most commonly used" \
		"$("${cexec[@]}" wp user create weakling weakling@example.com --user_pass="$weak_pw" 2>&1 || true)"
	check_not "on: rejected user was not created" "${cexec[@]}" wp user get weakling
	check_contains "on: common password rejected (WP-CLI update)" "most commonly used" \
		"$("${cexec[@]}" wp user update hasher --user_pass="$weak_pw" 2>&1 || true)"
	check_eq "on: rejected update kept the old password" "ok" \
		"$("${cexec[@]}" wp eval "\$u = get_user_by( 'login', 'hasher' ); echo wp_check_password( '$strong_pw', \$u->user_pass, \$u->ID ) ? 'ok' : 'changed';")"
	check_contains "on: common password rejected (profile save)" "most commonly used" \
		"$("${cexec[@]}" wp eval --user=admin "require_once ABSPATH . 'wp-admin/includes/user.php'; \$_POST = array( 'email' => 'admin@example.com', 'nickname' => 'admin', 'pass1' => '$weak_pw', 'pass2' => '$weak_pw' ); \$r = edit_user( get_current_user_id() ); echo is_wp_error( \$r ) ? \$r->get_error_message() : 'saved';")"
	check_eq "on: common password rejected (REST)" "400" \
		"$("${cexec[@]}" wp eval --user=admin "\$r = new WP_REST_Request( 'POST', '/wp/v2/users' ); \$r->set_body_params( array( 'username' => 'restuser', 'email' => 'rest@example.com', 'password' => '$weak_pw' ) ); echo rest_do_request( \$r )->get_status();")"
	rp_key="$("${cexec[@]}" wp eval 'echo get_password_reset_key( get_user_by( "login", "hasher" ) );')"
	curl -s -o /dev/null -c "$tmp/rp-cookies" "$base/wp-login.php?action=rp&key=$rp_key&login=hasher"
	check_contains "on: common password rejected (reset form)" "most commonly used" \
		"$(curl -s -b "$tmp/rp-cookies" -d "pass1=$weak_pw&pass2=$weak_pw&rp_key=$rp_key&wp-submit=Save+Password" "$base/wp-login.php?action=resetpass")"
	check "on: strong password accepted" "${cexec[@]}" wp user create strongling strongling@example.com --user_pass="$strong_pw"
	check_eq "on: file editor disabled" "no" \
		"$("${cexec[@]}" wp eval --user=admin 'echo current_user_can("edit_themes") ? "yes" : "no";')"
	check_eq "on: application passwords disabled" "no" \
		"$("${cexec[@]}" wp eval '$_SERVER["HTTPS"] = "on"; echo wp_is_application_passwords_available() ? "yes" : "no";')"

	# Only WP-CLI (no OPcache) reads the changed file from here on.
	"${cexec[@]}" sh -c "echo \"define( 'PRESSFLEET_PASSWORD_HASHING', 'argon2id' );\" >> wp-content/pf-smoke-security.php"
	check "argon2id: password changed" "${cexec[@]}" wp user update strongling --user_pass="$argon_pw"
	check_contains "argon2id: new hashes are Argon2id" '$argon2id$' "$("${cexec[@]}" wp user get strongling --field=user_pass)"
	check_eq "argon2id: new hash verifies" "ok" \
		"$("${cexec[@]}" wp eval "\$u = get_user_by( 'login', 'strongling' ); echo wp_check_password( '$argon_pw', \$u->user_pass, \$u->ID ) ? 'ok' : 'bad';")"
	check_eq "argon2id: existing bcrypt hash still logs in" "ok" \
		"$("${cexec[@]}" wp eval "echo is_wp_error( wp_authenticate( 'hasher', '$strong_pw' ) ) ? 'bad' : 'ok';")"
	check_contains "argon2id: bcrypt hash upgraded at login" '$argon2id$' "$("${cexec[@]}" wp user get hasher --field=user_pass)"
	"${cexec[@]}" rm -f wp-content/pf-smoke-security.php

	# --- One-click administrator login (#127, §5.9) ---------------------------
	# Tokens are signed here with a throwaway key pair; the site gets the
	# public key (base64url, as the control plane writes it) and its ID, as
	# the generated wp-config.php would. Each request uses its own client IP,
	# clear of the login rate limit.
	local login_keys login_sk login_site="smoke-site-1" login_n=0
	login_keys="$("${cexec[@]}" php -r '$k = sodium_crypto_sign_keypair(); echo sodium_bin2hex(sodium_crypto_sign_secretkey($k)), " ", rtrim(strtr(base64_encode(sodium_crypto_sign_publickey($k)), "+/", "-_"), "=");')"
	login_sk="${login_keys%% *}"
	docker exec -i "$php" sh -c 'cat > wp-content/pf-smoke-login.php' <<-PHP
		<?php
		define( 'PRESSFLEET_SITE_ID', '$login_site' );
		define( 'PRESSFLEET_LOGIN_PUBLIC_KEY', '${login_keys#* }' );
	PHP
	# mint_token USER SITE IAT_OFFSET EXP_OFFSET [tamper]: a v1 token on stdout.
	mint_token() {
		docker exec -e "SK=$login_sk" "$php" php -r '
			[ , $user, $site, $iat, $exp, $tamper ] = array_pad( $argv, 6, "" );
			$b64 = fn( $d ) => rtrim( strtr( base64_encode( $d ), "+/", "-_" ), "=" );
			$p = $b64( json_encode( array( "site" => $site, "user" => $user, "actor" => "usr_smoke", "jti" => bin2hex( random_bytes( 16 ) ), "iat" => time() + (int) $iat, "exp" => time() + (int) $exp ) ) );
			$sig = $b64( sodium_crypto_sign_detached( "v1." . $p, sodium_hex2bin( getenv( "SK" ) ) ) );
			if ( "tamper" === $tamper ) {
				$p = $b64( str_replace( "usr_smoke", "usr_evil", base64_decode( strtr( $p, "-_", "+/" ) ) ) );
			}
			echo "v1.$p.$sig";
		' -- "$@"
	}
	# login_request TOKEN [curl args...]: POSTs the token; prints "CODE REDIRECT_URL".
	# Headers go to $tmp/login-headers, the body to $tmp/login-body, cookies to $tmp/login-cookies.
	login_request() {
		local token="$1"
		shift
		# Counted in a file: callers run this in a command substitution, so a
		# shell variable would never advance.
		login_n=$(($(cat "$tmp/login-n" 2>/dev/null || echo 0) + 1))
		echo "$login_n" >"$tmp/login-n"
		rm -f "$tmp/login-cookies"
		curl -s -o "$tmp/login-body" -D "$tmp/login-headers" -c "$tmp/login-cookies" \
			-w '%{http_code} %{redirect_url}' -H "X-Forwarded-For: 198.51.100.$((100 + login_n))" \
			--data-urlencode "token=$token" "$@" "$base/wp-login.php?action=pressfleet-login"
	}
	local login_refused="The login link is not valid" login_ok="302 $base/wp-admin/" login_res
	"${cexec[@]}" wp user create pf-editor pf-editor@example.com --role=editor --user_pass="$(head -c 18 /dev/urandom | base64 | tr -d '/+=')" >/dev/null
	check_eq "login: constants make the module load" "yes" \
		"$("${cexec[@]}" wp eval 'echo function_exists( "pressfleet_login_verify" ) ? "yes" : "no";')"

	check_eq "login: a valid token redirects to wp-admin" "$login_ok" "$(login_request "$(mint_token admin "$login_site" -1 59)")"
	check_contains "login: response is Cache-Control: no-store" "cache-control: no-store" "$(tr -d '\r' <"$tmp/login-headers" | tr '[:upper:]' '[:lower:]')"
	check_contains "login: sets the logged-in cookie" "wordpress_logged_in_" "$(cat "$tmp/login-cookies")"
	check_not "login: the cookie is a browser-session cookie" grep -qi 'expires=' "$tmp/login-headers"
	check_eq "login: the session reaches wp-admin as admin" "200 admin" \
		"$(curl -s -b "$tmp/login-cookies" -H 'X-Forwarded-For: 198.51.100.99' -o "$tmp/login-body" -w '%{http_code}' "$base/wp-admin/profile.php") $(grep -o 'id="user_login"[^>]*value="[^"]*"' "$tmp/login-body" | sed 's/.*value="//;s/"//')"
	check_eq "login: the session records actor and jti" "usr_smoke yes" \
		"$("${cexec[@]}" wp eval '$s = WP_Session_Tokens::get_instance( get_user_by( "login", "admin" )->ID )->get_all(); foreach ( $s as $x ) { if ( ! empty( $x["pressfleet_login"] ) ) { echo $x["pressfleet_login"]["actor"], " ", preg_match( "/^[0-9a-f]{32}$/", $x["pressfleet_login"]["jti"] ) ? "yes" : "no"; break; } }')"
	check_eq "login: the used token ID is recorded in the database" "1" \
		"$("${cexec[@]}" wp eval 'global $wpdb; echo $wpdb->get_var( "SELECT COUNT(*) FROM $wpdb->options WHERE option_name LIKE \"pressfleet\\_login\\_jti\\_%\"" ) > 0 ? 1 : 0;')"

	local replay_token
	replay_token="$(mint_token admin "$login_site" -1 59)"
	check_eq "login: first use of a token succeeds" "$login_ok" "$(login_request "$replay_token")"
	check_eq "login: a replayed token is refused" "403 " "$(login_request "$replay_token")"
	check_contains "login: refusal shows the generic error" "$login_refused" "$(cat "$tmp/login-body")"
	check_not "login: refusal sets no cookie" grep -q wordpress_logged_in_ "$tmp/login-cookies"
	check_eq "login: an expired token is refused" "403 " "$(login_request "$(mint_token admin "$login_site" -120 -60)")"
	check_contains "login: expiry looks like any other refusal" "$login_refused" "$(cat "$tmp/login-body")"
	check_eq "login: a token that outlives 60 s is refused" "403 " "$(login_request "$(mint_token admin "$login_site" -1 120)")"
	check_eq "login: a token issued in the future is refused" "403 " "$(login_request "$(mint_token admin "$login_site" 300 360)")"
	check_eq "login: a token for another site is refused" "403 " "$(login_request "$(mint_token admin other-site -1 59)")"
	check_eq "login: a tampered payload is refused" "403 " "$(login_request "$(mint_token admin "$login_site" -1 59 tamper)")"
	local sig_token
	sig_token="$(mint_token admin "$login_site" -1 59)"
	# Change the signature's first character (fully significant, unlike the last).
	check_eq "login: a tampered signature is refused" "403 " \
		"$(login_request "${sig_token%.*}.$(sed 's/^A/B/;t;s/^./A/' <<<"${sig_token##*.}")")"
	check_eq "login: a token signed by another key is refused" "403 " \
		"$(login_request "$(docker exec "$php" php -r '$b = fn( $d ) => rtrim( strtr( base64_encode( $d ), "+/", "-_" ), "=" ); $p = $b( json_encode( array( "site" => "'"$login_site"'", "user" => "admin", "actor" => "x", "jti" => bin2hex( random_bytes( 16 ) ), "iat" => time(), "exp" => time() + 30 ) ) ); echo "v1.$p.", $b( sodium_crypto_sign_detached( "v1.$p", sodium_crypto_sign_secretkey( sodium_crypto_sign_keypair() ) ) );')")"
	check_eq "login: a non-administrator is refused" "403 " "$(login_request "$(mint_token pf-editor "$login_site" -1 59)")"
	check_eq "login: an unknown user is refused" "403 " "$(login_request "$(mint_token nobody "$login_site" -1 59)")"
	check_eq "login: an empty token is refused" "403 " "$(login_request "")"
	check_eq "login: GET is refused" "405 " \
		"$(curl -s -o "$tmp/login-body" -w '%{http_code} %{redirect_url}' -H 'X-Forwarded-For: 198.51.100.90' "$base/wp-login.php?action=pressfleet-login&token=$(mint_token admin "$login_site" -1 59)")"
	check_contains "login: GET refusal shows the generic error" "$login_refused" "$(cat "$tmp/login-body")"
	check_contains "login: refusals are logged with a reason" "pressfleet-login: refused: token expired" "$(docker logs "$php" 2>&1)"
	check_not "login: tokens never reach the logs" grep -qE 'v1\.[A-Za-z0-9_-]{20,}\.' <(docker logs "$php" 2>&1)
	check_eq "login: password login is unaffected" "302 $base/wp-admin/" \
		"$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' -H 'X-Forwarded-For: 198.51.100.91' -b 'wordpress_test_cookie=WP%20Cookie%20check' -d "log=admin&pwd=$admin_password&wp-submit=Log+In&testcookie=1" "$base/wp-login.php")"

	# Two-factor required for administrators (#43). The bundled Two Factor
	# plugin was installed above but never activated: the module keeps it
	# active and enrols administrators at their next login. Requests use
	# their own client IP, clear of the login rate limit checks below.
	echo "<?php define( 'PRESSFLEET_REQUIRE_TWO_FACTOR', true );" |
		docker exec -i "$php" sh -c 'cat > wp-content/pf-smoke-two-factor.php'
	check "2fa: Two Factor is kept active" "${cexec[@]}" wp plugin is-active two-factor
	"${cexec[@]}" wp plugin deactivate two-factor >/dev/null 2>&1 || true
	check "2fa: deactivating Two Factor doesn't stick" "${cexec[@]}" wp plugin is-active two-factor
	local tfa_check='echo pressfleet_two_factor_setup_required( get_user_by( "login", "admin" ) ) ? "yes" : "no";'
	check_eq "2fa: the admin must set up a second factor" "yes" "$("${cexec[@]}" wp eval "$tfa_check")"
	check_eq "2fa: no application passwords until then" "no" \
		"$("${cexec[@]}" wp eval '$_SERVER["HTTPS"] = "on"; echo wp_is_application_passwords_available_for_user( get_user_by( "login", "admin" ) ) ? "yes" : "no";')"
	local tfa_headers tfa_cookies tfa_ip='X-Forwarded-For: 198.51.100.20'
	tfa_headers="$(curl -s -o /dev/null -D - -H "$tfa_ip" -b 'wordpress_test_cookie=WP%20Cookie%20check' \
		-d "log=admin&pwd=$admin_password&wp-submit=Log+In&testcookie=1" "$base/wp-login.php" | tr -d '\r')"
	check_contains "2fa: logging in lands on the two-factor settings" \
		"location: $base/wp-admin/profile.php#two-factor-options" "$(tr '[:upper:]' '[:lower:]' <<<"$tfa_headers")"
	tfa_cookies="$(sed -n 's/^[Ss]et-[Cc]ookie: \([^;]*\).*/\1/p' <<<"$tfa_headers" | paste -sd ';' -)"
	check_eq "2fa: the dashboard sends them back there" "302 $base/wp-admin/profile.php#two-factor-options" \
		"$(curl -s -o /dev/null -w '%{http_code} %{redirect_url}' -H "$tfa_ip" -H "Cookie: $tfa_cookies" "$base/wp-admin/")"
	check_contains "2fa: the profile says why" "requires two-factor authentication for administrators" \
		"$(curl -s -H "$tfa_ip" -H "Cookie: $tfa_cookies" "$base/wp-admin/profile.php")"
	# One-click login (#127): exempt from the enrollment redirect and notice.
	check_eq "2fa: a dashboard login is not redirected to enrollment" "$login_ok" "$(login_request "$(mint_token admin "$login_site" -1 59)")"
	check_eq "2fa: a dashboard login reaches the dashboard" "200" \
		"$(curl -s -o "$tmp/login-body" -w '%{http_code}' -b "$tmp/login-cookies" -H "$tfa_ip" "$base/wp-admin/")"
	check_not "2fa: a dashboard login sees no enrollment notice" grep -q "requires two-factor authentication" "$tmp/login-body"
	check_eq "2fa: a dashboard login still has no application passwords" "no" \
		"$("${cexec[@]}" wp eval '$_SERVER["HTTPS"] = "on"; echo wp_is_application_passwords_available_for_user( get_user_by( "login", "admin" ) ) ? "yes" : "no";')"
	"${cexec[@]}" wp eval 'Two_Factor_Core::enable_provider_for_user( get_user_by( "login", "admin" )->ID, "Two_Factor_Email" );'
	check_eq "2fa: with a second factor, a dashboard login still asks for none" "$login_ok" "$(login_request "$(mint_token admin "$login_site" -1 59)")"
	check_eq "2fa: with a second factor, a dashboard login reaches the dashboard" "200" \
		"$(curl -s -o "$tmp/login-body" -w '%{http_code}' -b "$tmp/login-cookies" -H "$tfa_ip" "$base/wp-admin/")"
	check_eq "2fa: with a second factor the admin is done" "no" "$("${cexec[@]}" wp eval "$tfa_check")"
	"${cexec[@]}" rm -f wp-content/pf-smoke-two-factor.php
	"${cexec[@]}" wp plugin deactivate two-factor >/dev/null 2>&1 || true
	check_not "2fa off: Two Factor can be deactivated" "${cexec[@]}" wp plugin is-active two-factor

	# --- WordPress security set (§6.1), nginx ---------------------------------
	# Security headers, on by default: on PHP pages, static files, and errors,
	# once each even where WordPress sends its own (wp-login.php).
	local headers path
	for path in / /wp-includes/css/dashicons.min.css /no-such-page/ /wp-login.php; do
		headers="$(curl -s -o /dev/null -D - "$base$path" | tr -d '\r' | tr '[:upper:]' '[:lower:]')"
		check_eq "headers on $path: Referrer-Policy once" "1" "$(grep -c '^referrer-policy: ' <<<"$headers")"
		check_eq "headers on $path: X-Frame-Options once" "1" "$(grep -c '^x-frame-options: sameorigin$' <<<"$headers")"
		check_contains "headers on $path: Permissions-Policy" "permissions-policy: camera=(), microphone=(), usb=()" "$headers"
		check_contains "headers on $path: X-Content-Type-Options" "x-content-type-options: nosniff" "$headers"
	done

	# Login rate limit, on by default (10r/m, burst 10), keyed on the real
	# client IP from the trusted proxy's X-Forwarded-For.
	local codes="" i
	for i in $(seq 15); do
		codes+="$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Forwarded-For: 198.51.100.10' "$base/wp-login.php") "
	done
	check_contains "login rate limit: first requests pass" "200 200 200" "$codes"
	check_contains "login rate limit: excess requests get 429" "429" "${codes##* 200 }"
	check_eq "login rate limit: another client IP is unaffected" "200" \
		"$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Forwarded-For: 198.51.100.11' "$base/wp-login.php")"
	check_eq "login rate limit: other pages are unaffected" "200" \
		"$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Forwarded-For: 198.51.100.10' "$base/")"

	# Both turned off, on a second nginx for the same site.
	local nginx_off="$p-nginx-off" base_off
	docker run -d --label "$label" --name "$nginx_off" --network "$net" \
		--user "$user" "${hardening[@]}" \
		-v "$wp_content:/var/www/wordpress/wp-content:ro" \
		-v "$cache:/var/cache/pressfleet/fastcgi" \
		-e NGINX_SECURITY_HEADERS=off -e NGINX_LOGIN_RATE_LIMIT=off \
		-p 127.0.0.1::8080 --health-interval 2s "$nginx_image" >/dev/null
	check "nginx with the security set off is healthy" wait_healthy "$nginx_off"
	base_off="http://127.0.0.1:$(docker port "$nginx_off" 8080/tcp | head -n 1 | sed 's/.*://')"
	headers="$(curl -s -o /dev/null -D - "$base_off/" | tr -d '\r' | tr '[:upper:]' '[:lower:]')"
	check_not "headers off: no Referrer-Policy" grep -q '^referrer-policy:' <<<"$headers"
	check_not "headers off: no X-Frame-Options" grep -q '^x-frame-options:' <<<"$headers"
	check_not "headers off: no Permissions-Policy" grep -q '^permissions-policy:' <<<"$headers"
	check_contains "headers off: X-Content-Type-Options stays" "x-content-type-options: nosniff" "$headers"
	codes=""
	for i in $(seq 15); do
		codes+="$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Forwarded-For: 198.51.100.12' "$base_off/wp-login.php") "
	done
	check_not "login rate limit off: no 429" grep -q 429 <<<"$codes"
	docker rm -f "$nginx_off" >/dev/null
	check_not "invalid NGINX_LOGIN_RATE refused" docker run --rm --user "$user" "${hardening[@]}" \
		-e NGINX_LOGIN_RATE='10r/m; evil' "$nginx_image" true
	check_not "invalid NGINX_SECURITY_HEADERS refused" docker run --rm --user "$user" "${hardening[@]}" \
		-e NGINX_SECURITY_HEADERS=maybe "$nginx_image" true

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

	docker rm -f "$nginx" "$php" "$db" "$mailpit" >/dev/null
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
