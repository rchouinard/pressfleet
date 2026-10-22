# Capacity test: 50 sites on an 8 GiB host (#98)

**Result: fail.** Traefik was OOM-killed 38 times against its 96 MiB limit under steady light traffic. The host itself never ran short of memory.

The test follows the plan in the handoff for #98: 50 sites, light traffic, and the backup, prune, and check windows, on a host with 8 GiB of memory. The harness is `scripts/capacity/`; [Testing](testing.md#capacity-test) says how to rerun it.

## Setup

| | |
| --- | --- |
| Host | DigitalOcean `s-4vcpu-8gb-amd` (4 shared vCPUs, 8 GiB, 160 GB disk), Ubuntu 24.04, kernel 6.8, nyc3. `MemTotal` is 7.75 GiB (8,131,792 kB): the hypervisor and kernel take the rest. |
| cgroup | v2 (`cgroup2fs`). Docker 29.9.2, Compose 5.6.1, `default-address-pools` 10.200.0.0/16 in /24s, log rotation 10 MiB × 3. |
| Swap | None. |
| Build | `main` at `be8225f` (`v0.1.0-141-gbe8225f`): CLI, control plane image, and site images built on the host. `pressfleet init` installed the control plane image with the CLI's own version tag, so `--control-plane-image` wasn't needed. Site images for PHP 8.4 and 8.5 only (WordPress 7.1.3). |
| Install | The real `pressfleet init --non-interactive`; all 12 preflight checks passed. |
| TLS and DNS | Option (a): Pebble and its DNS server on the load generator, not the host, so the host ran nothing but PressFleet. Domains are `fleet.138-197-126-181.sslip.io` and `*.preview.138-197-126-181.sslip.io`. Traefik's ACME directory is Pebble's at the load generator's private address. |
| Deviation | A `compose.override.yaml` makes the control plane trust Pebble's root (`SSL_CERT_FILE`), as the end-to-end harness does, since the control plane checks its sites over HTTPS. |
| Load generator | `s-2vcpu-4gb` in the same VPC, running k6 v2.3.0. |
| Config | Defaults, except `backup.schedule.window_start: "18:55"`, `window_hours: 1`, and `backup.check.interval_days: 1`. |

### Workload

- 50 sites in 5 client groups of 10, created through the API. Each has 300 posts, 12 media files (1600×1000 JPEG noise), the platform's default plugins plus Classic Editor, pretty permalinks, and an editor user for wp-admin logins. Each site then had 3 backups, with 5 posts added before each, so the repositories had history.
- Traffic: k6 at a constant 5 requests per second across all sites for 4h15m (`18:11:26` to `22:26:25` UTC), starting after a reboot and a 5-minute idle baseline so that every `memory.peak` started at zero. About 75% cached pages (home page and posts), 22% uncached (search, REST API, an unseen query string), and 3% wp-admin logins.
- The 4h15m is long enough for the backup window (one hour, opening 40 minutes after traffic started), the prunes, and the checks, which start within about two hours of the window closing.
- The thresholds, confirmed with the owner before the run: error rate below 1%, p95 below 500 ms for cached requests and below 2 s for uncached ones.

## Verdict

| Criterion | Result | Evidence |
| --- | --- | --- |
| No OOM kill | **Fail** | Traefik was killed by the kernel 38 times (48 `oom` events), from 19:50:06 to 22:25:11. No other container was killed. |
| No failed healthcheck or container restart | **Fail** | Traefik restarted 38 times. Docker logged 2 `unhealthy` events for it, the first at about 18:14. The platform's own alerts fired for sites' HTTP checks during the outages. The other 106 containers had no restarts. |
| No failed backup, prune, or check job | **Pass** | All 50 scheduled `site.backup` jobs, 5 `backup.prune`, and 5 `backup.check` jobs succeeded; none failed. One `site.wp_cron` job is listed as failed ("never finished"): the reboot before the run interrupted it. |
| Traffic error rate and latency | **Fail** | 78,206 requests, **4.79% failed** (threshold 1%). Cached p95 was 513 ms over the run (threshold 500 ms); uncached p95 was 793 ms (threshold 2 s). |

