# Capacity test: 50 sites on an 8 GiB host (#98)

**Result: pass, with Traefik's limit raised from 96 MiB to 192 MiB.** The first run, at the shipped defaults, failed: Traefik was OOM-killed 38 times under steady light traffic. The second run, identical except for Traefik's limit, had no OOM kill, no restart, no failed job, and no failed request. The host itself never ran short of memory in either run.

The test follows the plan in the handoff for #98: 50 sites, light traffic, and the backup, prune, and check windows, on a host with 8 GiB of memory. The harness is `scripts/capacity/`; [Testing](testing.md#capacity-test) says how to rerun it.

| Criterion | Run 1: shipped limits | Run 2: Traefik 192 MiB |
| --- | --- | --- |
| No OOM kill | **Fail**: Traefik, 38 kills | Pass |
| No failed healthcheck or container restart | **Fail**: Traefik restarted 38 times; sites' HTTP-check alerts fired | Pass: no restarts, no unhealthy events, no alerts during traffic |
| No failed backup, prune, or check job | Pass: 50 backups, 5 prunes, 5 checks | Pass: 50 backups, 5 prunes, 5 checks |
| Error rate below 1% | **Fail**: 4.79% of 78,206 requests | Pass: 0 of 78,729 |
| p95 below 500 ms cached, 2 s uncached | Cached 513 ms (fail), uncached 793 ms | Cached 19 ms, uncached 326 ms |

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
| Config | Defaults, except the backup window (`window_hours: 1`, opening at 18:55 UTC in run 1 and 00:30 in run 2) and `backup.check.interval_days: 1`. |
| Run 2 only | The same override file sets Traefik's memory limit to 192 MiB and its `GOMEMLIMIT` to 144 MiB, the 75% ratio the rendered file uses (72 of 96 MiB). Set on the test host by an override file; the product default followed in the commit after the report. |

### Workload

- 50 sites in 5 client groups of 10, created through the API. Each has 300 posts, 12 media files (1600×1000 JPEG noise), the platform's default plugins plus Classic Editor, pretty permalinks, and an editor user for wp-admin logins. Each site then had 3 backups, with 5 posts added before each, so the repositories had history.
- Traffic: k6 at a constant 5 requests per second across all sites for 4h15m, after a reboot (so every `memory.peak` starts at zero) and a 5-minute idle baseline. About 75% cached pages (home page and posts), 22% uncached (search, REST API, an unseen query string), and 3% wp-admin logins.
- The backup window opens 40 to 50 minutes after traffic starts. Prune and check start within about two hours after it closes, and the run is long enough to see them.
- Thresholds, confirmed with the owner before the first run: error rate below 1%, p95 below 500 ms for cached requests and below 2 s for uncached ones.

## Run 1: shipped limits (fail)

Traffic ran 18:11:26 to 22:26:25 UTC.

| Phase (UTC) | Requests | Failed |
| --- | --- | --- |
| 18:11 to 18:56, before the backup window | 13,847 | 0 |
| 18:56 to 19:50, backup window open until the first kill | 16,351 | 985 |
| 19:50 to 19:55, first kills, window closing | 1,289 | 158 |
| 19:55 to 22:26, window closed | 46,717 | 2,604 |

- Traefik was killed by the kernel 38 times (48 `oom` events), from 19:50:06 to 22:25:11, every 102 to 495 seconds (median 210). Docker restarted it each time. No other container was killed or restarted.
- The kills did not depend on backups: they continued for 2½ hours after the window closed at 19:55, and **stopped as soon as traffic stopped**. Idle, Traefik holds about 45 MiB of anonymous memory; at 5 requests per second it climbed to 80 MiB within two minutes of a restart and over 90 MiB before each kill.
- The first request failures came at about 19:46, four minutes before the first kill: Traefik was thrashing at its limit. The failures were connection errors and timeouts. Every site stayed `running`; they were unreachable through the edge, not down.
- Docker logged an `unhealthy` event for Traefik at about 18:14, before the failures. Its `memory.peak` was already 96 MiB in the 15-minute dry run, with a median of 80 MiB.
- Host pressure was elevated: PSI memory `some` reached 18.8% and `full` 9.0% while the window was open, against at most 1.1% and 0.35% in run 2. The pressure came from the thrashing, not from the host running short.
- Cached p95 was 513 ms (812 ms in the first phase, while the cache warmed after the reboot). Overall: 78,206 requests, 4.79% failed.

