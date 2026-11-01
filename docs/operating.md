# Operating PressFleet

How the platform is laid out on the host, where its logs are, which commands run where, and how to watch it.

## The platform project

The platform is one Docker Compose project, named `pressfleet`, rendered by `pressfleet init` into `<data_dir>/platform/` (default `/var/lib/pressfleet/platform`). Don't edit it: `init` and `upgrade` rewrite it. To change anything, change the [config file](configuration.md#changing-settings) and run `pressfleet upgrade`.

| Service | Container | What it does |
| --- | --- | --- |
| `traefik` | `pressfleet-traefik-1` | The edge router. Ports 80 and 443. Gets certificates. |
| `socket-proxy` | `pressfleet-socket-proxy-1` | Gives Traefik read-only access to Docker, and nothing else. |
| `control-plane` | `pressfleet-control-plane-1` | The API, dashboard, job workers, metrics collector, and SFTP gateway. Port 2222. |
| `mariadb` | `pressfleet-mariadb-1` | Every site's database. |
| `redis` | `pressfleet-redis-1` | The object cache (Valkey), shared by all sites. |
| `victoria-metrics` | `pressfleet-victoria-metrics-1` | Metrics storage. |
| `maintenance` | `pressfleet-maintenance-1` | The page shown while a site is in maintenance mode. |

Each site is a separate Compose project, `pf-<site-id>`, with two containers, `pf-<site-id>-nginx-1` and `pf-<site-id>-php-1`, on a network of its own. The control plane renders and manages them; their files are in `<data_dir>/sites/<site-id>/`.

The command for the platform project is long, so define a shorthand (as root, in your shell profile). The examples in these docs use it:

```sh
alias pfc='docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/platform'
pfc ps                       # the platform's containers and health
pfc logs --tail 100 traefik  # recent logs of one service
```

Use your own `--project-directory` if you chose another data directory. If `platform_project` isn't `pressfleet`, use that name, and the container names change with it.

Don't run `docker compose restart`, `down`, or `up` on the platform by hand to apply changes; use `pressfleet upgrade` ([Changing settings](configuration.md#changing-settings)). To bounce a stuck service, `pfc restart <service>` is fine.

### What is where

| What | Where |
| --- | --- |
| Config file | `/etc/pressfleet/pressfleet.yaml` (or `--config`, `$PRESSFLEET_CONFIG`) |
| Master key | `/etc/pressfleet/master.key`, mode 0600 |
| Data directory | `/var/lib/pressfleet` (mode 0700 everywhere inside) |
| State database (SQLite) | `<data_dir>/state/pressfleet.db`; backups taken before migrations in `<data_dir>/state/backups/` |
| Platform project and secrets | `<data_dir>/platform/` |
| Site projects | `<data_dir>/sites/<site-id>/` |
| Local backups | `<data_dir>/backups/repositories/<client-group-id>/`, state backups in `<data_dir>/backups/state/` |
| Uploads, exports, scratch | `<data_dir>/uploads/`, `exports/`, `tmp/` |
| Upgrade records | `<data_dir>/upgrade/` |
| Certificates | The `traefik-acme` Docker volume |
| Site files, databases, metrics | Docker volumes |