Failures by phase:

| Phase (UTC) | Requests | Failed |
| --- | --- | --- |
| 18:11 to 18:56, before the backup window | 13,847 | 0 |
| 18:56 to 19:50, backup window open until the first kill | 16,351 | 985 |
| 19:50 to 19:55, first kills, window closing | 1,289 | 158 |
| 19:55 to 22:26, window closed | 46,717 | 2,604 |

The failures are connection errors and timeouts while Traefik was down or thrashing; the first ones came at about 19:46, four minutes before the first kill. Every site stayed `running` throughout; they were unreachable through the edge, not down.

## Traefik

- Idle, with no traffic, Traefik holds about 45 MiB of anonymous memory. At 5 requests per second across the 50 sites it climbs to 80 MiB within two minutes of a restart and to over 90 MiB before the kernel kills it. The kills came every 102 to 495 seconds (median 210) and **stopped as soon as traffic stopped**.
- They did not depend on backups: they continued for 2½ hours after the window closed at 19:55.
- Its `memory.peak` was already 96 MiB (the limit) in the 15-minute dry run, with a median of 80 MiB. The first hour and a half of the measured run had no failures, so the margin was there but thin.
- The cause isn't established. Candidates are the working set for 50 sites' routes and certificates, connection buffers under load, and the startup work after each restart (the log shows "Testing certificate renew" for both ACME resolvers). The run doesn't separate them.

## Measurements

Peaks are the cgroup's `memory.peak` since the container started. They include page cache, which the kernel reclaims, so the maximum anonymous memory sampled (every 5 seconds) is shown as well.

| Service | Limit (MiB) | Peak | Max sampled | Median | Max anon |
| --- | --- | --- | --- | --- | --- |
| MariaDB | 768 | 258 | 256 | 247 | 192 |
| Redis (Valkey) | 320 | 32 | 32 | 28 | 14 |
| VictoriaMetrics | 256 | 155 | 153 | 138 | 126 |
| Control plane | 256 | **256** | 229 | 161 | 124 |
| Traefik | 96 | **96** | 96 | 85 | **92** |
| Socket proxy | 24 | 24 | 24 | 18 | 11 |
| Maintenance page | 24 | 24 | 21 | 11 | 13 |

| Site container (50 each) | Limit (MiB) | Peak: max / p95 / median | Max anon: max / p95 / median |
| --- | --- | --- | --- |
| php | 448 | 166 / 117 / 110 | 73 / 73 / 66 |
| nginx | 64 | 20 / 14 / 13 | 6 / 6 / 6 |

**Control plane.** The cgroup peaked at exactly 256 MiB, but it wasn't killed and its anonymous memory peaked at 141 MiB (`cgmem.sh`). The main process's anonymous peak was 37 MiB; the rest is the `docker` and `compose` processes it starts (up to 18 at once, 443 MiB of RSS together, 226,000 `docker` processes over the run) plus page cache. `GOMEMLIMIT=96MiB` only constrains the Go heap, so these children are what the 256 MiB has to hold. It is a thin margin that this run did not break; it is not confirmed as sufficient.

**Sites.** The php containers' median peak (110 MiB) is under the 128 MiB reservation; the largest (166 MiB) is over it and far below the 448 MiB limit. The nginx containers use about 13 MiB.

**Host.**

| Phase | Min `MemAvailable` (MiB) | Max load1 | PSI memory `some` avg10 max | `full` avg10 max |
| --- | --- | --- | --- | --- |
| Idle after reboot | 5,243 | 23.6 | 0.95 | 0.39 |
| Traffic, before the window | 3,817 | 22.7 | 7.51 | 2.91 |
| Backup window open | 3,748 | 22.3 | 18.78 | 9.01 |
| After the window | 3,706 | 20.3 | 2.01 | 0.80 |
| Traffic stopped | 3,812 | 17.9 | 0.02 | 0.00 |