The cause of the high memory use isn't established. Candidates are the working set for 50 sites' routes and certificates, connection buffers under load, the startup work after each restart (the log shows "Testing certificate renew" for both ACME resolvers), and the Go runtime working against `GOMEMLIMIT=72MiB`. Run 2 shows that a larger limit, with `GOMEMLIMIT` scaled to match, is enough; it does not separate these.

## Run 2: Traefik 192 MiB (pass)

Traffic ran 23:41:04 UTC on 22 October to 03:56:04 on 23 October.

- 78,729 requests, **0 failed**. Cached: median 8 ms, p95 19 ms, p99 46 ms. Uncached: median 194 ms, p95 326 ms, p99 431 ms. wp-admin logins: p95 about 600 ms. No threshold was crossed, and the backup window made no visible difference (cached p95 19 ms inside it, 20 ms outside). The slowest single requests took 12 s (cached) and 17 s (uncached); they were isolated.
- Jobs: 50 `site.backup` in the window (00:30 to 01:30), then 5 `backup.prune` and 5 `backup.check` from 01:38 to 03:13, all succeeded. No job failed. The one `site.wp_cron` entry in the failed-jobs list is the job the reboot before run 1 interrupted; it is the same job in both runs.
- Containers: no OOM kill, no restart, no `unhealthy` event, and no kernel OOM message. HTTP-check alerts appeared only in the first two samples (23:36 to 23:37), during the reboot before traffic started, and cleared at once.
- Traefik: anonymous memory plateaued between 116 and 139 MiB for the whole run with no upward trend. Its total (with page cache) sat at 182 MiB median and was pinned at the 192 MiB limit, as the kernel fills spare memory with cache. So its working set under this traffic is about 125 to 139 MiB, which is more than the old 96 MiB limit could hold.

## Measurements (run 2)

Peaks are the cgroup's `memory.peak` since the container started. They include page cache, which the kernel reclaims, so the maximum anonymous memory sampled (every 5 seconds) is shown as well. Run 1's values were within a few MiB for everything except Traefik.

| Service | Limit (MiB) | Peak | Max sampled | Median | Max anon |
| --- | --- | --- | --- | --- | --- |
| MariaDB | 768 | 269 | 268 | 258 | 204 |
| Redis (Valkey) | 320 | 33 | 32 | 28 | 15 |
| VictoriaMetrics | 256 | 160 | 155 | 142 | 129 |
| Control plane | 256 | **256** | 224 | 166 | 131 |
| Traefik | 192 (default 96) | 193 | 192 | 182 | 139 |
| Socket proxy | 24 | 24 | 23 | 19 | 10 |
| Maintenance page | 24 | 21 | 14 | 7 | 15 |

| Site container (50 each) | Limit (MiB) | Peak: max / p95 / median | Max anon: max / p95 / median |
| --- | --- | --- | --- |
| php | 448 | 160 / 119 / 110 | 75 / 72 / 60 |
| nginx | 64 | 21 / 14 / 12 | 10 / 6 / 6 |

**Control plane.** Its cgroup peaked at exactly 256 MiB in both runs and was never killed. `cgmem.sh` (sampling every 50 ms) shows 252 MiB of usage and 158 MiB of anonymous memory at the peak in run 2 (141 MiB in run 1). The main process's anonymous peak is only 38 MiB; the rest is the `docker` and `compose` processes it starts (up to 18 at once, 464 MiB of RSS together; 213,000 `docker` processes over the run) and page cache. `GOMEMLIMIT=96MiB` constrains only the Go heap, so these children are what the 256 MiB has to hold. The limit held through two runs of 4h15m, but the margin on anonymous memory is about 100 MiB, so it is the next-tightest limit after Traefik's.

**Sites.** The php containers' median peak (110 MiB) is under the 128 MiB reservation; the largest (160 MiB) is over it and far below the 448 MiB limit. The nginx containers use about 12 MiB.

**Host (run 2).**

| Phase | Min `MemAvailable` (MiB) | Max load1 | PSI memory `some` avg10 max | `full` avg10 max |
| --- | --- | --- | --- | --- |
| Idle after reboot | 5,262 | 22.6 | 0.03 | 0.00 |
| Traffic, before the window | 3,741 | 21.6 | 1.09 | 0.28 |
| Backup window open | 3,674 | 19.5 | 0.96 | 0.28 |
| After the window (prune and check) | 3,625 | 18.2 | 1.05 | 0.35 |
| Traffic stopped | 3,836 | 13.8 | 0.57 | 0.14 |

