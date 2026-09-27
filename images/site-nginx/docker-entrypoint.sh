#!/bin/sh
# site-nginx entrypoint. Runs as the site UID, never root.
#
# Renders the nginx configuration into /tmp/nginx from the environment, then
# execs the command (nginx by default). Values are validated so that an
# environment variable can never inject nginx directives.
set -eu

run=/tmp/nginx
conf=/etc/nginx/pressfleet

die() {
	echo "pressfleet: $*" >&2
	exit 1
}

# scalar NAME PATTERN: the variable must match the shell case PATTERN.
scalar() {
	eval "value=\${$1-}"
	# shellcheck disable=SC2254
	case "$value" in
	'' | *[!$2]*) die "invalid $1: '$value'" ;;
	esac
}

scalar NGINX_WORKER_PROCESSES 'a-z0-9'
scalar NGINX_WORKER_CONNECTIONS '0-9'
scalar NGINX_CLIENT_MAX_BODY_SIZE '0-9kKmMgG'
scalar NGINX_FASTCGI_READ_TIMEOUT '0-9smh'
scalar NGINX_RESOLVER 'a-zA-Z0-9.: -'
scalar PHP_FPM_HOST 'a-zA-Z0-9.-'
scalar PHP_FPM_PORT '0-9'
scalar NGINX_FASTCGI_CACHE_VALID '0-9smhdwMy'
scalar NGINX_FASTCGI_CACHE_MAX_SIZE '0-9kKmMgG'

# address_list NAME: splits a comma/space separated list of IPs or CIDRs.
address_list() {
	eval "value=\${$1-}"
	for addr in $(echo "$value" | tr ',' ' '); do
		case "$addr" in
		*[!0-9a-fA-F.:/]*) die "invalid address in $1: '$addr'" ;;
		esac
		echo "$addr"
	done
}

mkdir -p "$run/client_body" "$run/fastcgi" "$run/proxy" "$run/uwsgi" "$run/scgi"

: >"$run/realip.conf"
: >"$run/trusted-proxies.geo"
for cidr in $(address_list NGINX_TRUSTED_PROXIES); do
	echo "set_real_ip_from $cidr;" >>"$run/realip.conf"
	echo "$cidr 1;" >>"$run/trusted-proxies.geo"
done

: >"$run/status-allow.conf"
for addr in $(address_list NGINX_STATUS_ALLOW); do
	echo "allow $addr;" >>"$run/status-allow.conf"
done
echo "deny all;" >>"$run/status-allow.conf"

case "$NGINX_XMLRPC" in
allow | on | 1)
	cat >"$run/xmlrpc.conf" <<-EOF
		include $conf/snippets/fastcgi-php.conf;
		include $conf/php.d/*.conf;
		fastcgi_pass php;
	EOF
	;;
deny | off | 0)
	echo "return 403;" >"$run/xmlrpc.conf"
	;;
*) die "invalid NGINX_XMLRPC: '$NGINX_XMLRPC' (want allow or deny)" ;;
esac

# FastCGI page cache (#40). levels=1:2 is what Nginx Helper's file purge
# expects (see snippets/fastcgi-cache.conf). use_temp_path=off writes
# responses straight into the cache volume instead of copying them over from
# /tmp. The cache files belong to the site UID nginx runs as, so PHP (same
# UID) can delete them.
cache_dir=/var/cache/pressfleet/fastcgi
case "$NGINX_FASTCGI_CACHE" in
on | 1 | true)
	[ -w "$cache_dir" ] || echo "pressfleet: warning: $cache_dir is not writable; pages will not be cached" >&2
	cat >"$run/fastcgi-cache-http.conf" <<-EOF
		fastcgi_cache_path $cache_dir levels=1:2 keys_zone=pressfleet:10m max_size=$NGINX_FASTCGI_CACHE_MAX_SIZE inactive=1d use_temp_path=off;
	EOF
	cat >"$run/fastcgi-cache-php.conf" <<-EOF
		include $conf/snippets/fastcgi-cache.conf;
		fastcgi_cache_valid 200 301 $NGINX_FASTCGI_CACHE_VALID;
	EOF
	;;
off | 0 | false)
	: >"$run/fastcgi-cache-http.conf"
	: >"$run/fastcgi-cache-php.conf"
	;;
*) die "invalid NGINX_FASTCGI_CACHE: '$NGINX_FASTCGI_CACHE' (want on or off)" ;;
esac

# Login rate limit (§6.1): per real client IP (after real_ip), 429 when
# exceeded. The zone is always defined; limit_req only when enabled.
case "$NGINX_LOGIN_RATE_LIMIT" in
on | 1 | true)
	digits="${NGINX_LOGIN_RATE%%[!0-9]*}"
	case "$digits:${NGINX_LOGIN_RATE#"$digits"}" in
	[0-9]*:r/s | [0-9]*:r/m) ;;
	*) die "invalid NGINX_LOGIN_RATE: '$NGINX_LOGIN_RATE' (want e.g. 10r/m)" ;;
	esac
	scalar NGINX_LOGIN_BURST '0-9'
	echo "limit_req_zone \$binary_remote_addr zone=pressfleet_login:1m rate=$NGINX_LOGIN_RATE;" >"$run/login-zone.conf"
	echo "limit_req zone=pressfleet_login burst=$NGINX_LOGIN_BURST nodelay;" >"$run/login-limit.conf"
	;;
off | 0 | false)
	: >"$run/login-zone.conf"
	: >"$run/login-limit.conf"
	;;
*) die "invalid NGINX_LOGIN_RATE_LIMIT: '$NGINX_LOGIN_RATE_LIMIT' (want on or off)" ;;
esac

# Security headers (§6.1). Each is sent only when the response doesn't have
# its own (see the maps in nginx.conf), so WordPress's and plugins' values win.
case "$NGINX_SECURITY_HEADERS" in
on | 1 | true)
	cat >"$run/security-headers.conf" <<-'EOF'
		add_header Referrer-Policy $pressfleet_referrer_policy always;
		add_header X-Frame-Options $pressfleet_x_frame_options always;
		add_header Permissions-Policy $pressfleet_permissions_policy always;
	EOF
	;;
off | 0 | false)
	: >"$run/security-headers.conf"
	;;
*) die "invalid NGINX_SECURITY_HEADERS: '$NGINX_SECURITY_HEADERS' (want on or off)" ;;
esac

# shellcheck disable=SC2016 # the variable names are for envsubst, not the shell
envsubst '${NGINX_WORKER_PROCESSES} ${NGINX_WORKER_CONNECTIONS} ${NGINX_CLIENT_MAX_BODY_SIZE} ${NGINX_FASTCGI_READ_TIMEOUT} ${NGINX_RESOLVER} ${PHP_FPM_HOST} ${PHP_FPM_PORT}' \
	<"$conf/nginx.conf.template" >"$run/nginx.conf"

if [ "$(id -u)" = "0" ]; then
	echo "pressfleet: warning: running as root; site containers should run as the site UID" >&2
fi

exec "$@"
