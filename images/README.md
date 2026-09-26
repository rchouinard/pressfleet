# Site images

Container images for site stacks (design doc §4.1, §5.5):

| Image | Tag | Contents |
| --- | --- | --- |
| [`site-php`](site-php/README.md) | `ghcr.io/rchouinard/pressfleet/site-php:php<PHP>-wp<WP>` (e.g. `php8.4-wp7.1.2`) | PHP-FPM, WordPress core, WP-CLI, platform mu-plugins |
| [`site-nginx`](site-nginx/README.md) | `ghcr.io/rchouinard/pressfleet/site-nginx:wp<WP>` (e.g. `wp7.1.2`) | nginx with the platform's WordPress config, the same WordPress core, platform mu-plugins |

`<PHP>` is a PHP minor branch (`8.4`), `<WP>` a full WordPress release (`7.1.2`). A site's two containers must always use the same `<WP>`.

## Version manifest

[`manifest.json`](manifest.json) is the single source of truth for which site images exist. The Makefile, [`test.sh`](test.sh), the CI workflows, and the control plane (package `control-plane/internal/catalog`, which embeds it through [`images.go`](images.go)) all read it; shell users go through [`matrix.sh`](matrix.sh).

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

The scheduled [Image updates workflow](../.github/workflows/image-updates.yml) runs [`update-manifest.sh`](update-manifest.sh) daily and opens a pull request when php.net or wordpress.org report changes. Merging it publishes the new images.

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
| `/var/cache/pressfleet/fastcgi` | volume root; image mount point is `root 1777` | **named volume** `cache`, read-write in both | FastCGI page cache (M2, #40). PHP (Nginx Helper, #41) purges it. Unused until M2 but may already be mounted. |
| `/tmp` | writable | tmpfs recommended | PHP upload staging, sessions, WP-CLI cache; nginx rendered config and temp files. Not persistent. |
| `/opt/pressfleet/wp-content-skel` | `root`, read-only | image, php only | The release's bundled `wp-content` (default themes, Akismet, Hello Dolly), used to seed an empty volume. |

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
- **HTTPS behind Traefik needs no wp-config code.** nginx sets the FastCGI `HTTPS=on` parameter (and `REQUEST_SCHEME`, `SERVER_PORT=443`, `HTTP_X_FORWARDED_PROTO`) when a trusted proxy (`NGINX_TRUSTED_PROXIES`) sends `X-Forwarded-Proto: https`, so `is_ssl()` works natively. Untrusted clients cannot spoof it: the raw header never reaches PHP. `REMOTE_ADDR` is already the real client IP.
- The FastCGI cache path for Nginx Helper (M2) is `/var/cache/pressfleet/fastcgi`.
- Do not use `getenv()` for site settings: the FPM pool runs with `clear_env = yes`, so container environment variables are not visible to PHP.

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

Values are validated at startup; an invalid value stops the container with an error instead of producing a broken or injectable config.

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

### Logs

Everything goes to the container's stdout/stderr (§7.4):

- nginx access log: stdout, one JSON object per request with `time`, `remote_addr` (real client IP), `host`, `method`, `uri`, `protocol`, `status`, `body_bytes_sent`, `request_time`, `upstream_response_time`, `upstream_cache_status` (empty until M2), `referer`, `user_agent`. The status port is not logged.
- nginx error log: stderr.
- PHP-FPM log, PHP errors, and the FPM slow log: stderr. There is no FPM access log (nginx logs every request).