- The host never ran low on memory in either run: `MemAvailable` stayed above 3.6 GiB and nothing swapped (there is no swap). In run 1 the containers' anonymous memory summed to at most 1.25 GiB, and the host used about 4.2 GiB including page cache and tmpfs. The limits add up to nearly all 8 GiB, but they are caps, not reservations.
- The load average was 14 to 23 on 4 vCPUs through both runs: the 10-minute `site.wp_cron` jobs start a task container for every site at about the same time, and the control plane runs `docker` constantly. The vCPUs are shared, which may exaggerate CPU contention compared with a dedicated host.
- Task containers (backup, prune, check) are too short-lived for the 5-second sampling; their individual peaks were not captured. Prune and check ran on repositories of 10 sites with a handful of snapshots each, far below the sizes where restic's memory matters, so this run says nothing about the 512 MiB task limit at scale. `scripts/memory/restic-bench.sh` covers that.

## Timeline (UTC)

| Time | Run 1 | Run 2 |
| --- | --- | --- |
| Reboot, idle baseline | 18:05, 18:06:46 | 23:35, 23:36 |
| Traffic starts | 18:11:26 | 23:41:04 |
| Backup window | 18:55 to 19:55 | 00:30 to 01:30 |
| Prune and check | Between 20:03 and 21:38 | Between 01:38 and 03:13 |
| First Traefik kill | 19:50:06 (38 kills until 22:25:11) | None |
| Traffic ends | 22:26:25 | 03:56:04 |

## Changes

1. **Done:** Traefik's default is now 192 MiB with `GOMEMLIMIT` 144 MiB (`DefaultTraefikMemoryMB` in `platform.go`; the Go limit is 75% of it), and §4.1 shows 192 MiB.

   Basis: at 192 MiB, 50 sites at 5 requests per second held steady for 4h15m with anonymous memory peaking at 139 MiB, 72% of the limit. 96 MiB was not enough. Whether something between (for example 160 MiB) would do wasn't tested; the anonymous peak leaves only 21 MiB under 160 MiB, so I wouldn't go lower without another run. More sites' routes or more traffic may need more.
2. **§4.1 arithmetic:** the table's limits summed to 8,192 MiB (50 × 128 for sites, 768, 320, 256, 256, 96, 24, 24, and 48 for the OS). With Traefik at 192 MiB they sum to 8,288 MiB, so the "left of 8 GiB" line is now −48 MiB. The measured use is far below either, so the table now says the limits exceed 8 GiB by 48 MiB and that they are caps, not reservations. `pressfleet doctor`'s `platformMemory` budget was a hand-summed copy of the same limits (1792 MiB, still counting Traefik at 96 MiB) when this test ran; it is now derived from the `platform.go` limits plus a 256 MiB OS reserve, so an 8 GiB host reports about 47 sites (45 at 7.75 GiB).
3. **Keep the control plane at 256 MiB.** It held, but with about 100 MiB of margin on anonymous memory, mostly because of the `docker` and `compose` children. A longer run or a heavier site count could narrow it, and `GOMEMLIMIT` can't help with the children. This closes #192's cgroup v2 check with that caveat.
4. **No change** for the socket proxy and maintenance page (24 MiB; anonymous memory 10 to 15 MiB), MariaDB (269 of 768 MiB), Redis (33 of 320), or VictoriaMetrics (160 of 256).

## Other findings

- **`pressfleet upgrade` refuses a development build.** `v0.1.0-141-g…` sorts before `v0.1.0` as a pre-release, so `upgrade` says it can't upgrade a control plane at its own version. The harness restarts the control plane instead, which works because the config file is bind-mounted. This only affects builds before the first release.
- **A crash loop is not contained.** In run 1 Docker restarted Traefik 38 times, and the dashboard and all sites were unreachable for much of the time. Nothing in the platform stopped it, and the only alerts were the sites' HTTP checks. Whether to alert on a container restart is a product decision, not made here.
- **Page cache needs pretty permalinks.** With WordPress's default `?p=ID` URLs posts bypass the cache (visible as 200 to 250 ms "cached" requests in the dry run). The harness sets `/%postname%/`.

## Caveats

- Shared vCPUs on a cloud VM, not bare metal or dedicated CPU.
- Kernel 6.8: `memory.peak` can't be reset, so the host was rebooted before each run.
- Certificates come from Pebble, and k6 doesn't verify them (it can't load a custom CA).
- k6 measures at the edge only. Server-side timings (Traefik's access log) are in the raw data, not in this report.
- One run at each setting. The load is light by design (5 requests per second); heavier traffic, more sites, or larger repositories could change every number here.
- The raw results (about 400 MB for both runs, mostly k6's per-request CSV) are not committed.
