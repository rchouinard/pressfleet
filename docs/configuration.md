# Configuration

The `pressfleet` binary reads its settings from four sources. Later sources override earlier ones:

1. Built-in defaults
2. The config file (YAML)
3. `PRESSFLEET_*` environment variables
4. Command-line flags

`pressfleet version` ignores configuration, so it works even when the config is invalid.

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
```

## Settings

| Key | Environment variable | Flag | Default | Description |
| --- | --- | --- | --- | --- |
| `data_dir` | `PRESSFLEET_DATA_DIR` | `--data-dir` | `/var/lib/pressfleet` | Control plane state and rendered Compose projects. Must be an absolute path. |
| `listen` | `PRESSFLEET_LISTEN` | `--listen` (`serve` only) | `:8080` | HTTP listen address for the API and dashboard. |
| `log.level` | `PRESSFLEET_LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn`, or `error`. |
| `log.format` | `PRESSFLEET_LOG_FORMAT` | `--log-format` | `text` | `text` or `json`. Logs go to stderr. |
| `dashboard_domain` | `PRESSFLEET_DASHBOARD_DOMAIN` | | (unset) | Domain serving the dashboard and API, e.g. `fleet.agency.example`. |
| `preview_base_domain` | `PRESSFLEET_PREVIEW_BASE_DOMAIN` | | (unset) | Domain whose wildcard record points at this host; previews use its subdomains. Give the base (`preview.agency.example`), not `*.preview.agency.example`. |
| `public_ip` | `PRESSFLEET_PUBLIC_IP` | | (unset) | Optional. This host's public IP address, used only to verify DNS records when the host is behind NAT (see [Preflight checks](#preflight-checks)). |
| `database.driver` | `PRESSFLEET_DATABASE_DRIVER` | | `sqlite` | State database: `sqlite` or `postgres`. See [State database](#state-database). |
| `database.sqlite_path` | `PRESSFLEET_DATABASE_SQLITE_PATH` | | `<data_dir>/state/pressfleet.db` | SQLite database file. Must be an absolute path. |
| `database.postgres_dsn` | `PRESSFLEET_DATABASE_POSTGRES_DSN` | | | PostgreSQL connection string (URL or `key=value`). Required when the driver is `postgres`. Treated as a secret: it is never logged. |
| `master_key_file` | `PRESSFLEET_MASTER_KEY_FILE` | | (unset) | File holding the platform master key. Must be an absolute path outside `data_dir`. See [Master key](#master-key). |
| (environment only) | `PRESSFLEET_MASTER_KEY` | | (unset) | The platform master key itself. Takes precedence over `master_key_file`. It can't be set in the config file. |
| `jobs.concurrency` | `PRESSFLEET_JOBS_CONCURRENCY` | | `4` | How many background jobs (provisioning, backups, WP-Cron runs) run at once, from 1 to 64. Jobs that change the same site always run one at a time. |

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
