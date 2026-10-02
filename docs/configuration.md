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
metrics:
  retention_days: 90
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
backup:
  backend: local                 # local, s3; see "Backup repositories"
  # s3:
  #   endpoint: https://s3.eu-west-1.amazonaws.com
  #   bucket: agency-backups
  #   prefix: pressfleet         # optional
  #   region: eu-west-1          # optional; default us-east-1
  #   access_key_id: AKIA...
  #   secret_access_key: pfenc1:...   # encrypted with the master key by pressfleet init
  schedule:
    window_start: "02:00"        # UTC; see "Scheduled backups"
    window_hours: 4
  retention:                     # per site; see "Retention and integrity checks"
    keep_last: 3
    keep_daily: 7
    keep_weekly: 4
    keep_monthly: 6
  check:
    interval_days: 7
    read_data_percent: 10
uploads:                         # see "Uploads"
  max_size_mb: 10240
  expiry_hours: 24
alerts:                          # see "Alerts"
  enabled: true
  evaluation_interval_seconds: 30
  webhook_timeout_seconds: 10    # see "Alert channels"
  webhook_allow_local_targets: false
database_export:
  ttl_seconds: 3600              # see "Database exports"
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
| `public_ip` | `PRESSFLEET_PUBLIC_IP` | `--public-ip` (`init` only) | (unset) | Optional. This host's public IP address, where DNS records must point. `pressfleet doctor` checks the dashboard and preview records against it when the host is behind NAT (see [Preflight checks](#preflight-checks)), and the control plane checks custom domains against it, and against the dashboard domain's addresses, before a site goes live (see [DNS check](#dns-check)). `pressfleet init` passes it to the control plane container. |
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
| `metrics.retention_days` | `PRESSFLEET_METRICS_RETENTION_DAYS` | | `90` | How many days VictoriaMetrics keeps metrics, from 1 to 3660. Longer retention needs more disk. It is rendered into the platform project, so a change applies the next time the project is rendered (`pressfleet upgrade`). See [Metrics storage](#metrics-storage). |
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
| `platform_project` | `PRESSFLEET_PLATFORM_PROJECT` | | `pressfleet` | Compose project name of the platform services. The control plane uses it to find the platform's MariaDB, object cache, and Traefik containers (`<project>-mariadb-1`, `<project>-redis-1`, `<project>-traefik-1`), which it connects to each site's network, and labels site containers with it so that this platform's Traefik routes them. The platform Compose project sets it for the control plane; change it only for a platform installed under another project name. |
| `task_image` | `PRESSFLEET_TASK_IMAGE` | | (unset) | Image of the short-lived containers that run backup, restore, and site import tasks against a site's data (`restic`, `mariadb-dump`, `tar`). The platform Compose project sets it to the control plane's own image, which ships those tools; change it only to test another build. Unset means backups, restores, and site imports can't run. |
| `smtp.host` | `PRESSFLEET_SMTP_HOST` | `--smtp-host` (`init` only) | (unset) | SMTP relay every site sends email through (host name or IP address). Unset means sites send no email: the site images have no local mail server. See [Outbound email](#outbound-email). |
| `smtp.port` | `PRESSFLEET_SMTP_PORT` | `--smtp-port` (`init` only) | by encryption | Relay port. Unset (`0`) means 587 for `starttls`, 465 for `tls`, and 25 for `none`. |
| `smtp.encryption` | `PRESSFLEET_SMTP_ENCRYPTION` | `--smtp-encryption` (`init` only) | `starttls` | `starttls` (a plain connection upgraded with STARTTLS, which is required), `tls` (TLS from the start, "SMTPS"), or `none` (no TLS, for a relay on the same host or network). |
| `smtp.username` | `PRESSFLEET_SMTP_USERNAME` | `--smtp-username` (`init` only) | (unset) | Relay login. Leave it and `smtp.password` unset for a relay that doesn't require one. |
| `smtp.password` | `PRESSFLEET_SMTP_PASSWORD` | `--smtp-password-file` (`init` only) | (unset) | Relay password, encrypted with the master key (`pfenc1:...`). `pressfleet init` reads the plaintext from `--smtp-password-file` and writes only the encrypted value; a plaintext value is rejected. The control plane decrypts it at startup and refuses to start if it can't. |
| `smtp.from_email` | `PRESSFLEET_SMTP_FROM_EMAIL` | `--smtp-from-email` (`init` only) | (unset) | Sender address that replaces WordPress's default `wordpress@<site domain>`, which relays usually refuse. It is also the envelope sender of every message. |
| `smtp.from_name` | `PRESSFLEET_SMTP_FROM_NAME` | `--smtp-from-name` (`init` only) | (unset) | Sender name that replaces WordPress's default, "WordPress". |
| `backup.backend` | `PRESSFLEET_BACKUP_BACKEND` | `--backup-backend` (`init` only) | `local` | Where client group backup repositories are created: `local` (under `<data_dir>/backups/repositories/`) or `s3` (an S3-compatible bucket). Existing repositories keep the location they were created with. See [Backup repositories](#backup-repositories). |
| `backup.s3.endpoint` | `PRESSFLEET_BACKUP_S3_ENDPOINT` | `--backup-s3-endpoint` (`init` only) | (unset) | Required with `s3`. Base URL of the S3 service, `https://` or `http://`, without a path: e.g. `https://s3.eu-west-1.amazonaws.com` or `http://minio.internal:9000`. |
| `backup.s3.bucket` | `PRESSFLEET_BACKUP_S3_BUCKET` | `--backup-s3-bucket` (`init` only) | (unset) | Required with `s3`. The bucket; restic creates it on first use if the access key may. |
| `backup.s3.prefix` | `PRESSFLEET_BACKUP_S3_PREFIX` | `--backup-s3-prefix` (`init` only) | (unset) | Optional key prefix inside the bucket, without leading or trailing slashes. Each client group's repository is `<prefix>/<client group ID>`. |
| `backup.s3.region` | `PRESSFLEET_BACKUP_S3_REGION` | `--backup-s3-region` (`init` only) | `us-east-1` | The bucket's region. Set it for Amazon S3 buckets outside `us-east-1`. |
| `backup.s3.access_key_id` | `PRESSFLEET_BACKUP_S3_ACCESS_KEY_ID` | `--backup-s3-access-key-id` (`init` only) | (unset) | Required with `s3`. The access key ID. |
| `backup.s3.secret_access_key` | `PRESSFLEET_BACKUP_S3_SECRET_ACCESS_KEY` | `--backup-s3-secret-access-key-file` (`init` only) | (unset) | Required with `s3`. The secret access key, encrypted with the master key (`pfenc1:...`). `pressfleet init` reads the plaintext from `--backup-s3-secret-access-key-file` and writes only the encrypted value; a plaintext value is rejected. The control plane decrypts it at startup and refuses to start if it can't. |
| `backup.schedule.window_start` | `PRESSFLEET_BACKUP_SCHEDULE_WINDOW_START` | | `02:00` | UTC time of day (`HH:MM`) when the backup window opens. Sites' scheduled backups start within the window unless a site has chosen an hour of its own. See [Scheduled backups](#scheduled-backups). |
| `backup.schedule.window_hours` | `PRESSFLEET_BACKUP_SCHEDULE_WINDOW_HOURS` | | `4` | Length of the backup window in hours, from 1 to 24. A longer window spreads backups out more. |
| `backup.retention.keep_last` | `PRESSFLEET_BACKUP_RETENTION_KEEP_LAST` | | `3` | Each site keeps its newest this many snapshots. From 0 to 10000. See [Retention and integrity checks](#retention-and-integrity-checks). |
| `backup.retention.keep_hourly` | `PRESSFLEET_BACKUP_RETENTION_KEEP_HOURLY` | | `0` | Each site keeps its newest snapshot of each of the last this many hours that have one. |
| `backup.retention.keep_daily` | `PRESSFLEET_BACKUP_RETENTION_KEEP_DAILY` | | `7` | Likewise for days. |
| `backup.retention.keep_weekly` | `PRESSFLEET_BACKUP_RETENTION_KEEP_WEEKLY` | | `4` | Likewise for weeks. |
| `backup.retention.keep_monthly` | `PRESSFLEET_BACKUP_RETENTION_KEEP_MONTHLY` | | `6` | Likewise for months. |
| `backup.retention.keep_yearly` | `PRESSFLEET_BACKUP_RETENTION_KEEP_YEARLY` | | `0` | Likewise for years. With every `keep_*` setting `0`, no snapshot is ever removed. |
| `backup.check.interval_days` | `PRESSFLEET_BACKUP_CHECK_INTERVAL_DAYS` | | `7` | Days between integrity checks of each backup repository, from 1 to 365. |
| `backup.check.read_data_percent` | `PRESSFLEET_BACKUP_CHECK_READ_DATA_PERCENT` | | `10` | Share of a repository's data, from 0 to 100, that each check downloads and verifies, chosen at random each time. `0` checks only the repository's structure. On S3, each check downloads this share of the repository. |
| `uploads.max_size_mb` | `PRESSFLEET_UPLOADS_MAX_SIZE_MB` | | `10240` | Largest file, in MiB, that can be sent through the upload API for a site import or a code deployment, from 1 to 1048576. Uploads are stored under `<data_dir>/uploads/`, so leave room there. See [Uploads](#uploads). |
| `uploads.expiry_hours` | `PRESSFLEET_UPLOADS_EXPIRY_HOURS` | | `24` | How long an upload that no import or deployment has taken may go without a new chunk before it is removed as abandoned, from 1 to 720. |
| `alerts.enabled` | `PRESSFLEET_ALERTS_ENABLED` | | `true` | Whether the control plane evaluates alert rules. The rules and alerts API works either way; with `false`, nothing fires. See [Alerts](#alerts). |
| `alerts.evaluation_interval_seconds` | `PRESSFLEET_ALERTS_EVALUATION_INTERVAL_SECONDS` | | `30` | Seconds between evaluations of every alert rule, from 5 to 900. |
| `alerts.webhook_timeout_seconds` | `PRESSFLEET_ALERTS_WEBHOOK_TIMEOUT_SECONDS` | | `10` | Seconds one delivery to an [alert channel](#alert-channels) may take: a webhook request, or an SMTP conversation. From 1 to 60. |
| `alerts.webhook_allow_local_targets` | `PRESSFLEET_ALERTS_WEBHOOK_ALLOW_LOCAL_TARGETS` | | `false` | Lets alert channels connect to loopback, link-local, and cloud metadata addresses (`127.0.0.1`, `169.254.169.254`, and the like). Off, a webhook URL or per-channel SMTP server that resolves to one is refused. Turn it on to deliver to a service on the host itself. |
| `database_export.ttl_seconds` | `PRESSFLEET_DATABASE_EXPORT_TTL_SECONDS` | | `3600` | How long a database export can be downloaded once it is ready, from 10 seconds to a day (86400); then its file is deleted. See [Database exports](#database-exports). |
| `sftp.port` | `PRESSFLEET_SFTP_PORT` | | `2222` | Host port of the SFTP gateway, which `pressfleet init` publishes and `pressfleet doctor` checks is free. Not 80 or 443. See [SFTP gateway](#sftp-gateway). |
| `sftp.listen` | `PRESSFLEET_SFTP_LISTEN` | | `:2222` | The gateway's listen address inside the control plane container. Empty turns the gateway off. |
| `sftp.host_key_file` | `PRESSFLEET_SFTP_HOST_KEY_FILE` | | `/run/secrets/ssh_host_key` | The gateway's SSH host key. `pressfleet init` generates it once, keeps it in `<data_dir>/platform/secrets/ssh_host_key`, and mounts it here. If the file doesn't exist, the gateway is off and the control plane logs a warning. |
| `sftp.idle_timeout_minutes` | `PRESSFLEET_SFTP_IDLE_TIMEOUT_MINUTES` | | `15` | Ends SFTP connections without traffic for this long, from 1 to 1440 minutes. |
| `sftp.max_session_hours` | `PRESSFLEET_SFTP_MAX_SESSION_HOURS` | | `8` | Ends every SFTP connection this long after it started, from 1 to 24 hours. |
| `updates.window_start` | `PRESSFLEET_UPDATES_WINDOW_START` | | (none) | UTC time of day (`HH:MM`) the maintenance window opens; rollouts only start site updates inside it. Empty: no window. See [WordPress core updates](#wordpress-core-updates). |
| `updates.window_hours` | `PRESSFLEET_UPDATES_WINDOW_HOURS` | | `4` | Length of the maintenance window in hours, 1 to 24. |
| `updates.max_failures` | `PRESSFLEET_UPDATES_MAX_FAILURES` | | `3` | Failed site updates in a row, 1 to 100, after which a rollout halts until it is resumed or new releases arrive. |
| `updates.core.major_auto_approve_days` | `PRESSFLEET_UPDATES_CORE_MAJOR_AUTO_APPROVE_DAYS` | | `0` | Approve a major WordPress release automatically this many days after its images were published (the manifest's `published` date), for sites and groups without a hold or approval of their own. `0`: never. |
| `updates.core.hold_warning_days` | `PRESSFLEET_UPDATES_CORE_HOLD_WARNING_DAYS` | | `90` | Warn (`held_long`) when a site has been held on an older major branch this many days, 1 to 3650. |
| `metrics.enabled` | `PRESSFLEET_METRICS_ENABLED` | | `true` | Run the metrics collector, which writes site and platform metrics to VictoriaMetrics. |
| `metrics.url` | `PRESSFLEET_METRICS_URL` | | `http://victoria-metrics:8428` | VictoriaMetrics' base URL, used for writes and queries. The platform's own service by default. |
| `metrics.interval_seconds` | `PRESSFLEET_METRICS_INTERVAL_SECONDS` | | `30` | How often collectors run by default, from 5 to 3600 seconds. A collector may run less often. |
| `updates.images.check_hours` | `PRESSFLEET_UPDATES_IMAGES_CHECK_HOURS` | | `6` | How often, in hours (0 to 168), the image rollout pulls the site image tags in use from their registry to find new builds. `0`: never; only builds that reach the host otherwise (built locally, pulled by hand) roll out. See [Platform image updates](#platform-image-updates). |
| `image_manifest_file` | `PRESSFLEET_IMAGE_MANIFEST_FILE` | | (built in) | Absolute path of a site image version manifest (`images/manifest.json` format) that replaces the one built into the control plane, read at startup. For rolling out a WordPress release before the PressFleet release that carries it, and for tests. |
| `wordpress_security.block_user_enumeration` | `PRESSFLEET_WORDPRESS_SECURITY_BLOCK_USER_ENUMERATION` | | `true` | Hide which user names exist from visitors. See [WordPress security set](#wordpress-security-set). |
| `wordpress_security.password_policy.enabled` | `PRESSFLEET_WORDPRESS_SECURITY_PASSWORD_POLICY_ENABLED` | | `true` | Check new WordPress passwords on the server: minimum length and a common-password list. |
| `wordpress_security.password_policy.min_length` | `PRESSFLEET_WORDPRESS_SECURITY_PASSWORD_POLICY_MIN_LENGTH` | | `12` | Minimum WordPress password length, from 8 to 64 characters. |
| `wordpress_security.password_hashing` | `PRESSFLEET_WORDPRESS_SECURITY_PASSWORD_HASHING` | | `bcrypt` | Algorithm for new WordPress password hashes: `bcrypt` (WordPress's default) or `argon2id`. |
| `wordpress_security.login_rate_limit.enabled` | `PRESSFLEET_WORDPRESS_SECURITY_LOGIN_RATE_LIMIT_ENABLED` | | `true` | Rate-limit `wp-login.php` per client IP; excess requests get `429`. |
| `wordpress_security.login_rate_limit.requests_per_minute` | `PRESSFLEET_WORDPRESS_SECURITY_LOGIN_RATE_LIMIT_REQUESTS_PER_MINUTE` | | `10` | Sustained `wp-login.php` requests per minute per client IP, from 1 to 600. |
| `wordpress_security.login_rate_limit.burst` | `PRESSFLEET_WORDPRESS_SECURITY_LOGIN_RATE_LIMIT_BURST` | | `10` | Requests above the rate allowed at once before `429`, from 0 to 100. |
| `wordpress_security.disable_file_edit` | `PRESSFLEET_WORDPRESS_SECURITY_DISABLE_FILE_EDIT` | | `true` | Turn off the theme and plugin file editor (`DISALLOW_FILE_EDIT`). Updates keep working. |
| `wordpress_security.headers` | `PRESSFLEET_WORDPRESS_SECURITY_HEADERS` | | `true` | Send `Referrer-Policy`, `X-Frame-Options`, and `Permissions-Policy` from every site, and HSTS from live sites' primary domains and aliases. |
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
- **Security headers** (on): `Referrer-Policy: strict-origin-when-cross-origin`, `X-Frame-Options: SAMEORIGIN`, and `Permissions-Policy: camera=(), microphone=(), usb=()` on every response, unless WordPress or a plugin already sent its own. Once a site is live, its primary domain and aliases also send `Strict-Transport-Security: max-age=31536000`, without `includeSubDomains` (a client's other subdomains may not serve HTTPS) and without `preload`; it comes from the edge router, and preview domains never send it (see [Custom domains](#custom-domains)).
- **Two-factor authentication** (off): the WordPress.org Two Factor plugin, installed on new sites when not `off`.
- **Disable application passwords** (off).

**Changing a setting** takes effect when the control plane restarts. At startup it queues a `site.reconcile` job for every site (skipping sites being deleted, and not queuing a second one for a site whose job hasn't run yet). The job re-renders running and stopped sites with the current settings: `wp-config.php` and the site's Compose project are rewritten, and only containers whose configuration changed are recreated. A stopped site stays stopped and picks the change up when it starts. Sites that are failed are left alone and pick up the settings with their next update. If a job gives up, the error is recorded in the site's status message, and the site is marked failed only if its containers are no longer running.

## Edge router and certificates

Traefik is the platform's edge router (design doc §3). It publishes ports 80 and 443 (TCP, and UDP 443 for HTTP/3) on every interface of the host, and redirects every plain-HTTP request to HTTPS permanently, except ACME HTTP-01 challenges. HTTPS serves HTTP/1.1, HTTP/2, and HTTP/3, and advertises HTTP/3 with an `Alt-Svc` header.

The dashboard domain is routed to the control plane. Its certificate comes from Let's Encrypt (or `acme.ca_server`, or Let's Encrypt's staging environment with `acme.staging`) through the HTTP-01 challenge, so the domain must resolve to this host and port 80 must be reachable from the internet. Traefik keeps its ACME account and certificates in the platform's `traefik-acme` volume. A request for any other host name gets `404` and Traefik's self-signed default certificate.

Traefik learns routes from Docker container labels, but never touches the Docker socket: access to it is equivalent to root on the host (design doc §6). A socket proxy ([wollomatic/socket-proxy](https://github.com/wollomatic/socket-proxy)) mounts the socket read-only and forwards only the read-only requests Traefik's Docker provider makes (list and inspect containers, the event stream, the API version), and only from the Traefik container, over a network no other service joins. Everything else, including every write, container logs, and file downloads, is refused. Only the control plane has read-write access to the socket. Traefik only routes containers labelled `pressfleet.platform=<platform_project>`, so it ignores containers of anything else on the host.

Traefik writes JSON access logs to its container log, which Docker rotates like every platform log, without request headers (they carry cookies and credentials). It serves Prometheus metrics at `http://traefik:8082/metrics` on the platform network; the port isn't published on the host. The control plane's `traefik` collector scrapes it every collection interval and stores each site's traffic as `pressfleet_site_http_requests_total{status_class}` and the `pressfleet_site_http_request_duration_seconds` histogram. Traefik's per-service series map to sites by the service name `pf-<site-id>@docker`, so requests Traefik answers itself (the maintenance page, alias and preview redirects) aren't counted. The address is fixed (`http://traefik:8082/metrics`) and not configurable.

### Site routes

Each site's `nginx` container carries the Traefik labels that route its preview domain to it, over HTTPS only (plain HTTP redirects). The control plane connects Traefik to every site network under the alias `pf-traefik`, like MariaDB and the object cache, and reconnects it at startup, so the platform's Traefik reaches every site's `nginx` and nothing else of it; sites still can't reach each other. Responses on preview domains carry `X-Robots-Tag: noindex`, so search engines keep previews out of their results. Routers and middlewares are named after the site's Compose project (`pf-<site-id>-preview`, `pf-<site-id>-noindex`, ...); the scheme is documented in the `control-plane/internal/ingress` package. A live site's primary domain and aliases are routed differently, from a file; see [Custom domains](#custom-domains).

Traefik tells `nginx` the client's address and scheme in `X-Forwarded-For` and `X-Forwarded-Proto`. It sets both itself rather than passing on what a client sent, and `nginx` believes them only from its own network (`NGINX_TRUSTED_PROXIES=local`, see [images/README.md](../images/README.md)), where Traefik is the only proxy. So WordPress sees HTTPS requests as HTTPS, and the access log and the login rate limit see the client's address rather than Traefik's.

### Preview password protection

A site's preview domain can ask for HTTP basic auth, for sensitive sites in development (design doc §5.1). It is off by default and set per site, by Operators and Admins, with `PATCH /api/v1/sites/{id}` and `{"preview_protection": {"enabled": true}}`; there is no install-wide setting. Production domains are never protected.

- The platform generates the password (26 random characters) the first time protection is turned on and returns it in that response only; `POST /api/v1/sites/{id}/preview-password` replaces it with a new one, returned the same way. The username defaults to the site's slug and can be changed with `preview_protection.username`. Turning protection off keeps the credentials, so turning it back on restores them.
- The password is stored encrypted with the master key. Traefik only gets a bcrypt hash of it, in the site's container labels. The hash uses bcrypt's lowest cost: Traefik checks it on every request, and a random password needs no stretching.
- Traefik checks the credentials and removes the `Authorization` header before the request reaches WordPress, so the header can't be mistaken for an application password.
- The platform's own work doesn't go through the preview domain: WP-Cron and other tasks run WP-CLI in the site's containers, cache purges use the filesystem, and Traefik answers ACME HTTP-01 challenges on port 80 before any router.

### Maintenance mode

`PUT /api/v1/sites/{id}/maintenance` with `{"enabled": true}` (Operators and Admins) puts every domain of a site on the platform's maintenance page: `503 Service Unavailable`, `Retry-After: 120`, `Cache-Control: no-store`, and a small self-contained page. Restores and core rollouts (M3) use the same mechanism. It takes effect a couple of seconds after the job has run, stays on across updates, restarts, and the control plane's startup reconciliation, and is turned off with `{"enabled": false}`. WP-CLI, and so WP-Cron, keeps working.

The page is served by the platform's `maintenance` service: the control plane image running `pressfleet maintenance-page`, with no data directory, secrets, or Docker socket, a read-only filesystem, and a 24 MiB memory limit. For a site in maintenance mode, the control plane writes a file into Traefik's file provider directory, `<data_dir>/platform/traefik/dynamic/`, with a higher-priority router for each of the site's domains that points at that service; it removes the file to end maintenance mode. Because these routes don't come from the site's containers, the page stays up while they are stopped or recreated. Installs rendered before this directory existed need their platform re-rendered with this version (`pressfleet init --force` until `pressfleet upgrade` exists, #95) before maintenance mode works.

### Custom domains

`POST /api/v1/sites/{id}/go-live` with `{"primary_domain": "www.acme.example", "aliases": ["acme.example"]}` (Operators and Admins) switches a running site from its preview domain to its production domain (design doc §5.2). The request is checked (see [DNS check](#dns-check)) and recorded, and a `site.go_live` job then:

1. Routes the primary domain and each alias. Traefik gets a certificate for each domain on its own with the HTTP-01 challenge, from Let's Encrypt (or `acme.ca_server`), the first time it routes it, so port 80 must be reachable from the internet at every address the domain resolves to. One alias whose certificate fails doesn't hold up the others.
2. Rewrites the site's URLs in its database from `//<preview domain>` to `//<primary domain>` (plain and JSON-escaped, so `http://`, `https://`, and URLs in block attributes all move) with `wp search-replace` over every table with the site's prefix, serialized data included, leaving the `guid` column alone as WordPress requires. It then sets `home` and `siteurl` to `https://<primary domain>`.
3. Purges the object cache and the page cache.

From then on the preview domain and the aliases answer `301 Moved Permanently` to `https://<primary domain>` with the same path and query. The preview domain keeps `X-Robots-Tag: noindex` and, if it is protected, asks for its password before it redirects, so the password still hides where the site went. The primary domain and aliases never ask for the preview password and never send `noindex`; with `wordpress_security.headers` on, they send HSTS (see [WordPress security set](#wordpress-security-set)). Plain HTTP on any of them redirects to HTTPS first, like everywhere.

**Aliases** of a live site change on their own with `PUT /api/v1/sites/{id}/aliases` and `{"aliases": ["acme.example", "shop.acme.example"]}` (Operators and Admins; `[]` removes them all). The list replaces the site's aliases. New aliases get the [DNS check](#dns-check) and `skip_dns_check` like go-live; aliases the site already has aren't checked again. A `site.aliases` job reroutes them: each new alias gets its own router, redirect, and certificate, and removed ones stop being served (their certificates stay in Traefik's store until they expire). WordPress and the caches aren't touched, since aliases only redirect, and the site's containers aren't recreated, so it serves throughout; stopped sites can change their aliases too. If the job fails, the previous aliases are put back.

Going live again with another primary domain moves the site: the old primary domain's URLs are rewritten too, and it stops being served unless it is listed among the aliases (list it there to keep old links working). Going back to the preview domain alone isn't supported.

**No downtime.** The custom domains' routes aren't container labels: the control plane writes them to a file in Traefik's file provider directory, `<data_dir>/platform/traefik/dynamic/pf-<site-id>-domains.yaml` (with a copy in the site's directory), which Traefik picks up within a couple of seconds. So going live and changing domains recreate no container. Like maintenance mode, this needs a platform rendered with that directory (`pressfleet init --force` on older installs).

**Failures.** If the job fails before it starts rewriting URLs (for example, the site's database can't be reached), it rolls back: the previous domains and routes are put back, and the site's status message starts with `going live failed and was reverted`. Once the rewrite has started, the job doesn't roll back: undoing `wp search-replace` isn't safe, since it would also rewrite links to the new domain that were in the content before. The site keeps its new domains, which serve it, and its status message says the go-live didn't finish. Going live again with the same domains finishes it; the host names still to be rewritten are kept until a go-live has rewritten them all.

#### DNS check

Before a go-live request changes anything, the control plane checks that each domain the site doesn't have yet resolves to this host: every A and AAAA record of the domain must be one of this host's addresses, and there must be at least one. A single stale record fails the check, because the certificate authority may validate against any of them (Let's Encrypt prefers IPv6). This host's addresses are `public_ip` plus whatever the dashboard domain resolves to, which already has to point here; set `public_ip` if the dashboard domain resolves somewhere else, such as a CDN. With neither, the check can't run.

A domain that fails gets `422 Unprocessable Content` with the problem type `https://github.com/rchouinard/pressfleet/blob/main/docs/configuration.md#dns-check` and one entry per domain in `errors`, saying what it resolves to and which addresses to point it at. Nothing is recorded. Fix the records and send the same request again once they have propagated: the control plane asks its resolver each time, though a resolver that cached the domain's earlier absence may take up to the zone's negative-caching TTL to see a new record.

Hosts behind a CDN or load balancer, whose domains legitimately resolve elsewhere, send `"skip_dns_check": true`. The certificate still needs HTTP-01 requests for the domain to reach this host's port 80.

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

Downloads of database exports are the one kind of read that is recorded, whatever its result, since they hand out a site's data (see [Database exports](#database-exports)).

Admins can list and filter the log with `GET /api/v1/audit-events`. A daily job deletes events older than `audit.retention_days`; a changed retention applies from the next run after the control plane restarts. Events outlive the users and tokens they name, so an event can refer to a user who has since been deleted.

Some events carry `details`, such as the SFTP gateway's sessions (see below).

## Metrics storage

Metrics are stored in a single-node VictoriaMetrics service of the platform project, limited to 256 MiB of memory, with its data on the `victoria-metrics-data` volume. It is reachable only on the platform network, as `http://victoria-metrics:8428`: it publishes no host port and has no Traefik route, so it isn't exposed to the internet. Data older than `metrics.retention_days` is deleted by VictoriaMetrics itself.

## Uptime and TLS checks

The `uptime` collector requests `https://<domain>/` for every running site from the control plane, at `metrics.interval_seconds`, and writes `pressfleet_site_up`, `pressfleet_site_http_check_status_code`, `pressfleet_site_http_check_duration_seconds`, and `pressfleet_site_tls_certificate_expiry_timestamp_seconds{domain}`. The availability metrics come from the site's primary domain, or its preview domain before go-live; the certificate expiry comes from every domain (preview, primary, and aliases). A site is up when it answers with a status below 500 (a site in maintenance mode answers 503 and still counts as up). Redirects aren't followed. Stopped, provisioning, updating, and failed sites aren't checked.

When preview password protection is on, the preview domain's check sends the site's preview credentials (§5.1), so it sees the site and not a 401. No other domain is sent them.

A certificate that fails verification (expired, wrong name, untrusted) makes the check fail: the site reads as down and no expiry is written for that domain.

**Limitation:** the checks run on the PressFleet host itself, so a full host outage can't be detected by them: nothing runs to report it, and the alert engine sees missing data, not `pressfleet_site_up` 0. Use an external uptime monitor for that. External checking is a future consideration (design doc §7.3). The control plane must also be able to reach its own public domains (hairpin routing to the host's public address).

### Host and storage metrics

The control plane collects these itself (no extra service):

- **Host CPU, memory, and disk I/O** come from the host's `/proc/stat`, `/proc/meminfo`, and `/proc/diskstats`. The platform project mounts the host's `/proc` read-only at `/host/proc` in the control plane container, and the control plane reads it there when it exists (otherwise its own `/proc`). Disk I/O counts physical disks only (`sd*`, `vd*`, `xvd*`, `hd*`, `nvme*`, `mmcblk*`, not their partitions, and not device-mapper, md, or loop devices, which would count the same bytes twice). Installs rendered before this mount existed need `pressfleet init --force` (until `pressfleet upgrade` exists, #95); until then the container's own `/proc`, which on a stock Docker host shows the same host-wide counters, is used.
- **Host disk usage** is the filesystem holding `data_dir`, labeled `mountpoint="<data_dir>"`. Docker's own volumes are on a filesystem the control plane can't see; it is the same one on a default install.
- **wp-content size** (`pressfleet_site_wp_content_size_bytes`) is measured every 10 minutes for running and stopped sites by a short-lived task container that runs `du -sk` on the volume, through the provisioner. It is disk usage in whole KiB.
- **Restic repository size** (`pressfleet_restic_repository_size_bytes`, per client group) is measured every 30 minutes with `restic stats --mode raw-data`, for the client groups that have a site and a repository, for both the `local` and `s3` backends.

## Metrics API

The dashboard reads metrics through curated, read-only endpoints (design doc §7.1, §7.2, tag `metrics` in `api/openapi.yaml`); it never sends queries. Every role can use them. They read VictoriaMetrics (`metrics.url`), and answer `503` while it can't be reached or when `metrics.enabled` is false.

| Endpoint | Returns |
| --- | --- |
| `GET /api/v1/metrics/catalog` | Every metric: id, title, unit, kind, group, scope, series labels, and the request limits. |
| `GET /api/v1/sites/{site_id}/metrics` | Time series of a site's metrics, for charts. `404` for an unknown site. |
| `GET /api/v1/sites/{site_id}/metrics/summary` | The current value of a site's metrics (instant queries), cheap enough for the site list. |
| `GET /api/v1/metrics/platform` | Time series of platform metrics and per-client-group metrics; `client_group_id` narrows the latter. |
| `GET /api/v1/metrics/platform/summary` | Current values of platform metrics, for the overview. |

Select metrics with repeatable `metric` (ids from the catalog) and `group` parameters; a range request needs at least one, and a summary without any returns a small default set. A range request takes `start`, `end`, and `step` (seconds): by default the last hour with a step chosen to give about 300 points. `start` and `end` are aligned down to the step, `end` is capped at now, and the response reports what was queried. A request may select at most 20 metrics; ranges are limited to 366 days, steps to at least 15 seconds and at most 1500 points per series. Violations are `400` problems. A metric that has no data yet is returned with an empty `series` (or `values`) list, not an error. Every query of a site endpoint selects on the `site_id` in the path.

Each series has `labels` that tell it apart (such as `status_class`, `domain`, or `percentile`), its `points`, and the `latest` one. Values are in base units: seconds, bytes, and ratios from 0 to 1 (never percentages).

The catalog is defined once, in package `metricsapi`; each entry's PromQL template is parameterized only by the validated site or client group ID and a rate window derived from the step. The stored metrics each entry reads, which the collectors write, are listed in that package's documentation and here:

| Id | Scope | Group | Unit | Kind | Stored metrics |
| --- | --- | --- | --- | --- | --- |
| `uptime` | site | availability | ratio | ratio | `pressfleet_site_up` |
| `http_check_status` | site | availability | code | gauge | `pressfleet_site_http_check_status_code` |
| `response_time` | site | availability | seconds | gauge | `pressfleet_site_http_check_duration_seconds` |
| `request_rate` | site | traffic | per_second | rate | `pressfleet_site_http_requests_total` |
| `status_codes` | site | traffic | per_second | rate | `pressfleet_site_http_requests_total` |
| `latency` | site | traffic | seconds | percentile | `pressfleet_site_http_request_duration_seconds_bucket` |
| `page_cache_ratio` | site | page_cache | ratio | ratio | `pressfleet_site_page_cache_requests_total` |
| `php_workers` | site | php | count | gauge | `pressfleet_site_php_fpm_active_processes`, `pressfleet_site_php_fpm_idle_processes`, `pressfleet_site_php_fpm_max_children` |
| `php_max_children_reached` | site | php | count | rate | `pressfleet_site_php_fpm_max_children_reached_total` |
| `php_slow_requests` | site | php | count | rate | `pressfleet_site_php_fpm_slow_requests_total` |
| `cpu` | site | resources | cores | rate | `pressfleet_container_cpu_usage_seconds_total` |
| `memory` | site | resources | bytes | gauge | `pressfleet_container_memory_usage_bytes` |
| `restarts` | site | resources | count | rate | `pressfleet_container_restarts_total` |
| `wp_content_size` | site | storage | bytes | gauge | `pressfleet_site_wp_content_size_bytes` |
| `database_size` | site | storage | bytes | gauge | `pressfleet_site_database_size_bytes` |
| `object_cache_keys` | site | object_cache | count | gauge | `pressfleet_site_object_cache_keys` |
| `object_cache_memory` | site | object_cache | bytes | gauge | `pressfleet_site_object_cache_memory_bytes` |
| `object_cache_hit_ratio` | site | object_cache | ratio | ratio | `pressfleet_site_object_cache_hits_total`, `pressfleet_site_object_cache_misses_total` |
| `tls_expiry` | site | tls | timestamp | gauge | `pressfleet_site_tls_certificate_expiry_timestamp_seconds` |
| `backup_last_success` | site | backups | timestamp | gauge | `pressfleet_site_backup_last_success_timestamp_seconds` |
| `backup_size` | site | backups | bytes | gauge | `pressfleet_site_backup_size_bytes` |
| `backup_duration` | site | backups | seconds | gauge | `pressfleet_site_backup_duration_seconds` |
| `host_cpu` | platform | host | ratio | ratio | `pressfleet_host_cpu_seconds_total` |
| `host_memory` | platform | host | bytes | gauge | `pressfleet_host_memory_available_bytes`, `pressfleet_host_memory_total_bytes` |
| `host_disk_usage` | platform | host | ratio | ratio | `pressfleet_host_filesystem_avail_bytes`, `pressfleet_host_filesystem_size_bytes` |
| `host_disk_io` | platform | host | bytes_per_second | rate | `pressfleet_host_disk_read_bytes_total`, `pressfleet_host_disk_written_bytes_total` |
| `mariadb_connections` | platform | mariadb | count | gauge | `pressfleet_mariadb_max_connections`, `pressfleet_mariadb_threads_connected` |
| `mariadb_slow_queries` | platform | mariadb | per_second | rate | `pressfleet_mariadb_slow_queries_total` |
| `mariadb_buffer_pool` | platform | mariadb | bytes | gauge | `pressfleet_mariadb_innodb_buffer_pool_size_bytes`, `pressfleet_mariadb_innodb_buffer_pool_used_bytes` |
| `redis_memory` | platform | redis | bytes | gauge | `pressfleet_redis_memory_used_bytes` |
| `redis_hit_ratio` | platform | redis | ratio | ratio | `pressfleet_redis_keyspace_hits_total`, `pressfleet_redis_keyspace_misses_total` |
| `redis_evictions` | platform | redis | per_second | rate | `pressfleet_redis_evicted_keys_total` |
| `job_queue_depth` | platform | jobs | count | gauge | `pressfleet_jobs_queue_depth` |
| `job_failure_rate` | platform | jobs | ratio | ratio | `pressfleet_jobs_finished_total` |
| `restic_repository_size` | client_group | backups | bytes | gauge | `pressfleet_restic_repository_size_bytes` |

Notes on the database and cache metrics (collectors `mariadb` and `redis`, which read the servers through the MariaDB and object cache managers): `pressfleet_site_database_size_bytes` is data plus indexes from `information_schema`. `pressfleet_site_object_cache_keys` is the key count of the site's Redis database, and `pressfleet_site_object_cache_memory_bytes` is an estimate: the average `MEMORY USAGE` of 32 random keys times the key count, because the server has no per-database memory figure. The server counts hits and misses for the whole instance only, so `pressfleet_site_object_cache_hits_total` and `_misses_total` are not collected and `object_cache_hit_ratio` returns no data; `redis_hit_ratio` covers the instance.

## Alerts

Alert rules (design doc §7.3) watch the metrics in VictoriaMetrics and raise alerts. A rule is a PromQL expression plus what makes its result a condition: a **threshold** rule compares the value of each series the expression returns with its `operator` and `threshold`, and an **expression** rule treats every series the expression returns as true (the expression filters, as in `pressfleet_site_up == 0`). A rule's `scope` is `site` (every series carries a `site_id` label, an alert is raised per site, and the rule can be limited to one client group) or `platform`. `for_seconds` is how long the condition must hold before the alert fires, so one failed check doesn't page anyone. `labels` are added to every alert the rule raises, and the `summary` and `description` annotations are [Go templates](https://pkg.go.dev/text/template) over `.Labels`, `.Value`, `.Threshold`, and `.Rule`, with the functions `humanizePercentage` (a ratio as a percentage) and `humanizeDuration` (seconds as days, hours, and minutes).

The control plane evaluates every enabled rule every `alerts.evaluation_interval_seconds` while `alerts.enabled` is true. Each series a condition holds for becomes an alert that is `pending` until the rule's duration has passed, then `firing`, then `resolved` when the condition stops or the series disappears. Each alert emits two events at most, one when it fires and one when it resolves; a condition that keeps holding emits nothing more, and a pending alert that stops holding before it fires is dropped without a trace. Disabling a rule resolves its alerts. If VictoriaMetrics can't be queried, alerts are left exactly as they are: an outage of the metrics store neither fires nor resolves anything. The state is kept in the database, so a restart keeps firing alerts firing without announcing them again, and events that weren't delivered before it are delivered after it. Resolved alerts are kept for 90 days.

Events go to the [alert channels](#alert-channels). Every event is also written to the control plane's log as it is routed.

These default rules are created at the first start with the version that has them, and enabled. They can be edited (thresholds, durations, severity, text) and disabled, but not deleted, and later starts never change them back. The metrics they use, which the collectors emit, are listed in the documentation of package `alerts`.

| Rule | Scope | Condition | For | Severity |
| --- | --- | --- | --- | --- |
| Site down | site | the HTTP check fails | 2 minutes | critical |
| Elevated 5xx error rate | site | more than 5% of responses are 5xx (sites with almost no traffic are ignored) | 5 minutes | warning |
| TLS certificate expiring | site | less than 14 days (1209600 seconds) until a certificate expires | 15 minutes | warning |
| TLS certificate renewal failing | site | less than 3 days (259200 seconds) until a certificate expires, so renewal has been failing for weeks | 15 minutes | critical |
| Backup failed | site | the latest backup failed | none | critical |
| Backup stale | site | no successful backup for 36 hours (129600 seconds) | 15 minutes | warning |
| Disk nearly full | platform | a filesystem is more than 90% full | 10 minutes | warning |
| Container restart loop | platform | a container restarted more than 3 times in 15 minutes | none | critical |
| PHP-FPM worker saturation | site | more than 90% of a site's PHP-FPM workers are busy | 5 minutes | warning |
| Redis eviction spike | platform | the object cache evicts more than 1 key per second | 5 minutes | warning |

Operators and Admins manage rules with `/api/v1/alert-rules` (list, create, get, update, delete, `enable`, and `disable`); every role can read rules and the alerts they raised with `GET /api/v1/alerts`, filtered by `state`, `rule_id`, `site_id`, `client_group_id`, and the time they started. Changes to rules are recorded in the [audit log](#audit-log) as `alert_rule.manage`. The expression's brackets and quotes are checked when a rule is saved, but its PromQL is only checked when VictoriaMetrics evaluates it, which is logged. One control plane process evaluates the rules.

### Alert channels

A channel is somewhere alert events go (design doc §7.3). Admins manage them with `/api/v1/alert-channels` (list, create, get, update, delete, `enable`, `disable`, and `test`); every role can read them. Changes are recorded in the [audit log](#audit-log) as `platform.settings.manage`. By default a channel receives every firing and resolved event; `min_severity` (`info`, `warning`, or `critical`), `rule_id`, and `client_group_id` narrow it (a channel limited to a client group doesn't receive platform alerts, which belong to no group).

Credentials are encrypted with the [master key](#master-key) and never returned by the API: the SMTP password, a webhook's URL (Slack and Teams URLs are credentials, since they let anyone post), and the signing secret. The API shows whether each is set, and a webhook URL as its scheme and host (`https://hooks.slack.com/…`). To change a credential, send a new value; to remove one, send an empty string.

**Email.** `email.to` lists the recipients. Without `email.host` the channel sends through the platform's SMTP relay (the `smtp.*` settings under [Outbound email](#outbound-email)) and uses its sender; with `email.host` it has an SMTP server of its own (`port`, `encryption` of `starttls`, `tls`, or `none`, `username`, `password`, and a required `from_email`). A message has a plain-text and an HTML part, and the subject reads `[PressFleet] [FIRING] critical: <summary>`.

**Webhook.** The control plane POSTs JSON to `webhook.url` with a timeout of `alerts.webhook_timeout_seconds`. Any 2xx response is success. `webhook.format` is one of:

- `generic` (the default): PressFleet's payload, below. It has a top-level `text`, so a Slack incoming webhook accepts it as it is.
- `slack`: `{"text": "*[FIRING] critical: ...*", "attachments": [{"color", "fields", ...}]}`.
- `teams`: a `message` with an Adaptive Card attachment, as Teams Workflows webhooks expect.

The generic payload is stable: new fields may be added, and `version` changes only if one is removed or changes meaning.

```json
{
  "version": 1,
  "event": "firing",
  "key": "01J9ZQ3V6T8K4M2N5P7R9S1X3Y:firing",
  "text": "[FIRING] critical: Site example.com is down\nThe HTTP check has failed for 2 minutes.",
  "sent_at": "2026-09-30T12:00:05Z",
  "alert": {
    "id": "01J9ZQ3V6T8K4M2N5P7R9S1X3Y",
    "rule_id": "01J9ZQ3V6T8K4M2N5P7R9S1X4Z",
    "rule_name": "Site down",
    "state": "firing",
    "severity": "critical",
    "summary": "Site example.com is down",
    "description": "The HTTP check has failed for 2 minutes.",
    "site_id": "01J9ZQ3V6T8K4M2N5P7R9S1X5A",
    "client_group_id": "01J9ZQ3V6T8K4M2N5P7R9S1X6B",
    "value": 0,
    "labels": {"site_id": "01J9ZQ3V6T8K4M2N5P7R9S1X5A"},
    "started_at": "2026-09-30T11:58:00Z",
    "fired_at": "2026-09-30T12:00:00Z"
  }
}
```

`event` is `firing`, `resolved` (the alert then has `state` `resolved` and a `resolved_at`), or `test`. `key` is the same for every retry of an event, so a receiver can deduplicate. `site_id`, `client_group_id`, `fired_at`, and `resolved_at` are left out when they don't apply. Requests carry the headers `X-PressFleet-Event`, `X-PressFleet-Delivery` (the key), and `X-PressFleet-Timestamp` (Unix seconds). If the channel has a signing secret, `X-PressFleet-Signature` is `sha256=` and the hex HMAC-SHA256, keyed with the secret, of the timestamp, a `.`, and the raw body; a receiver should check it and reject old timestamps.

**Delivery and retries.** When the alert engine hands over an event, the control plane records one delivery for each enabled channel that wants it, and a worker sends them. A failed delivery is retried with exponential backoff (30 seconds, doubling, up to 15 minutes between attempts) for up to 8 attempts, then marked `failed`; failures that retrying can't fix, such as a webhook answering 404 or a blocked address, fail at once. Each channel has its own delivery, so one broken channel never makes the others send twice, and an event offered again after a restart doesn't reach a channel that already has it. `GET /api/v1/alert-deliveries` lists the history (filter by `alert_id`, `channel_id`, and `state`) with the attempts, the latest error, and when a pending delivery is tried next; errors never contain credentials. Deliveries are deleted with their alert after 90 days, or with their channel. A channel that is disabled when a queued delivery comes due fails it. `POST /api/v1/alert-channels/{id}/test` sends a synthetic notification and reports the outcome; it works on disabled channels and isn't recorded in the history.

**Where channels may connect.** Webhook URLs and per-channel SMTP servers are typed in through the API, so the control plane checks the address it actually connects to, after DNS resolution: loopback, link-local (including the `169.254.169.254` cloud metadata address), unspecified, and multicast addresses are refused unless `alerts.webhook_allow_local_targets` is on. Redirects aren't followed, and proxy environment variables are ignored. Private ranges such as `10.0.0.0/8` are allowed, since an internal chat server is a common target. The platform SMTP relay is set up by an administrator and isn't restricted.

## SFTP gateway

Developers upload files to a site's `wp-content` through one SSH gateway on the host's `sftp.port` (design doc §5.8):

```sh
sftp -P 2222 <site-slug>@fleet.agency.example
```

The SSH username names the site; the key names the platform user. There are no passwords: each user registers their public keys with `POST /api/v1/users/me/ssh-keys` (`{"public_key": "ssh-ed25519 AAAA... me@laptop"}`), and Admins manage anyone's under `/api/v1/users/{user_id}/ssh-keys`. Ed25519, ECDSA, security key (`sk-`), and RSA keys of at least 2048 bits are accepted. A key belongs to one user only. Any role may register keys, but only Operators and Admins may connect.

Each session runs in a short-lived worker container for the site: the control plane image's `pressfleet sftp-server`, running as the site's user with only the site's `wp-content` volume mounted, no network, and a read-only root filesystem. The client sees `wp-content` as `/`. Files it uploads are owned by the site's user, and nothing else of the host, the platform, or other sites is reachable. The gateway offers no shell, commands, or port forwarding other than the tunnels of [database sessions](#database-sessions). Symbolic and hard links can't be created.

Connections end when the client leaves, after `sftp.idle_timeout_minutes` without traffic, after `sftp.max_session_hours`, and within seconds when the user loses access: their key is removed, they are disabled, deleted, or demoted to Viewer. Each session is audited under `site.sftp.connect` when it starts (or is refused) and `site.sftp.disconnect` when it ends, with the user, site, client address, and `details`: the session ID, key ID and fingerprint, and on the end event the bytes received from and sent to the client, the duration, and the reason it ended (`client_closed`, `idle_timeout`, `max_session_length`, `access_revoked`, or `shutdown`).

The gateway's host key is generated by `pressfleet init` and reused by `pressfleet init --force`, so developers' `known_hosts` entries stay valid. Open `sftp.port` in the host's firewall to reach it from outside.

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

The Docker check requires Docker Engine 28 or later. The Compose driver attaches platform services (MariaDB, the object cache, Traefik) to each site network with a lower gateway priority (`docker network connect --gw-priority -1`, API 1.48). Without it, Docker makes the site network Traefik's default gateway, because `pf-<id>` sorts before the platform network's name, and Traefik's published ports 80 and 443 stop answering.

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
  backups/
    repositories/<client-group-id>/
                      the client group's Restic repository (backup.backend local;
                      mode 2770, group 60000; see "Backup repositories")
  uploads/<upload-id> files sent through the upload API (never backed up; see "Uploads")
  exports/            database exports waiting to be downloaded, deleted when
                      they expire (see "Database exports")
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

## Backup repositories

Each client group has its own [Restic](https://restic.net) repository, encrypted with its own random password (design doc §5.3). Sites in a group deduplicate against each other, and offboarding a client means handing over or deleting one repository. The passwords are stored in the state database, encrypted with the master key: **without the master key, no backup can be read**.

Where repositories are created is chosen at install time with `backup.backend`:

- `local`: `<data_dir>/backups/repositories/<client group ID>`. The directory belongs to group 60000 with mode `2770`, and restic keeps new files group-readable, because backup tasks run as each site's own UID with that group added. Local repositories live on the same disk as the sites, so they don't protect against losing the host; copy them elsewhere, or use `s3`.
- `s3`: `s3:<endpoint>/<bucket>/<prefix>/<client group ID>` on Amazon S3, MinIO, or another S3-compatible service, elsewhere or on the same host (see [Self-hosted S3](#self-hosted-s3)).

A group's repository is created the first time the group needs one, i.e. on its first backup, or ahead of time with `pressfleet repository create`. A client group can't be deleted while its repository exists (`409`), so backups are never lost by accident. To offboard a client, delete its sites, then, inside the control plane container:

```sh
docker compose exec control-plane pressfleet repository export <client-group-id> /var/lib/pressfleet/tmp/acme
docker compose exec control-plane pressfleet repository delete <client-group-id> --yes
```

`export` copies the repository into a new or empty directory (downloading it from S3 if needed) together with a `password.txt` (mode `0600`) and a `README.txt`, and checks with `restic check` that the copy opens with the password file alone. The client needs nothing but restic:

```sh
restic -r repository --password-file password.txt snapshots
restic -r repository --password-file password.txt restore latest --tag site:<site-id> --target restored
```

Only the data directory is shared between the control plane container and the host, so export under it and move the result off the host; it holds the password in plaintext. Don't export while the repository is being pruned. `delete` removes the repository's files or objects, then its password; it is refused while the group still has sites.

### Self-hosted S3

For development, demos, or a small install that wants S3 without a cloud account, run an S3-compatible service such as MinIO or RustFS on the same host and attach it to the platform's **backup network**, `<platform_project>_backup` (`pressfleet_backup` by default). The control plane and every backup and restore container join that network, so both reach the service by its container name; nothing else does. The network is internal: it gives no route out of the host, and sites can't reach it.

A Compose project for the service, in a directory of its own (not the platform's, which `pressfleet init` and `upgrade` rewrite):

```yaml
# s3/compose.yaml; start it after the platform (docker compose up -d),
# which creates the network.
services:
  s3:
    image: rustfs/rustfs:1.0.0      # or a MinIO image
    restart: unless-stopped
    environment:
      RUSTFS_ACCESS_KEY: pressfleet
      RUSTFS_SECRET_KEY: change-me-to-a-long-random-string
    volumes:
      - s3-data:/data
    networks:
      - backup
networks:
  backup:
    name: pressfleet_backup         # <platform_project>_backup
    external: true
volumes:
  s3-data: {}
```

Then point PressFleet at it, with `pressfleet init --backup-backend s3 --backup-s3-endpoint http://s3:9000 --backup-s3-bucket pressfleet --backup-s3-access-key-id pressfleet --backup-s3-secret-access-key-file <file>` (restic creates the bucket on first use). The service's data volume holds every backup, so it lives on the same disk as the sites: it protects against mistakes, not against losing the host. Stop the service before the platform when taking everything down (`docker compose down` refuses to remove a network that containers are still attached to).

To reach the service from the host too, for example its web console, publish a port on another network of its own; the backup network can't publish ports.

### Retention and integrity checks

Every repository is pruned daily and checked every `backup.check.interval_days` days, by jobs of type `backup.prune` and `backup.check` whose target is the client group. Both start after the backup window closes (by 02:00 to 04:00 UTC with the default window), at a time derived from the group's ID; checks also fall on a day of their interval derived from it.

A prune applies the retention policy (`backup.retention`) to each site's snapshots separately, with `restic forget`, then removes the data that only the forgotten snapshots used (`restic prune`). A snapshot is kept if any rule keeps it: by default, a site keeps its last 3 snapshots, the newest of each of the last 7 days, 4 weeks, and 6 months. While a site has snapshots from fewer days (weeks, ...) than a rule asks for, its oldest snapshot is kept too, so the policy covers as much time as it can (restic 0.17 and later). Every kind of snapshot counts, including pre-restore ones. The snapshots of deleted sites follow the same policy, so their newest ones stay until the repository is deleted. Backups whose snapshots are gone are then marked with an `expired_at` time in the API; they stay in the list, but can't be restored.

A check runs `restic check`, reading back `backup.check.read_data_percent` of the data. If it finds damage, its job fails and logs the errors; repair the repository with restic (`restic repair index`, `restic repair snapshots`; see restic's troubleshooting guide) from inside the control plane container. Other failures, such as a lost connection to S3, are retried.

Prune and check take restic's exclusive lock on the repository, and never run at the same time as each other. Backups and restores of the group's sites wait for them (for up to 30 minutes, then the backup job retries), and they wait for running backups; they don't overlap. Before either runs, stale locks left by restic processes that died (for example a backup container that was killed) are removed; live locks are not.

To prune or check a repository now, inside the control plane container:

```sh
docker compose exec control-plane pressfleet repository prune <client-group-id>
docker compose exec control-plane pressfleet repository check <client-group-id>
```

Each queues the job and prints its ID; follow it with `GET /api/v1/jobs/{id}`. Changed retention and check settings apply after the control plane restarts.

## Site backups

`POST /api/v1/sites/{id}/backups` queues a backup of a site; `GET` on the same path lists the site's backups, including running and failed ones. A backup job dumps the site's database from the control plane into a transfer directory under `<data_dir>/tmp/`, then runs `restic backup` in a short-lived container on the site's network (the control plane image, `task_image`), with the site's `wp-content` mounted read-only. The transfer directory is removed afterwards, whether or not the backup worked. WordPress core and `wp-config.php` are never backed up: they are rebuilt from the site's WordPress version and settings.

Every snapshot holds the same two paths, for every site, so sites deduplicate against each other and a snapshot restores into any site:

| Path | Contents |
| --- | --- |
| `/site/wp-content/` | The site's `wp-content` |
| `/transfer/database.sql` | The site's database, as SQL from `mariadb-dump`, without `CREATE DATABASE`/`USE` and with `DEFINER` clauses removed |

Snapshots are tagged `site:<site ID>`, `wp-core:<WordPress version>`, `kind:<manual, scheduled, or pre_restore>`, and `backup:<backup ID>` (the ID the API lists), and their host name is `site-<site ID>`, so retention (`restic forget --group-by host`) and restic's change detection work per site.

A backup that can't read some files (for example a file in `wp-content` that the site's own user can't read) still succeeds, without those files; the backup's `message` lists them. Backups of different sites run at the same time, also in one repository; a site is never backed up twice at once, and a backup waits for the site's other jobs (an update, a deletion) to finish.

With the `s3` backend, the backup container reaches S3 from the site's network, which has the same outbound access as the site itself (the site network is an ordinary Docker bridge network), so Amazon S3 and other services on the Internet work; the control plane image it runs has the usual CA certificates. It also joins the platform's backup network, where an S3 service on the same host is reached by its container name (see [Self-hosted S3](#self-hosted-s3)). It never joins the platform network, so services there, such as MariaDB, aren't reachable under their platform names.

### Scheduled backups

Every site is backed up once a day, from the moment it is created. Its backup starts at a time within the backup window (`backup.schedule`, 02:00 to 06:00 UTC by default) that is derived from the site's ID: the same every day, and different between sites, so backups are spread over the window instead of all starting at once. A scheduled backup is listed like a manual one, with kind `scheduled`, and waits for any other job working on the site.

Each site can change its schedule with `PUT /api/v1/sites/{id}/backup-schedule` (`GET` shows it and when the next backup is due):

```json
{"enabled": true, "interval_hours": 12, "hour": 3}
```

- `interval_hours` is the time between backups: 1, 2, 3, 4, 6, 8, or 12, or whole days up to a week (24, 48, ..., 168). The default is 24.
- `hour` is the UTC hour the site's backups start in, instead of the window; with an interval shorter than a day, one of the day's backups starts in that hour. Omitted or `null` means the window.
- `"enabled": false` stops scheduled backups; the site's existing backups stay and manual backups still work. Omitted fields take their defaults, so `{"enabled": true}` restores the daily backup in the window.

Changing the schedule takes effect at once. Backups due while the control plane was down run when it starts again, once however many were missed. The control plane checks every site's schedule when it starts, so a changed backup window moves the schedules of the sites that use it, and sites created before scheduled backups existed get one. Sites that are being deleted, or whose provisioning failed, have no schedule; if a scheduled backup finds its site gone or being deleted, it is skipped (the job succeeds and says why in its log).

## Site restores

`POST /api/v1/sites/{id}/restore` with `{"snapshot_id": "..."}` restores one of the site's snapshots into it; add `"source_site_id"` to restore a snapshot of another site of the same client group (its content then replaces this site's). The site must be running. The job:

1. takes a pre-restore backup of the site (kind `pre_restore`, listed with its other backups);
2. puts the site in maintenance mode;
3. moves the site to a newer WordPress core first, if the snapshot was taken on a newer one than the site runs (the oldest site image with at least that version; a snapshot from a core newer than any image is refused up front);
4. replaces `wp-content` with the snapshot's, removing files added since, and the database with the snapshot's dump, restarts the site's containers (so PHP doesn't keep running cached code of replaced plugins), and runs `wp core update-db`;
5. rewrites the database's URLs to the site's own with `wp search-replace` if they name another host (a snapshot of another site, or one taken before go-live);
6. purges the caches and takes the site out of maintenance mode, unless it was in maintenance mode before the restore.

The files are restored by `restic restore` in a short-lived container that runs as the site's user, so they belong to the site, whichever site the snapshot is of. The database dump is extracted into a transfer directory under `<data_dir>/tmp/` (`restore-<site ID>-<snapshot>`), which is removed afterwards.

**Failures.** A failed step is retried; the ones that can't succeed on a retry (a missing snapshot, WordPress failing to load the restored site) fail at once. If the job fails after the site's data was touched, it restores the pre-restore backup, moves the site back to its previous WordPress core, and leaves the site **in maintenance mode** with the error in its `status_message`: check the site, then turn maintenance mode off (`PUT /api/v1/sites/{id}/maintenance`). If even the pre-restore backup can't be restored, the site is marked `failed` and the status message names the pre-restore backup; restore it like any other once the cause is fixed.

**Client groups.** Restores stay within a client group: each group's repository has its own key, and a restore task opens one repository. Restoring a snapshot into a site of another group is refused with `422`.

## Uploads

Large files reach the control plane through a resumable upload API: the `wp-content` archive and SQL dump of a site import, and the theme or plugin package of a code deployment (design doc §5.7, §5.8).

1. `POST /api/v1/uploads` declares what the file is for (`"purpose": "site_import"` with the `client_group_id` the new site will join, or `"purpose": "site_deploy"` with the `site_id`), its exact `size_bytes` (at most `uploads.max_size_mb`), and optionally its `sha256`.
2. `PATCH /api/v1/uploads/{id}?offset=<n>` with an `application/octet-stream` body sends the next chunk, at most 32 MiB (`max_chunk_bytes`), starting at the upload's `received_bytes`. A chunk counts only once all of it is on disk: if a connection drops, `GET /api/v1/uploads/{id}` says where the upload stands, and the client resumes from there.
3. `POST /api/v1/uploads/{id}/complete` checks the content against the SHA-256 checksum (given now or in step 1). Content that doesn't match is removed; start again.

The import or deployment then takes the upload by its ID. An upload needs the permission of the job it is for (Operators and Admins; not read-only tokens), and only the user who created it can send, see, complete, cancel (`DELETE`), or use it. Every request is recorded in the audit log as `upload.site_import` or `upload.site_deploy`.

Uploads are stored in `<data_dir>/uploads/`, which no backup includes. A job every 10 minutes removes uploads that went `uploads.expiry_hours` without a chunk while no job had taken them, uploads whose job has ended (succeeded or failed), and files without a record. One control plane process serves uploads, since they live on its disk.

```sh
# Upload wp-content.tar.gz in 16 MiB chunks with a bearer token.
size=$(stat -c %s wp-content.tar.gz); sum=$(sha256sum wp-content.tar.gz | cut -d' ' -f1)
id=$(curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"purpose\":\"site_import\",\"client_group_id\":\"$GROUP\",\"size_bytes\":$size,\"sha256\":\"$sum\"}" \
  "$BASE/api/v1/uploads" | jq -r .id)
split -b 16M -d wp-content.tar.gz chunk.
offset=0
for f in chunk.*; do
  curl -sf -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/octet-stream' \
    --data-binary @"$f" "$BASE/api/v1/uploads/$id?offset=$offset" >/dev/null
  offset=$((offset + $(stat -c %s "$f")))
done
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{}' "$BASE/api/v1/uploads/$id/complete"
```
## WordPress core updates

The site image version manifest (built into the control plane, or `image_manifest_file`) lists one WordPress release per major branch: the latest, and the newest release of each older branch that sites may be held on or awaiting approval to leave. When the control plane starts, and every 15 minutes, a rollout pass (job `rollout.pass`) looks for running sites with a newer release to move to (design doc §5.5):

- **Minor and security releases** (x.y.Z) of a site's own branch roll out automatically.
- **Major releases** (x.Y) wait for approval: `PUT /api/v1/sites/{id}/core/policy` with `{"approved_branch": "7.2"}`, or the same on the client group (`PUT /api/v1/client-groups/{id}/core-policy`), or `updates.core.major_auto_approve_days` after the release's images were published.
- **Holds**: `{"hold": true}` keeps a site (or every site of a group) on its current branch, which still gets its minor and security releases. A site's own policy wins over its group's; `{}` clears it. `GET /api/v1/sites/{id}/core` shows the site's next release, a pending major release, and warnings: held on an older branch for longer than `updates.core.hold_warning_days`, a branch no longer built, or one wordpress.org reports as insecure.

Sites are updated one at a time, inside the maintenance window if one is set. Each update is a `site.core_update` job: a pre-update backup (kind `pre_update`), both containers recreated on the new release's images (`php` and `nginx` always carry the same core), `wp core update-db`, and a health check (an HTTP request to the site's nginx from inside its network, and WP-CLI loading WordPress and reporting the new version). If anything fails, the job restores the pre-update backup if the database schema changed, moves the site back to its previous images, and says so in the site's `status_message`; the rollout doesn't try that release on the site again (`POST /api/v1/sites/{id}/core/update` does, at once). After `updates.max_failures` failed updates in a row the rollout halts; `GET /api/v1/core-rollout` shows it, and `POST /api/v1/core-rollout/resume` or a new manifest resumes it.

## Site import

`POST /api/v1/sites/import` (Operators and Admins) creates a site from an existing WordPress install (design doc §5.7): a `wp-content` archive and a SQL dump, each sent as a `site_import` upload for the client group first.

```sh
wp db export site.sql && tar -czf wp-content.tar.gz wp-content   # on the old host
# upload both (see Uploads), then:
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d "{\"client_group_id\":\"$GROUP\",\"archive_upload_id\":\"$ARCHIVE\",\"dump_upload_id\":\"$DUMP\"}" \
  "$BASE/api/v1/sites/import"
```

- **What is accepted.** The archive is a `.tar.gz`, `.tar.bz2`, `.tar`, or `.zip` of `wp-content`: its contents, a `wp-content` directory, or a whole install that has one. The dump is a `.sql` or `.sql.gz` from `wp db export`, mysqldump, mariadb-dump, or phpMyAdmin, of one single-site WordPress database with any table prefix.
- **Checked before anything is created.** The job's first step reads both files. A multisite network, a dump that isn't a readable WordPress database, a database whose `db_version` is newer than the newest site image's WordPress, or an archive without `wp-content` fails the job, and no site is created. The response already names the site's ID; `GET /api/v1/sites/{id}` finds the site once this step has passed.
- **What the import does.** The site is created like a new one, on the newest WordPress, with the dump's table prefix, and named after the imported site unless `name` is given. Its `wp-content` is replaced with the archive's, the dump is imported (its `DEFINER` clauses, `CREATE DATABASE`/`USE`, and MySQL GTID statements removed), `wp core update-db` runs, the old `siteurl` and `home` are rewritten to the preview URL with `wp search-replace` (`http` and `https`), and the default plugins are set up. The site keeps its users and their passwords; no admin credentials are generated.
- **What is replaced or skipped.** `object-cache.php`, `advanced-cache.php`, and `db.php` belong to the old host and are left out; the platform installs its own object cache. Must-use plugins are kept and listed as warnings in the job's log, naming the host when it is a known one. Symbolic links are skipped. The old `wp-config.php` is never used, so constants defined there must be set again through the platform.
- **Failure.** An import that fails after the site was created deletes the site again, with its containers, database, and files. The uploads are removed when the job ends, whether it succeeded or not.

## Platform image updates

The platform rebuilds the site images for PHP patch releases, security fixes, and base OS updates without changing their tags (`site-php:php8.4-wp7.1.2` stays `site-php:php8.4-wp7.1.2`). So that a rebuild never reaches a site untested, each site's containers run a pinned build of each image (`repository@sha256:<digest>`, or the image ID of a locally built image), recorded in the state database when the site is first rendered with that tag. Re-rendering a site, or pulling a tag, doesn't move it; the image rollout does (design doc §5.5):

- When the control plane starts, and every 15 minutes, a rollout pass (job `rollout.pass`) asks Docker for the build each tag in use names now. Every `updates.images.check_hours` it pulls the tags from their registry first; a failed pull is logged and the local build is used.
- A running site pinned to an older build than that is updated: one site at a time, inside the maintenance window if one is set (`updates.window_start`, shared with core updates). Each update is a `site.image_update` job: the site's `php` and `nginx` containers are recreated on the new builds, which carry the same WordPress core, followed by the same health check as a core update (an HTTP request to the site's nginx, and WP-CLI loading WordPress and reporting the site's core version). If it fails, the site is moved back to its previous builds and its `status_message` says so, and the rollout doesn't try those builds on the site again.
- There is no pre-update backup: the core doesn't change, so the database schema doesn't either, and the images hold no site data, so moving back to the previous build undoes the update.
- After `updates.max_failures` failed updates in a row the rollout halts; `GET /api/v1/image-rollout` shows it with the newest builds seen and the failed sites, and `POST /api/v1/image-rollout/resume`, or a newer build, resumes it.

Each pinned build is also tagged `<repository>:pin-<first 16 hex digits of its image ID>`, so Docker keeps it after the tag moves on and a site can be moved back to it. These tags are not removed yet; `docker image rm` the ones no site uses to reclaim space.

## Site databases

Each site's database lives on the platform's shared MariaDB server. The control plane connects as `mariadb.admin_user` to create a database and a user for each site, both named `pf_<site-id>`. Each site user has a random password, stored encrypted in the state database, and privileges on its own database only. It is also limited to 20 simultaneous connections (`MAX_USER_CONNECTIONS`), so one site can't use up the server's 150 connections. With PHP-FPM's `ondemand` mode, idle sites hold no connections.

The defaults match the platform Compose project, so installs made with `pressfleet init` need no MariaDB settings. The control plane connects only when it needs to, so `serve` starts even while MariaDB is down, and it re-reads the password file for every new connection.

Sites reach MariaDB as `pf-mariadb` on their own network: the control plane connects the platform's MariaDB container (named after `platform_project`) to each site network. Docker drops those connections when it recreates the container, for example during an upgrade, so `serve` reconnects MariaDB to every site's network when it starts. It does so in the background and only logs failures, so a missing MariaDB or Docker doesn't stop the control plane from starting.

## Database exports

Agencies get a copy of a site's database without its password (design doc §5.8). Operators and Admins export it with `POST /api/v1/sites/{id}/database/export`, which queues a job. The job dumps the database with `mariadb-dump` as the site's own user (a consistent snapshot that doesn't lock the site's tables, so the site keeps serving, stopped sites included), compresses it with gzip into `<data_dir>/exports/<export ID>.sql.gz`, and puts the export's `download_url`, `size_bytes`, and `expires_at` in its result. The dump has no `CREATE DATABASE` or `USE` statement, so it imports into whichever database you choose: `gunzip < dump.sql.gz | mariadb mydb`.

Download it from `download_url` (`GET /api/v1/sites/{id}/database/exports/{export ID}/download`) with a session or a full API token; read-only tokens and Viewers can't. The export is downloadable, as often as needed, for `database_export.ttl_seconds` after it is ready (an hour by default). Then its file is deleted and its link returns `404`: a download that finds it expired deletes it at once, and a job every five minutes deletes the rest. That job also deletes files left behind by exports that failed or were interrupted. Every export and every download, successful or not, is recorded in the [audit log](#audit-log).

To replace a site's database, restore a backup (see [Site restores](#site-restores)).

## Database sessions

Developers reach a site's database with their own tools (TablePlus, DBeaver, the `mariadb` client) through a temporary database session tunneled through the [SFTP gateway](#sftp-gateway) (design doc §5.8). Open one with `POST /api/v1/sites/{id}/database/sessions`:

```json
{"access": "read_only", "ttl_seconds": 3600}
```

Both fields are optional. The control plane creates a temporary MariaDB user, `pft_<session ID>`, with privileges on the site's database only, and returns its username and password (shown only once) with the command that opens the tunnel:

```sh
ssh -N -L 3307:mariadb:3306 -p 2222 <site-slug>@fleet.agency.example
```

Run it with an SSH key registered on your own account, then connect to `127.0.0.1:3307` with the session's credentials. The gateway forwards only to the database (`mariadb.address`), only for the user who opened the session, only on its site, and only while the session is active and the user's role still allows it.

- **Access:** read-only by default (`SELECT` and `SHOW VIEW`). Read-write sessions have the site's own privileges on its database and need an Admin. Operators and Admins may open read-only sessions; Viewers and read-only API tokens can't open any.
- **Length:** `ttl_seconds`, from 60 to 28800 (eight hours); an hour by default. Tunneled connections stop at expiry. A job every minute expires sessions, drops their MariaDB users, and ends their connections; it also drops temporary users that no active session owns, such as ones a crash left behind, and runs once when the control plane starts.
- **Ending early:** `DELETE /api/v1/sites/{id}/database/sessions/{session ID}` drops the user and ends its connections at once. `GET /api/v1/sites/{id}/database/sessions` lists the active sessions, without passwords.
- **Role changes:** a user demoted below what a session needs (an Admin with a read-write session demoted to Operator, anyone demoted to Viewer) can no longer tunnel with it; the gateway ends the session the next time they try.

The site's own database password is never revealed, and session passwords are never stored, logged, or audited. The audit log records opening a session (`site.database.session.create`, or `site.database.session.create_read_write`), ending it (`site.database.session.end`), expiry (`site.database.session.expire`), and sessions ended because the user's role no longer allows them (`site.database.session.revoke`), with the session ID, access, and expiry in `details`. The SFTP connection that carries the tunnel is audited like any other. Opening a session returns `409` when the SFTP gateway is off.

## Object cache

Sites share one object cache server, Valkey (Redis-compatible), capped at 256 MiB with least-recently-used eviction across all sites and no persistence. Its default user is disabled. The control plane connects as `redis.admin_user` and gives each site an ACL user `pf_<site-id>` with a random password, stored encrypted in the state database. A site user can only use keys starting with `<site-id>:` in a logical database of its own, and can't run commands that reach the server as a whole or other databases (`KEYS`, `FLUSHALL`, `CONFIG`, `MONITOR`, `CLIENT LIST`, Pub/Sub, functions, ...). It can `SCAN` and `FLUSHDB`, which only see its own database. The design and the reasons for it are in the documentation of package `control-plane/internal/objectcache/redis`.

The server keeps site users only in memory, so the control plane restores them from the state database: when it starts, within about 10 seconds of noticing the server restarted, and every 10 minutes. The same pass removes site users that have no site. Sites created before the object cache existed get their users from the `site.reconcile` job queued for every site at startup (see [WordPress security set](#wordpress-security-set)). Sites reach the server as `pf-redis:6379` on their own network; the control plane connects it to site networks like MariaDB.
