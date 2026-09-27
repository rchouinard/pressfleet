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
```

## Settings

| Key | Environment variable | Flag | Default | Description |
| --- | --- | --- | --- | --- |
| `data_dir` | `PRESSFLEET_DATA_DIR` | `--data-dir` | `/var/lib/pressfleet` | Control plane state and rendered Compose projects. Must be an absolute path. |
| `listen` | `PRESSFLEET_LISTEN` | `--listen` (`serve` only) | `:8080` | HTTP listen address for the API and dashboard. |
| `log.level` | `PRESSFLEET_LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn`, or `error`. |
| `log.format` | `PRESSFLEET_LOG_FORMAT` | `--log-format` | `text` | `text` or `json`. Logs go to stderr. |
| `dashboard_domain` | `PRESSFLEET_DASHBOARD_DOMAIN` | | (unset) | Domain serving the dashboard and API, e.g. `fleet.agency.example`. |
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
| `platform_project` | `PRESSFLEET_PLATFORM_PROJECT` | | `pressfleet` | Compose project name of the platform services. The control plane uses it to find the platform's MariaDB container (`<project>-mariadb-1`), which it connects to each site's network. The platform Compose project sets it for the control plane; change it only for a platform installed under another project name. |
| `smtp.host` | `PRESSFLEET_SMTP_HOST` | `--smtp-host` (`init` only) | (unset) | SMTP relay every site sends email through (host name or IP address). Unset means sites send no email: the site images have no local mail server. See [Outbound email](#outbound-email). |
| `smtp.port` | `PRESSFLEET_SMTP_PORT` | `--smtp-port` (`init` only) | by encryption | Relay port. Unset (`0`) means 587 for `starttls`, 465 for `tls`, and 25 for `none`. |
| `smtp.encryption` | `PRESSFLEET_SMTP_ENCRYPTION` | `--smtp-encryption` (`init` only) | `starttls` | `starttls` (a plain connection upgraded with STARTTLS, which is required), `tls` (TLS from the start, "SMTPS"), or `none` (no TLS, for a relay on the same host or network). |
| `smtp.username` | `PRESSFLEET_SMTP_USERNAME` | `--smtp-username` (`init` only) | (unset) | Relay login. Leave it and `smtp.password` unset for a relay that doesn't require one. |
| `smtp.password` | `PRESSFLEET_SMTP_PASSWORD` | `--smtp-password-file` (`init` only) | (unset) | Relay password, encrypted with the master key (`pfenc1:...`). `pressfleet init` reads the plaintext from `--smtp-password-file` and writes only the encrypted value; a plaintext value is rejected. The control plane decrypts it at startup and refuses to start if it can't. |
| `smtp.from_email` | `PRESSFLEET_SMTP_FROM_EMAIL` | `--smtp-from-email` (`init` only) | (unset) | Sender address that replaces WordPress's default `wordpress@<site domain>`, which relays usually refuse. It is also the envelope sender of every message. |
| `smtp.from_name` | `PRESSFLEET_SMTP_FROM_NAME` | `--smtp-from-name` (`init` only) | (unset) | Sender name that replaces WordPress's default, "WordPress". |

## Outbound email

Sites send email (password resets, notifications, form submissions) through one SMTP relay configured for the whole platform (design doc §5.6): a transactional email service such as Mailgun, Postmark, or Amazon SES, or your own mail server. `pressfleet init` collects the `smtp.*` settings and passes them to the control plane, which writes them into every site's generated `wp-config.php` as `PRESSFLEET_SMTP_*` constants. The platform must-use plugin `pressfleet-smtp.php` in the site images configures WordPress's mailer from them; without `smtp.host` it does nothing, and sites can't send email.

The relay password is encrypted with the master key everywhere PressFleet stores it: in the config file, in the platform Compose project, and in the data directory's backups. It is decrypted only by the control plane, which renders it into each site's `wp-config.php`, next to the site's database password: that file is readable only by the site's UID and mounted read-only into the site's `php` container (never into `nginx`).

- **Sender.** With `smtp.from_email` set, messages WordPress would send as `wordpress@<site domain>` use it instead, and every message uses it as the envelope sender, so bounces go to the platform's domain. A From address chosen by a plugin (WooCommerce's store address, say) is kept; the relay must accept it.
- **Per-site mail.** The plugin configures the mailer early, so a site that installs its own mail plugin, for example to use a client's own provider, overrides it.
- **Changes.** The settings are read when the control plane starts. Rerun `pressfleet init --force` to change them; a site picks up new settings the next time its `wp-config.php` is rendered.
- **Failures.** A message the relay refuses is logged to the site's `php` container log (`docker logs`), with the relay's error.

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

The platform master key encrypts every secret in the state database: site database and Redis passwords, backup repository keys, and DNS provider and S3 credentials. `pressfleet serve` refuses to start without it.

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