- The host never ran low on memory. `MemAvailable` stayed above 3.7 GiB, and nothing swapped (there is no swap). The limits add up to nearly all 8 GiB, but they are caps, not reservations, and the whole host used about 4 GiB at its busiest.
- Memory pressure did show up: PSI `some` reached 18.8% and `full` 9% while the backup window was open. This is system-wide, and containers pressed against their own limits (Traefik, the control plane) cause reclaim stalls. This run doesn't tell the causes apart.
- The load average was 18 to 24 on 4 vCPUs through the run. The 10-minute `site.wp_cron` jobs start a task container for every site at about the same time, and the control plane runs `docker` constantly. In the dry run, the slowest requests coincided with these bursts. The vCPUs are shared, so CPU contention may exaggerate the effect, and CPU contention can raise memory peaks too.
- Before the window opened, the slowest request took 19.8 s (an uncached one); the run's slowest took 60 s (k6's timeout), during Traefik's outages. Cached p95 was 812 ms in the first phase, while the cache warmed after the reboot and the wp-cron bursts ran.

## Timeline (UTC)

| Time | Event |
| --- | --- |
| 18:05 | Host rebooted for clean `memory.peak` values; all 107 containers healthy within 80 s |
| 18:06:46 | Collectors started; idle baseline |
| 18:11:26 | Traffic starts |
| 18:55 to 19:55 | Backup window (50 backups, then 20:03 to 21:38 five prunes and five checks, all succeeded) |
| ~19:46 | First request failures |
| 19:50:06 | First Traefik OOM kill; 37 more until 22:25:11 |
| 22:26:25 | Traffic ends; no further kills |
| 23:24 | Collectors stopped |

## Proposed changes (not applied)

Nothing in `platform.go`, the config defaults, or §4.1 has been changed; each change needs the owner's approval.

1. **Traefik's limit needs to go up, by an amount this run doesn't determine.** 96 MiB isn't enough for 50 sites at 5 requests per second. Rerun with a larger limit on the test host only (192 MiB through a compose override) to find the actual peak, then set the default and the §4.1 row from that. The §4.1 table has no room to add 96 MiB: the OS line (48 MiB) would shrink, though the host's 3.7 GiB of free memory shows the caps overstate real use.
2. **Keep the control plane at 256 MiB for now,** but treat it as untested at that limit. Its cgroup reached the limit without a kill. The rerun with a larger Traefik will show whether it stays alive; a longer or busier run might not.
3. **Socket proxy and maintenance page (24 MiB):** their peaks reached 24 MiB (mostly cache); anonymous memory was 11 and 13 MiB. No change proposed.
4. **MariaDB, Redis, and VictoriaMetrics** have a lot of headroom (258 of 768, 32 of 320, 155 of 256). No change proposed.

## Other findings

- **`pressfleet upgrade` refuses a development build.** `v0.1.0-141-g…` sorts before `v0.1.0` as a pre-release, so `upgrade` says it can't upgrade a control plane at its own version. The harness restarts the control plane instead, which works because the config file is bind-mounted. This only affects builds before the first release.
- **A crash loop is not contained.** Docker restarted Traefik 38 times with no backoff beyond its own, and the dashboard and all sites were unreachable for much of the time. The first failure wasn't noticed until the traffic numbers were read. Whether to alert on a container restart is a product decision, not made here.
- **Page cache needs pretty permalinks.** With WordPress's default `?p=ID` URLs posts bypass the cache (visible as 200 to 250 ms "cached" requests in the dry run). The harness sets `/%postname%/`.

## Caveats

- Shared vCPUs on a cloud VM, not bare metal or dedicated CPU; CPU contention is likely higher than on a real 8 GiB host.
- Kernel 6.8: `memory.peak` can't be reset, so the host was rebooted before the run.
- Certificates come from Pebble, and k6 doesn't verify them (it can't load a custom CA).
- k6 samples only at the edge. Server-side timings (Traefik's access log) are in the raw data, not in this report.
- One run. The kill cadence and the 1.5 hours before it make the failure clear, but not how close the margin was before the dry run's 96 MiB peak.
- The control plane's alerts for sites' HTTP checks fired during the outages; their counts aren't tabulated here.
- The raw results (about 80 MB, mostly k6's per-request CSV) are not committed.
