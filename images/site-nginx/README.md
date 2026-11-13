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
- **Config rendering.** At start, `pressfleet-entrypoint` validates the environment, renders [conf/nginx.conf.template](conf/nginx.conf.template) with `envsubst` (only the listed variables, so nginx's own `$variables` are untouched), and generates the list-valued and optional includes (trusted proxies, status allow-list, xmlrpc rule, login rate limit, security headers) in `/tmp/nginx`. Then it execs `nginx -c /tmp/nginx/nginx.conf`.

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
- Real client IP from `X-Forwarded-For` (recursive) and scheme from `X-Forwarded-Proto`, trusted only from `NGINX_TRUSTED_PROXIES` (by default `local`: the container's own network, where Traefik is the only proxy; on Kubernetes the driver sets the pod network's ranges, see [images/README.md](../README.md#kubernetes)). PHP gets `HTTPS=on` for proxied HTTPS requests; client-supplied `X-Forwarded-Proto` never reaches PHP.
- `absolute_redirect off`, so nginx's own redirects never leak port 8080.
- `X-Content-Type-Options: nosniff`, gzip for text types, `server_tokens off`, `HTTP_PROXY` cleared (httpoxy).
- WordPress security set (§6.1): `/wp-login.php` has its own location with a per-client-IP `limit_req` (`NGINX_LOGIN_RATE_LIMIT`, `429` beyond the limit), and `/xmlrpc.php` shares that limit when `NGINX_XMLRPC=allow` (it checks passwords too), and `Referrer-Policy`, `X-Frame-Options`, and `Permissions-Policy` are added to every response (`NGINX_SECURITY_HEADERS`) unless PHP sent its own. `add_header_inherit merge` (nginx 1.29.3+) is set in `http {}`, so a location that adds a header of its own (such as a cache status in `php.d`) keeps the server-level headers instead of silently replacing them.

## Internal status port (8081)

Restricted to `NGINX_STATUS_ALLOW` (default loopback only) and not logged:

| Path | Response |
| --- | --- |
| `/healthz` | `ok` (image health check; does not depend on PHP) |
| `/nginx-status` | `stub_status` |
| `/fpm-status` | PHP-FPM pool status (`?json`, `?full`) |
| `/fpm-ping` | `pong` from PHP-FPM |

## FastCGI page cache

Anonymous page views are cached in `/var/cache/pressfleet/fastcgi` (the site's `cache` volume); the rules, cache key, and file layout are in the [runtime contract](../README.md#fastcgi-page-cache). The configuration is split across the config's include directories so other features can add files next to it:

| File | Included in | Content |
| --- | --- | --- |
| [`conf/http.d/fastcgi-cache.conf`](conf/http.d/fastcgi-cache.conf) | `http {}` | bypass `map`s, the cache key's method; includes the generated zone |
| [`conf/server.d/fastcgi-cache.conf`](conf/server.d/fastcgi-cache.conf) | site `server {}` | `X-Cache` header (at server level, so locations keep inheriting the server's other headers) |
| [`conf/php.d/fastcgi-cache.conf`](conf/php.d/fastcgi-cache.conf) | the PHP location, `/wp-login.php` (and `/xmlrpc.php` when allowed) | includes the generated cache directives |
| [`conf/snippets/fastcgi-cache.conf`](conf/snippets/fastcgi-cache.conf) | the generated file, when caching is on | `fastcgi_cache`, key, bypass, locking, stale use |

The entrypoint writes `/tmp/nginx/fastcgi-cache-http.conf` (the `fastcgi_cache_path` zone) and `/tmp/nginx/fastcgi-cache-php.conf` (the snippet and `fastcgi_cache_valid`) when `NGINX_FASTCGI_CACHE=on`, and empty files when it is `off`.
