# syntax=docker/dockerfile:1
#
# site-php: PHP-FPM + WordPress core for PressFleet site stacks.
# Build context is images/ (shared files live in images/shared):
#
#   docker build -f images/site-php/Dockerfile \
#     --build-arg PHP_VERSION=8.4 --build-arg WP_VERSION=7.1.2 \
#     -t ghcr.io/rchouinard/pressfleet/site-php:php8.4-wp7.1.2 images
#
# The runtime contract (paths, env vars, ports, UID) is in images/README.md.

ARG PHP_VERSION
ARG DEBIAN_SUITE=trixie

# ---------------------------------------------------------------------------
# Download tools for the WordPress, plugin, and PECL stages.
#
# Every RUN that downloads sources shared/extra-ca.sh first, which trusts the
# optional extra_ca build secret for that RUN only (builds behind a
# TLS-intercepting proxy; docs/testing.md). Without the secret it does
# nothing.
FROM debian:${DEBIAN_SUITE}-slim AS fetch
RUN --mount=type=secret,id=extra_ca,required=false \
	--mount=type=bind,source=shared/extra-ca.sh,target=/run/extra-ca.sh \
	. /run/extra-ca.sh \
	&& apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates curl jq \
	&& rm -rf /var/lib/apt/lists/*

# ---------------------------------------------------------------------------
# Default plugins (shared/plugins.json), verified against their pinned
# SHA-256. Provisioning installs them into wp-content from the image, so it
# needs no network access (images/README.md, "Default plugins").
FROM fetch AS plugins
COPY shared/fetch-plugins.sh /usr/local/bin/fetch-plugins
COPY shared/plugins.json /tmp/plugins.json
RUN --mount=type=secret,id=extra_ca,required=false \
	--mount=type=bind,source=shared/extra-ca.sh,target=/run/extra-ca.sh \
	. /run/extra-ca.sh \
	&& sh /usr/local/bin/fetch-plugins /tmp/plugins.json /out

# ---------------------------------------------------------------------------
# PECL extension tarballs (shared/pecl.json), verified against their pinned
# SHA-256, so the PHP stage builds them without network access.
FROM fetch AS pecl
COPY shared/fetch-pecl.sh /usr/local/bin/fetch-pecl
COPY shared/pecl.json /tmp/pecl.json
RUN --mount=type=secret,id=extra_ca,required=false \
	--mount=type=bind,source=shared/extra-ca.sh,target=/run/extra-ca.sh \
	. /run/extra-ca.sh \
	&& sh /usr/local/bin/fetch-pecl /tmp/pecl.json /out

# ---------------------------------------------------------------------------
# WordPress core download and verification (shared with site-nginx).
FROM fetch AS wordpress
ARG WP_VERSION
ARG WP_SHA1=""
COPY shared/fetch-wordpress.sh /usr/local/bin/fetch-wordpress
RUN --mount=type=secret,id=extra_ca,required=false \
	--mount=type=bind,source=shared/extra-ca.sh,target=/run/extra-ca.sh \
	. /run/extra-ca.sh \
	&& sh /usr/local/bin/fetch-wordpress "$WP_VERSION" "$WP_SHA1" /out

# ---------------------------------------------------------------------------
# WP-CLI download and verification against the pinned SHA-512 from the
# release's published wp-cli-<version>.phar.sha512.
FROM debian:${DEBIAN_SUITE}-slim AS wpcli
RUN --mount=type=secret,id=extra_ca,required=false \
	--mount=type=bind,source=shared/extra-ca.sh,target=/run/extra-ca.sh \
	. /run/extra-ca.sh \
	&& apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates curl \
	&& rm -rf /var/lib/apt/lists/*
ARG WPCLI_VERSION=2.12.0
ARG WPCLI_SHA512=be928f6b8ca1e8dfb9d2f4b75a13aa4aee0896f8a9a0a1c45cd5d2c98605e6172e6d014dda2e27f88c98befc16c040cbb2bd1bfa121510ea5cdf5f6a30fe8832
RUN --mount=type=secret,id=extra_ca,required=false \
	--mount=type=bind,source=shared/extra-ca.sh,target=/run/extra-ca.sh \
	. /run/extra-ca.sh \
	&& curl -fsSL --retry 3 -o /wp \
		"https://github.com/wp-cli/wp-cli/releases/download/v${WPCLI_VERSION}/wp-cli-${WPCLI_VERSION}.phar" \
	&& echo "${WPCLI_SHA512}  /wp" | sha512sum -c - \
	&& chmod 0755 /wp

# ---------------------------------------------------------------------------
FROM php:${PHP_VERSION}-fpm-${DEBIAN_SUITE}

ARG PHP_VERSION
ARG WP_VERSION

LABEL org.opencontainers.image.title="pressfleet site-php" \
	org.opencontainers.image.description="PHP-FPM and WordPress core for PressFleet site stacks" \
	org.opencontainers.image.source="https://github.com/rchouinard/pressfleet" \
	org.opencontainers.image.licenses="MIT" \
	io.pressfleet.php-version="${PHP_VERSION}" \
	io.pressfleet.wordpress-version="${WP_VERSION}"

# Extensions: see site-php/README.md for the list and rationale. Build
# dependencies are purged afterwards; runtime libraries are kept by resolving
# the shared objects the new extensions link against. PECL extensions build
# from the pecl stage's verified tarballs (versions in shared/pecl.json), so
# pecl itself never downloads.
#
# imagick is built from its tarball as a bundled extension rather than with
# `pecl install`: pecl's extraction doesn't keep file times, which leaves the
# shipped *_arginfo.h older than their .stub.php, and `make install` then
# regenerates them with gen_stub.php, which downloads PHP-Parser from GitHub.
# tar keeps the times, and touching the headers makes sure the shipped ones
# are used.
RUN --mount=type=secret,id=extra_ca,required=false \
	--mount=type=bind,source=shared/extra-ca.sh,target=/run/extra-ca.sh \
	--mount=type=bind,from=pecl,source=/out,target=/run/pecl \
	. /run/extra-ca.sh; \
	set -eux; \
	savedAptMark="$(apt-mark showmanual)"; \
	apt-get update; \
	apt-get install -y --no-install-recommends \
		libavif-dev libfreetype-dev libicu-dev libjpeg62-turbo-dev \
		libmagickwand-dev libpng-dev libwebp-dev libzip-dev; \
	docker-php-ext-configure gd --with-avif --with-freetype --with-jpeg --with-webp; \
	docker-php-ext-install -j"$(nproc)" bcmath exif gd intl mysqli zip; \
	# OPcache is compiled into PHP 8.5+; older branches build it as an extension.
	if ! php -m | grep -qi '^zend opcache$'; then docker-php-ext-install opcache; fi; \
	pecl install /run/pecl/igbinary.tgz; \
	docker-php-ext-enable igbinary; \
	pecl install -D 'enable-redis-igbinary="yes" enable-redis-lzf="no" enable-redis-zstd="no" enable-redis-msgpack="no" enable-redis-lz4="no"' /run/pecl/redis.tgz; \
	docker-php-ext-enable redis; \
	docker-php-source extract; \
	mkdir /usr/src/php/ext/imagick; \
	tar -xzf /run/pecl/imagick.tgz -C /usr/src/php/ext/imagick --strip-components 1 --wildcards 'imagick-*/*'; \
	find /usr/src/php/ext/imagick -name '*_arginfo.h' -exec touch '{}' +; \
	docker-php-ext-install imagick; \
	docker-php-source delete; \
	rm -rf /tmp/pear ~/.pearrc; \
	apt-mark auto '.*' > /dev/null; \
	apt-mark manual $savedAptMark > /dev/null; \
	find /usr/local/lib/php/extensions -type f -name '*.so' -exec ldd '{}' ';' \
		| awk '/=>/ { so = $(NF-1); if (index(so, "/usr/local/") == 1) { next }; gsub("^/(usr/)?", "", so); printf "*%s\n", so }' \
		| sort -u | xargs -r dpkg-query --search | cut -d: -f1 | sort -u | xargs -r apt-mark manual; \
	apt-get install -y --no-install-recommends libfcgi-bin; \
	apt-get purge -y --auto-remove -o APT::AutoRemove::RecommendsImportant=false; \
	rm -rf /var/lib/apt/lists/*; \
	php -m

