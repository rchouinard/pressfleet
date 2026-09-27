# Site images

Container images for site stacks (design doc §4.1, §5.5):

| Image | Tag | Contents |
| --- | --- | --- |
| [`site-php`](site-php/README.md) | `ghcr.io/rchouinard/pressfleet/site-php:php<PHP>-wp<WP>` (e.g. `php8.4-wp7.1.2`) | PHP-FPM, WordPress core, WP-CLI, platform mu-plugins, default plugin zips |
| [`site-nginx`](site-nginx/README.md) | `ghcr.io/rchouinard/pressfleet/site-nginx:wp<WP>` (e.g. `wp7.1.2`) | nginx with the platform's WordPress config, the same WordPress core, platform mu-plugins |

`<PHP>` is a PHP minor branch (`8.4`), `<WP>` a full WordPress release (`7.1.2`). A site's two containers must always use the same `<WP>`.

## Version manifest

[`manifest.json`](manifest.json) is the single source of truth for which site images exist. (The plugins the php image bundles are pinned separately, in [`shared/plugins.json`](shared/plugins.json): see [Default plugins](#default-plugins).) The Makefile, [`test.sh`](test.sh), the CI workflows, and the control plane (package `control-plane/internal/catalog`, which embeds it through [`images.go`](images.go)) all read it; shell users go through [`matrix.sh`](matrix.sh).

```json
{
  "registry": "ghcr.io/rchouinard/pressfleet",
  "php": [
    { "version": "8.4", "activeSupportEnds": "2026-12-31", "securitySupportEnds": "2028-12-31" }
  ],
  "wordpress": {
    "latest": { "version": "7.1.2", "sha1": "<SHA-1 of wordpress-7.1.2.tar.gz>" },
    "pinned": [ { "version": "7.0.6", "sha1": "..." } ]
  }
}
```

| Field | Meaning |
| --- | --- |
| `registry` | Repository prefix of the site images |
| `php[]` | PHP branches with php.net's support dates (the last day of active and of security support). A branch is built through its `securitySupportEnds` date. Branches past it stay listed, so the control plane can report sites on them as end of life, but are no longer built; remove them by hand once no site uses them. |
| `wordpress.latest` | The newest WordPress release and the SHA-1 of its tarball (`wordpress.org/wordpress-<WP>.tar.gz.sha1`) |
| `wordpress.pinned[]` | Older releases to keep rebuilding for every supported PHP branch because sites are pinned to them (§5.5). Images of versions not listed here stay published but stop receiving rebuilds. |

The build matrix is every WordPress version (latest and pinned) times every PHP branch that is not past end of life, plus one `site-nginx` per WordPress version. Image digests are not in the manifest: the weekly rebuild moves tags to new digests, and the control plane resolves digests from the registry when it checks for image updates (#58).

The scheduled [Image updates workflow](../.github/workflows/image-updates.yml) runs [`update-manifest.sh`](update-manifest.sh) daily and opens a pull request when php.net or wordpress.org report changes, including new releases of the bundled plugins (`shared/plugins.json`). Merging it publishes the new images.

## Publishing

The [Images workflow](../.github/workflows/images.yml) builds every image for linux/amd64. Pull requests build without pushing (site images only when `images/` changes, plus the smoke test for the latest WordPress on the newest and oldest PHP branch). Pushes to `main`, `v*` tags, a weekly schedule (uncached, for base image patches), and manual runs on `main` push to GHCR. The control plane image is tagged `sha-<7-char commit>` from `main` and with the tag name (e.g. `v1.2.3`) from releases, matching the version the binary embeds.

## Building and testing

The build context for both images is this `images/` directory (shared files live in [`shared/`](shared)). The Makefile and `test.sh` need `jq`.

```sh
make images                    # build the whole matrix from manifest.json
make images-test               # build, then smoke test (test.sh) every PHP branch on the latest WordPress
images/test.sh 8.5             # smoke test a single PHP version; SKIP_BUILD=1 reuses existing images
WP_VERSION=7.0.6 images/test.sh  # test a pinned WordPress version from the manifest
images/matrix.sh php           # the PHP branches being built
```

Build args:

| Image | Arg | Required | Meaning |
| --- | --- | --- | --- |
| both | `WP_VERSION` | yes | WordPress release to bake in |
| both | `WP_SHA1` | no | Expected SHA-1 of `wordpress-<WP>.tar.gz`. When empty, it is fetched from wordpress.org. Either way, every extracted file is also checked against the MD5s from the WordPress checksums API. |
| site-php | `PHP_VERSION` | yes | PHP branch; selects the `php:<PHP>-fpm-trixie` base image |
| site-php | `WPCLI_VERSION`, `WPCLI_SHA512` | no | WP-CLI release and the SHA-512 it is verified against (defaults pinned in the Dockerfile) |
| site-php | `IGBINARY_VERSION`, `REDIS_VERSION`, `IMAGICK_VERSION` | no | PECL extension versions (pinned in the Dockerfile) |
| site-nginx | `NGINX_VERSION` | no | `nginx:<version>-alpine` base (default `1.30`, the stable branch) |

## Runtime contract

The control plane (wp-config generation #21, Compose templates #24, provisioning #27) is written against this section. Changing anything here is a breaking change for those components.

### Containers and ports

| Container | Port | Protocol | Purpose |
| --- | --- | --- | --- |
| `php` | 9000 | FastCGI | PHP-FPM. Only nginx connects to it. |
| `nginx` | 8080 | HTTP | The site. Traefik routes here. |
| `nginx` | 8081 | HTTP | Internal status: health, `stub_status`, FPM status. Never routed by Traefik; only addresses in `NGINX_STATUS_ALLOW` may use it. |

nginx finds PHP-FPM at `PHP_FPM_HOST:PHP_FPM_PORT` (default `php:9000`), so the php service must be reachable as `php` on the site network (the Compose service name does this). nginx re-resolves the name through Docker's DNS every 10 s, so it can start before php and follows a recreated php container (for example after a PHP version change) without a restart.

### User

- Both containers run as the site's UID and GID, set by Compose `user: "<uid>:<gid>"`. Any non-root UID works; it does not need to exist in `/etc/passwd`. Using the same UID in both containers is required: nginx must read files PHP writes to `wp-content`, and PHP must delete nginx's FastCGI cache files (§5.6).
- Neither image needs root or any capabilities: `cap_drop: [ALL]` and `security_opt: [no-new-privileges:true]` are supported. The images default to `65534:65534` (nobody) only as a fallback when `user:` is not set.
- The containers support a read-only root filesystem (`read_only: true`) provided `/tmp` is a writable tmpfs (`tmpfs: [/tmp]`). The smoke test runs them this way.

### Filesystem

| Path (both containers unless noted) | Owner / mode | Mount | Notes |
| --- | --- | --- | --- |
| `/var/www/wordpress` | `root`, dirs `0755`, files `0644` | image | WordPress core (`ABSPATH`). Read-only for the site UID. nginx's `root`. |
| `/var/www/wordpress/wp-content` | volume root; image mount point is `root 1777` | **named volume** `wp-content`: read-write in php, read-only (`:ro`) in nginx | The only writable, persistent site data. |
| `/var/www/wordpress/wp-config.php` | any, readable by the site UID (`0644` or `0640` with the site GID) | **bind mount, read-only**, php only | Generated by the control plane (#21). WordPress finds it in `ABSPATH`. nginx deliberately does not have it (requests for it return 404). |
| `/opt/pressfleet/mu-plugins` | `root`, read-only | image | Platform must-use plugins (`WPMU_PLUGIN_DIR`). Same files in both images; nginx serves static assets from it at `/_pressfleet/mu-plugins/`. |
| `/var/cache/pressfleet/fastcgi` | volume root; image mount point is `root 1777` | **named volume** `cache`, read-write in both | FastCGI page cache (#40), written by nginx as the site UID. PHP (Nginx Helper, #41) purges it by deleting files. See [FastCGI page cache](#fastcgi-page-cache). |
| `/tmp` | writable | tmpfs recommended | PHP upload staging, sessions, WP-CLI cache; nginx rendered config and temp files. Not persistent. |
| `/opt/pressfleet/wp-content-skel` | `root`, read-only | image, php only | The release's bundled `wp-content` (default themes, Akismet, Hello Dolly), used to seed an empty volume. |
| `/opt/pressfleet/plugins` | `root`, read-only | image, php only | Release zips of the platform's default plugins (`<slug>.zip`) and the `plugins.json` they were built from. The control plane installs them into `wp-content` at provisioning. See [Default plugins](#default-plugins). |

**wp-content permissions.** Docker gives a new, empty named volume the mode of the image's mount point, so a fresh `wp-content` or `cache` volume is `1777` and writable by whatever UID the stack runs as. The control plane may instead `chown <uid>:<gid>` and `chmod 0755` the volume root before first start; the images work either way.

**wp-content seeding.** When the php container starts and `wp-content` is completely empty, the entrypoint copies `/opt/pressfleet/wp-content-skel` into it so `wp core install` has a theme. A volume with any content (an existing or restored site) is never modified. Set `PRESSFLEET_SEED_WP_CONTENT=0` to disable.

**wp-config.php updates.** A single-file bind mount pins the file's inode. If the control plane rewrites `wp-config.php` by writing a new file and renaming it over the old one, the running container keeps seeing the old content; either write it in place or recreate the php container after a change.

### wp-config.php requirements

The generated `wp-config.php` (#21) must define the following in addition to the usual database settings, table prefix, and salts:

```php
define( 'WPMU_PLUGIN_DIR', '/opt/pressfleet/mu-plugins' );
define( 'WPMU_PLUGIN_URL', '/_pressfleet/mu-plugins' ); // root-relative: stays valid across domain changes
define( 'FS_METHOD', 'direct' );       // core is root-owned, so WordPress's ownership check would otherwise ask for FTP credentials
define( 'WP_AUTO_UPDATE_CORE', false );
define( 'DISABLE_WP_CRON', true );
if ( ! defined( 'ABSPATH' ) ) {
	define( 'ABSPATH', __DIR__ . '/' );  // resolves to /var/www/wordpress/
}
require_once ABSPATH . 'wp-settings.php';
```

- `DB_HOST` is the MariaDB service reachable on the site network (`test.sh` uses `db`).
- **WordPress security set (§6.1), optional.** Each module of the platform must-use plugin runs only when its constant is defined with a true value (see [site-php/README.md](site-php/README.md#security-set-modules)): `PRESSFLEET_BLOCK_USER_ENUMERATION` (`true`), `PRESSFLEET_PASSWORD_POLICY_MIN_LENGTH` (an integer, at least 8: the common-password list holds nothing shorter), `PRESSFLEET_PASSWORD_HASHING` (`'argon2id'`; leave it undefined for WordPress's bcrypt), and `PRESSFLEET_DISABLE_APPLICATION_PASSWORDS` (`true`). `DISALLOW_FILE_EDIT` is WordPress's own.
- **HTTPS behind Traefik needs no wp-config code.** nginx sets the FastCGI `HTTPS=on` parameter (and `REQUEST_SCHEME`, `SERVER_PORT=443`, `HTTP_X_FORWARDED_PROTO`) when a trusted proxy (`NGINX_TRUSTED_PROXIES`) sends `X-Forwarded-Proto: https`, so `is_ssl()` works natively. Untrusted clients cannot spoof it: the raw header never reaches PHP. `REMOTE_ADDR` is already the real client IP.
- The FastCGI cache path for Nginx Helper (#41) is `/var/cache/pressfleet/fastcgi` (`RT_WP_NGINX_HELPER_CACHE_PATH`). See [FastCGI page cache](#fastcgi-page-cache).
- **Object cache** (#39): the Redis Object Cache plugin's `WP_REDIS_*` constants (host, port, `WP_REDIS_PASSWORD` as `array( user, password )`, `WP_REDIS_DATABASE`, `WP_REDIS_PREFIX`, and so on). The control plane renders them from the site's object cache credentials; package `objectcache/redis` documents the values its ACL users need. The image's phpredis is built with igbinary, so `WP_REDIS_IGBINARY` works.
- Do not use `getenv()` for site settings: the FPM pool runs with `clear_env = yes`, so container environment variables are not visible to PHP.
- **Outbound email** (#47) is configured by these optional constants, read by the platform mu-plugin `pressfleet-smtp.php`. Without `PRESSFLEET_SMTP_HOST` the plugin does nothing (and PHP's `mail()` has no mail server to hand messages to).

  | Constant | Type | Meaning |
  | --- | --- | --- |
  | `PRESSFLEET_SMTP_HOST` | string | Relay host |
  | `PRESSFLEET_SMTP_PORT` | int | Relay port |
  | `PRESSFLEET_SMTP_ENCRYPTION` | string | PHPMailer's `SMTPSecure`: `tls` (STARTTLS), `ssl` (TLS from the start), or `''` (no TLS; opportunistic STARTTLS is turned off too) |
  | `PRESSFLEET_SMTP_USERNAME`, `PRESSFLEET_SMTP_PASSWORD` | string | Relay login; an empty user name means no authentication |
  | `PRESSFLEET_SMTP_FROM_EMAIL`, `PRESSFLEET_SMTP_FROM_NAME` | string | Replace WordPress's default sender (`wordpress@<domain>`, "WordPress"); the address is also the envelope sender. Empty keeps the default. |

### Environment variables

All are optional; defaults are shown.

**php container**

| Variable | Default | Effect |
| --- | --- | --- |
| `PHP_FPM_MAX_CHILDREN` | `5` | `pm.max_children`. The control plane derives it from the site's memory limit (roughly `(php memory limit - ~64 MiB) / per-worker memory`). |
| `PHP_FPM_PROCESS_IDLE_TIMEOUT` | `10s` | `pm.process_idle_timeout`: how long an idle worker lives (`pm = ondemand`). |
| `PHP_FPM_MAX_REQUESTS` | `500` | Requests per worker before it is recycled. |
| `PHP_FPM_REQUEST_TERMINATE_TIMEOUT` | `300s` | Hard per-request kill timeout. |
| `PHP_FPM_SLOWLOG_TIMEOUT` | `5s` | Requests slower than this get a stack trace in the slow log (stderr). `0` disables. |
| `PHP_MEMORY_LIMIT` | `256M` | `memory_limit` (FPM and WP-CLI). |
| `PHP_UPLOAD_MAX_FILESIZE` | `64M` | `upload_max_filesize`. Keep ≤ `PHP_POST_MAX_SIZE` ≤ `NGINX_CLIENT_MAX_BODY_SIZE`. |
| `PHP_POST_MAX_SIZE` | `64M` | `post_max_size`. |
| `PHP_MAX_EXECUTION_TIME` | `60` | `max_execution_time` and `max_input_time` (seconds). |
| `PHP_MAX_INPUT_VARS` | `3000` | `max_input_vars`. |
| `PHP_OPCACHE_MEMORY_CONSUMPTION` | `64` | OPcache shared memory in MiB (counts against the container's memory). |
| `PHP_OPCACHE_REVALIDATE_FREQ` | `60` | Seconds between OPcache timestamp checks. |
| `PRESSFLEET_SEED_WP_CONTENT` | `1` | Seed an empty `wp-content` at startup (see above). |

**nginx container**

| Variable | Default | Effect |
| --- | --- | --- |
| `PHP_FPM_HOST` | `php` | Host name of the php container. |
| `PHP_FPM_PORT` | `9000` | PHP-FPM port. |
| `NGINX_RESOLVER` | `127.0.0.11` | DNS server used to resolve `PHP_FPM_HOST` (Docker's embedded DNS). |
| `NGINX_TRUSTED_PROXIES` | `10.0.0.0/8 172.16.0.0/12 192.168.0.0/16` | Space/comma separated IPs/CIDRs whose `X-Forwarded-For` and `X-Forwarded-Proto` are trusted. The control plane should narrow it to the site network's subnet (where Traefik is the only proxy). If a CDN sits in front of Traefik, Traefik must be configured to trust it. |
| `NGINX_STATUS_ALLOW` | `127.0.0.1 ::1` | IPs/CIDRs allowed on the internal status port 8081. Widen it if the metrics collector (M4) scrapes over the network rather than exec. |
| `NGINX_XMLRPC` | `deny` | `deny` returns 403 for `/xmlrpc.php`; `allow` passes it to PHP (needed by e.g. Jetpack and the mobile apps). |
| `NGINX_CLIENT_MAX_BODY_SIZE` | `64m` | Maximum request body (uploads). |
| `NGINX_FASTCGI_READ_TIMEOUT` | `300s` | How long nginx waits for PHP to respond. |
| `NGINX_WORKER_PROCESSES` | `1` | nginx workers; one is plenty per site and keeps memory low. |
| `NGINX_WORKER_CONNECTIONS` | `512` | Connections per worker. |
| `NGINX_FASTCGI_CACHE` | `on` | `on` caches anonymous page views in the FastCGI cache; `off` disables it. The control plane sets it from the site's `EnableCache`. |
| `NGINX_FASTCGI_CACHE_VALID` | `1h` | How long a cached `200` or `301` stays fresh (nginx time, e.g. `10m`). Nginx Helper purges changed pages sooner. |
| `NGINX_FASTCGI_CACHE_MAX_SIZE` | `256m` | Size limit of the cache volume's content; the least recently used pages are removed beyond it. |
| `NGINX_LOGIN_RATE_LIMIT` | `on` | `on` rate-limits `/wp-login.php` per client IP (`limit_req`, keyed on the real client IP after `NGINX_TRUSTED_PROXIES`) and answers `429` beyond the limit; `off` disables it. WordPress security set (§6.1). |
| `NGINX_LOGIN_RATE` | `10r/m` | Sustained rate per client IP, `<n>r/m` or `<n>r/s`. |
| `NGINX_LOGIN_BURST` | `10` | Requests above the rate let through at once (`nodelay`) before `429`. |
| `NGINX_SECURITY_HEADERS` | `on` | `on` adds `Referrer-Policy: strict-origin-when-cross-origin`, `X-Frame-Options: SAMEORIGIN`, and `Permissions-Policy: camera=(), microphone=(), usb=()` to every site response (PHP, FastCGI cache hits, static files, errors), each only if PHP didn't send that header itself; `off` sends none of them. `X-Content-Type-Options: nosniff` is always sent. HSTS is not set here (Traefik adds it on primary domains). |

Values are validated at startup; an invalid value stops the container with an error instead of producing a broken or injectable config.

### FastCGI page cache

nginx caches PHP responses for anonymous visitors (#40, §5.6) when `NGINX_FASTCGI_CACHE=on`. Every PHP response carries `X-Cache` with nginx's cache status (`HIT`, `MISS`, `BYPASS`, `EXPIRED`, `STALE`, `UPDATING`, `REVALIDATED`); `POST` and other methods that are never cached get none.

| Setting | Value |
| --- | --- |
| Path | `/var/cache/pressfleet/fastcgi` in both containers (the `cache` volume) |
| Zone | `keys_zone=pressfleet:10m`, `levels=1:2`, `inactive=1d`, `max_size=$NGINX_FASTCGI_CACHE_MAX_SIZE`, `use_temp_path=off` |
| Key | `$pressfleet_scheme$pressfleet_cache_method$host$request_uri`: the scheme the visitor used (`https` when a trusted proxy says so, although nginx itself only sees HTTP), `GET` (`HEAD` shares the `GET` entry), the host without port, and the path (URLs with a query string are never cached) |
| File | `<path>/<last hex digit of md5(key)>/<the two before it>/md5(key)`, owned by the site UID, so PHP can delete it |
| Cached | `GET` and `HEAD` responses `200` and `301`, for `NGINX_FASTCGI_CACHE_VALID` |
| Not cached | responses with `Set-Cookie`, `Cache-Control: no-cache`/`no-store`/`private`, or a past `Expires` (nginx honors them; WordPress sends no-cache headers on 404s, previews, and logged-in pages, WooCommerce on its cart, checkout, and account pages) |
| Bypassed | methods other than `GET`/`HEAD`; any query string; an `Authorization` header; cookies `wordpress_logged_in_*`, `wordpress_sec_*`, `wordpress_<hash>`, `wp-postpass_*`, `comment_author_*`, `wordpress_no_cache`, `woocommerce_items_in_cart`, `woocommerce_cart_hash`, `wp_woocommerce_session_*`; `/wp-admin/`, top-level `wp-*.php`, `xmlrpc.php`, `index.php`, `/wp-json/`, feeds, sitemaps, `/cart/`, `/checkout/`, `/my-account/`, `/wc-api/` |
| Stale | while a page is being refreshed, and when PHP errors, times out, or returns `500`/`503` |

This matches Nginx Helper's file-based purge (`unlink_files`), which computes `md5(scheme . "GET" . host . path)` of a page's URL under `RT_WP_NGINX_HELPER_CACHE_PATH` with `levels=1:2`, and purges everything by deleting all files below the path. nginx treats a deleted cache file as a miss. The key uses the host the visitor asked for, so a page is cached (and must be purged) separately for each domain that serves it rather than redirecting.

### Default plugins

The php image bundles the platform's default WordPress plugins (design doc §5.1 step 7) as release zips under `/opt/pressfleet/plugins`. [`shared/plugins.json`](shared/plugins.json) pins each one's WordPress.org slug, version, and the SHA-256 of `downloads.wordpress.org/plugin/<slug>.<version>.zip`; the build ([`shared/fetch-plugins.sh`](shared/fetch-plugins.sh)) fails if a download doesn't match. The scheduled [Image updates workflow](../.github/workflows/image-updates.yml) moves the pins to new releases.

| Plugin | Purpose |
| --- | --- |
| [Nginx Helper](https://wordpress.org/plugins/nginx-helper/) (`nginx-helper`) | Purges the FastCGI page cache when content changes (#41): purging on, `cache_method` `enable_fastcgi`, `purge_method` `unlink_files` (in the `rt_wp_nginx_helper_options` option), under `RT_WP_NGINX_HELPER_CACHE_PATH`. See [FastCGI page cache](#fastcgi-page-cache). |
| [Redis Object Cache](https://wordpress.org/plugins/redis-cache/) (`redis-cache`) | Object cache on the platform's shared Valkey server, through its `object-cache.php` drop-in (#39) |

The control plane installs a plugin with `wp plugin install /opt/pressfleet/plugins/<slug>.zip` when the site doesn't have it, activates it, and configures it (package `sites`, `plugins.go`), at provisioning and whenever the control plane starts. Installing from the image needs no network access and gets exactly the version CI tested. After that the plugin is an ordinary plugin in `wp-content`: WordPress updates it like any other, and the control plane never replaces a version the site already has.

Why not load them from the image, like the must-use plugins? The object cache drop-in has to live in `wp-content`, and the plugins would then only change with image rollouts, while their own releases (security fixes included) reach sites through WordPress's updates.

### Health and status endpoints

| Check | How | Healthy when |
| --- | --- | --- |
| php container (image `HEALTHCHECK`) | `/usr/local/bin/pressfleet-healthcheck`: FastCGI request to `127.0.0.1:9000/fpm-ping` via `cgi-fcgi` | response contains `pong` |
| nginx container (image `HEALTHCHECK`) | `GET http://127.0.0.1:8081/healthz` | `200 ok`. Independent of PHP, so a php restart does not mark nginx unhealthy. |
| nginx status | `GET :8081/nginx-status` | `stub_status` output |
| PHP-FPM status | `GET :8081/fpm-status` (add `?json` or `?full`) | FPM pool status, proxied to php (M4 metrics) |
| PHP-FPM ping | `GET :8081/fpm-ping` | `pong`, end-to-end nginx to PHP-FPM |
| WordPress | HTTP through Traefik, plus `wp core is-installed` via WP-CLI | control plane checks (§5.5) |

Both image health checks run every 30 s with a 5 s timeout and 3 retries; Compose may override the interval.

### WP-CLI

`wp` is at `/usr/local/bin/wp` in the php container, and the working directory and WP-CLI config (`WP_CLI_CONFIG_PATH=/etc/wp-cli/config.yml`) point at `/var/www/wordpress`, so `docker exec <php> wp <command>` works without `--path`. Run it as the site UID (the container's default user when started with `user:`). WP-CLI's cache and packages directories are under `/tmp`. `open_basedir` applies only to requests served by FPM, not to WP-CLI.

The Compose driver runs WP-CLI as `timeout --kill-after=10s <n>s /usr/local/bin/wp <args>` (`Provisioner.ExecWPCLI`), because killing a `docker exec` client does not stop the process it started. The image must therefore provide GNU coreutils `timeout` on `PATH`.

### Logs

Everything goes to the container's stdout/stderr (§7.4):

- nginx access log: stdout, one JSON object per request with `time`, `remote_addr` (real client IP), `host`, `method`, `uri`, `protocol`, `status`, `body_bytes_sent`, `request_time`, `upstream_response_time`, `upstream_cache_status` (`HIT`, `MISS`, `BYPASS`, ...; empty for static files and with the cache off), `referer`, `user_agent`. The status port is not logged.
- nginx error log: stderr.
- PHP-FPM log, PHP errors, and the FPM slow log: stderr. There is no FPM access log (nginx logs every request).
