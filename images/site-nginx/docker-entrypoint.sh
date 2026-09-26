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

# shellcheck disable=SC2016 # the variable names are for envsubst, not the shell
envsubst '${NGINX_WORKER_PROCESSES} ${NGINX_WORKER_CONNECTIONS} ${NGINX_CLIENT_MAX_BODY_SIZE} ${NGINX_FASTCGI_READ_TIMEOUT} ${NGINX_RESOLVER} ${PHP_FPM_HOST} ${PHP_FPM_PORT}' \
	<"$conf/nginx.conf.template" >"$run/nginx.conf"

if [ "$(id -u)" = "0" ]; then
	echo "pressfleet: warning: running as root; site containers should run as the site UID" >&2
fi

exec "$@"
