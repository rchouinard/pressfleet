#!/usr/bin/env bash
# Pod Security "restricted" test for the images (issue #222).
#
#   images/test-restricted.sh [php-version]
#
# Without a cluster, runs the images the way a restricted pod runs them: an
# arbitrary non-root UID plus a supplemental group (fsGroup), no capabilities,
# no-new-privileges, Docker's default seccomp profile, a read-only root
# filesystem with writable emptyDir-like volumes, and nginx and php sharing a
# network namespace (a pod's containers share one) with nginx reaching PHP at
# 127.0.0.1. Probes run as exec commands. Set CONTROL_PLANE_IMAGE to also run
# the control plane image as a task pod. The kind tests (#228) cover the
# cluster side: admission, fsGroup, and probes.
#
# Environment: SKIP_BUILD=1, IMAGE_REPO, WP_VERSION, and the build proxy
# variables are as for images/test.sh; SITE_UID (default 10001) and
# SITE_FSGROUP (default 10002) pick the identity; VERBOSE=1 prints logs.
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
matrix="$here/matrix.sh"
WP_VERSION="${WP_VERSION:-$("$matrix" latest-wp)}"
WP_SHA1="$("$matrix" sha1 "$WP_VERSION")"
php_version="${1:-$("$matrix" php | head -n 1)}"
registry="${IMAGE_REPO:-$("$matrix" registry)}"
uid="${SITE_UID:-10001}"
fsgroup="${SITE_FSGROUP:-10002}"
run_id="pf-k8stest-$$"
label="io.pressfleet.imgtest=$run_id"
php_image="$registry/site-php:php$php_version-wp$WP_VERSION"
nginx_image="$registry/site-nginx:wp$WP_VERSION"

if [ "${SKIP_BUILD:-}" != 1 ]; then
	# build-flags.sh prints separate words.
	read -r -a flags <<<"$("$here/build-flags.sh")"
	docker build "${flags[@]}" -f "$here/site-nginx/Dockerfile" -t "$nginx_image" \
		--build-arg "WP_VERSION=$WP_VERSION" --build-arg "WP_SHA1=$WP_SHA1" "$here"
	docker build "${flags[@]}" -f "$here/site-php/Dockerfile" -t "$php_image" \
		--build-arg "PHP_VERSION=$php_version" --build-arg "WP_VERSION=$WP_VERSION" \
		--build-arg "WP_SHA1=$WP_SHA1" "$here"
fi

cleanup() {
	local ids
	ids="$(docker ps -aq --filter "label=$label")"
	# shellcheck disable=SC2086 # one ID per word
	[ -z "$ids" ] || docker rm -f $ids >/dev/null
	ids="$(docker volume ls -q --filter "label=$label")"
	# shellcheck disable=SC2086
	[ -z "$ids" ] || docker volume rm $ids >/dev/null
}
trap cleanup EXIT

failures=0
pass() { printf '  ok    %s\n' "$1"; }
fail() {
	printf '  FAIL  %s\n' "$1"
	[ -z "${2:-}" ] || printf '        %s\n' "$2"
	failures=$((failures + 1))
}
check() {
	local desc="$1" out
	shift
	if out="$("$@" 2>&1)"; then pass "$desc"; else fail "$desc" "$(tail -n 3 <<<"$out")"; fi
}
check_not() {
	local desc="$1"
	shift
	if "$@" >/dev/null 2>&1; then fail "$desc" "command unexpectedly succeeded: $*"; else pass "$desc"; fi
}
check_eq() {
	if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "want '$2', got '$3'"; fi
}
check_contains() {
	if grep -qF -- "$2" <<<"$3"; then pass "$1"; else fail "$1" "missing '$2' in: $(head -c 300 <<<"$3")"; fi
}
# retry COMMAND...: waits up to 30 s for the command to succeed.
retry() {
	for _ in $(seq 30); do
		"$@" >/dev/null 2>&1 && return 0
		sleep 1
	done
	"$@"
}

# What a restricted pod's securityContext comes to.
restricted=(--user "$uid:$uid" --group-add "$fsgroup" --read-only --cap-drop ALL
	--security-opt no-new-privileges:true --security-opt seccomp=builtin)

echo "==> restricted pod: $php_image + $nginx_image"
wp_content="$run_id-wp-content"
cache="$run_id-cache"
docker volume create --label "$label" "$wp_content" >/dev/null
docker volume create --label "$label" "$cache" >/dev/null
# What an emptyDir or a PVC looks like with fsGroup: owned by root, writable
# through the group.
for v in "$wp_content" "$cache"; do
	docker run --rm --user 0 -v "$v:/v" --entrypoint sh "$nginx_image" -c "chown 0:$fsgroup /v && chmod 2775 /v"
done
docker run --rm -i --user 0 -v "$wp_content:/v" --entrypoint sh "$nginx_image" \
	-c 'cat > /v/pf-probe.php && chmod 0644 /v/pf-probe.php' <<'PHP'
<?php
echo 'https=', $_SERVER['HTTPS'] ?? '', "\n";
echo 'remote_addr=', $_SERVER['REMOTE_ADDR'] ?? '', "\n";
echo "php=ok\n";
PHP

