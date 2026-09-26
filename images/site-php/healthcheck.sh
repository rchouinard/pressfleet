#!/bin/sh
# Container health check: asks PHP-FPM's ping endpoint over FastCGI.
set -eu

response="$(SCRIPT_NAME=/fpm-ping SCRIPT_FILENAME=/fpm-ping REQUEST_METHOD=GET \
	cgi-fcgi -bind -connect 127.0.0.1:9000 2>/dev/null)" || exit 1

case "$response" in
*pong*) exit 0 ;;
*) exit 1 ;;
esac
