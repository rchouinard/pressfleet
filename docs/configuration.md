# Configuration

The `pressfleet` binary reads its settings from four sources. Later sources override earlier ones:

1. Built-in defaults
2. The config file (YAML)
3. `PRESSFLEET_*` environment variables
4. Command-line flags

`pressfleet version` ignores configuration, so it works even when the config is invalid. `pressfleet init` writes the config file during installation; see [Installation](installation.md).

## Config file

The config file is chosen in this order:

1. The `--config` flag
2. The `PRESSFLEET_CONFIG` environment variable
3. `/etc/pressfleet/pressfleet.yaml`

If the file was chosen with `--config` or `PRESSFLEET_CONFIG`, it must exist. A missing file at the default location is fine: defaults and environment variables apply. Unknown keys are rejected, so a typo produces an error instead of being silently ignored.

```yaml
data_dir: /var/lib/pressfleet
listen: ":8080"
log:
  level: info    # debug, info, warn, error
  format: text   # text, json
dashboard_domain: fleet.agency.example
preview_base_domain: preview.agency.example
public_ip: 203.0.113.7   # optional
database:
  driver: sqlite # sqlite, postgres
  # sqlite_path: /var/lib/pressfleet/state/pressfleet.db
  # postgres_dsn: postgres://pressfleet:secret@db.example:5432/pressfleet
master_key_file: /etc/pressfleet/master.key   # outside data_dir
jobs:
  concurrency: 4
http:
  secure_cookies: auto   # auto, always
  # trusted_proxies: [172.18.0.0/16]   # default: loopback and private networks
mariadb:
  address: mariadb:3306
  admin_user: root
  admin_password_file: /run/secrets/mariadb_root_password
redis:
  address: redis:6379
  admin_user: pressfleet
  admin_password_file: /run/secrets/redis_admin_password
audit:
  retention_days: 365
platform_project: pressfleet
smtp:                    # optional; without a host, sites send no email
  host: smtp.mailgun.org
  port: 587              # default: 587 for starttls, 465 for tls, 25 for none
  encryption: starttls   # starttls, tls, none
  username: postmaster@mg.agency.example
  password: pfenc1:...   # encrypted with the master key by pressfleet init
  from_email: sites@agency.example
  from_name: Agency Sites
wordpress_security:            # install-wide; see "WordPress security set"
  block_user_enumeration: true
  password_policy:
    enabled: true
    min_length: 12
  password_hashing: bcrypt     # bcrypt, argon2id
  login_rate_limit:
    enabled: true
    requests_per_minute: 10
    burst: 10
  disable_file_edit: true
  headers: true
  two_factor: "off"            # off, optional, required_for_admins
  disable_application_passwords: false
acme:
  email: ops@agency.example   # optional
  # ca_server: https://acme-v02.api.letsencrypt.org/directory
  # staging: false
  dns_provider: cloudflare    # optional; see "Preview certificates"
  dns_credentials: pfenc1:... # encrypted with the master key by pressfleet init
  # dns_resolvers: ["1.1.1.1:53"]
  # ca_certificates: /etc/pressfleet/pebble.minica.pem
```

## Settings

