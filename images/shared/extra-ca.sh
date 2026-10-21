# shellcheck shell=sh
# Trusts an optional extra CA for the rest of a Dockerfile RUN, for builds
# behind a TLS-intercepting proxy (docs/testing.md, "Building behind a
# TLS-intercepting proxy"). Source it at the start of a RUN that downloads,
# with the CA mounted as the BuildKit secret extra_ca:
#
#   RUN --mount=type=secret,id=extra_ca,required=false \
#       --mount=type=bind,source=shared/extra-ca.sh,target=/run/extra-ca.sh \
#       . /run/extra-ca.sh && curl ...
#
# Without the secret it does nothing, so the build is what it is without
# this file. With it, it writes the system bundle (if any) plus the extra CA
# to a temporary bundle and points the tools at it through their
# environment variables, rather than adding the CA to the system store with
# update-ca-certificates: the store is never changed, so no image layer can
# carry the CA. The temporary bundle is removed when the RUN's shell exits.
if [ -s /run/secrets/extra_ca ]; then
	extra_ca_dir="$(mktemp -d)"
	trap 'rm -rf "$extra_ca_dir"' EXIT
	for f in /etc/ssl/certs/ca-certificates.crt /etc/ssl/cert.pem; do
		if [ -s "$f" ]; then
			cat "$f" >"$extra_ca_dir/bundle.pem"
			break
		fi
	done
	cat /run/secrets/extra_ca >>"$extra_ca_dir/bundle.pem"
	# OpenSSL (curl, wget, PHP's openssl, pecl, composer), Go, and git.
	export SSL_CERT_FILE="$extra_ca_dir/bundle.pem"
	export CURL_CA_BUNDLE="$extra_ca_dir/bundle.pem"
	export GIT_SSL_CAINFO="$extra_ca_dir/bundle.pem"
	# Node.js (npm, pnpm) keeps its own roots and adds these.
	export NODE_EXTRA_CA_CERTS=/run/secrets/extra_ca
	# apt ignores SSL_CERT_FILE; this only matters for HTTPS mirrors.
	printf 'Acquire::https::CaInfo "%s";\n' "$extra_ca_dir/bundle.pem" >"$extra_ca_dir/apt.conf"
	export APT_CONFIG="$extra_ca_dir/apt.conf"
	echo "extra-ca: trusting the extra_ca build secret" >&2
fi
