#!/bin/sh
# Prints the extra `docker build` options, one per line, for builds behind a
# TLS-intercepting proxy (docs/testing.md, "Building behind a
# TLS-intercepting proxy"). The Makefile and images/test.sh add them to every
# build; the e2e harness (compose.BuildImage) applies the same rules. With
# none of the variables below set it prints nothing.
#
# Environment:
#   PRESSFLEET_BUILD_EXTRA_CA  path of a PEM file of extra CA certificates,
#                              passed as the BuildKit secret extra_ca
#                              (images/shared/extra-ca.sh trusts it)
#   PRESSFLEET_BUILD_NETWORK   the builds' network (--network), e.g. host
#   HTTP_PROXY, HTTPS_PROXY, NO_PROXY, ALL_PROXY, FTP_PROXY and their
#   lowercase forms: passed as build args when set. Docker's predefined proxy
#   args reach RUN steps but are not kept in the image or its history. A
#   proxy on a loopback address is skipped unless PRESSFLEET_BUILD_NETWORK is
#   host, as build containers can't reach the host's loopback otherwise.
set -eu

if [ -n "${PRESSFLEET_BUILD_EXTRA_CA:-}" ]; then
	echo "--secret"
	echo "id=extra_ca,src=$PRESSFLEET_BUILD_EXTRA_CA"
fi
if [ -n "${PRESSFLEET_BUILD_NETWORK:-}" ]; then
	echo "--network"
	echo "$PRESSFLEET_BUILD_NETWORK"
fi
proxied=
for v in HTTP_PROXY http_proxy HTTPS_PROXY https_proxy ALL_PROXY all_proxy FTP_PROXY ftp_proxy; do
	eval "value=\${$v:-}"
	[ -n "$value" ] || continue
	if [ "${PRESSFLEET_BUILD_NETWORK:-}" != host ]; then
		case "$value" in
		*://localhost | *://localhost[:/]* | *://127.* | *://\[::1\]* | localhost | localhost[:/]* | 127.* | \[::1\]*) continue ;;
		esac
	fi
	# Without a value, docker takes the build arg from the environment.
	echo "--build-arg"
	echo "$v"
	proxied=1
done
if [ -n "$proxied" ]; then
	for v in NO_PROXY no_proxy; do
		eval "value=\${$v:-}"
		[ -z "$value" ] || printf -- '--build-arg\n%s\n' "$v"
	done
fi