| Key | Environment variable | Flag | Default | Description |
| --- | --- | --- | --- | --- |
| `data_dir` | `PRESSFLEET_DATA_DIR` | `--data-dir` | `/var/lib/pressfleet` | Control plane state and rendered Compose projects. Must be an absolute path. |
| `listen` | `PRESSFLEET_LISTEN` | `--listen` (`serve` only) | `:8080` | HTTP listen address for the API and dashboard. |
| `log.level` | `PRESSFLEET_LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn`, or `error`. |
| `log.format` | `PRESSFLEET_LOG_FORMAT` | `--log-format` | `text` | `text` or `json`. Logs go to stderr. |
| `dashboard_domain` | `PRESSFLEET_DASHBOARD_DOMAIN` | | (unset) | Domain serving the dashboard and API, e.g. `fleet.agency.example`. Traefik routes it to the control plane with a Let's Encrypt certificate (see [Edge router and certificates](#edge-router-and-certificates)), and `pressfleet init` passes it to the control plane container. |
| `preview_base_domain` | `PRESSFLEET_PREVIEW_BASE_DOMAIN` | | (unset) | Domain whose wildcard record points at this host; previews use its subdomains. Give the base (`preview.agency.example`), not `*.preview.agency.example`. Each site's preview domain is `<slug>.<preview_base_domain>`, so site creation is refused (`409`) while it is unset. `pressfleet init` passes it to the control plane container. |
| `public_ip` | `PRESSFLEET_PUBLIC_IP` | | (unset) | Optional. This host's public IP address, used only to verify DNS records when the host is behind NAT (see [Preflight checks](#preflight-checks)). |
| `database.driver` | `PRESSFLEET_DATABASE_DRIVER` | | `sqlite` | State database: `sqlite` or `postgres`. See [State database](#state-database). |
| `database.sqlite_path` | `PRESSFLEET_DATABASE_SQLITE_PATH` | | `<data_dir>/state/pressfleet.db` | SQLite database file. Must be an absolute path. |
| `database.postgres_dsn` | `PRESSFLEET_DATABASE_POSTGRES_DSN` | | | PostgreSQL connection string (URL or `key=value`). Required when the driver is `postgres`. Treated as a secret: it is never logged. |
| `master_key_file` | `PRESSFLEET_MASTER_KEY_FILE` | | (unset) | File holding the platform master key. Must be an absolute path outside `data_dir`. See [Master key](#master-key). |
| (environment only) | `PRESSFLEET_MASTER_KEY` | | (unset) | The platform master key itself. Takes precedence over `master_key_file`. It can't be set in the config file. |
| `jobs.concurrency` | `PRESSFLEET_JOBS_CONCURRENCY` | | `4` | How many background jobs (provisioning, backups, WP-Cron runs) run at once, from 1 to 64. Jobs that change the same site always run one at a time. |
| `http.secure_cookies` | `PRESSFLEET_HTTP_SECURE_COOKIES` | | `auto` | `auto` marks the session cookie `Secure` when the request arrived over HTTPS; `always` marks it `Secure` on every response. See [Sessions and HTTPS](#sessions-and-https). |
| `http.trusted_proxies` | `PRESSFLEET_HTTP_TRUSTED_PROXIES` (comma-separated) | | loopback and private networks | IP addresses or CIDR networks whose `X-Forwarded-For` and `X-Forwarded-Proto` headers are believed. |
| `mariadb.address` | `PRESSFLEET_MARIADB_ADDRESS` | | `mariadb:3306` | `host:port` of the shared MariaDB server that holds site databases, as seen from the control plane. See [Site databases](#site-databases). |
| `mariadb.admin_user` | `PRESSFLEET_MARIADB_ADMIN_USER` | | `root` | MariaDB account that creates and drops site databases and users. |
| `mariadb.admin_password_file` | `PRESSFLEET_MARIADB_ADMIN_PASSWORD_FILE` | | `/run/secrets/mariadb_root_password` | File holding the admin password; a trailing newline is ignored. Must be an absolute path. The default is where the platform Compose project mounts the MariaDB root password secret. |
| `audit.retention_days` | `PRESSFLEET_AUDIT_RETENTION_DAYS` | | `365` | How many days audit log events are kept, from 1 to 3660. A daily job deletes older events. See [Audit log](#audit-log). |
| `redis.address` | `PRESSFLEET_REDIS_ADDRESS` | | `redis:6379` | `host:port` of the shared object cache server (Valkey), as seen from the control plane. See [Object cache](#object-cache). |
| `redis.admin_user` | `PRESSFLEET_REDIS_ADMIN_USER` | | `pressfleet` | Object cache ACL user that creates and deletes site users. |
| `redis.admin_password_file` | `PRESSFLEET_REDIS_ADMIN_PASSWORD_FILE` | | `/run/secrets/redis_admin_password` | File holding the admin password; a trailing newline is ignored. Must be an absolute path. The default is where the platform Compose project mounts the password `pressfleet init` generates. |
| `acme.email` | `PRESSFLEET_ACME_EMAIL` | `--acme-email` (`init` only) | (unset) | Optional contact address of the ACME account Traefik registers, for important notices from the certificate authority. |
| `acme.ca_server` | `PRESSFLEET_ACME_CA_SERVER` | `--acme-ca-server` (`init` only) | Let's Encrypt | ACME directory URL (`https://` only) of the certificate authority Traefik gets certificates from. Change it only to test against another CA, such as [Pebble](https://github.com/letsencrypt/pebble). |
| `acme.staging` | `PRESSFLEET_ACME_STAGING` | `--acme-staging` (`init` only) | `false` | Use [Let's Encrypt's staging environment](https://letsencrypt.org/docs/staging-environment/): much higher rate limits, but browsers don't trust its certificates. For trying out an install. Can't be combined with `acme.ca_server`. |
| `acme.ca_certificates` | `PRESSFLEET_ACME_CA_CERTIFICATES` | `--acme-ca-certificates` (`init` only) | (unset) | Absolute path on the host of a PEM file of extra root certificates Traefik trusts when talking to the ACME server, on top of the system's, e.g. Pebble's `pebble.minica.pem`. |
| `acme.dns_provider` | `PRESSFLEET_ACME_DNS_PROVIDER` | `--dns-provider` (`init` only) | (unset) | [lego DNS provider](https://go-acme.github.io/lego/dns/) code, such as `cloudflare`, `route53`, or `digitalocean`. With it, Traefik gets one wildcard certificate for the preview base domain with the DNS-01 challenge; without it, each preview domain gets its own HTTP-01 certificate. Needs `preview_base_domain`. See [Preview certificates](#preview-certificates). |
| `acme.dns_credentials` | `PRESSFLEET_ACME_DNS_CREDENTIALS` | `--dns-credentials-file` (`init` only) | (unset) | The DNS provider's credentials, encrypted with the master key (`pfenc1:...`). `pressfleet init` reads them from `--dns-credentials-file` and writes only the encrypted value; a plaintext value is rejected. The control plane decrypts them at startup and refuses to start if it can't. |
| `acme.dns_resolvers` | `PRESSFLEET_ACME_DNS_RESOLVERS` (comma-separated) | `--acme-dns-resolvers` (`init` only) | the host's resolvers | DNS servers (`host:port`) Traefik asks to check that a DNS-01 challenge record has propagated. Set public resolvers such as `1.1.1.1:53` if the host's resolver answers from a stale cache or a split-horizon zone. |
| `platform_project` | `PRESSFLEET_PLATFORM_PROJECT` | | `pressfleet` | Compose project name of the platform services. The control plane uses it to find the platform's MariaDB and object cache containers (`<project>-mariadb-1`, `<project>-redis-1`), which it connects to each site's network. The platform Compose project sets it for the control plane; change it only for a platform installed under another project name. |
| `smtp.host` | `PRESSFLEET_SMTP_HOST` | `--smtp-host` (`init` only) | (unset) | SMTP relay every site sends email through (host name or IP address). Unset means sites send no email: the site images have no local mail server. See [Outbound email](#outbound-email). |
| `smtp.port` | `PRESSFLEET_SMTP_PORT` | `--smtp-port` (`init` only) | by encryption | Relay port. Unset (`0`) means 587 for `starttls`, 465 for `tls`, and 25 for `none`. |
| `smtp.encryption` | `PRESSFLEET_SMTP_ENCRYPTION` | `--smtp-encryption` (`init` only) | `starttls` | `starttls` (a plain connection upgraded with STARTTLS, which is required), `tls` (TLS from the start, "SMTPS"), or `none` (no TLS, for a relay on the same host or network). |
| `smtp.username` | `PRESSFLEET_SMTP_USERNAME` | `--smtp-username` (`init` only) | (unset) | Relay login. Leave it and `smtp.password` unset for a relay that doesn't require one. |
| `smtp.password` | `PRESSFLEET_SMTP_PASSWORD` | `--smtp-password-file` (`init` only) | (unset) | Relay password, encrypted with the master key (`pfenc1:...`). `pressfleet init` reads the plaintext from `--smtp-password-file` and writes only the encrypted value; a plaintext value is rejected. The control plane decrypts it at startup and refuses to start if it can't. |
| `smtp.from_email` | `PRESSFLEET_SMTP_FROM_EMAIL` | `--smtp-from-email` (`init` only) | (unset) | Sender address that replaces WordPress's default `wordpress@<site domain>`, which relays usually refuse. It is also the envelope sender of every message. |
| `smtp.from_name` | `PRESSFLEET_SMTP_FROM_NAME` | `--smtp-from-name` (`init` only) | (unset) | Sender name that replaces WordPress's default, "WordPress". |
| `wordpress_security.block_user_enumeration` | `PRESSFLEET_WORDPRESS_SECURITY_BLOCK_USER_ENUMERATION` | | `true` | Hide which user names exist from visitors. See [WordPress security set](#wordpress-security-set). |
| `wordpress_security.password_policy.enabled` | `PRESSFLEET_WORDPRESS_SECURITY_PASSWORD_POLICY_ENABLED` | | `true` | Check new WordPress passwords on the server: minimum length and a common-password list. |
| `wordpress_security.password_policy.min_length` | `PRESSFLEET_WORDPRESS_SECURITY_PASSWORD_POLICY_MIN_LENGTH` | | `12` | Minimum WordPress password length, from 8 to 64 characters. |
| `wordpress_security.password_hashing` | `PRESSFLEET_WORDPRESS_SECURITY_PASSWORD_HASHING` | | `bcrypt` | Algorithm for new WordPress password hashes: `bcrypt` (WordPress's default) or `argon2id`. |
| `wordpress_security.login_rate_limit.enabled` | `PRESSFLEET_WORDPRESS_SECURITY_LOGIN_RATE_LIMIT_ENABLED` | | `true` | Rate-limit `wp-login.php` per client IP; excess requests get `429`. |
| `wordpress_security.login_rate_limit.requests_per_minute` | `PRESSFLEET_WORDPRESS_SECURITY_LOGIN_RATE_LIMIT_REQUESTS_PER_MINUTE` | | `10` | Sustained `wp-login.php` requests per minute per client IP, from 1 to 600. |
| `wordpress_security.login_rate_limit.burst` | `PRESSFLEET_WORDPRESS_SECURITY_LOGIN_RATE_LIMIT_BURST` | | `10` | Requests above the rate allowed at once before `429`, from 0 to 100. |
| `wordpress_security.disable_file_edit` | `PRESSFLEET_WORDPRESS_SECURITY_DISABLE_FILE_EDIT` | | `true` | Turn off the theme and plugin file editor (`DISALLOW_FILE_EDIT`). Updates keep working. |
| `wordpress_security.headers` | `PRESSFLEET_WORDPRESS_SECURITY_HEADERS` | | `true` | Send `Referrer-Policy`, `X-Frame-Options`, and `Permissions-Policy` from every site. |
| `wordpress_security.two_factor` | `PRESSFLEET_WORDPRESS_SECURITY_TWO_FACTOR` | | `off` | The Two Factor plugin: `off`, `optional` (installed and activated; users opt in), or `required_for_admins` (also: every administrator must set up a second factor at their next login, including the one provisioning creates, and the plugin can't be deactivated). Reaches existing sites when the control plane restarts. Turning it off later stops requiring it but leaves the plugin installed. |
| `wordpress_security.disable_application_passwords` | `PRESSFLEET_WORDPRESS_SECURITY_DISABLE_APPLICATION_PASSWORDS` | | `false` | Turn off WordPress application passwords. Off by default because integrations rely on them. |

## Outbound email

Sites send email (password resets, notifications, form submissions) through one SMTP relay configured for the whole platform (design doc §5.6): a transactional email service such as Mailgun, Postmark, or Amazon SES, or your own mail server. `pressfleet init` collects the `smtp.*` settings and passes them to the control plane, which writes them into every site's generated `wp-config.php` as `PRESSFLEET_SMTP_*` constants. The platform must-use plugin `pressfleet-smtp.php` in the site images configures WordPress's mailer from them; without `smtp.host` it does nothing, and sites can't send email.

The relay password is encrypted with the master key everywhere PressFleet stores it: in the config file, in the platform Compose project, and in the data directory's backups. It is decrypted only by the control plane, which renders it into each site's `wp-config.php`, next to the site's database password: that file is readable only by the site's UID and mounted read-only into the site's `php` container (never into `nginx`).

- **Sender.** With `smtp.from_email` set, messages WordPress would send as `wordpress@<site domain>` use it instead, and every message uses it as the envelope sender, so bounces go to the platform's domain. A From address chosen by a plugin (WooCommerce's store address, say) is kept; the relay must accept it.
- **Per-site mail.** The plugin configures the mailer early, so a site that installs its own mail plugin, for example to use a client's own provider, overrides it.
- **Changes.** The settings are read when the control plane starts. Rerun `pressfleet init --force` to change them; at startup the control plane re-renders every site (see [WordPress security set](#wordpress-security-set)), so running and stopped sites pick up the new settings.
- **Failures.** A message the relay refuses is logged to the site's `php` container log (`docker logs`), with the relay's error.

## WordPress security set

The `wordpress_security` settings are the platform's WordPress hardening (design doc §6.1). They apply to every site; there are no per-site overrides. The images already make WordPress core and `wp-config.php` read-only, disable core updates, deny `/xmlrpc.php`, and refuse to run PHP from `uploads`; these settings add to that:

- **Block user enumeration** (on): visitors who aren't logged in get a 404 for `?author=N` instead of a redirect to the author's archive, a 401 from `/wp-json/wp/v2/users`, oEmbed data without the author, and no users sitemap. The login form gives one error for unknown users and wrong passwords, and the lost password form answers the same whether or not the account exists.
- **Password policy** (on, 12 characters): new passwords must be at least `min_length` characters long and not among the 100,000 most commonly used passwords. It is checked on the server when a password is set in the profile, by an admin, through the reset form, the REST API, or WP-CLI. Existing passwords keep working until they are changed.
- **Password hashing** (`bcrypt`): `argon2id` hashes new passwords with Argon2id. Each hash then takes about 64 MiB of memory for a moment, which counts against the site's memory. Existing hashes keep working and are rehashed with the configured algorithm at the user's next login, in either direction.
- **Login rate limiting** (on, 10 a minute, burst 10): nginx answers `429` to a client IP that exceeds the rate on `wp-login.php`. Loading the form and submitting it are two requests.
- **Disable the file editor** (on).
- **Security headers** (on): `Referrer-Policy: strict-origin-when-cross-origin`, `X-Frame-Options: SAMEORIGIN`, and `Permissions-Policy: camera=(), microphone=(), usb=()` on every response, unless WordPress or a plugin already sent its own. HSTS on production domains comes with go-live.
- **Two-factor authentication** (off): the WordPress.org Two Factor plugin, installed on new sites when not `off`.
- **Disable application passwords** (off).

**Changing a setting** takes effect when the control plane restarts. At startup it queues a `site.reconcile` job for every site (skipping sites being deleted, and not queuing a second one for a site whose job hasn't run yet). The job re-renders running and stopped sites with the current settings: `wp-config.php` and the site's Compose project are rewritten, and only containers whose configuration changed are recreated. A stopped site stays stopped and picks the change up when it starts. Sites that are failed are left alone and pick up the settings with their next update. If a job gives up, the error is recorded in the site's status message, and the site is marked failed only if its containers are no longer running.

## Edge router and certificates

Traefik is the platform's edge router (design doc §3). It publishes ports 80 and 443 (TCP, and UDP 443 for HTTP/3) on every interface of the host, and redirects every plain-HTTP request to HTTPS permanently, except ACME HTTP-01 challenges. HTTPS serves HTTP/1.1, HTTP/2, and HTTP/3, and advertises HTTP/3 with an `Alt-Svc` header.

The dashboard domain is routed to the control plane. Its certificate comes from Let's Encrypt (or `acme.ca_server`, or Let's Encrypt's staging environment with `acme.staging`) through the HTTP-01 challenge, so the domain must resolve to this host and port 80 must be reachable from the internet. Traefik keeps its ACME account and certificates in the platform's `traefik-acme` volume. A request for any other host name gets `404` and Traefik's self-signed default certificate.

Traefik learns routes from Docker container labels, but never touches the Docker socket: access to it is equivalent to root on the host (design doc §6). A socket proxy ([wollomatic/socket-proxy](https://github.com/wollomatic/socket-proxy)) mounts the socket read-only and forwards only the read-only requests Traefik's Docker provider makes (list and inspect containers, the event stream, the API version), and only from the Traefik container, over a network no other service joins. Everything else, including every write, container logs, and file downloads, is refused. Only the control plane has read-write access to the socket. Traefik only routes containers labelled `pressfleet.platform=<platform_project>`, so it ignores containers of anything else on the host.

Traefik writes JSON access logs to its container log, which Docker rotates like every platform log, without request headers (they carry cookies and credentials). It serves Prometheus metrics at `http://traefik:8082/metrics` on the platform network; the port isn't published on the host.

## Preview certificates

Every site is reachable on `https://<slug>.<preview_base_domain>/` as soon as it is provisioned. Traefik gets certificates for preview domains in one of two ways (design doc §4.1):

- **With a DNS provider** (`acme.dns_provider` and `acme.dns_credentials`, recommended): when Traefik starts, it gets one wildcard certificate for `*.<preview_base_domain>` (and the base domain itself) with the DNS-01 challenge, by creating a TXT record through the provider's API. Every preview domain, including those of sites created later, is then served with that certificate at once; nothing is issued per site, so Let's Encrypt's rate limits don't come into play. Traefik renews it before it expires.
- **Without one**: each preview domain gets its own certificate with the HTTP-01 challenge the first time it is routed. That takes a few seconds per site, and is subject to Let's Encrypt's [rate limits](https://letsencrypt.org/docs/rate-limits/) (50 certificates per registered domain per week), which a busy agency can reach.

`pressfleet init` asks for the provider and a credentials file. The file holds one `VARIABLE=value` line for each environment variable the provider reads, as listed on the provider's page in the [lego documentation](https://go-acme.github.io/lego/dns/); blank lines and `#` comments are ignored. For Cloudflare, with an API token that can edit the zone's DNS:

```
CF_DNS_API_TOKEN=...
```

Give the token the narrowest permissions the provider allows: it can change DNS records of the zone.

### How the credentials are kept

The credentials are encrypted with the master key everywhere PressFleet stores them, like the SMTP password: in the config file, in the platform Compose project, and in the data directory's backups. Only the control plane decrypts them, and only to hand them to Traefik:

1. When the control plane starts, it decrypts them and writes one file per variable into a memory-only (tmpfs) Docker volume shared with Traefik, which mounts it read-only. No other service mounts it.
2. Traefik's entrypoint waits for them (for up to 60 seconds, so a reboot that starts Traefik before the control plane doesn't stall site traffic), then starts Traefik with `<VARIABLE>_FILE` pointing at each file. Traefik's DNS client reads the files when it first needs the provider.

So the plaintext is never written to disk, never appears in a container's configuration or environment (`docker inspect` shows only the `_FILE` paths), and disappears when the platform stops. The trade-off: while the platform runs, anyone who can read the volume on the host (root) or get into the control plane or Traefik container can read them, as they could read the master key; and Traefik holds them in memory, where a compromised Traefik could use them.

To change the provider or its credentials, rerun `pressfleet init --force` with `--dns-provider` and `--dns-credentials-file`; init recreates Traefik so it reads the new ones. If the control plane can't decrypt the credentials, it refuses to start, and Traefik starts without them after its wait: sites stay up with the certificates Traefik already has, but it can't get new wildcard certificates.

Testing against [Pebble](https://github.com/letsencrypt/pebble): point `acme.ca_server` at Pebble's directory and `acme.ca_certificates` at its `pebble.minica.pem`, and use a DNS provider that can write to Pebble's DNS server, such as lego's `exec` provider with a script that calls `pebble-challtestsrv`.

## Sessions and HTTPS

The dashboard logs in with a session cookie, `pressfleet_session`. It is always `HttpOnly`, `SameSite=Lax`, and `Path=/`. With `http.secure_cookies: auto` (the default) it is also `Secure` whenever the request arrived over HTTPS: either directly over TLS, or through a trusted proxy that sent `X-Forwarded-Proto: https`. Traefik terminates TLS for the dashboard domain and sends that header, so production cookies are `Secure`, while plain-HTTP development against `http://localhost:8080` still works. (Browsers and `curl` don't send `Secure` cookies over plain HTTP to anything but, in browsers, `localhost`.) Set `always` to require HTTPS regardless of headers.

`X-Forwarded-For` and `X-Forwarded-Proto` are only believed when the connection comes from `http.trusted_proxies`. The default, loopback and private networks, fits the Compose deployment: the control plane's port isn't published on the host, so only containers on its Docker network, Traefik among them, can reach it. The client address taken from `X-Forwarded-For` is used for login throttling and recorded with each session. If you publish the control plane port or put it behind another proxy, list exactly the proxy's addresses.

Sessions end after 12 hours without use, 7 days after login, on logout, and when the user's password is changed or reset or the user is disabled. Failed logins are throttled in memory (5 failures per account and 20 per client address in 15 minutes); restarting the control plane resets the counters.

## Users

Users are managed in the dashboard or the API. To create the first user, or to recover access, run inside the control plane container:

```sh
docker compose exec control-plane pressfleet user create --email admin@example.com [--role admin|operator|viewer]
docker compose exec control-plane pressfleet user reset-password --email admin@example.com
```

Each user has one role (design doc §4.6): `viewer` can see everything except users and the audit log, `operator` can also create and change sites and client groups, and `admin` can also delete sites and client groups, manage users, and change platform settings. Role changes apply at once to the user's existing sessions and API tokens, and a `read-only` API token can only read, whatever its owner's role. The last enabled admin can't be demoted, disabled, or deleted.

The default role is `admin`. Both commands generate a random password and print it once. To choose the password instead, pipe it in with `--password-stdin` (and `docker compose exec -T`). Passwords must be 12 to 256 characters. `reset-password` signs the user out everywhere and revokes their API tokens.

## Audit log

The control plane records every authenticated request that changes state, every request refused by authorization, and every login (successful or not) and logout. Each event holds the user and API token that made the request, the action (such as `site.delete` or `user.update`), the target, the result (`success`, `failure`, or `denied`), the client address (from `X-Forwarded-For` when the request came through a trusted proxy), and the time. Request bodies, passwords, and tokens are never recorded. Reads that succeed aren't recorded.

Admins can list and filter the log with `GET /api/v1/audit-events`. A daily job deletes events older than `audit.retention_days`; a changed retention applies from the next run after the control plane restarts. Events outlive the users and tokens they name, so an event can refer to a user who has since been deleted.

## Master key

> **Warning:** The master key is required to decrypt backup repository keys. If the host is lost and the master key was not saved elsewhere, **all backups are unrecoverable**. Store the key somewhere safe (e.g. a password manager), separate from the host and its backups.

The platform master key encrypts every secret in the state database: site database and Redis passwords, backup repository keys, and S3 credentials. It also encrypts the credentials `pressfleet init` writes to the config file: the SMTP password and the DNS provider credentials. `pressfleet serve` refuses to start without it.

`pressfleet init` generates the key during installation. To make one by hand (for development, say), run `pressfleet key generate`. It prints a key like `pfmk1:` followed by 48 characters and doesn't touch the configuration or the state database:

```sh
(umask 077; pressfleet key generate > /etc/pressfleet/master.key)
```

Give the key to the control plane in one of two ways:

1. `PRESSFLEET_MASTER_KEY`: the key itself. If it is set, `master_key_file` is ignored and a warning is logged.
2. `master_key_file` (or `PRESSFLEET_MASTER_KEY_FILE`): a file containing the key. The file must not be inside `data_dir`, because the data directory is backed up, and `serve` refuses to start if it is. If other users can read or write the file, `serve` logs a warning; use `chmod 600`. The platform Compose project mounts the file into the control plane container as a Compose secret.

The first time the control plane starts with a key, it records the key's ID and a check value in the state database. If it later starts with a different key, it refuses to start with `master key does not match this installation`. A mistyped or truncated key is reported as garbled, because the encoded key includes a checksum.

The key never appears in logs. Log lines and errors name it by its key ID, a 16-character fingerprint that doesn't reveal the key. Master key rotation isn't supported yet.

## Preflight checks

`pressfleet doctor` checks the host against the requirements in design doc §4.1 and prints a fix for every problem. `--json` prints the same report as JSON. The exit status is:

| Exit | Meaning |
| --- | --- |
| 0 | Every check passed or was skipped. |
| 1 | At least one required check failed. |
| 2 | No required check failed, but a recommended check failed or some check warned. |

`pressfleet serve` runs the subset of checks that apply to a running control plane (Docker, Compose, cgroup v2, address pools, disk, memory) and refuses to start if a required one fails. Checks that only make sense before installation (ports 80/443, DNS, log rotation in the host's `daemon.json`) run only in `doctor`. On a development machine, `serve --skip-preflight` (or `PRESSFLEET_SKIP_PREFLIGHT=true`, e.g. for a control plane container in CI) starts anyway and logs a warning. The flag takes precedence over the environment variable.

The DNS checks are skipped until `dashboard_domain` and `preview_base_domain` are set. For the wildcard, doctor resolves a random name under the preview base domain. A record that doesn't resolve, or resolves only to loopback, fails. Otherwise:

- With `public_ip` set, the check passes if the record includes that address and fails if it doesn't.
- Without it, the check passes if the record includes an address of one of the host's network interfaces. If not, it warns rather than fails: cloud hosts behind NAT only have private interface addresses, so doctor can't tell a correct record from a wrong one. Set `public_ip` to make the check conclusive.

## Data directory layout

`pressfleet serve` creates this layout on startup. Every directory is mode `0700` because it holds rendered credentials.

```
<data_dir>/
  state/              control plane database (SQLite)
    backups/          state database backups taken before migrations and restores
  platform/           rendered platform services Compose project
  sites/<site-id>/    rendered per-site Compose project and wp-config.php
  tmp/                scratch space, e.g. database dumps during backups
```

Operators may inspect rendered files here, but shouldn't edit them. The control plane regenerates them from its database.

The platform master key is deliberately **not** stored in the data directory. Keep it separate from the host and its backups (see the master key warning in the design doc, §4.1).

## State database

`pressfleet serve` applies pending schema migrations when it starts. Migrations only go forward. If the database was migrated by a newer PressFleet release, `serve` refuses to start and says so; run a release that supports the schema, or restore a backup.

**SQLite** (the default) needs no extra services. Before applying any migration to a database that already holds data, the control plane writes a consistent copy of it to `<data_dir>/state/backups/pressfleet-<timestamp>-schema<NNNN>.db`. Backups are not pruned automatically.

To put a backup back, stop the control plane and run, inside the control plane container:

```sh
pressfleet state restore <backup>
```

`<backup>` is a path, or a file name in the backups directory. The restore refuses backups from a newer schema than the binary supports, so run it with the PressFleet version that made the backup. The database being replaced is saved to the backups directory first (`...-pre-restore.db`), so a restore can be undone the same way.

**PostgreSQL** is supported for larger installs. PressFleet does not back up or restore PostgreSQL itself; that is the operator's responsibility. Take a backup (`pg_dump`, a managed snapshot, or point-in-time recovery) before upgrading PressFleet, because the control plane migrates the schema on startup and only logs a warning. Dumping from inside the control plane would mean shipping PostgreSQL client tools matched to your server's major version, and PostgreSQL operators usually already have better tools for this. `pressfleet state restore` only supports SQLite.

## Site databases

Each site's database lives on the platform's shared MariaDB server. The control plane connects as `mariadb.admin_user` to create a database and a user for each site, both named `pf_<site-id>`. Each site user has a random password, stored encrypted in the state database, and privileges on its own database only. It is also limited to 20 simultaneous connections (`MAX_USER_CONNECTIONS`), so one site can't use up the server's 150 connections. With PHP-FPM's `ondemand` mode, idle sites hold no connections.

The defaults match the platform Compose project, so installs made with `pressfleet init` need no MariaDB settings. The control plane connects only when it needs to, so `serve` starts even while MariaDB is down, and it re-reads the password file for every new connection.

Sites reach MariaDB as `pf-mariadb` on their own network: the control plane connects the platform's MariaDB container (named after `platform_project`) to each site network. Docker drops those connections when it recreates the container, for example during an upgrade, so `serve` reconnects MariaDB to every site's network when it starts. It does so in the background and only logs failures, so a missing MariaDB or Docker doesn't stop the control plane from starting.

## Object cache

Sites share one object cache server, Valkey (Redis-compatible), capped at 256 MiB with least-recently-used eviction across all sites and no persistence. Its default user is disabled. The control plane connects as `redis.admin_user` and gives each site an ACL user `pf_<site-id>` with a random password, stored encrypted in the state database. A site user can only use keys starting with `<site-id>:` in a logical database of its own, and can't run commands that reach the server as a whole or other databases (`KEYS`, `FLUSHALL`, `CONFIG`, `MONITOR`, `CLIENT LIST`, Pub/Sub, functions, ...). It can `SCAN` and `FLUSHDB`, which only see its own database. The design and the reasons for it are in the documentation of package `control-plane/internal/objectcache/redis`.

The server keeps site users only in memory, so the control plane restores them from the state database: when it starts, within about 10 seconds of noticing the server restarted, and every 10 minutes. The same pass removes site users that have no site. Sites created before the object cache existed get their users from the `site.reconcile` job queued for every site at startup (see [WordPress security set](#wordpress-security-set)). Sites reach the server as `pf-redis:6379` on their own network; the control plane connects it to site networks like MariaDB.