# The pod's network namespace is the php container's.
docker run -d --label "$label" --name "$run_id-php" "${restricted[@]}" --tmpfs /tmp \
	-v "$wp_content:/var/www/wordpress/wp-content" -v "$cache:/var/cache/pressfleet/fastcgi" \
	"$php_image" >/dev/null

start_nginx() {
	docker rm -f "$run_id-nginx" >/dev/null 2>&1 || true
	docker run -d --label "$label" --name "$run_id-nginx" --network "container:$run_id-php" \
		"${restricted[@]}" --tmpfs /tmp -e PHP_FPM_HOST=127.0.0.1 "$@" \
		-v "$wp_content:/var/www/wordpress/wp-content:ro" -v "$cache:/var/cache/pressfleet/fastcgi" \
		"$nginx_image" >/dev/null
	retry docker exec "$run_id-nginx" /usr/local/bin/pressfleet-healthcheck || true
}
# fetch URL: a request from inside the pod's network namespace, so from
# 127.0.0.1.
fetch() {
	docker run --rm --network "container:$run_id-php" --entrypoint curl "$php_image" -fsS \
		-H 'X-Forwarded-For: 203.0.113.7' -H 'X-Forwarded-Proto: https' "$@"
}
probe_url=http://127.0.0.1:8080/wp-content/pf-probe.php

# The driver sets NGINX_TRUSTED_PROXIES to the pod network's ranges; here the
# peer is loopback.
start_nginx -e "NGINX_TRUSTED_PROXIES=127.0.0.1/32"

check "php probe passes" retry docker exec "$run_id-php" /usr/local/bin/pressfleet-healthcheck
check "nginx probe passes" docker exec "$run_id-nginx" /usr/local/bin/pressfleet-healthcheck
check_eq "php runs as the site UID" "$uid" "$(docker exec "$run_id-php" id -u)"
check_eq "nginx runs as the site UID" "$uid" "$(docker exec "$run_id-nginx" id -u)"
check_eq "php has no capabilities" "0000000000000000" \
	"$(docker exec "$run_id-php" awk '/^CapEff:/ { print $2 }' /proc/self/status)"
check_not "php root filesystem is read-only" docker exec "$run_id-php" touch /var/www/wordpress/x
check_not "nginx root filesystem is read-only" docker exec "$run_id-nginx" touch /etc/nginx/x

body="$(retry fetch "$probe_url" >/dev/null 2>&1; fetch "$probe_url" || true)"
check_contains "nginx reaches PHP at 127.0.0.1 in the pod" "php=ok" "$body"
check_contains "forwarded proto trusted from the configured range" "https=on" "$body"
check_contains "forwarded client IP trusted from the configured range" "remote_addr=203.0.113.7" "$body"
check_eq "trusted proxies are the configured range" "set_real_ip_from 127.0.0.1/32;" \
	"$(docker exec "$run_id-nginx" cat /tmp/nginx/realip.conf)"

start_nginx -e "NGINX_TRUSTED_PROXIES=192.0.2.0/24"
body="$(fetch "$probe_url" || true)"
check_contains "a peer outside the range still reaches nginx" "remote_addr=" "$body"
check_not "forwarded proto ignored from other peers" grep -qF "https=on" <<<"$body"
check_not "forwarded client IP ignored from other peers" grep -qF "remote_addr=203.0.113.7" <<<"$body"

check "cache volume writable through the group" docker exec "$run_id-nginx" touch /var/cache/pressfleet/fastcgi/x
check "wp-content writable by php" docker exec "$run_id-php" touch /var/www/wordpress/wp-content/x
check_not "wp-content read-only in nginx" docker exec "$run_id-nginx" touch /var/www/wordpress/wp-content/y

if [ "${VERBOSE:-}" = 1 ]; then
	docker logs --tail 20 "$run_id-php" >&2 || true
	docker logs --tail 20 "$run_id-nginx" >&2 || true
fi

# --- The control plane image as a task image ---------------------------------
if [ -n "${CONTROL_PLANE_IMAGE:-}" ]; then
	echo "==> restricted task pod: $CONTROL_PLANE_IMAGE"
	task_sh() {
		docker run --rm "${restricted[@]}" --tmpfs /tmp --env HOME=/tmp \
			--entrypoint sh "$CONTROL_PLANE_IMAGE" -c "$1"
	}
	check_eq "task runs as the site UID" "$uid" "$(task_sh 'id -u')"
	check "restic is on PATH" task_sh 'restic version'
	check "mariadb-dump is on PATH" task_sh 'mariadb-dump --version'
	check "the mariadb client is on PATH" task_sh 'mariadb --version'
	check "a restic repository works on a read-only root" task_sh \
		'export RESTIC_PASSWORD=x; restic init -r /tmp/repo && echo data > /tmp/f && restic backup -r /tmp/repo /tmp/f && restic check -r /tmp/repo'
	check "the SFTP worker ends with its input" task_sh \
		'mkdir /tmp/root && pressfleet sftp-server --root /tmp/root </dev/null'
	check "no Docker socket in the task" task_sh '! [ -e /var/run/docker.sock ]'
fi

if [ "$failures" -gt 0 ]; then
	echo "$failures check(s) failed" >&2
	exit 1
fi
echo "all checks passed"