# FPM and PHP configuration. The upstream pool files are removed so the only
# pool is ours (no user/group directives: the container runs as the site UID).
RUN rm -f /usr/local/etc/php-fpm.d/*.conf /usr/local/etc/php-fpm.conf.default \
	&& cp /usr/local/etc/php/php.ini-production /usr/local/etc/php/php.ini
COPY site-php/conf/php-fpm.conf /usr/local/etc/php-fpm.conf
COPY site-php/conf/pool.conf /usr/local/etc/php-fpm.d/site.conf
COPY site-php/conf/pressfleet.ini /usr/local/etc/php/conf.d/zz-pressfleet.ini
COPY site-php/conf/wp-cli.yml /etc/wp-cli/config.yml

COPY --from=wpcli /wp /usr/local/bin/wp
COPY site-php/docker-entrypoint.sh /usr/local/bin/pressfleet-entrypoint
COPY site-php/healthcheck.sh /usr/local/bin/pressfleet-healthcheck

# Core is root-owned and not writable by the site UID. wp-content and the
# FastCGI cache directory are mount points for volumes; they are 1777 so that a
# fresh named volume (which copies the mount point's mode) is writable by
# whatever UID the stack runs as.
COPY --from=wordpress --chown=root:root /out/wordpress /var/www/wordpress
COPY --from=wordpress --chown=root:root /out/wp-content-skel /opt/pressfleet/wp-content-skel
COPY --chown=root:root shared/mu-plugins /opt/pressfleet/mu-plugins
COPY --from=plugins --chown=root:root /out /opt/pressfleet/plugins
RUN set -eux; \
	chmod -R u=rwX,go=rX /var/www/wordpress /opt/pressfleet; \
	chmod 1777 /var/www/wordpress/wp-content; \
	mkdir -p /var/cache/pressfleet/fastcgi; \
	chmod 0755 /var/cache/pressfleet; \
	chmod 1777 /var/cache/pressfleet/fastcgi; \
	chmod 0755 /usr/local/bin/wp /usr/local/bin/pressfleet-entrypoint /usr/local/bin/pressfleet-healthcheck; \
	rm -rf /var/www/html

ENV PHP_MEMORY_LIMIT=256M \
	PHP_UPLOAD_MAX_FILESIZE=64M \
	PHP_POST_MAX_SIZE=64M \
	PHP_MAX_EXECUTION_TIME=60 \
	PHP_MAX_INPUT_VARS=3000 \
	PHP_OPCACHE_MEMORY_CONSUMPTION=64 \
	PHP_OPCACHE_REVALIDATE_FREQ=60 \
	PHP_FPM_MAX_CHILDREN=5 \
	PHP_FPM_PROCESS_IDLE_TIMEOUT=10s \
	PHP_FPM_MAX_REQUESTS=500 \
	PHP_FPM_REQUEST_TERMINATE_TIMEOUT=300s \
	PHP_FPM_SLOWLOG_TIMEOUT=5s \
	PRESSFLEET_SEED_WP_CONTENT=1 \
	WP_CLI_CONFIG_PATH=/etc/wp-cli/config.yml \
	WP_CLI_CACHE_DIR=/tmp/wp-cli/cache \
	WP_CLI_PACKAGES_DIR=/tmp/wp-cli/packages

WORKDIR /var/www/wordpress
# Fallback only: the control plane always sets the site UID with `user:`.
USER 65534:65534
EXPOSE 9000
STOPSIGNAL SIGQUIT

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
	CMD ["/usr/local/bin/pressfleet-healthcheck"]

ENTRYPOINT ["/usr/local/bin/pressfleet-entrypoint"]
CMD ["php-fpm"]
