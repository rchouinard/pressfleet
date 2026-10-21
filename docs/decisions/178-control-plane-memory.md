# #178: Control plane memory limit vs. the work it runs

Investigation for [#178](https://github.com/rchouinard/pressfleet/issues/178): the control plane is OOM-killed at its 192 MiB default. This note records what was measured, what each option buys, what option 3 (group-level restic in task containers) would take, and a recommendation. **The decision is the owner's to make**; nothing here changes product code.

## Summary

- **The 192 MiB limit is already exceeded by ordinary nightly work, before repository size matters at all.** Eight backups at the default `jobs.concurrency: 4` peaked at **206 MiB charged, 196 MiB anonymous** against repositories holding a few snapshots each. Each backup job runs two restic commands *inside* the control plane (`cat config` to probe, `snapshots` to check for an earlier attempt). Each costs at least 38 MiB, because every restic command derives the repository key with scrypt (32 MiB at this repository's parameters). Provisioning at concurrency 4 peaked at 160 MiB charged, 146 MiB anonymous; one login burst (about +60 MiB) on top of that is the #176 OOM.
- **Repository-size-dependent restic is worse, and it doesn't wait for the night.** On a synthetic group of 50 sites × 90 daily snapshots (4,500 snapshots, 622,000 blobs), `check` peaks at 105 MiB, `prune` at 138 MiB, and, once a prune has rewritten the index, `check` and `stats --mode raw-data` at about 150 MiB. The storage metrics collector runs that `stats` in-process for every group every 30 minutes and at every start.
- **`docker compose` costs about 18 MiB per concurrent invocation**, not 60. One invocation's processes show about 60 MiB of RSS (docker CLI 26 + compose plugin 31–34), but the binaries' pages are shared and charged once. The cost is linear: 17, 36, 71, 142 MiB at 1, 2, 4, 8 invocations at once.
- **`GOMEMLIMIT` at 85% of the limit does nothing**: the Go process never gets that close. Set to what the Go process should get, it works: 96 MiB caps a 12-login burst at 96 MiB (from 104–131); 64 MiB caps 12 or 48 logins at 64 MiB, but 48 logins then take 5.5 s instead of about 1.7 s. It doesn't help the docker CLI children, which inherit it (no change at 160 or 16 MiB).
- **Recommendation:** option 3 for *all* group-level restic, so the control plane never runs restic, together with option 4 (`GOMEMLIMIT=96MiB`) and a small option 1 (**256 MiB** default). Keep `jobs.concurrency: 4`; option 2 isn't needed. Until option 3 lands, ship 384 MiB with `GOMEMLIMIT=96MiB`. Give task containers a memory limit too: they have none today, and one nightly backup task on the synthetic group uses 160 MiB.
- **Side finding, a data-loss bug:** concurrent first backups of a new client group race `restic init` and can leave the repository permanently unreadable (14 of 20 trials with plain restic, and once in the e2e harness). See [the init race](#side-finding-concurrent-restic-init-breaks-new-repositories). It needs its own issue, whatever is decided here.

## Environment, and how far to trust the numbers

| | |
| --- | --- |
| Sandbox | Firecracker VM, 4 vCPUs, 16 GiB, Linux 6.18 |
| Docker | Engine 29.9.1, `cgroupfs` driver, **cgroup v1** (hybrid: the memory controller is on v1; the unified hierarchy has only `hugetlb`). The VM boots that way and can't be switched to v2 without a reboot with `cgroup_no_v1`, so **these numbers are cgroup v1 too**, like #176's. |
| Control plane image | This branch's code, built on the host and wrapped in a sandbox-only Dockerfile that adds the proxy CA, because the repository's Dockerfile can't fetch through the sandbox's TLS proxy. Same final stage (Debian 13 slim, docker CLI 29.8.1 with its Compose plugin, restic 0.19.1). |
| Sites | `site-php:php8.5-wp7.1.2`, `site-nginx:wp7.1.2` from GHCR |

How to read the columns below:

- **charged** is the kernel's peak charge for the container: `memory.max_usage_in_bytes`, reset at the start of each measurement (v1), or `memory.peak` (v2). It counts anonymous memory, page cache, and kernel memory, and it is what the limit is compared with.
- **anon** is the highest anonymous memory sampled (`total_rss` on v1, `anon` on v2). Page cache is reclaimed before an OOM kill, so anon is what decides whether one happens. Both cgroup versions charge page cache to the cgroup that first reads it. A freshly created control plane container therefore gets charged for the docker binaries' pages when it first runs them (10–60 MiB here, all reclaimable). That is most of the gap between the two columns, and why the same round charged different amounts in different runs.
- **Go** is the control plane process's own peak `RssAnon`. **procs** is the most docker, compose, restic, and mariadb-dump processes alive at once.

How far to trust them:

- **Anonymous memory and per-process figures don't depend on the cgroup version**: they are the processes' own memory. Charged figures should be close on v2 for the same work. v2 additionally charges a little more kernel memory (socket buffers), and its page cache reclaim differs, which only moves the reclaimable part. **Expect v2 numbers within ±10% of these; rerun the scripts below on the #98 host to confirm.**
- **Peaks are sampled every 20 ms**, so short-lived processes are slightly under-counted. Treat them as lower bounds, to within a few MiB.
- **CPU contention raises memory.** A rerun while the synthetic repository was being built (all 4 CPUs busy) gave 15–45 MiB higher peaks, because docker CLI calls ran longer and overlapped more. A real 8 GiB host with 50 sites is busier than this sandbox, so the uncontended numbers below are the optimistic case.
- **Eight sites, not 50.** Each metrics collector visits sites one after another, so a 50-site host has about the same number of collector processes at once, just for longer.
- **The synthetic repository is shaped, not sized, like the real thing** (see [restic](#restic-against-a-group-with-realistic-history)). restic's index memory follows the number of blobs, and 622,000 is a plausible group. The data, though, is 3.7 GiB instead of tens, so there are fewer pack files than a real repository would have, and prune's repacking and `check --read-data-subset` are under-measured. Above the per-command floor, memory grew by about 0.19 KiB per blob. Linear extrapolation (not measured) puts a 2-million-blob group near 400 MiB.
- **The key derivation floor depends on the host.** restic calibrates scrypt at `init` to take about 0.5 s, up to 60 MiB. The repositories here got N=32768, r=8 (32 MiB). On a faster server it is likely the 60 MiB cap, raising the floor of *every* restic command from about 38 to about 66 MiB anonymous. This is probably why #176 saw about 90 MiB for `restic init`.

## Measurements

The workloads ran in the e2e harness with the control plane's limit at 4096 MiB (`e2e.StartWithControlPlaneMemory`). A throwaway test, not committed, restarted the control plane with `PRESSFLEET_JOBS_CONCURRENCY` and `GOMEMLIMIT` for each round and ran [`scripts/memory/cgmem.sh`](../../scripts/memory/cgmem.sh) on the control plane container. Each restart recreates the container, so its cgroup starts fresh.

### The control plane at rest

| Situation | charged | anon | Go | procs |
| --- | ---: | ---: | ---: | ---: |
| Just started, no sites | 52 | 50 | 50 | 0 |
| Just restarted with 8 sites (every collector starts at once), 90 s | 101 | 92 | 53 | 6 |
| Steady state between jobs (from the delete rounds) | — | — | 23–27 | — |

The Go process settles at about 25 MiB and peaks near 50–55 MiB around startup and during jobs. Every restart starts all the metrics collectors at once, with a burst of docker CLI processes: the container stats collector `docker exec`s into each site, and the wp-content collector runs a `du` task per site.

### Provisioning at concurrency 1, 2 and 4

Eight sites in one client group, created at once; then deleted at once.

| `jobs.concurrency` | charged | anon | Go | procs | Wall time (8 sites) |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 118 | 107 | 54 | 9 | 126 s |
| 2 | 124 | 112 | 54 | 10 | 66 s |
| 4 | 160 | 146 | 54 | 12 | 42 s |
| 4 (earlier run) | 141 | 126 | 54 | 13 | 58 s |
| 4 with `GOMEMLIMIT=163MiB` (earlier run) | 139 | 128 | 54 | 13 | 58 s |
| Delete, 1 / 2 / 4 | 47 / 61 / 86 | 40 / 53 / 77 | 24–26 | 2 / 4 / 8 | 12 / 8 / 6 s |

The busiest provisioning process was the compose plugin at 33–34 MiB RSS, and the docker CLI at 27 MiB. Even at concurrency 1, nine processes ran at once, because the collectors kept running beside the job. Going from 1 to 4 adds about 40 MiB and makes bulk provisioning three times faster.

### `docker compose` invocations, with and without `GOMEMLIMIT`

[`scripts/memory/compose-bench.sh`](../../scripts/memory/compose-bench.sh) runs N small two-service projects through a site's lifecycle at once (`up --wait`, `ps`, `exec`, `down`). It runs them in a container from the control plane image, with the variables the docker CLI would inherit from the control plane.

| At once | none | `GOMEMLIMIT=160MiB` | `GOMEMLIMIT=16MiB` | `GOGC=50` |
| ---: | ---: | ---: | ---: | ---: |
| 1 | 17 | 18 | 17 | 17 |
| 2 | 36 | 37 | 39 | 31 |
| 4 | 71 | 70 | 72 | 62 |
| 8 | 142 | 142 | 143 | 121 |

These are charged MiB (anon is 2–10 MiB lower). One invocation costs about 18 MiB against the limit, growing linearly; #176's 60 MiB is one invocation's RSS, which counts the shared binaries. `GOMEMLIMIT` doesn't change the docker CLI at any setting, because its heap is small and short-lived. `GOGC=50` saves about 15%.

### Login bursts, with and without `GOMEMLIMIT`

Concurrent password logins against a control plane that had just started (argon2id at 19 MiB per hash, at most two at once: `auth.maxConcurrentHashes`).

| Logins | `GOMEMLIMIT` | charged | Go | Wall time |
| ---: | --- | ---: | ---: | ---: |
| 12 | none | 115 | 104 | 0.6 s |
| 12 | none (another run) | 125 | 122 (anon) | 0.7 s |
| 12 | 163 MiB (85% of 192) | 107–133 | 104–131 | 0.6–0.9 s |
| 12 | 96 MiB | 100 | 96 | 0.7 s |
| 12 | 64 MiB | 69 | 59 | 1.0 s |
| 48 | none | 121 | 118 (anon) | 1.6 s |
| 48 | 163 MiB | 123 | 120 (anon) | 1.8 s |
| 48 | 64 MiB | 70 | 64 | 5.5 s |

The semaphore bounds live memory at about two hashes, but with the default `GOGC=100` the heap grows to about twice its live size between collections, hence 100–130 MiB. `GOMEMLIMIT` near the container limit never comes into play; set near what the process should use, it holds. 96 MiB cost nothing measurable; 64 MiB triples the time of a large burst.

### Backup bursts

Eight sites in two client groups, each group's repository created first by one backup at a time ([see the init race](#side-finding-concurrent-restic-init-breaks-new-repositories)). Then all eight were backed up at once, twice. The repositories hold a handful of snapshots, so this is the size-independent cost.

| `jobs.concurrency` | charged | anon | Go | procs | restic in the control plane |
| ---: | ---: | ---: | ---: | ---: | --- |
| 1 | 113 | 102 | 27 | 7 | 16 runs, 58 MiB RSS each |
| 4 | 186 / 206 | 177 / 196 | 26–27 | 6–7 | 16 runs, 58 MiB RSS each |
| 4 with `GOMEMLIMIT=163MiB` | 185 | 175–176 | 25 | 6–10 | 16 runs |

**This is over the 192 MiB limit with nothing else going on.** Per backup, the control plane runs `restic cat config` (`repository.Manager.probe`, from `Ensure` in `TaskAccess`), `restic snapshots --tag site:…,backup:…` (`Engine.find`), and `mariadb-dump` (14 MiB), plus the docker CLI that drives the task. The Go process stays at 27 MiB, so `GOMEMLIMIT` can't help here.

### restic against a group with realistic history

[`scripts/memory/restic-bench.sh`](../../scripts/memory/restic-bench.sh) `build` simulates one client group of 50 sites backed up daily for 90 days, using restic's own options (`--time`, `--host site-<id>`, the backup engine's tags) and no retention. Each site has 12 plugins hard-linked from a shared pool of 40, in three versions, so sites deduplicate as they do for real; plugins are updated about once a month. Each site also has 9,000 uploads over three years plus 20 a day, and a 4 MiB database dump with a few regions rewritten and rows appended daily. Files are 0.2–6 KiB of words from a fixed vocabulary, one blob each. The result: **4,500 snapshots, 621,925 blobs, 3.7 GiB**.

`measure` then runs each command in a fresh container, with `--no-cache` as the control plane does. The forget and prune runs use a hard-linked copy of the repository.

| Command (used by) | charged | anon | restic RSS | Wall time |
| --- | ---: | ---: | ---: | ---: |
| `cat config` (`probe`, every backup) | 39 | 38 | 57 | 0.7 s |
| `unlock` (before prune, check, forget) | 39 | 37 | 57 | 0.6 s |
| `snapshots --json` (all, or one site's tag) | 55 | 52 | 73 | 0.8 s |
| `forget` with the default policy (all 4,500 snapshots) | 50 | 48 | 68 | 1.1 s |
| `dump` of one site's database (export) | 95 | 93 | 113 | 1.8 s |
| `stats --mode raw-data` (storage collector) | 99 | 96 | 117 | 24 s |
| `prune` with nothing to remove (dry run) | 99 | 97 | 117 | 13 s |
| `check` | 109 | 105 | 127 | 9 s |
| `check --read-data-subset=10%` (the default) | 117 | 116 | 136 | 14 s |
| `prune` after `forget` (removes 3,900 of 4,500 snapshots) | 696 | **138** | 158 | 59 s |
| *After that prune:* `check` | 154 | **152** | 172 | 4.5 s |
| *After that prune:* `stats --mode raw-data` | 152 | **150** | 169 | 7.3 s |
| *After that prune:* `snapshots --json` (600 left) | 45 | 39 | 59 | 0.6 s |
| `GOGC=20`: check / check 10% / prune dry run / prune | 90 / 99 / 83 / 688 | 89 / 96 / 81 / 117 | | +2–15% |

The size-independent floor is about 38 MiB: the scrypt key derivation, plus the Go runtime. Above it, memory follows the index. `prune` rewrites the index into fewer, larger files; once it has, `check` and `stats` decode those in one piece and peak about 45 MiB higher. That pruned shape is the steady state of every repository after its first retention run. The prune's 696 MiB charge is page cache from the repacked files: reclaimable, but under a tight limit it means writeback stalls. `GOGC=20` saves 15–20% of anonymous memory for up to 15% more time.

For comparison, a **site backup task** (`restic backup` of one site, run as today in a task container with `HOME` on a tmpfs) on this repository peaked at **92 MiB anonymous plus 68 MiB of cache in the tmpfs: 160 MiB charged**. Without a cache it was 98 MiB. That memory is shmem, which can't be reclaimed without swap. Task containers have no memory limit (`taskCreateArgs` passes no `--memory`), and the §4.1 table has no line for them.

### Task container start latency

A `RunTask`-style lifecycle (`docker container create` with the task flags, `start --attach`, `inspect`, `rm --force`) of a command that exits at once took **0.39–0.53 s, median 0.46 s**, over 20 runs. Connecting to the backup network for S3 adds one more docker CLI call. An in-process exec has no such overhead.

## What runs inside the limit, by the numbers

Anonymous MiB, at the defaults, uncontended:

| Consumer | Typical | Peak seen |
| --- | ---: | ---: |
| Control plane Go process | 25 | 54 in jobs; 104–131 in a login burst without `GOMEMLIMIT` |
| Metrics collectors' docker CLI (8 sites, after a restart) | — | about 40 |
| Each concurrent `docker compose` | 18 | — |
| Provisioning at concurrency 4 (children only) | — | about 92 |
| Each in-process restic, any repository | 38 (up to 66 on a fast host) | — |
| Each in-process restic, 622k-blob group (stats, check, prune) | — | 96–152 |
| Backups at concurrency 4 (children only, small repositories) | — | about 170 |

Combinations that happen without anyone doing anything unusual:

- **Backup window at concurrency 4:** 196 measured, before the size-dependent part. Add the 30-minute `stats` of one 622k-blob group (150) and it is near 350.
- **Maintenance after the window:** prune and check jobs of different groups run in parallel, up to `jobs.concurrency`. Four at about 150 each is 600 MiB, in-process.
- **Dashboard work:** provisioning at concurrency 4 (146) plus a login burst (+60–80) is 210–225, the #176 OOM.

## Options

### Option 1: raise the default limit

| Default | Covers | Doesn't cover |
| --- | --- | --- |
| 384 MiB | backup window at concurrency 4 (196); provisioning with a login burst (about 225); one 622k-blob `stats` or `check` beside either | concurrent maintenance of several large groups (4 × 150); groups past about a million blobs; fast hosts (the scrypt floor rises to 66) |
| 512 MiB | the above with more headroom | the same, later |

It buys time and is one constant (`platform.DefaultControlPlaneMemoryMB`) plus the design doc. But it doesn't bound anything: the size-dependent part grows with every group's history. And an OOM still takes down the API, the dashboard, the job runner, and the alert engine together. Cost: +192 or +320 MiB from the §4.1 budget (see [capacity](#effect-on-the-41-capacity-table)).

### Option 2: lower the default `jobs.concurrency`

At 2, provisioning peaks 36 MiB lower than at 4 (124 vs 160 charged) and takes 57% longer (66 s vs 42 s for 8 sites); at 1 it is three times slower (126 s). Backups at concurrency 1 peaked at 113 MiB vs 206 at 4, and took 2.4 times longer (24 s vs 10 s for 8 small sites; real backups take minutes each). So option 2 trades a real amount of memory for a real amount of time. The 50-site nightly backup window, which repositories' maintenance has to wait for, stretches with it. It does nothing about repository-size-dependent restic, and even at concurrency 1 a single large group's `stats` or `check` (150) plus the collectors (40) and the Go process (25–54) needs 215–245 MiB. **Not recommended.**

### Option 3: group-level restic in task containers

What it buys: everything in the restic rows above leaves the control plane. Without in-process restic, the backup window at concurrency 4 would need the Go process, `mariadb-dump`, and the docker CLI driving each task: an estimated 100–120 MiB instead of 196, to be confirmed once built. The size-dependent part gets its own limit, and exceeding it fails one job instead of killing the control plane. The memory doesn't disappear, it moves: prune, check, and stats run as tasks of about 100–160 MiB (622k blobs). That has to be budgeted, as backup tasks already should be. The workload is detailed [below](#option-3-workload).

### Option 4: `GOMEMLIMIT` on the control plane

It only bounds the Go process, and only when set to the share the Go process should get rather than 85% of the container (163 MiB of 192 was never reached). At 96 MiB a login burst stays at 96 MiB with no measurable slowdown; at 64 MiB, at 60–64 MiB, with a large burst three times slower. The docker CLI inherits it from the control plane's environment (`compose.DockerExec` doesn't set `cmd.Env`), harmlessly; restic doesn't (`repository.Manager.exec` builds its environment from `inheritedEnv`). It's cheap and worth doing whatever else is chosen, but on its own it doesn't touch the backup window, which is all child processes. A related tweak: `GOGC=50` for the docker CLI children saves about 15% of what they use, for no measurable time.

### Effect on the §4.1 capacity table

The table's lines and the limits the platform actually sets have already drifted apart (`platform.go`):

| Consumer | Table | Limit today | Option 1 (384) | Recommended (256) |
| --- | ---: | ---: | ---: | ---: |
| Sites (50 × 128 MiB reservation) | 6400 | 6400 | 6400 | 6400 |
| MariaDB | 768 | 768 | 768 | 768 |
| Redis (`maxmemory`; container limit +64) | 256 | 320 | 320 | 320 |
| VictoriaMetrics | 256 | 256 | 256 | 256 |
| Traefik + control plane | 256 | 96 + 192 = 288 | 96 + 384 = 480 | 96 + 256 = 352 |
| Socket proxy, maintenance page | (from headroom) | 24 + 24 | 48 | 48 |
| **Left for the OS and headroom** | 256 | **112** | **−80** | **48** |
| Backup and maintenance tasks (transient, no line today) | — | unbounded | unbounded | `jobs.concurrency` × task limit |

At 384 MiB the limits add up to more than 8 GiB; at 256 MiB, 48 MiB is left. Both work only because limits aren't reservations: sites reserve 128 MiB and burst, and platform services rarely sit at their limits. Task containers already sit outside the table, with nothing bounding them, and four nightly backup tasks on 622k-blob groups take about 640 MiB. Whatever is chosen, the table should list the real limits and a line for tasks, taken from site burst capacity at night, and the #98 capacity test should run through a backup window and a maintenance window, not just traffic.

## Option 3 workload

### Call sites

The restic call sites go through two narrow interfaces: `restic.Repositories` (`Restic`, `ResticTo`, `TaskAccess`) in `backup/restic/engine.go`, and `metrics.ResticRunner` in `metrics/storage.go`. Both are implemented by `*repository.Manager`. **So the change is concentrated in `backup/repository`, and most callers don't change.**

| Where | Today | Change |
| --- | --- | --- |
| `repository/restic.go` `run`, `runTo`, `exec` | `exec.CommandContext(restic)` with a clean environment | Build a `provisioner.Task` instead: `Command: restic --no-cache …`, `Env`/`SecretEnv` as `TaskAccess` already builds them, `RepositoryDir` for local repositories, `BackupNetwork` for S3. `runTo` streams through `Task.Stdout`, which exists. Map `*provisioner.ExecError.ExitCode` 10/11/12 to `errRepositoryMissing`/`ErrLocked`/`ErrWrongPassword`, as `exec` maps `*exec.ExitError` now. |
| `repository.go` `Restic`, `ResticTo` | in-process | Through the task runner; signatures unchanged. |
| `repository.go` `initialize`, `probe` (`init`, `cat config`) | in-process, on every backup | `init` in a task. The probe shouldn't run on every backup: the repository record can say it was initialized (or a failed `restic backup`'s exit code 10 can trigger `Ensure`). Fix the init race in the same place ([below](#side-finding-concurrent-restic-init-breaks-new-repositories)). `shareLocal` stays in the control plane (it's chown and chmod). |
| `export.go` `Export`'s `restic check` of the copy | in-process, against a full copy | A task mounting the export directory. That needs the destination under the data directory (the only path that is the same on the host and in the control plane), so `pressfleet repository export` would stage there. |
| `restic/engine.go` `snapshots` (backup idempotency, `ListSnapshots`, `resolveSnapshot`) | `Restic` | No change. Only run `find` when the job's step is a retry (an earlier attempt may have finished), which saves a task per backup. |
| `restic/forget.go` `ForgetSite`, `restic/maintain.go` `Prune`, `Check`, `unlock`, `GroupSnapshots` | `Restic` | No change. Several commands per job become several tasks (0.5 s each). `Prune`'s `unlock` + `forget` + `prune` could become one task (`sh -c`) later if that matters. |
| `restic/dump.go` `DumpFiles`, `DumpDatabase` | `ResticTo` | No change. Works for deleted sites, which is why it can't be a site task: the group-level task needs no site. |
| `repomaint` | jobs calling `Engine.Prune`/`Check` | No change; the lock key `repo:<group>` keeps serializing them. |
| `metrics/storage.go` `RepositoryCollector` | `Restic` (`stats --mode raw-data`) | No change, through the interface. Because `stats` takes a lock without `--retry-lock`, it keeps failing (and is retried next run) while a prune holds the exclusive lock. |
| `cli/repository.go` `backupRepositories` | `repository.New` without a driver | Pass the driver's task runner. `pressfleet repository …` commands run in the control plane container, which has the driver. |

### A group- or platform-level task in the provisioner

`RunTask` is keyed by site because it borrows the site's network, UID, and wp-content volume. Group tasks need none of those. Proposed, in `provisioner/driver.go`:

```go
// RepositoryTaskRunner runs tasks against a client group's backup
// repository, with no site's data: maintenance (forget, prune, check),
// measurement, and reads of snapshots, including of deleted sites.
type RepositoryTaskRunner interface {
	// RunRepositoryTask runs task for the client group and waits for it.
	// task.WPContent must be MountNone; RepositoryDir, BackupNetwork,
	// Env, SecretEnv, Stdout, Timeout, and MemoryLimitMB apply as in
	// RunTask. A non-zero exit yields an *ExecError.
	RunRepositoryTask(ctx context.Context, clientGroupID string, task Task) (*TaskResult, error)
}
```

`Driver` embeds it, and `provisionertest.Fake` implements it. `Task` gains `MemoryLimitMB` (zero: the driver's default), used by both runners, so backup tasks get a limit too.

In `compose/task.go`, the work is mostly a refactor. `taskSpec` and `taskCreateArgs` already separate validation from argv; split the site part (`taskSiteAccess`, `attachShared`, `--network <site>`, `--user <site uid>`, wp-content) from the rest. A repository task:

- is named `pf-group-<id>-task-<random>`, with `pressfleet.task=true` and `pressfleet.client-group-id=<id>`, so the existing leftover cleanup by `pressfleet.task` finds it;
- runs as `RepositoryGID:RepositoryGID` (60000), not root and not a site's UID. Local repositories are `root:60000` with setgid directories and group bits set by `shareLocal`, and restic derives new files' modes from `config`'s group bits. Prune can therefore delete and write in the repository, and site tasks can still use it;
- keeps the rest of the hardening: `--read-only`, `--tmpfs /tmp` as `HOME`, `--cap-drop ALL`, `no-new-privileges`, `--init`, `--log-driver none`, plus `--memory`.

For Kubernetes, it maps to a Job in the platform's namespace, mounting the repositories' PVC (local) or getting the S3 credentials from a Secret, with no site PVC and a NetworkPolicy that allows egress to the S3 endpoint only. Nothing in the interface is Compose-specific.

### How the backends reach the repository

- **Local:** `RepositoryDir` is the group's directory under `<data dir>/backups/repositories/`, bind-mounted at `/repository` as site tasks do now. It relies on the data directory being at the same path on the host and in the control plane, which `dirUnder` already checks. Network: `--network none`.
- **S3:** credentials through `SecretEnv` (the env-file) as `TaskAccess` builds them, and `RESTIC_REPOSITORY=s3:…`. A self-hosted service lives on the platform's internal backup network, which `connectBackupNetwork` joins already. A remote one (AWS and the like) needs a route out. Site tasks get it from the site network; a repository task shouldn't join the platform network, where MariaDB, Redis, and the control plane's API live. The driver would create one more network on demand, as it does site networks: a plain bridge with nothing else on it, for repository tasks only. With `--gw-priority` it is the default route, and the backup network is joined beside it.

### Locks and concurrency

- **Job-level serialization doesn't change.** Prune, check, offboarding, and repository maintenance share `repomaint.LockKey`; backups and restores meet them at restic's lock with `--retry-lock` (`LockWait`, 30 minutes).
- **restic's locks work across containers** as they do now between the control plane and backup tasks. A lock is stale when it is older than 30 minutes without a refresh, or when it was taken on the same host by a process that no longer exists. Each task container has its own host name, so only the age rule applies across them. A task killed by its timeout, its context, or an OOM leaves a lock that `restic unlock` (run before prune, check, and forget) won't remove for up to 30 minutes, and the next exclusive operation waits up to `LockWait`. That's today's behavior for backup tasks. **Don't give tasks a shared host name**: restic would then judge other live containers' locks by PIDs in its own namespace, and could remove a held lock.
- **Bound repository tasks across the platform.** With option 3, four maintenance jobs at once mean four task containers of 100–160 MiB (622k blobs) on the host, at night beside the backup tasks. A small semaphore in the repository manager (proposed: 2 at once, configurable) bounds that independently of `jobs.concurrency`.

### Latency

Each call gains about 0.5 s (0.39–0.53 s measured), or about 0.6 s on S3 with the backup network connected:

- **Backups:** +1 s each with today's two calls, or none if the probe and the idempotency listing leave the hot path as above.
- **Restore requests** (`sites/restore.go` `findSnapshot`, which lists snapshots inside the API request before answering `202`): +0.5 s.
- **Prune and check jobs:** +1–1.5 s on jobs that take seconds to minutes.
- **The storage collector:** +0.5 s per group every 30 minutes.

None of these is on a page load. Keeping `snapshots` in-process would save the 0.5 s on restores, at 39–55 MiB a call; with the floor at 66 MiB on fast hosts, and restores rare, moving it too is simpler: "the control plane never runs restic".

### Tests

- `compose`: unit tests of the repository task's argv: no site network, `--network none` or the task network plus the backup network, user and group, labels, mounts, `--memory`, refusal of wp-content. A Docker test (`task_docker_test.go`) runs `restic init`, `snapshots`, and `prune` in a repository task against a local repository shared with a site task (ownership and modes), and with S3 on the backup network.
- `provisionertest`: `RunRepositoryTask` on the fake, with call recording like `RunTask`'s.
- `repository`: `Restic`/`ResticTo` through a fake runner (environment, secrets only in `SecretEnv`, exit code mapping to sentinels, streaming); `Ensure` concurrency (with the init race fix); `Export` staging. Existing tests that run real restic keep doing so through a runner that execs locally, so they don't need Docker.
- `restic`, `repomaint`, `metrics`: unchanged behavior against fakes; small updates where tests assert in-process execution.
- e2e: `TestSiteBackup`, `TestSiteBackupS3`, `TestSiteRestore`, `TestSiteExport`, and the maintenance jobs already cover the paths end to end. Add one assertion that no restic process ran in the control plane container during a backup, prune, and check (it can't be read from the API; `docker top` from the harness can), and run the dashboard suite at the default limit, removing `e2e.StartWithControlPlaneMemory` from `e2e/dashboard/dashboard_test.go`.
- Both state databases where records change (the initialized marker); archtest stays as it is, since no new code runs `docker`.

### Size and split

**M overall**: roughly 600–900 lines of code and 800–1,200 of tests. The callers' interfaces don't change, and `compose/task.go` has most of the pieces. Proposed issues, in order:

1. **Control plane memory defaults (S).** `GOMEMLIMIT` on the control plane (`ControlPlaneGoMemLimitMB` in the platform template, as for Traefik: 96 MiB), the default limit to 384 MiB for now, `GOGC=50` for docker CLI children if wanted, and the §4.1 table corrected to the real limits. Unblocks #98 and lets the dashboard suite drop `StartWithControlPlaneMemory`.
2. **Repository tasks in the provisioner (M).** `RepositoryTaskRunner`, the Compose implementation and network, `Task.MemoryLimitMB` with a config setting (proposed `backup.task_memory_mb`, default 512), the fake, and Docker tests. Design doc §4.4, Driver Extensions table.
3. **Run group-level restic in repository tasks (M).** `repository.Manager` through the runner, exit codes, init in a task, the platform-wide task semaphore, Export staging, and the hot-path trims (probe, retry-only `find`). Design doc §5.3 and the `repository` package documentation.
4. **Default memory limit to 256 MiB (S)**, after 3, with the e2e assertion that the control plane runs no restic, and the #98 capacity test run through a backup and a maintenance window.

The init race is separate and should go first (see below).

## Recommendation

1. **Option 3 for all group-level restic**, including `snapshots`, `cat config`, `unlock`, and `forget`: the control plane never runs restic. It is the only option that bounds the repository-size-dependent part, and it also removes most of the backup window's size-independent cost (two restic runs per backup at 38–66 MiB each).
2. **Option 4: `GOMEMLIMIT=96MiB`.** Not 85% of the limit: the control plane's limit is mostly for its children, and the Go process should use about 25–55 MiB with bursts capped near 96.
3. **Option 1, modestly: 256 MiB by default once option 3 lands; 384 MiB until then.** After option 3, measured and estimated peaks are about 96 (Go, capped) + 92 (provisioning children at concurrency 4) + about 40 (collectors after a restart, partly overlapping), which is 190–230 at the worst coincidence, so 192 is too tight to keep. Taking `GOMEMLIMIT` down to 64 MiB to stay at 192 would cost login bursts a threefold slowdown and still leave about 10% headroom.
4. **Keep `jobs.concurrency: 4`** (no option 2).
5. **Limit task containers' memory** (default 512 MiB, about three times the 160 MiB measured for a 622k-blob group), and add a line for them to the §4.1 table.

Revisit the numbers on the #98 host (cgroup v2, 50 real sites) with the scripts below; if `prune` or `check` of a real group needs more than the task limit, raise the task limit, not the control plane's.

## Side finding: concurrent `restic init` breaks new repositories

`repository.Manager.Ensure` says it is "safe to run concurrently for the same group", but `initialize` probes and then runs `restic init` with no lock. When several backups of a group without a repository start at once (its first backup window, at any `jobs.concurrency` ≥ 2), each sees no repository and runs `init` with the same password. Each `init` writes its own master key and `config`. The surviving `config` is encrypted with one master key, while restic opens the repository with whichever key file it reads first, and from then on every command fails:

```
Fatal: config or key <id> is damaged: ciphertext verification failed
```

The repository doesn't recover by itself; until someone removes the extra key files, the group has no backups. It happened in the e2e harness (eight sites of a new group backed up at concurrency 4). With plain restic 0.19.1, four concurrent `init`s on one directory broke it in **14 of 20 trials**. S3 has the same check-then-init window. The fix is to make repository creation single-flight per group, across control plane restarts and on both state databases; a task for it has been suggested separately. Option 3 doesn't fix it by itself: moving `init` into a task keeps the race.

## Reproducing

All scripts run as root on the Docker host and need only Docker; they work on cgroup v1 and v2.

- [`scripts/memory/cgmem.sh <container>`](../../scripts/memory/cgmem.sh) samples a container until it exits or is stopped, and prints its charged and anonymous peaks, its main process's, and each child program's. Run it on the control plane container (`docker ps --filter label=com.docker.compose.service=control-plane`) while you provision sites, run backups, or log in, from a freshly restarted control plane so `memory.peak` starts from zero on v2.
- [`scripts/memory/compose-bench.sh [N...]`](../../scripts/memory/compose-bench.sh) measures concurrent `docker compose` lifecycles with environment variants (`ENV_VARIANTS`).
- [`scripts/memory/restic-bench.sh build|measure <dir>`](../../scripts/memory/restic-bench.sh) builds the synthetic group (`SITES`, `DAYS`, `UPLOADS`, and other knobs; about 75 minutes and 7 GiB at the defaults) and measures each group-level command against it.

Set `IMAGE` to the control plane image under test (default `ghcr.io/rchouinard/pressfleet/control-plane:latest`). The e2e rounds (provisioning, backup bursts, logins at each concurrency and `GOMEMLIMIT`) came from a throwaway test that wasn't committed: `e2e.StartWithControlPlaneMemory(t, 4096)`, then `p.Restart(t, map[string]string{"PRESSFLEET_JOBS_CONCURRENCY": …, "GOMEMLIMIT": …})` per round, with `cgmem.sh` running on the control plane container during the workload. On a real host, `cgmem.sh` beside the same workloads through the dashboard or API gives the same numbers.
