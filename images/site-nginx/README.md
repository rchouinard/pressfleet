# site-nginx

nginx with the platform's WordPress configuration, WordPress core, and the platform must-use plugins. Published as `ghcr.io/rchouinard/pressfleet/site-nginx:wp<WP>`; it depends only on the WordPress version. The runtime contract (paths, environment variables, ports, UID, health checks) is in [../README.md](../README.md#runtime-contract).

```sh
docker build -f images/site-nginx/Dockerfile \
  --build-arg WP_VERSION=7.1.2 \
  -t ghcr.io/rchouinard/pressfleet/site-nginx:wp7.1.2 images
```

## Base

The official `nginx:1.30-alpine` image (nginx stable). The upstream entrypoint scripts are removed; they write to `/etc/nginx` and assume root.

## How it works

- **Core from its own copy.** The build downloads and verifies the same WordPress release as site-php and installs it at `/var/www/wordpress`, so `wp-admin` and `wp-includes` assets are served without PHP. The PHP files are included too: nginx checks that a script exists before passing it to PHP-FPM, and `SCRIPT_FILENAME` is the same path in both containers.
- **`wp-content`** comes from the shared volume, mounted read-only.
- **Non-root.** The master and workers run as the site UID on unprivileged ports (8080 site, 8081 status). All runtime files (rendered config, pid, temp files) are under `/tmp/nginx`.
- **Config rendering.** At start, `pressfleet-entrypoint` validates the environment, renders [conf/nginx.conf.template](conf/nginx.conf.template) with `envsubst` (only the listed variables, so nginx's own `$variables` are untouched), and generates the list-valued includes (trusted proxies, status allow-list, xmlrpc rule) in `/tmp/nginx`. Then it execs `nginx -c /tmp/nginx/nginx.conf`.

Check the rendered config:

```sh
docker run --rm ghcr.io/rchouinard/pressfleet/site-nginx:wp7.1.2 nginx -e stderr -c /tmp/nginx/nginx.conf -t
```

## Rules

In the site server block (port 8080):

- WordPress permalinks: `try_files $uri $uri/ /index.php?$args`; PHP scripts only if the file exists.
- Static assets (CSS, JS, images, fonts, media, PDF) served from disk with `expires 30d`; XML/TXT/JSON fall through to WordPress because it generates sitemaps and `robots.txt`.
- Denied with 403: hidden files except `/.well-known/`; PHP in `wp-content/{uploads,files,cache,upgrade}`; `wp-admin/includes/`, top-level `wp-includes/*.php`, TinyMCE language PHP, `wp-includes/theme-compat/`; `/xmlrpc.php` unless `NGINX_XMLRPC=allow`; PHP under `/_pressfleet/mu-plugins/`.
- Not found (404): `wp-config.php`, `wp-config-sample.php`, `readme.html`, `license.txt`, and `*.log`, `*.sql`, `*.bak`, `*.old`, `*.orig`, `*.save`, `*.swp`, `*.dist`, `*.ini`, `*.sh`.
- Real client IP from `X-Forwarded-For` (recursive) and scheme from `X-Forwarded-Proto`, trusted only from `NGINX_TRUSTED_PROXIES`. PHP gets `HTTPS=on` for proxied HTTPS requests; client-supplied `X-Forwarded-Proto` never reaches PHP.
- `absolute_redirect off`, so nginx's own redirects never leak port 8080.
- `X-Content-Type-Options: nosniff`, gzip for text types, `server_tokens off`, `HTTP_PROXY` cleared (httpoxy).

## Internal status port (8081)

Restricted to `NGINX_STATUS_ALLOW` (default loopback only) and not logged:

| Path | Response |
| --- | --- |
| `/healthz` | `ok` (image health check; does not depend on PHP) |
| `/nginx-status` | `stub_status` |
| `/fpm-status` | PHP-FPM pool status (`?json`, `?full`) |
| `/fpm-ping` | `pong` from PHP-FPM |

## Extension points for FastCGI caching (M2)

Caching is not implemented here. The config has three empty include directories so #40 can add it without restructuring:

| Directory | Included in | Intended content |
| --- | --- | --- |
| `/etc/nginx/pressfleet/http.d/*.conf` | `http {}` | `fastcgi_cache_path /var/cache/pressfleet/fastcgi ...`, bypass `map`s |
| `/etc/nginx/pressfleet/server.d/*.conf` | site `server {}`, before the locations | cache-bypass variables (cookies, POST, query strings, `wp-admin`, WooCommerce) |
| `/etc/nginx/pressfleet/php.d/*.conf` | the PHP location (and `/xmlrpc.php` when allowed) | `fastcgi_cache`, `fastcgi_cache_bypass`, `fastcgi_no_cache`, `X-Cache` header |

The cache volume mount point `/var/cache/pressfleet/fastcgi` already exists in both images, and the access log already records `$upstream_cache_status`. An `EnableCache` toggle can be implemented in the entrypoint the same way as `NGINX_XMLRPC`.