The full layout: [Data directory layout](configuration.md#data-directory-layout).

## Logs

| What | How to read it |
| --- | --- |
| **The control plane** (API requests, jobs, errors) | `pfc logs control-plane`. It writes to stderr, so Docker keeps it. `log.level` and `log.format` (`text` or `json`) are in the config; use `debug` temporarily. The access log records paths, never query strings. |
| **Other platform services** | `pfc logs traefik` (JSON access log without request headers, and ACME messages), `pfc logs mariadb`, `pfc logs redis`, `pfc logs victoria-metrics`. |
| **A site's logs** | In the dashboard: **Logs**, or the site's page. They show nginx access and error logs, the PHP-FPM slow log, and PHP output, filterable by source and level. **Operators and Admins only**, because access logs hold query strings. API: `GET /api/v1/sites/{id}/logs` (and `/logs/stream`). On the host: `docker logs pf-<site-id>-nginx-1` and `-php-1`. A site's mail failures are in the `php` container's log. |
| **What the platform did to a site** | The site's **Logs** > **Jobs** tab, or `GET /api/v1/sites/{id}/job-logs`. Every job (provisioning, backups, updates, go-live, restores) logs its steps. The **Jobs** page shows one job's steps and its error. |
| **Who did what** | **Audit log** (Admins), `GET /api/v1/audit-events`. Kept `audit.retention_days` (365). |
| **The host CLI** | It prints to your terminal. `--log-level debug` for more. |

Docker rotates container logs by the `log-opts` you set ([Docker settings](host-preparation.md#docker-settings)): with the recommended 10 MiB by 3 files, a site's runtime logs go back only a few tens of megabytes. For long-term logs, ship them with a Docker logging driver or an agent. PressFleet doesn't keep site logs itself.

## Commands that run in the control plane container

The host CLI never reads or writes the state database. Anything that needs the database runs **inside the control plane container**, where the same `pressfleet` binary is on the `PATH` and the data directory is at the same path.

| Run on the host | Run in the control plane container |
| --- | --- |
| `pressfleet init`, `upgrade`, `recover`, `doctor` | `pressfleet user create`, `reset-password`, `reset-two-factor` |
| `pressfleet version`, `key generate` | `pressfleet state backup`, `backups`, `restore`, `recover` |
| `pressfleet metrics-access rotate` | `pressfleet repository create`, `check`, `prune`, `export`, `delete` |

(`state recover` is what `pressfleet recover` runs for you; you don't call it yourself.)

Run one command with `pfc exec`. Add `-T` when you pipe input in or capture output in a script (`--password-stdin`, for example):

```sh
pfc exec control-plane pressfleet user reset-password --email admin@agency.example
pfc exec -T control-plane pressfleet user create --email ops@agency.example --role operator --password-stdin <<<"$PASSWORD"
```

For a shell, `pfc exec control-plane sh`. The container runs as root, with Docker access and `restic`; treat it that way. Use `pressfleet version --client` inside it to see its own version, and `pressfleet admin info` for its version and readiness.

Everything else is done in the dashboard or the API.

## Monitoring and alerts

### What is collected

The control plane collects metrics every 30 seconds (`metrics.interval_seconds`) into VictoriaMetrics, kept `metrics.retention_days` (90). Per site: whether it is up, response time, request rate and status codes, page and object cache hit ratio, PHP workers, CPU and memory, restarts, disk use, database size, certificate expiry, and backup age and size. Platform-wide: host CPU, memory, and disk, MariaDB and Redis, the job queue, and each client group's repository size. See the [catalog](configuration.md#metrics-api).

Where to look:

- **Platform overview:** the preflight report (the same checks as `doctor`, plus the ones that need the database), and a capacity warning if the sites' memory reservations approach the host's.
- **Metrics:** platform and per-site charts. A site's page links to its own metrics.
- **Alerts:** firing and resolved alerts, the rules, and the channels.

The uptime check runs from the PressFleet host itself, so **it cannot tell you the host is down**. Use an external uptime monitor on the dashboard domain, and on a few important sites.

### Alert rules

Eleven rules are created at first start and are on by default: site down, elevated 5xx rate, certificate expiring and certificate renewal failing, backup failed and backup stale, disk nearly full, container restart loop, PHP-FPM worker saturation, Redis eviction spike, and plugin or theme updates pending. You can edit their thresholds, durations, and text, or disable them, but not delete them. The table is in [Alerts](configuration.md#alerts). The site rules skip [staging sites](staging.md) unless a rule (a built-in one can be edited too) sets **Include staging sites**. Operators and Admins add rules on the **Alerts** > **Rules** page (`/api/v1/alert-rules`): a PromQL expression, a threshold or an "is true" condition, a duration, and a severity.

An alert is `pending` until its duration has passed, then `firing`, then `resolved`. Resolved alerts are kept 90 days. If VictoriaMetrics is down, alerts stay as they are: nothing fires or resolves.

### Alert channels

Rules do nothing visible until a **channel** receives them. **Alerts** > **Channels** (Admins to manage, everyone to read) has two kinds:

- **Email.** To one or more addresses. Without its own server it sends through the platform's SMTP relay (the `smtp.*` settings from `init`, which are for site email too); give a host to use a different server.
- **Webhook.** A JSON POST. Format `generic` (PressFleet's payload; Slack incoming webhooks accept it), `slack`, or `teams`. A signing secret adds an `X-PressFleet-Signature` header that your receiver can verify.

A channel can be limited to a minimum severity, one rule, or one client group. Use **Test** on a new channel before you rely on it. Failed deliveries are retried for up to 8 attempts with backoff, and the history is in **Alerts** (`GET /api/v1/alert-deliveries`). Webhook URLs are secrets, stored encrypted and never shown again. Webhooks to loopback and cloud metadata addresses are refused unless `alerts.webhook_allow_local_targets` is on.

A sensible minimum: one email channel to a shared mailbox for every severity, and one chat webhook for `critical` only. If you have set up nothing else, add a channel before you add client sites.

### Grafana

To query the metrics from your own Grafana:

1. Create a DNS record for a metrics domain, such as `metrics.agency.example`, pointing at the host. It must differ from the dashboard domain and not be under the preview base domain.
2. Add it to the config file:

   ```yaml
   metrics:
     public_domain: metrics.agency.example
   ```
3. Run `pressfleet init --force` with the same answers as the first install (interactively, Enter keeps each prefilled answer; it asks you to confirm the master key is stored again). It keeps the master key and credentials, and prints the credential **once**: user `grafana` and a generated password. Store it.
4. In Grafana add a **Prometheus** data source: URL `https://metrics.agency.example`, authentication **Basic authentication** with that user and password, HTTP method `POST` or `GET`.

Only the read paths of the query API are served (`/api/v1/query`, `query_range`, `series`, `labels`, `label/<name>/values`, `metadata`, `status/buildinfo`, and `/federate`); everything else answers `404`. The metric names are the ones in the [catalog](configuration.md#metrics-api). To replace the password, run `pressfleet metrics-access rotate` on the host (it prints the new one once, and the old one stops within seconds), then update Grafana. To turn it off, remove the setting and run `pressfleet init --force`. Details: [Connecting Grafana](configuration.md#connecting-grafana).

## Routine tasks

| When | Do |
| --- | --- |
| Daily | Glance at **Alerts** and **Jobs** for failures. |
| Weekly | Check that the newest backup of a few sites is recent. Confirm the off-host copy of local backups is current. |
| Monthly | Run a restore into a scratch site. Look at disk use (`df`, **Metrics** > host disk). Review the user list and API tokens. Check [the release check](upgrading.md#release-check) for a new version. |
| Before a big change | `pressfleet state backup` in the container, and a manual backup of the affected sites. |
| Once, before real client sites | The [disaster recovery drill](disaster-recovery.md#rehearsing). |
| When someone leaves | Disable or delete their user (this ends their sessions and revokes their tokens), remove their SSH keys, rotate any secret they could read ([Security](security.md#rotating-secrets)). |
