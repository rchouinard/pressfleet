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

## Preflight checks

`pressfleet doctor` checks the host against the requirements in design doc §4.1 and prints a fix for every problem. `--json` prints the same report as JSON. The exit status is:

| Exit | Meaning |
| --- | --- |
| 0 | Every check passed or was skipped. |
| 1 | At least one required check failed. |
| 2 | No required check failed, but a recommended check failed or some check warned. |

`pressfleet serve` runs the subset of checks that apply to a running control plane (Docker, Compose, cgroup v2, address pools, disk, memory) and refuses to start if a required one fails. Checks that only make sense before installation (ports 80/443, DNS, log rotation in the host's `daemon.json`) run only in `doctor`. On a development machine, `serve --skip-preflight` starts anyway and logs a warning.

The DNS checks are skipped until `dashboard_domain` and `preview_base_domain` are set. For the wildcard, doctor resolves a random name under the preview base domain. A record that doesn't resolve, or resolves only to loopback, fails. Otherwise:

- With `public_ip` set, the check passes if the record includes that address and fails if it doesn't.
- Without it, the check passes if the record includes an address of one of the host's network interfaces. If not, it warns rather than fails: cloud hosts behind NAT only have private interface addresses, so doctor can't tell a correct record from a wrong one. Set `public_ip` to make the check conclusive.

## Data directory layout

`pressfleet serve` creates this layout on startup. Every directory is mode `0700` because it holds rendered credentials.

```
<data_dir>/
  state/              control plane database (SQLite)
  platform/           rendered platform services Compose project
  sites/<site-id>/    rendered per-site Compose project and wp-config.php
  tmp/                scratch space, e.g. database dumps during backups
```

Operators may inspect rendered files here, but shouldn't edit them. The control plane regenerates them from its database.

The platform master key is deliberately **not** stored in the data directory. Keep it separate from the host and its backups (see the master key warning in the design doc, §4.1).
