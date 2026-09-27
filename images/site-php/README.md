# site-php

PHP-FPM with WordPress core, WP-CLI, the platform must-use plugins, and the default plugins' release zips. Published as `ghcr.io/rchouinard/pressfleet/site-php:php<PHP>-wp<WP>` for every supported PHP branch. The runtime contract (paths, environment variables, ports, UID, health checks) is in [../README.md](../README.md#runtime-contract).

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
- removes the core update nag and background core updates, and stops the version-check cron event from checking core;
- removes Site Health's background-updates test, which would flag the intentional setup;
- keeps plugin and theme auto-updates working: WordPress starts background updates from the version-check cron event it no longer uses for core, so the plugin starts them from that event itself.

Plugin and theme updates are unaffected.

[../shared/mu-plugins/pressfleet-smtp.php](../shared/mu-plugins/pressfleet-smtp.php) sends site email through the platform's SMTP relay (#47, §5.6), configured by the `PRESSFLEET_SMTP_*` constants in `wp-config.php` ([runtime contract](../README.md#wp-configphp-requirements)). It configures PHPMailer on `phpmailer_init` at priority 1, so a site's own mail plugin can still override it; replaces WordPress's default sender with `PRESSFLEET_SMTP_FROM_EMAIL`/`_FROM_NAME`; sets a 15 s SMTP timeout; and logs delivery failures to stderr. Without `PRESSFLEET_SMTP_HOST` it does nothing.
### Security set modules

[../shared/mu-plugins/pressfleet-security.php](../shared/mu-plugins/pressfleet-security.php) is the PHP half of the WordPress security set (§6.1). It loads a module from [../shared/mu-plugins/pressfleet-security/](../shared/mu-plugins/pressfleet-security) only when `wp-config.php` defines the module's constant with a true value; the control plane generates those constants from its install-wide settings. See [../README.md](../README.md#wp-configphp-requirements) for the constants.

| Module | Constant | Effect |
| --- | --- | --- |
| `user-enumeration.php` | `PRESSFLEET_BLOCK_USER_ENUMERATION` | For visitors who aren't logged in: `?author=N` returns 404 instead of redirecting to the author archive; `/wp/v2/users` (any case, also via `?rest_route=` and embeds) returns 401; oEmbed responses drop `author_name` and `author_url`; the users sitemap is removed. Everyone gets one generic login error for unknown users and wrong passwords (an unknown user costs a password hash too, so timing doesn't tell them apart), and the lost password form answers the same whether or not an account exists. |
| `password-policy.php` | `PRESSFLEET_PASSWORD_POLICY_MIN_LENGTH` (integer) | Rejects new passwords shorter than the minimum or on the common-password list (`common-passwords.txt`: the NCSC's 100,000 most used passwords, from Have I Been Pwned via SecLists, of 8 or more characters, compared case-insensitively). Checked on profile save, Users > Add New, the password reset form, the REST API (`400 rest_weak_password`), and WP-CLI (`wp user create`, `wp user update --user_pass`, `wp core install`). Existing passwords keep working. |
| `password-hashing.php` | `PRESSFLEET_PASSWORD_HASHING` (`'argon2id'`) | Hashes new passwords with Argon2id through `wp_hash_password_algorithm` instead of WordPress's bcrypt. Existing hashes still verify and are rehashed at the user's next login. PHP's default Argon2id cost uses about 64 MiB per hash, outside `memory_limit` but inside the container's memory. |
| `application-passwords.php` | `PRESSFLEET_DISABLE_APPLICATION_PASSWORDS` | Disables application passwords (`wp_is_application_passwords_available`). |
| `two-factor.php` | `PRESSFLEET_REQUIRE_TWO_FACTOR` | Requires administrators (users who can `manage_options`) to use the [Two Factor](https://wordpress.org/plugins/two-factor/) plugin, which the control plane installs. The plugin counts as active whenever it is installed (deactivating it has no effect, and its Deactivate link is replaced by a note). An administrator without a second factor is enrolled at their next login: logging in lands on their profile's Two-Factor Options, every other wp-admin screen redirects there with a notice until a provider is set up, and application passwords are unavailable to them meanwhile. AJAX and the REST API are left alone, since setting up a provider uses them. The administrator provisioning creates is covered like any other: its one-time password goes to whoever creates the site, who sets up the second factor at first login. Without the plugin (deleted) nothing is enforced and administrators see a warning; the control plane reinstalls it when it next reconciles the site. |

## Default plugins

The release zips of the platform's default plugins, pinned with their SHA-256 in [../shared/plugins.json](../shared/plugins.json), are verified in a build stage and copied to `/opt/pressfleet/plugins`. The control plane installs them into `wp-content` from there; see [../README.md](../README.md#default-plugins).

## WP-CLI

WP-CLI 2.12.0 at `/usr/local/bin/wp`, verified against the SHA-512 published with the release.

## Size

The image is about 1 GB uncompressed, most of it the official PHP base image (which keeps its compiler toolchain for `docker-php-ext-install`). The extension layer is about 110 MB and WordPress about 120 MB.
