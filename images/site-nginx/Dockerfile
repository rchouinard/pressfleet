# syntax=docker/dockerfile:1
#
# site-nginx: nginx + WordPress core static files for PressFleet site stacks.
# Depends only on the WordPress version. Build context is images/:
#
#   docker build -f images/site-nginx/Dockerfile \
#     --build-arg WP_VERSION=7.1.2 \
#     -t ghcr.io/rchouinard/pressfleet/site-nginx:wp7.1.2 images
#
# The runtime contract (paths, env vars, ports, UID) is in images/README.md.

ARG NGINX_VERSION=1.30

# ---------------------------------------------------------------------------
# WordPress core download and verification (shared with site-php).
FROM debian:trixie-slim AS wordpress
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates curl jq \
	&& rm -rf /var/lib/apt/lists/*
ARG WP_VERSION
ARG WP_SHA1=""
COPY shared/fetch-wordpress.sh /usr/local/bin/fetch-wordpress
RUN sh /usr/local/bin/fetch-wordpress "$WP_VERSION" "$WP_SHA1" /out

# ---------------------------------------------------------------------------
FROM nginx:${NGINX_VERSION}-alpine

ARG WP_VERSION

LABEL org.opencontainers.image.title="pressfleet site-nginx" \
	org.opencontainers.image.description="nginx and WordPress core static files for PressFleet site stacks" \
	org.opencontainers.image.source="https://github.com/rchouinard/pressfleet" \
	org.opencontainers.image.licenses="MIT" \
	io.pressfleet.wordpress-version="${WP_VERSION}"

# The upstream entrypoint scripts write to /etc/nginx and expect root; ours
# renders into /tmp as the site UID instead.
RUN rm -rf /docker-entrypoint.sh /docker-entrypoint.d /etc/nginx/conf.d /usr/share/nginx/html

# nginx.conf.template, snippets/, and the http.d/, server.d/, and php.d/
# include directories.
COPY site-nginx/conf/ /etc/nginx/pressfleet/
COPY site-nginx/docker-entrypoint.sh /usr/local/bin/pressfleet-entrypoint

# The same core tree as site-php (PHP files included, so nginx can check that
# a script exists before passing it to PHP), and the same mu-plugins at the
# same path. Mount points match site-php; see the comment there.
COPY --from=wordpress --chown=root:root /out/wordpress /var/www/wordpress
COPY --chown=root:root shared/mu-plugins /opt/pressfleet/mu-plugins
RUN set -eux; \
	mkdir -p /etc/nginx/pressfleet/http.d /etc/nginx/pressfleet/server.d /etc/nginx/pressfleet/php.d; \
	chmod -R u=rwX,go=rX /etc/nginx/pressfleet /var/www/wordpress /opt/pressfleet; \
	chmod 1777 /var/www/wordpress/wp-content; \
	mkdir -p /var/cache/pressfleet/fastcgi; \
	chmod 0755 /var/cache/pressfleet; \
	chmod 1777 /var/cache/pressfleet/fastcgi; \
	chmod 0755 /usr/local/bin/pressfleet-entrypoint

ENV PHP_FPM_HOST=php \
	PHP_FPM_PORT=9000 \
	NGINX_RESOLVER=127.0.0.11 \
	NGINX_TRUSTED_PROXIES=local \
	NGINX_STATUS_ALLOW="127.0.0.1 ::1" \
	NGINX_XMLRPC=deny \
	NGINX_CLIENT_MAX_BODY_SIZE=64m \
	NGINX_FASTCGI_READ_TIMEOUT=300s \
	NGINX_WORKER_PROCESSES=1 \
	NGINX_WORKER_CONNECTIONS=512 \
	NGINX_FASTCGI_CACHE=on \
	NGINX_FASTCGI_CACHE_VALID=1h \
	NGINX_FASTCGI_CACHE_MAX_SIZE=256m \
	NGINX_LOGIN_RATE_LIMIT=on \
	NGINX_LOGIN_RATE=10r/m \
	NGINX_LOGIN_BURST=10 \
	NGINX_SECURITY_HEADERS=on

# Fallback only: the control plane always sets the site UID with `user:`.
USER 65534:65534
EXPOSE 8080 8081
STOPSIGNAL SIGQUIT

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
	CMD ["wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8081/healthz"]

ENTRYPOINT ["/usr/local/bin/pressfleet-entrypoint"]
CMD ["nginx", "-e", "stderr", "-c", "/tmp/nginx/nginx.conf", "-g", "daemon off;"]
