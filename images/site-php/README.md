# site-php

PHP-FPM with WordPress core, WP-CLI, and the platform must-use plugins. Published as `ghcr.io/rchouinard/pressfleet/site-php:php<PHP>-wp<WP>` for every supported PHP branch. The runtime contract (paths, environment variables, ports, UID, health checks) is in [../README.md](../README.md#runtime-contract).

```sh
docker build -f images/site-php/Dockerfile \
  --build-arg PHP_VERSION=8.4 --build-arg WP_VERSION=7.1.2 \
  -t ghcr.io/rchouinard/pressfleet/site-php:php8.4-wp7.1.2 images
```

## Base and PHP versions

- Base: the official `php:<PHP>-fpm-trixie` image (Debian 13).
- Supported branches (php.net, checked 2026-09-26): **8.2** (security fixes until 2026-12-31), **8.3** (security, until 2027-12-31), **8.4** and **8.5** (active). The list and dates live in [../manifest.json](../manifest.json).

## PHP extensions

The set is fixed and identical for every PHP version. It follows the WordPress hosting team's [recommended extensions](https://make.wordpress.org/hosting/handbook/server-environment/#php-extensions).

Added by this image:

| Extension | Why |
| --- | --- |
| `mysqli` | Required: WordPress's database driver |
| `gd` (FreeType, JPEG, PNG, WebP, AVIF) | Image resizing and editing fallback; WebP/AVIF uploads (WordPress 6.5+) |
| `imagick` 3.8.1 | WordPress's preferred image editor (ImageMagick 7) |
| `intl` | IDN, locale-aware formatting; recommended |
| `zip` | Plugin/theme uploads and updates without `unzip` |
| `exif` | Photo metadata and orientation |
| `bcmath` | Arbitrary-precision math used by common plugins (e.g. WooCommerce) |
| `opcache` | Bytecode cache (built in on PHP 8.5, built as an extension on older branches) |
| `redis` 6.3.0 (phpredis, with igbinary serializer support) | Redis Object Cache plugin (M2) |
| `igbinary` 3.2.17RC1 | Compact serializer for the object cache; recommended by the hosting team. 3.2.17RC1 is the first release that builds on PHP 8.5 (PECL marks it stable). |

Already in the official image and relied on: `curl`, `dom`, `fileinfo`, `filter`, `hash`, `iconv`, `json`, `libxml`, `mbstring`, `mysqlnd`, `openssl`, `pcre`, `SimpleXML`, `sodium`, `xml`, `xmlreader`, `xmlwriter`, `zlib` (plus `ctype`, `PDO`, `pdo_sqlite`, `Phar`, `posix`, `readline`, `session`, `sqlite3`, `tokenizer`).

Deliberately not included: `apcu` and `memcached` (the object cache is Redis), `ssh2`/`ftp`/`sockets` (WordPress writes to `wp-content` directly with `FS_METHOD=direct`), and JIT (disabled).

`docker run --rm <image> php -m` prints the exact list.

## Configuration

| File in the image | Source | Purpose |
| --- | --- | --- |
| `/usr/local/etc/php/php.ini` | upstream `php.ini-production` | Base settings |
| `/usr/local/etc/php/conf.d/zz-pressfleet.ini` | [conf/pressfleet.ini](conf/pressfleet.ini) | Limits (from env), temp dirs, OPcache |
| `/usr/local/etc/php-fpm.conf` | [conf/php-fpm.conf](conf/php-fpm.conf) | FPM global settings (log to stderr, foreground) |
| `/usr/local/etc/php-fpm.d/site.conf` | [conf/pool.conf](conf/pool.conf) | The only pool: TCP 9000, `pm = ondemand`, status/ping, slow log, `open_basedir` |
| `/etc/wp-cli/config.yml` | [conf/wp-cli.yml](conf/wp-cli.yml) | WP-CLI defaults (`path`) |

Notes:

- **No `user`/`group` in the pool.** The container is started as the site UID, and the upstream `www.conf`/`docker.conf` pools are removed.
- **`open_basedir`** (FPM only) is `/var/www/wordpress:/opt/pressfleet/mu-plugins:/var/cache/pressfleet/fastcgi:/tmp`. `upload_tmp_dir` and `sys_temp_dir` are `/tmp`. It is not applied to WP-CLI: the platform runs WP-CLI itself, and restricting it would break the phar.
- **OPcache** is tuned for code that does not change under it: `validate_timestamps=1` with a 60 s `revalidate_freq`, 64 MiB, 10,000 files, JIT disabled. Core and mu-plugins never change during a container's life; WordPress invalidates the files it writes during plugin and theme updates. Restores that replace `wp-content` should restart the php container.
- **Logs:** FPM errors, PHP errors (`log_errors`, `display_errors=Off`), and the slow log go to stderr.

## Platform must-use plugin

[../shared/mu-plugins/pressfleet.php](../shared/mu-plugins/pressfleet.php), installed at `/opt/pressfleet/mu-plugins` (shared with site-nginx), defines `PRESSFLEET_PLATFORM` and disables WordPress's core update machinery, since core is managed by the platform (§5.5):

- denies the `update_core` capability to everyone (hides the core section of Dashboard > Updates, update links, and footer nags);
- short-circuits the core update check (`pre_site_transient_update_core`), so WordPress never contacts api.wordpress.org about core;
- removes the core update nag and background core updates, and turns the core version-check cron event into a no-op;
- removes Site Health's background-updates test, which would flag the intentional setup.

Plugin and theme updates are unaffected.

## WP-CLI

WP-CLI 2.12.0 at `/usr/local/bin/wp`, verified against the SHA-512 published with the release.

## Size

The image is about 1 GB uncompressed, most of it the official PHP base image (which keeps its compiler toolchain for `docker-php-ext-install`). The extension layer is about 110 MB and WordPress about 120 MB.
