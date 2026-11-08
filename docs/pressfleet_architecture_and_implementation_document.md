# Project Specification: PressFleet Architecture & Implementation Plan

**Project Codename:** PressFleet  
**Status:** Draft  
**Last Updated:** 2026-11-04  
**Target Audience:** Agencies specializing in WordPress development and management  
**Core Vision:** A self-hostable, containerized application server for WordPress—a smaller-scale, open-source alternative to WP Engine featuring one-click deployments, automatic TLS, page/object caching, built-in monitoring, file management, and automated backup/restore routines.

---

## 1. Executive Summary & Goals

PressFleet aims to simplify WordPress management for agencies by delivering:
* **One Agency, One Platform:** Each PressFleet install serves a single agency. Sites are organized into client groups.
* **One-Click Provisioning:** Spin up fully configured WordPress sites from a single API call or dashboard action, immediately reachable on a preview subdomain.
* **Docker Compose First:** Docker Compose is the default deployment target for single-host, small, and demo installations. Kubernetes is a second target, for agencies that run their own cluster (§4.8).
* **Low Resource Footprint:** Shared platform services (database, object cache, metrics storage) are preferred over per-site copies wherever isolation can be preserved.
* **Built-in Performance:** Pre-configured Nginx FastCGI page caching and Redis object caching for every site.
* **Monitoring & Metrics as a Core Feature:** Uptime, performance, resource usage, and backup health for every site, with alerting.
* **Integrated Backup Engine:** Encrypted, deduplicated backups to local or S3-compatible storage with automated restoration/rollback pipelines.
* **Platform-Managed Runtime:** The platform owns the PHP runtime (versions, extensions, security patches) and WordPress core, both shipped in immutable site images. Plugin and theme updates are left to WordPress's native update mechanisms. Site data is limited to `wp-content` and the database, which keeps backups small and focused.

### 1.1. Non-Goals (MVP)

* **Multi-agency / PaaS hosting.** One install serves one agency. A multi-agency PaaS may be considered in the future.
* **Client-group-scoped user access.** MVP roles apply platform-wide; restricting users to specific client groups is a future consideration that §4.6 keeps possible.
* **Client access to the platform.** Only agency staff use the dashboard and API. Exposing platform features to clients (e.g. via WordPress plugins that call the platform API) is a future consideration (§4.2).
* **Staging clones.** Not in the MVP, which reserved room for them in its data model and restore pipeline. They are the first post-MVP feature (§5.11, §8; decided 2026-10-29, #196).
* **Kubernetes deployment.** Not in the MVP, which only kept the door open (§4.2). It is a post-MVP milestone (§4.8, §8; decided 2026-11-04, #212).
* **MCP server.** A built-in Model Context Protocol server that gives agentic clients the dashboard's operations is planned for after the MVP (§8). It goes through the same API authorization and audit log (§4.6), so the MVP needs nothing extra for it.
* **WordPress Multisite.**
* **Managing plugin and theme updates.** WordPress handles these natively; the dashboard only reports pending updates (§5.6).
* **Multi-host clustering** of the Docker Compose target. One PressFleet install manages one Docker host.
* **CDN / edge caching.** Caching is performed at the origin (Nginx + Redis). Agencies may place a third-party CDN in front.
* **Email hosting and DNS hosting.** PressFleet does not run mail servers or authoritative DNS.
* **Shell access, git push deployment, and a web database UI.** Developers use SFTP, the deploy API, database exports, and temporary database sessions (§5.8).

### 1.2. Glossary

| Term | Meaning |
| :--- | :--- |
| **Agency** | The single organization that owns and operates a PressFleet install. Implicit; not a database entity in the MVP. |
| **User** | An agency staff member with access to the dashboard and API. |
| **Role** | A user's platform-wide permission level: Admin, Operator, or Viewer (§4.6). |
| **Client Group** | A named grouping of sites belonging to one agency client. Also the unit of backup repository separation. |
| **Site** | A single WordPress installation and its dedicated containers, volumes, database, and credentials. Belongs to exactly one client group. |
| **Preview Domain** | A platform-assigned subdomain (e.g. `acme-blog.preview.agency.example`) that makes a site reachable before its production DNS is switched. |
| **Primary Domain** | The production domain a site serves after go-live. |
| **Control Plane** | The Go API server, background workers, metrics collector, and embedded dashboard UI. |
| **Host CLI** | The `pressfleet` binary run on the host for `init`, `doctor`, and `upgrade`. The same binary runs as the control plane inside a container (§4.7). |
| **Platform Services** | Shared infrastructure run once per install: Traefik, MariaDB, Redis, metrics storage, the SFTP gateway, control plane. |
| **Site Stack** | The set of containers and volumes making up one site (one Compose project per site, or one namespace on Kubernetes). |
| **Job** | An asynchronous unit of work (provision, go-live, backup, restore, delete) tracked by the control plane. |

---

## 2. High-Level System Architecture

```
                       [ Incoming Web Traffic ]
                                  │
                                  ▼
             ┌──────────────────────────────────────────┐
             │       Reverse Proxy / Edge Router        │
             │     (Traefik - Auto TLS / SNI / HTTP3)   │
             └────────────────────┬─────────────────────┘
                                  │
        ┌─────────────────────────┴────────────────────────┐
        ▼                                                  ▼
┌──────────────────────────┐                   ┌──────────────────────────┐
│   Platform Control Plane │                   │    Site Stack (per site) │
│ API + Workers + Metrics  │                   │  Nginx + PHP-FPM + WP    │
│ Collector + Embedded UI  │                   └───────────┬──────────────┘
└───────────┬──────────────┘                               │
            │                                              │
            ├─► Orchestrator Driver (Compose; K8s later) ──┤
            │                                              │
            ▼                                              ▼
┌──────────────────────────┐    ┌──────────────┐   ┌──────────────────────────┐
│ SQLite / Postgres        │    │ MariaDB      │   │ Site Volume              │
│ (System State / Jobs)    │    │ (per-site DB)│   │ (wp-content)             │
├──────────────────────────┤    ├──────────────┤   └───────────┬──────────────┘
│ VictoriaMetrics          │    │ Redis        │               │
│ (Metrics Storage)        │    │ (shared WP   │               ▼
└──────────────────────────┘    │ object cache)│   ┌──────────────────────────┐
                                └──────────────┘   │ Restic Repositories      │
                                                   │ (one per client group)   │
                                                   │ (Local disk / S3 / MinIO)│
                                                   └──────────────────────────┘
```

---

## 3. Technology Stack Specification

| Subsystem | Choice | Alternatives Considered | Rationale |
| :--- | :--- | :--- | :--- |
| **Control Plane API** | **Go** | — | Fast execution, low memory, single static binary, native Docker Engine API and Kubernetes `client-go` SDKs for the two drivers. |
| **Control Plane UI** | **Nuxt (Vue 3) + Tailwind** | — | Dashboard for site management, client groups, monitoring, and job logs. Built as a static SPA (`nuxt generate`) and embedded into the Go binary via `embed`, so no Node.js runtime is required in production. |
| **Orchestration (MVP)** | **Docker Compose** | Docker Engine API directly | One Compose project per site, rendered from templates. Human-readable, easy to inspect and debug on the host. |
| **Orchestration (Kubernetes)** | **Kubernetes (`k8s.io/client-go`), installed with a Helm chart** | `kubectl` in the control plane image; a hand-written REST client; plain manifests or an operator for packaging | Clusters an agency already runs (§4.8). The typed clientset, plus the dynamic client with server-side apply, and no informer caches. A `kubectl` child process per WP-CLI call, log read, or stats sample would cost the memory #178 measured for the docker CLI, and a hand-written client would reimplement exec, attach, and watch. |
| **Control Plane State** | **SQLite** (default) | PostgreSQL | SQLite for Compose installs (zero extra services); PostgreSQL supported for larger installs and required on Kubernetes, where the operator provides it (§4.8). |
| **Job Queue** | **Database-backed queue** | Redis-backed queue | Jobs are stored in the control plane database so the queue has no dependency on the shared Redis used by sites. |
| **Edge Proxy & TLS** | **Traefik** | Caddy | Dynamic container routing via Docker labels (on Kubernetes, routes the control plane serves to Traefik's HTTP provider) with automatic Let's Encrypt certificates, including DNS-01 wildcard certificates for preview domains. |
| **WP Web Server** | **Nginx + PHP-FPM** | OpenLiteSpeed | Web server tuned with `fastcgi_cache` pre-configured for WordPress page caching. |
| **Site Database** | **MariaDB LTS (11.4+)** | MySQL 8.4 LTS | Shared database server with per-site databases and users. |
| **Object Cache** | **Valkey** (Redis-compatible; shared, platform-managed) | Redis, Memcached, per-site Redis | A single server keeps memory usage low; per-site isolation is enforced with ACL users, per-site logical databases, and key prefixes (§6). Valkey is BSD-licensed (Redis 8 is AGPLv3/RSALv2/SSPLv1), publishes an official image, and speaks the Redis protocol, so the Redis Object Cache plugin works unchanged. Its per-database ACL rules (Valkey 9+), which Redis lacks, are what keep one site from listing another's keys (§6). "Redis" in PressFleet names the protocol and role, not the server. |
| **Metrics Storage** | **VictoriaMetrics (single-node)** | Prometheus | Prometheus-compatible, markedly lower memory and disk usage, single binary. |
| **Metrics Collection** | **Built-in Go collector** | cAdvisor, per-site exporter sidecars | The control plane scrapes container stats (Docker's, or `metrics.k8s.io` on Kubernetes), Traefik, PHP-FPM, Nginx, MariaDB, and Redis directly, avoiding a sidecar per site. |
| **Backup Engine** | **Restic** | Borg, Duplicity | Encrypted, deduplicated snapshots of `wp-content` and DB dumps pushed to local disk or S3/MinIO. |

### 3.1. Project Conventions

| Area | Convention |
| :--- | :--- |
| **License** | MIT |
| **Hosting** | GitHub (`github.com/rchouinard/pressfleet`) |
| **Go** | Latest stable release (currently Go 1.27); module path `github.com/rchouinard/pressfleet` |
| **CI/CD** | GitHub Actions |
| **Container registry** | GitHub Container Registry (`ghcr.io/rchouinard/pressfleet/...`) |
| **Supported platforms** | Linux hosts on `amd64`, and Kubernetes clusters with `amd64` nodes (§4.8). Other architectures (e.g. `arm64`) are not built or tested. |

---

## 4. Deployment Targets & Orchestrator Abstraction

### 4.1. Docker Compose (MVP / Default)

* **Platform Services** (Traefik, control plane, MariaDB, Redis, VictoriaMetrics, SFTP gateway) run as a single Compose project installed once per host. The host CLI renders it from templates embedded in the binary, with every image pinned by digest for that release (§4.7). It is never edited by hand.
* **Each site** is its own Compose project (e.g. `pf-<site-id>`) rendered by the control plane from templates in `/templates/compose` and applied with `docker compose -p pf-<site-id> up -d`.
* Rendered site Compose files are stored on disk under the control plane's data directory so operators can inspect them.
* Traefik discovers sites via the Docker provider and container labels.

#### Site Stack Topology
Each site runs two containers, following the one-process-per-container convention:

| Container | Image | Role |
| :--- | :--- | :--- |
| `nginx` | `ghcr.io/rchouinard/pressfleet/site-nginx:wp<version>` | Serves static files, FastCGI page cache, proxies PHP requests to `php`. |
| `php` | `ghcr.io/rchouinard/pressfleet/site-php:php<version>-wp<version>` | PHP-FPM running WordPress; target for WP-CLI execution. |

* **Networking:** Each site has its own Docker network (`pf-<site-id>`). Both site containers join only that network. The control plane attaches Traefik, MariaDB, and Redis to each site network, so a site can reach the shared services but never another site.
* **Volumes:** `wp-content` is mounted read-write in `php` and read-only in `nginx`. The FastCGI cache is a per-site volume mounted in `nginx` and shared with `php` so Nginx Helper can purge it (§5.6).
* **Future K8s mapping:** The two containers map directly to a two-container Pod (or two Deployments) sharing a PVC.

#### Host Requirements & Preflight
PressFleet ships a `pressfleet doctor` command (also run automatically at install and control plane startup) that checks the host and prints any required or recommended fixes:
* **Required:** Linux on amd64, Docker Engine 28 or later with Compose v2, cgroup v2 (for memory reservations and limits), ports 80 and 443 available, dashboard and wildcard preview DNS records resolving to the host.
* **Required:** Docker `default-address-pools` configured for small subnets. Docker's defaults run out of address space at roughly 30 networks, well below the per-site-network design. Recommended `daemon.json` setting:
  ```json
  { "default-address-pools": [ { "base": "10.200.0.0/16", "size": 24 } ] }
  ```
  This allows 256 site networks.
* **Recommended:** Default container log rotation (`log-opts` `max-size` / `max-file`), enough free disk space for sites, backups, and metrics, and total host memory matching the capacity plan below.

#### Capacity Target
The MVP target is **50 sites on a single 8 GiB host**.

| Consumer | Memory Budget |
| :--- | :--- |
| Sites (50 × 128 MiB baseline) | 6.25 GiB |
| MariaDB | 768 MiB |
| Redis (`maxmemory` 256 MiB; container limit +64 MiB) | 320 MiB |
| VictoriaMetrics | 256 MiB |
| Control plane | 256 MiB |
| Traefik | 192 MiB |
| Socket proxy, maintenance page | 24 MiB + 24 MiB |
| **Left of 8 GiB (OS and headroom)** | **−48 MiB** (limits exceed 8 GiB by 48 MiB) |
| Backup and maintenance tasks (transient; limit `backup.task_memory_mb`, default 512 MiB each) | `jobs.concurrency` × task limit, out of site burst capacity |

The limits are the ones `platform.go` sets, and they add up to 48 MiB more than 8 GiB. They are limits, not reservations, so this works: sites reserve 128 MiB and burst, and platform services rarely sit at theirs. The #98 capacity test ran 50 sites at 5 requests per second through a backup window, prune, and check on an 8 GiB host (7.75 GiB usable): the whole host used about 4.2 GiB at its busiest and never fell below 3.6 GiB of available memory (`docs/capacity-test.md`). Backup and maintenance task containers have no line of their own, but each has a hard memory limit with no swap (`backup.task_memory_mb`, #190), so the line is bounded; they run mostly at night, on the memory sites leave unused, and the capacity test (below) has to run through a backup window and a maintenance window, not only traffic.

* **Control plane memory (#178):** the control plane's limit is mostly for its child processes (`docker` and `docker compose`; restic ran inside it for group-level repository work until #191 moved it into task containers). Measured, ordinary nightly work (eight backups at `jobs.concurrency: 4`) peaked at 206 MiB, so the 192 MiB limit was too small, and a login burst on top of provisioning was the #176 out-of-memory kill. The decision: the Go process gets `GOMEMLIMIT=96MiB` (`platform.ControlPlaneGoMemLimitMB`, its share rather than a fraction of the container limit); the limit was **384 MiB** until group-level restic moved into task containers (#191), and is **256 MiB** since (#192): after #191, eight concurrent backups peaked at 110 MiB on cgroup v1, and the #98 capacity test confirms it on cgroup v2, where its cgroup reached the limit without a kill (anonymous memory peaked at 158 MiB); `jobs.concurrency` stays at 4; task containers get their own memory limit, `backup.task_memory_mb` (default 512 MiB, #190). `GOGC=50` for the docker CLI children (about 15% less memory) is not set: the control plane passes its environment to them unchanged, so a container-level `GOGC` would also apply to the control plane's own Go process. Details and measurements: `docs/decisions/178-control-plane-memory.md`.

* **Per-site memory:** each site has a **128 MiB reservation** (the guaranteed baseline, split between `nginx` and `php`) and a higher **hard limit** (default 512 MiB) that allows bursting. Bursting relies on not every site peaking at once. Reservations must not exceed host memory; `pressfleet doctor` and the dashboard warn when they do. Doctor counts strictly: the site reservations plus the platform services' limits (`platform.go`) plus 256 MiB for the OS, so it says an 8 GiB host fits about 47 sites, and about 45 when the host reports 7.75 GiB, as cloud VMs sized "8 GiB" do. The 50-site target is what the capacity test ran (#98), relying on limits being caps rather than reservations; plan for fewer when sites are busy or large.
* **PHP-FPM:** runs with `pm = ondemand` so idle sites hold no worker processes. `pm.max_children` is derived from the site's memory limit.
* **Validation:** before MVP release, a capacity test provisions 50 sites on an 8 GiB host and verifies they stay healthy under light traffic. It passed in #98 (`docs/capacity-test.md`) after Traefik's limit was raised to 192 MiB.

#### Install-Time Configuration
* **Dashboard domain:** where the control plane UI/API is served (e.g. `fleet.agency.example`).
* **Preview base domain:** a domain with a wildcard DNS record pointing at the host (e.g. `*.preview.agency.example`).
* **DNS provider credentials** (optional, recommended): lets Traefik issue a single wildcard certificate for the preview base domain via DNS-01. Without them, each preview subdomain gets its own HTTP-01 certificate, which is subject to Let's Encrypt rate limits.
* **Backup repository location:** local path or S3/MinIO endpoint and credentials.
* **Alert channels:** SMTP settings and/or webhook URLs.
* **WordPress security set:** which platform hardening features are enabled (§6.1). One install-wide setting; there are no per-site overrides.
* **SFTP gateway port:** the host port developers connect to for SFTP and database tunnels (default 2222; §5.8).
* **Platform master key:** generated by `pressfleet init`. It encrypts all stored secrets, including every client group's backup repository key (§6).

> **⚠️ Master key warning (must appear in setup documentation and the `pressfleet init` output):** The master key is required to decrypt backup repository keys. If the host is lost and the master key was not saved elsewhere, **all backups are unrecoverable**. Setup requires the operator to download the key and confirm it has been stored somewhere safe (e.g. a password manager), separate from the host and its backups.

The master key and the backup location are what rebuilds a lost host: the control plane also backs up its state database into the backup location, encrypted with a key derived from the master key (§5.10).

### 4.2. Future Compatibility Constraints

Kubernetes and client-facing integrations are out of scope for the MVP. To keep them achievable, the MVP must follow these constraints:
* All orchestration goes through the `Provisioner` interface (§4.4); no Docker/Compose calls outside the Compose driver.
* Site configuration is expressed as data (`SiteConfig`), never as Compose-specific fields.
* Control plane state must work on PostgreSQL, not just SQLite.
* Site containers must be stateless apart from declared volumes (`wp-content`) so they can later run as Pods with PVCs.
* API tokens carry a scope so that site-scoped tokens for future client-facing WordPress plugins can be added without redesigning authentication. A token can never do more than its owner's role allows (§4.6).
* Authorization checks resolve every target to its client group, so client-group-scoped user access can be added later without redesigning authorization (§4.6).
* The site model supports a parent/child relationship between sites, which links a staging site to its production site (§5.11).
* A staging site's environment type and outbound mail switch are `SiteConfig` data (`Environment`, `DisableMail`) rendered into the generated `wp-config.php`, so a Kubernetes driver needs nothing staging-specific (§5.11).

### 4.3. Domain Model (MVP)

| Entity | Key Fields / Relationships |
| :--- | :--- |
| **User** | Email, password hash, role (Admin, Operator, or Viewer; §4.6), API tokens, SSH public keys (§5.8). |
| **ClientGroup** | Name, notes, backup repository reference. Has many Sites. |
| **Site** | Client group, slug, preview domain, primary domain (nullable until go-live), aliases, PHP version, WordPress core version, core branch hold and major release approval (§5.5), resource limits, preview password protection (off by default; §5.1), status, parent site (nullable; set on staging sites, §5.11), outbound mail switch, and on staging sites the snapshot and time of the last clone or refresh and the time of the last push. |
| **Job** | Type, target, state, step log, timestamps, and optionally a job it waits for (§5.11). |
| **BackupRecord** | Site, snapshot ID, kind (`scheduled`, `manual`, `pre_restore`, `pre_update`, or `sync`, the source of a staging copy; §5.11), WordPress core version at backup time, size, status, timestamp. |
| **AlertRule / Alert** | Condition, target, channel, whether site-scoped rules include staging sites (§7.3); firing and resolved events. |
| **AuditEvent** | Actor (user and token), action, target, result, source IP, timestamp (§4.6). |

### 4.4. Adapter Contracts

Responsibilities are split into three interfaces so that orchestration, database management, and backups can evolve independently.

```go
package provisioner

import "context"

type SiteConfig struct {
    ID             string
    ClientGroupID  string
    PreviewDomain  string
    PrimaryDomain  string // empty until go-live
    Aliases        []string
    PHPVersion     string
    WPCoreVersion  string // platform-managed; selects the site image together with PHPVersion
    EnableCache    bool
    Resources      ResourceLimits
    DBSecretRef    string // reference to stored credentials, never the plaintext password
    RedisSecretRef string
}

// ResourceLimits apply to the site as a whole; the driver splits them between
// the nginx and php containers.
type ResourceLimits struct {
    CPU                 string // e.g. "1.0"
    MemoryReservationMB int    // guaranteed baseline; default 128
    MemoryLimitMB       int    // hard limit allowing bursts; default 512
}

type SitePhase string

const (
    SitePending SitePhase = "pending"
    SiteRunning SitePhase = "running"
    SiteStopped SitePhase = "stopped"
    SiteFailed  SitePhase = "failed"
)

type SiteStatus struct {
    ID      string
    Phase   SitePhase
    URL     string
    Message string
}

// Provisioner manages site runtime resources (containers, volumes, routing).
type Provisioner interface {
    CreateSite(ctx context.Context, cfg *SiteConfig) (*SiteStatus, error)
    UpdateSite(ctx context.Context, cfg *SiteConfig) (*SiteStatus, error)
    GetSite(ctx context.Context, siteID string) (*SiteStatus, error)
    DeleteSite(ctx context.Context, siteID string) error
    StartSite(ctx context.Context, siteID string) error
    StopSite(ctx context.Context, siteID string) error
    SetMaintenance(ctx context.Context, siteID string, enabled bool) error
    ExecWPCLI(ctx context.Context, siteID string, args []string) (string, error) // args never hold a secret: a driver may log argv (the Kubernetes audit log does)
    ExecWPCLIInput(ctx context.Context, siteID string, args []string, stdin io.Reader) (string, error) // secrets go on stdin
    PurgeCache(ctx context.Context, siteID string) error // page cache; the object cache manager flushes the object cache
}

// DatabaseManager manages per-site databases and users on the shared MariaDB server.
type DatabaseManager interface {
    CreateDatabase(ctx context.Context, siteID string) (secretRef string, err error)
    DropDatabase(ctx context.Context, siteID string) error
    Dump(ctx context.Context, siteID string, destPath string) error
    Import(ctx context.Context, siteID string, srcPath string) error
    // Temporary users of database sessions (§5.8), limited to the site's database.
    CreateTemporaryUser(ctx context.Context, siteID, sessionID string, access Access) (Credentials, error)
    DropTemporaryUser(ctx context.Context, sessionID string) error // also ends its connections
    ListTemporaryUsers(ctx context.Context) (sessionIDs []string, err error)
}

type SnapshotID string

// Snapshot describes a snapshot: its ID and time, the site ID, WordPress core
// version, and backup kind read back from its tags, and its sizes.
type Snapshot struct { /* ... */ }

// BackupOptions record why a backup was taken (manual, scheduled,
// pre-restore) and the platform's backup ID, which also makes Backup
// idempotent.
type BackupOptions struct { /* ... */ }

// BackupEngine creates and restores site snapshots.
// Each client group has its own Restic repository; snapshots are tagged by site ID.
type BackupEngine interface {
    Backup(ctx context.Context, siteID string, opts BackupOptions) (Snapshot, error)
    ListSnapshots(ctx context.Context, siteID string) ([]Snapshot, error)
    Restore(ctx context.Context, siteID string, snap SnapshotID, targetSiteID string, opts RestoreOptions) error
    // Prune applies the retention policy (restic forget --prune); Check
    // verifies the repository (restic check). Both take an exclusive
    // repository lock.
    Prune(ctx context.Context, clientGroupID string) (PruneResult, error)
    Check(ctx context.Context, clientGroupID string) error
    GroupSnapshots(ctx context.Context, clientGroupID string) ([]Snapshot, error)
}
```

#### Driver Extensions

Log viewing (§7.4), resource metrics (§7.1), and backup/restore (§5.3–5.4) also need orchestrator access, and §4.2 forbids reaching Docker outside the driver. They are separate interfaces, so each consumer depends only on what it uses. A driver implements all of them (`provisioner.Driver`):

```go
// LogReader reads site container logs. Iteration ends when logs are exhausted
// or, when following, when ctx is canceled.
type LogReader interface {
    Logs(ctx context.Context, siteID string, opts LogOptions) iter.Seq2[LogEntry, error]
}

// StatsReader samples per-component resource usage (cumulative CPU time,
// memory working set and limit, restart count).
type StatsReader interface {
    Stats(ctx context.Context, siteID string) ([]ComponentStats, error)
}

// TaskRunner runs a short-lived command with access to a site's data: the
// wp-content volume (read-only or read-write), a transfer directory shared
// with the control plane, and the platform services the site can reach.
type TaskRunner interface {
    RunTask(ctx context.Context, siteID string, task Task) (*TaskResult, error)
}

// RepositoryTaskRunner runs the same kind of task against a backup
// repository instead of a site: a client group's, or the platform's state
// repository (repository is provisioner.StateRepository). It has no
// wp-content and no site, so it works for deleted sites and for restic's
// group-level commands (init, snapshots, forget/prune, check, stats).
type RepositoryTaskRunner interface {
    RunRepositoryTask(ctx context.Context, repository string, task Task) (*TaskResult, error)
}
```

Every task has a hard memory limit, `Task.MemoryLimitMB` (default `backup.task_memory_mb`, 512 MiB), with no swap on top: restic's memory grows with the size of the repository, so it is bounded where it runs instead of inside the control plane (#178). A task over its limit is killed and fails.

`RepositoryTaskRunner` is `TaskRunner` keyed by repository, because `RunTask` borrows the site's network, UID, and wp-content volume. A repository task mounts the local repository directory (`Task.RepositoryDir`) at `/repository`, and gets secrets, environment, a transfer directory, and the memory limit as any task does; `Task.WPContent` must be `MountNone`. It runs as the user and group that own local repositories (`provisioner.RepositoryGID`), not as a site. It has no network, unless `Task.BackupNetwork` says the repository is on S3. In the Compose driver, that task joins a network of its own, a plain bridge shared only with other repository tasks (and not letting them talk to each other), which gives it a route to the internet without a path to MariaDB, Redis, the control plane, or any site; the platform's backup network is connected beside it, for a self-hosted S3 service. Each task has a unique host name, because restic records the host name in its locks and treats a lock of its own host as stale once its process is gone: with a shared host name, one container could remove another's live lock, and with unique ones only the lock's age (30 minutes without a refresh) counts across containers. The Kubernetes driver maps a repository task to a pod in the platform's namespace: it mounts the repository directory from the data volume (local) or reads the S3 credentials from a Secret, with no site volume and a NetworkPolicy that denies all traffic or allows egress to the S3 endpoint only (§4.8).

Site components are named by role (`nginx`, `php`), not by orchestrator object. Backups use `TaskRunner`: the control plane dumps the database into a transfer directory under its data directory, then runs `restic` in a task that mounts `wp-content` read-only plus the transfer directory. In the Compose driver, a task is a one-off container on the site's network. The data directory is mounted at the same path on the host and in the control plane container, so transfer directories can be bind-mounted into tasks. Task secrets (Restic repository passwords, S3 credentials) are never environment variables, because a container's configuration is readable through the socket proxy (§6): `Task.SecretFiles` holds them as files, which the driver mounts read-only at `/run/pressfleet/secrets` for the task's user alone and removes when the task ends, and the task's `Env` names their paths (`RESTIC_PASSWORD_FILE`, `AWS_SHARED_CREDENTIALS_FILE`). The Compose driver bind-mounts a directory in the data directory's `tmp/`; a Kubernetes driver mounts a Secret volume.

| Interface | Compose driver | Kubernetes driver (§4.8) |
| :--- | :--- | :--- |
| `LogReader` | Container logs | Pod logs API, per container |
| `StatsReader` | Docker stats (cumulative CPU) and container restart count | `metrics.k8s.io`, accumulated into cumulative CPU, and Pod status restart counts; no kubelet API, which needs `nodes/proxy` |
| `TaskRunner` | One-off container mounting the site volume, a bind-mounted transfer directory, and a bind-mounted directory of secret files | Pod on the site's node mounting the site PVC, its transfer directory from the ReadWriteMany data volume (through the site namespace's clone of it, §4.8), and a Secret volume |
| `RepositoryTaskRunner` | One-off container as `RepositoryGID` with the repository directory bind-mounted, no network (local) or a bridge network with a route out and the backup network (S3), a unique host name, and a bind-mounted directory of secret files | Pod in the platform namespace, as `RepositoryGID`, mounting the repository directory from the data volume and a Secret volume, with a deny-all NetworkPolicy (local) or egress to the S3 endpoint only, and a unique host name |
| `SessionRunner` | One-off container, attached to the gateway's streams, mounting only the site volume, with no network | Pod with `stdin`/`stdinOnce`, the site PVC, and a deny-all NetworkPolicy, attached through the API |

### 4.5. API Design

* **Spec-first:** The API is defined in an OpenAPI 3.1 document (`/api/openapi.yaml`), which is the source of truth. Go server interfaces and request/response types, and the TypeScript client used by the Nuxt UI, are generated from it. CI fails if generated code is out of date with the spec.
* **Versioned paths:** All endpoints live under `/api/v1/`. Breaking changes require a new version prefix; additive changes do not.
* **Conventions:** JSON request/response bodies, standard HTTP status codes, `202 Accepted` + job ID for long-running operations, and a consistent error body (RFC 9457 Problem Details).
* **Authentication:** Session cookies for the embedded UI, with an optional or required second factor (§6.2); bearer API tokens (with scopes, §4.2) for automation. Only sessions can create API tokens.
* **Authorization:** Role-based, enforced by the server on every endpoint (§4.6). `403 Forbidden` responses use the standard Problem Details body.

### 4.6. Authorization & Roles

Each user has one platform-wide role. The roles are fixed in the MVP; there are no custom roles.

| Action | Admin | Operator | Viewer |
| :--- | :---: | :---: | :---: |
| View sites, client groups, jobs, backups, metrics, and alerts | ✓ | ✓ | ✓ |
| Read a site's runtime logs (access, error, slow, and PHP logs; a job's own log lines stay with jobs) | ✓ | ✓ | |
| Create and import sites; change site settings (PHP version, core branch hold, resources, preview password protection); go-live and domains; start/stop; maintenance mode; cache purge | ✓ | ✓ | |
| Deploy code; connect over SFTP; export the database or the whole site, also from a backup (§5.7); open read-only database sessions (§5.8) | ✓ | ✓ | |
| Log in to a site's WordPress admin from the dashboard (§5.9) | ✓ | ✓ | |
| Open read-write database sessions | ✓ | | |
| Take backups, restore, and approve core updates | ✓ | ✓ | |
| Create, refresh, push, and delete staging sites (§5.11) | ✓ | ✓ | |
| Create and edit client groups; manage alert rules | ✓ | ✓ | |
| Delete production sites, and delete any site together with its backups (snapshots and backup records) | ✓ | | |
| Delete or offboard client groups (export or delete the backup repository and its key) | ✓ | | |
| Manage users, roles, and other users' API tokens; reset other users' second factors; view the audit log | ✓ | | |
| Change platform settings (DNS and S3 credentials, SMTP, alert channels, backup location, retention, WordPress security set) | ✓ | | |
| Manage their own profile (display name), password, second factors (§6.2), API tokens, and SSH keys | ✓ | ✓ | ✓ |

**Rules:**
* **Single check point:** Every API handler calls one authorization check with the caller, the action, and the target before doing anything. The UI hides actions the user can't take, but the server is authoritative.
* **Tokens:** A token's effective permissions are the intersection of its scope and its owner's *current* role. MVP scopes are `full` (everything the owner's role allows) and `read-only`. Future site-scoped tokens (§4.2) plug into the same check. Tokens are created only from dashboard sessions, never with another token, so a stolen token can't mint replacements that outlive its revocation; tokens can still list and revoke tokens.
* **Admin tokens (accepted risk, decided 2026-10-21, #182):** Creating, changing, and deleting users and resetting other users' second factors need the Admin role and are writes, so only an Admin's `full` token can do them; Operator tokens and read-only tokens are refused. Tokens aren't asked to reauthenticate (§6.2), so a full-scope Admin token is as powerful as the Admin account. In particular it can create a user (or set a user's password), and that access outlives the token's revocation, which is the one way around session-only token creation. The MVP accepts this rather than make these actions session-only, which would break automation that onboards users. Guidance: treat full-scope Admin tokens like the Admin password; prefer read-only or Operator-owned tokens for automation; after revoking a leaked Admin token, check the audit log for `user.create` and `user.update`. The dashboard warns when an Admin creates a full-scope token.
* **Immediate effect:** Role changes apply immediately to the user's existing sessions and tokens.
* **Two-factor:** Restricted sessions of users who must enroll, and sensitive actions that need a fresh factor check, are enforced by the same check (§6.2).
* **At least one Admin:** The last Admin can't be demoted, disabled, or deleted. `pressfleet init` creates the first user as an Admin, and `pressfleet user create --role` and `pressfleet user reset-two-factor` (run inside the control plane container, §4.7) exist for recovery.
* **Client groups:** The check resolves every target (site, backup, job, alert) to its client group. Client-group-scoped access (post-MVP) only needs a user↔group membership table on top of this.

**Audit log:** Every authenticated state-changing request and every authorization denial is recorded as an `AuditEvent`. Each event holds the actor (user and token), action, target, result, source IP, and timestamp, and never includes secrets. Admins can view and filter the audit log in the dashboard. Retention is configurable.


### 4.7. Versioning, Upgrades & Version Skew

PressFleet uses semantic versioning. Each release bundles the control plane image, the host CLI, and the platform definition: the platform Compose templates plus the exact image digests of every platform service.

**Responsibilities:**
* The **control plane** owns its state database: migrations, pre-migration backups, and restoring those backups.
* The **host CLI** owns the platform definition and renders the platform Compose project from templates embedded in its binary. It never reads or writes the state database directly. Commands that operate on state (e.g. `pressfleet user create`) run inside the control plane container (`docker compose exec control-plane pressfleet …`, or `kubectl exec` on Kubernetes), so their version always matches the database schema.

**Migrations:**
* Migrations are forward-only; there are no down migrations.
* The control plane applies pending migrations automatically at startup, after backing up the state database.
* The control plane refuses to start if the database schema is newer than the versions it supports.
* One-off data migrations must be safe to run more than once.

**Upgrades (`pressfleet upgrade`):**
* The host CLI upgrades the control plane to the CLI's own version. To upgrade, install the new CLI and run `pressfleet upgrade`.
* Upgrading directly from any older version is supported, provided it is at or above the minimum version that release supports upgrading from (`skew.MinUpgradeFrom`, named in the release notes). Downgrades are refused.
* Steps: run preflight checks (versions, the runtime host checks, the config loading with the new release, the MariaDB series); render the new platform Compose project into a staging directory and compare it with the running one; pull images; record the current platform project (copied to `<data_dir>/upgrade/previous-platform/`) and the image digests the services ran (`<data_dir>/upgrade/upgrade.json`); render the new project in place; recreate only the services that changed; wait for the control plane (which migrates at startup) and platform services to report healthy, and the control plane to report the new version. A service changed if its Compose configuration hash changed, or if a single file it bind-mounts (a configuration file or secret) changed content, which Compose doesn't detect on its own; directory mounts see new files without a recreate. Running the upgrade again when nothing changed does nothing. Nothing is changed before the pull succeeds.
* **A failed upgrade rolls back on its own** (decided 2026-11-04, #245; plan: [upgrade-rollback-plan.md](upgrade-rollback-plan.md)). The upgrade keeps what a rollback needs: the previous rendered platform project, the image digests it ran, the MariaDB series, and the control plane's pre-migration state backup. Any failure after the platform starts to change starts the rollback: rendering the new project, recreating services, the health checks, the version check, or SIGINT or SIGTERM during the apply. Failures before that (preflight, pull) change nothing. The exit code is `0` when the upgrade succeeded, `2` when it failed and was rolled back, and `1` when it failed and wasn't. `--no-rollback` leaves the failed state for debugging and prints the manual recovery steps instead.
* **The rollback sequence** is shared by automatic rollback and `--rollback`. Each step is idempotent, and `upgrade.json` records its progress (`rolling-back`, then `rolled-back` or `rollback-failed`), so a rollback that stops can be run again:
  1. Stop the control plane.
  2. If the upgrade migrated the state database: with SQLite, restore the pre-migration backup with the previous control plane image (`pressfleet state restore`, which first saves the database it replaces); with PostgreSQL, stop and print the steps that use the operator's own backup, since the previous control plane would refuse the migrated schema.
  3. Copy the rendered platform files back from `previous-platform` and remove the files the new release added. Traefik's dynamic directory, which the control plane writes at runtime (sites' domain and maintenance routes), is left to the previous control plane's startup reconcile.
  4. Start the previous release's services with `--remove-orphans`, force-recreating those that were recreated only because a mounted file changed.
  5. Wait until the control plane is ready at the previous version.

  The rollback runs on a context of its own, so the signal that started it doesn't cancel it; a second signal stops it. If a step fails, the CLI prints the manual recovery steps, filled in for where it stopped; they work after a partial rollback. The steps come from one template, which the operator guide (`docs/upgrading.md`) shows verbatim, and an end-to-end test runs them as printed (#142). Fixing the cause and running the upgrade again also works: a retry keeps the platform project saved by the failed attempt, so a rollback still leads back to the release before it.
* **Migration marker.** The control plane records each migration run in a marker file beside the state backups (the time, its version, the schema versions before and after, the dialect, and the pre-migration backup, if any), for PostgreSQL too. The CLI learns from it whether the upgrade migrated the database, without opening it. It reads the markers from the host, or through Docker when the host user can't read the root-owned backups directory, and falls back to the backups' file names when no marker exists.
* **An interrupted upgrade** (the record says in progress, but the CLI is gone): the next `pressfleet upgrade` offers to retry the same release or roll back, and won't start an upgrade to another release.
* **Held rollouts.** While the upgrade record's status is in progress, failed, or rolling back, the core, image, and template rollouts (§5.5) start no site jobs, so an unconfirmed control plane can't change sites. User-requested jobs and the startup reconcile run as usual. The rollout endpoints and the dashboard show the rollouts as held, and doctor warns. The record's statuses are a contract between the CLI and the control plane, versioned like the admin interface.
* **`pressfleet upgrade --rollback`** rolls back the most recent upgrade after it succeeded, once. Restoring the state database undoes what the control plane recorded since the upgrade but not what it did to sites, so the CLI first asks the running control plane what changed (`pressfleet admin changes --since <time>`) and sorts the changes into three groups:
  * **Blocking:** sites created, imported, or deleted, staging sites included; WordPress core updates; client groups deleted or offboarded. They would leave orphaned resources, a site recreated empty, or a site on an older core than its database. `--rollback` refuses them unless `--force` is given, and says what `--force` leaves behind.
  * **Converged:** site template and image updates, and settings changes. The previous control plane's reconcile moves sites back to match the restored records.
  * **Lost:** backup records, users, alert rules, staging refreshes and pushes, and the like. Their records go with the restore; snapshots stay in the repositories, and copied content stays on the sites.

  It lists all three and asks for confirmation (or `--yes`). If the control plane doesn't answer, as after an upgrade that failed with `--no-rollback`, the CLI skips the check and says why that's safe: the rollouts were held.
  * It is also the last of the manual recovery steps (decided 2026-11-07, #310). For an unconfirmed upgrade whose platform already runs the previous release (the control plane ready at the record's `from`, on the control plane image it ran before the upgrade, with every file of the saved platform project back), it only marks the record `rolled-back`, lifting the rollout hold, and exits `0`; it touches neither the state database nor the running services, and runs no changes check. A control plane ready at neither release is refused, as is a succeeded upgrade whose platform is already back.
* **Backup repositories created during the upgrade.** A client group's first backup can create its repository after the pre-migration backup was taken. Restoring that backup drops the repository's record and password but not its files, so the control plane has to handle a repository its stored password doesn't open, and never delete one (#277).
* A failed control plane upgrade leaves sites serving: sites run in their own Compose projects and Traefik keeps routing to them. The dashboard, API, and jobs are unavailable until the control plane is fixed or rolled back.
* Rolling back is for failed or immediately regretted upgrades. Anything recorded in the state database after the upgrade (e.g. new sites) is lost.
* PressFleet doesn't take pre-migration backups of PostgreSQL state databases (operators do), so with PostgreSQL the state restore step uses the operator's own pre-upgrade backup, and the upgrade asks for confirmation (or `--yes`) after saying so. A PostgreSQL install therefore rolls back on its own only when the upgrade ran no migration; otherwise the rollback stops before changing anything and prints the steps. A logical pre-migration backup of PostgreSQL, in the §5.10 format, is left for later. (The disaster recovery backups of §5.10 cover both drivers, but they are for rebuilding a lost host, not for rolling back an upgrade.)
* Only the most recent upgrade can be rolled back, because only one previous platform project is kept. On Kubernetes, rollback is `helm rollback` and the operator's PostgreSQL restore (§4.8).
* On Kubernetes, the Helm chart takes the place of the platform Compose project, and `helm upgrade` the place of `pressfleet upgrade`, with the same preflight in a pre-upgrade hook (§4.8).
* Upgrading only the control plane doesn't interrupt site traffic. Recreating Traefik, MariaDB, or Redis briefly interrupts every site, so the upgrade asks for confirmation (or `--yes`) before it does.
* MariaDB major version upgrades (a change of the X.Y release series) are one-way. In the MVP the upgrade refuses them, a rollback never crosses one, and each release pins MariaDB to one series (a unit test pins the series in `images.yaml`, since `MARIADB_AUTO_UPGRADE` would otherwise apply a change silently). Supporting them is future work, for a major PressFleet release: the upgrade would back up every site database and copy MariaDB's data volume so it can go back.
* Config keys and `PRESSFLEET_*` variables that a release renames or removes stay accepted, with a deprecation warning, for at least one minor release and until the oldest release the upgrade accepts uses the new name: package `config` keeps a table of them and rewrites the old names before its strict decoding, so an existing config file doesn't stop the new CLI or control plane. Setting both an old and a new name is an error.
* When site rendering changes (the site Compose templates, or the code that fills them in), sites are re-rendered one at a time with health checks by a third rollout, `templates`, built on the core and image rollouts' mechanism (§5.5). The driver fingerprints its rendering (a hash of the embedded site templates and a render version bumped when rendering code changes), and the state database records the fingerprint each site was last rendered with. Until the rollout reaches a running site, the startup reconciliation leaves its containers as they are, so a template change never reaches every site at once.

**Version skew between the host CLI and the control plane:**
* Patch versions within the same minor version are always compatible.
* From 1.0, the host CLI's minor version must be within one of the control plane's (n−1, n, or n+1, where n is the control plane's minor version). Before 1.0, the minor versions must match.
* Before every command, the CLI checks the control plane's version. Outside the policy it refuses to run and explains how to fix it; there is no override. `pressfleet version` and `pressfleet upgrade` are exempt.
* The host CLI talks to the control plane only through a small, versioned admin interface: it runs `pressfleet admin <command>` in the control plane container, which answers with JSON. Going through `docker exec` needs no network exposure or credentials of its own (Docker socket access is already root on the host) and works while Traefik is down. The interface version changes only on incompatible changes, and every version keeps reporting the control plane's version. If the control plane doesn't answer, the CLI reads its version from the container's image metadata. From 1.0, changes to that interface must stay backward compatible for at least one minor release, and CI tests the CLI against control planes at n−1, n, and n+1.

### 4.8. Kubernetes

PressFleet installs on a Kubernetes cluster with a Helm chart, and a Kubernetes driver runs every site operation in place of the Compose driver, behind the same interfaces (§4.4). The same control plane and site images serve both targets, from one binary. Compose stays the default for single-host installs (decided 2026-11-04, #212; plan: [kubernetes-driver-plan.md](kubernetes-driver-plan.md)).

* **Scope:** one cluster per install. The orchestrator is chosen at install time (`driver`) and never changes for that install. One control plane replica, one replica per site, and `amd64` nodes. A highly available control plane, sites scaled out to several replicas, multi-cluster installs, arm64 nodes, and moving an install between Compose and Kubernetes are left for later; the move could go through state recovery (§5.10), whose backup loads into PostgreSQL.
* **Client:** `k8s.io/client-go`, imported only in `provisioner/kubernetes` (§3). The driver renders each site's manifests from `/templates/k8s` into the site's directory under the data directory, where operators can inspect them, and applies them with server-side apply under the field manager `pressfleet`. Template changes reach running sites through the templates rollout (§4.7), with the driver's own render version.
* **Packaging and lifecycle:**
  * A Helm chart, published on GHCR as an OCI artifact with each release, pins every image by digest from the same `images.yaml` as the platform Compose project.
  * `pressfleet init --target kubernetes` asks init's questions and writes a values file and the Secrets: the master key (with the §4.1 warning and confirmation), the MariaDB and Valkey admin passwords, and the SSH host key. It never talks to the cluster.
  * `helm upgrade` runs a pre-upgrade hook on the new image that applies the upgrade preflight (§4.7): no downgrade, no MariaDB series change, and at least the minimum release it can upgrade from. The control plane migrates at startup, as on Compose. Backing up PostgreSQL before an upgrade is the operator's job, and rolling one back is `helm rollback` plus that backup.
  * State commands run with `kubectl exec` in the control plane pod. The version skew rules don't apply, because the chart and the control plane always come from the same release.
* **Platform services:** the chart runs Traefik, MariaDB, Valkey, VictoriaMetrics, and the maintenance page, configured as on Compose. The state database is a PostgreSQL the operator provides, with its DSN in a Secret, so `driver: kubernetes` requires `database.driver: postgres`. MariaDB can be external; Valkey can't, because the per-database ACL rules need Valkey 9 (§6).
* **Layout:** the platform runs in its own namespace. Each site has a namespace, `pf-<site-id>`, holding what its Compose project holds: a single-replica Deployment (strategy `Recreate`) whose pod runs `nginx` and `php`, the FastCGI cache as an `emptyDir`, `wp-config.php` in a Secret volume, a Service for nginx, its routes, and its NetworkPolicies. Stopping a site scales it to zero; deleting it deletes the namespace.
* **Storage:** a site's `wp-content` is a ReadWriteOnce PVC from a configurable storage class, so tasks and SFTP workers that mount it are scheduled on the node of the site's pod; for a stopped site, the volume's topology decides. The control plane's data directory is a ReadWriteMany volume, mounted by the control plane and, as subpaths, by task pods, so transfer directories, uploads, exports, and local backup repositories work as on Compose and the task contract (§4.4) is unchanged. Repository tasks, in the platform namespace, mount its claim; a claim belongs to one namespace, so for each site namespace the driver creates a PersistentVolume that points at the same storage (a copy of the data volume's CSI or NFS source, reclaim policy `Retain`, bound in advance to a claim in the site's namespace), which the site's tasks mount (decided 2026-11-06, #219). Deleting a site deletes the clone and its claim, never the storage. The chart grants cluster-wide create and delete on PersistentVolumes, and its admission policies admit only such clones, with the data volume's own source, and no other change; the data volume's own PersistentVolume should be `Retain` too. The data volume's storage must accept several PersistentVolumes on one volume (NFS CSI, CephFS, and EFS do), and doctor warns about provisioners known not to. Task pods set no `fsGroup`, which would change the group of the whole shared volume: the control plane shares a transfer directory through its own group instead. Volume ownership comes from `fsGroup` (`fsGroupChangePolicy: OnRootMismatch`); storage that ignores `fsGroup` isn't supported. Streaming transfers through tasks' standard streams, and requiring S3 for backups, would remove the ReadWriteMany requirement later.
* **Isolation (§6):**
  * Each site namespace enforces Pod Security `restricted`. Site, task, and session pods run as the site's UID with no capabilities, a read-only root filesystem, and no service account token.
  * Each site namespace denies all traffic by default. Ingress to nginx is allowed only from Traefik. Egress is allowed to DNS, MariaDB, Valkey, the SMTP relay, and the internet, but not to the cluster's pod, service, or node ranges, or the cloud metadata address (nodes and API server endpoints on public addresses are #324). nginx trusts forwarded headers from the pod network's ranges (`NGINX_TRUSTED_PROXIES`), which the NetworkPolicy narrows to Traefik.
  * The control plane's ServiceAccount can create and delete namespaces, and in each site namespace it creates binds a ClusterRole that covers the site's objects. It has no rights in other namespaces, no cluster-wide Secret access, and no `nodes/proxy`. RBAC can't name namespaces that don't exist yet, so the chart's ValidatingAdmissionPolicies narrow its cluster-wide rights: namespaces and RoleBindings to its own namespaces (named with its prefix and labeled with its platform namespace, so that installs sharing a cluster and a prefix stay apart, #326) and its own ServiceAccount, PersistentVolumes to clones of the data volume, and the pods and Services it creates to ones without a ServiceAccount token, host access, or exposure past Traefik (security review, #229). This replaces the Docker socket (§6).
  * The cluster's CNI must enforce NetworkPolicy, which doctor proves.
  * One namespace for all sites, with policies selected by label, would need only a namespaced Role, but isolation would then depend on every label being right.
* **Ingress and TLS:** the chart's Traefik, which has no access to the Kubernetes API and no ServiceAccount token (#322): the chart's own routes (dashboard, SFTP, the preview wildcard, the maintenance page's service) are in its file provider, and the control plane serves the sites' routes, named as on Compose, to its HTTP provider on a listener of its own (`kubernetes.traefik_provider_listen`) that a NetworkPolicy opens to Traefik alone; Traefik polls it every 2 seconds. A site's routes address its nginx Service (`nginx.<site namespace>.svc`), and preview basic auth is served as the password's bcrypt hash. Maintenance mode is a higher-priority twin of each router, pointing at the maintenance page. Certificates are issued as on Compose (HTTP-01, and DNS-01 for the preview wildcard), with Traefik's ACME storage on a PVC, so Traefik runs as one replica. A LoadBalancer Service, or NodePorts, carries ports 80, 443, and the SFTP port. Standard Ingress, Gateway API, and cert-manager are left for later; basic auth, noindex, HSTS, redirects, and maintenance priorities would need work for each controller.
* **Exec, tasks, and sessions:** WP-CLI, cache purges, code reloads, probes, and the PHP-FPM status run through the exec API. Traefik's metrics are read over the network from a metrics entry point that a NetworkPolicy opens to the control plane alone, so the control plane has no exec in the platform namespace; Traefik's ping stays on loopback. Tasks and SFTP workers are pods, as §4.4 describes. Task secrets are Secret volumes, readable only inside the task's pod, which runs one container as one user, and deleted with the pod, and pods a control plane restart leaves behind are removed at startup.
* **Metrics:** container CPU and memory come from `metrics.k8s.io` (metrics-server is required). The driver accumulates CPU usage into the cumulative CPU time `StatsReader` reports, and reads restarts from Pod status. The kubelet summary API would give cumulative CPU directly, but only through `nodes/proxy`, which can run commands on nodes. Host metrics become per-node CPU and memory (§7.2).
* **Capacity (§4.1):** a site's memory reservation and limit become its pods' requests and limits, split between nginx and php as on Compose. The scheduler decides whether a site fits; doctor and the dashboard warn when requests exceed the cluster's allocatable memory.
* **Supported clusters:** the three newest Kubernetes minor releases when a PressFleet release ships, with `amd64` nodes on cgroup v2, a CNI that enforces NetworkPolicy, a default ReadWriteOnce storage class and a ReadWriteMany one, metrics-server, and LoadBalancer Services or NodePorts. Doctor checks them at control plane startup and as a Helm test. CI tests on kind; the docs use k3s as the small-cluster example.

---

## 5. Core Operational Workflows

All long-running operations run as **Jobs**. API endpoints that start them return `202 Accepted` with a job ID; clients follow `GET /api/v1/jobs/{id}/events`, a server-sent event stream of the job's state, steps, and log that ends when the job does, or poll `GET /api/v1/jobs/{id}`. Each job step must be idempotent so a failed job can be retried safely, and a job that fails permanently must roll back resources it created.

### 5.1. One-Click Provisioning Workflow
1. **User Request:** Agency user calls `POST /api/v1/sites` with client group, site name, and admin email. Returns `202` + job ID.
2. **Resource Allocation:** Control plane assigns a site ID and slug, derives the preview domain (`<slug>.<preview-base-domain>`), and generates database credentials, a Redis ACL user, and a WordPress admin password (stored encrypted; the admin password is shown to the user once).
3. **Database Creation:** `DatabaseManager.CreateDatabase` creates the database and a user whose grants are limited to that database.
4. **Object Cache Credentials:** Creates the site's Redis ACL user, restricted to its own key prefix (§6).
5. **Container Provisioning:** `Provisioner.CreateSite` creates the site network and renders the site's Compose project (`nginx` and `php` containers from the images for the selected PHP version and current WordPress core, `wp-content` and cache volumes, generated `wp-config.php`, Traefik labels for the preview domain, resource limits) and brings it up.
6. **WordPress Installation:** Runs `wp core install` against the image's WordPress core with the preview domain as the site URL via `ExecWPCLIInput`, with the generated admin password on standard input (`--prompt=admin_password`), never in argv.
7. **Plugin Auto-Configuration:** Activates the default plugin stack: Nginx Helper, Redis Object Cache (configured with the site's ACL credentials, database, and key prefix), and the WordPress security set configured for the install (§6.1).
8. **Preview Ready:** The site is immediately reachable over HTTPS on its preview domain. Preview domains send `X-Robots-Tag: noindex` to keep them out of search engines.

**Preview password protection (optional):** For sensitive sites in development, a site's preview domain can require HTTP basic auth. It is off by default and turned on or off per site with `PATCH /api/v1/sites/{id}`.
* Traefik enforces it with a `basicAuth` middleware on the preview-domain router only, so WordPress is not involved. Production domains are never protected; after go-live the preview domain redirects to the primary domain (§5.2).
* Each site has one username and password. The platform generates the password, stores it encrypted, and shows it once (like the WordPress admin password); it can be regenerated. Only a bcrypt hash appears in the site's Traefik labels. The hash uses bcrypt's lowest cost, because Traefik checks it on every request and a random password needs no stretching.
* Platform operations don't go through the protected route: WP-Cron and other tasks use WP-CLI, cache purges use the filesystem, and Traefik answers ACME challenges before routing. Uptime checks (§7.1) send the site's credentials.

### 5.2. Go-Live (Domain Switch) Workflow
1. **User Request:** Agency user sets the primary domain (and optional aliases) on the site.
2. **DNS Check:** Before anything changes, the control plane verifies that each new domain's A and AAAA records all point at the host (its configured public IP or the dashboard domain's addresses) and reports clearly if not. Nothing is recorded, so the user simply retries once DNS has propagated. Hosts behind a CDN or load balancer can skip the check.
3. **Routing & TLS:** `Provisioner.UpdateSite` adds Traefik routes for the primary domain and aliases; Traefik obtains a certificate per domain via HTTP-01. The Compose driver puts these routes in Traefik's file provider rather than in container labels, so going live and later domain changes recreate no container.
4. **URL Update:** Runs `wp search-replace` from the preview URL to the primary URL and updates `home`/`siteurl`.
5. **Cache Purge:** Purges the FastCGI page cache and the site's object cache.
6. **Preview Redirect:** The preview domain (behind its noindex header and optional password) and the aliases permanently redirect to the primary domain.

A go-live that fails before the URL update rolls back to the previous domains. Once the URL update has started it no longer rolls back, because reverting `wp search-replace` isn't safe; the site keeps its new domains, and going live again with the same domains finishes the update. Changing the primary domain later is another go-live; returning to the preview domain alone isn't supported. Aliases can be added and removed on their own at any time after go-live: new ones get the same DNS check, each alias gets its own certificate, and the change reroutes them without touching WordPress or interrupting the site.

### 5.3. Backup Workflow
1. Triggered on schedule or on demand; runs as a Job in the control plane's worker pool.
2. The backup worker (which includes the `mariadb-dump` client) calls `DatabaseManager.Dump` over the network to a temporary directory. The worker itself never runs `restic`; every restic command runs in a task container (see "Where restic runs" below).
3. Runs `restic backup` in a site task over the site's `wp-content` volume (mounted read-only) and the dump file into the site's **client group repository**, tagged with the site ID and the site's WordPress core version. WordPress core and `wp-config.php` are not backed up; both are reproducible from the site image and the control plane's site record. A backup of a group whose repository exists is that one restic command: the worker only checks that the repository's `config` is there, without running restic, and looks for an earlier attempt's snapshot only when the job is retrying.
4. Applies the retention policy (`restic forget --prune`) and periodically verifies repository integrity (`restic check`), each in a repository task. Pruning takes an exclusive repository lock, so prune/check jobs are serialized per repository and scheduled away from backup windows.
5. Records the result as a `BackupRecord` and emits backup metrics (§7); failed or stale backups raise alerts.

#### Repository Layout
* One Restic repository per client group, each with its own encryption key.
* Sites within a group deduplicate against each other (shared plugins, themes, and media are common).
* Offboarding a client is a matter of handing over or deleting one repository and key, or of exporting its sites (§5.7).

#### Where restic runs
The control plane process never runs restic (#178, #191). restic derives the repository key with scrypt on every command (38 to 66 MiB) and, for `check`, `prune`, and `stats`, holds an index that grows with the repository's history (about 150 MiB for a group of 50 sites with 90 daily snapshots each), so it runs in task containers with their own memory limit (`backup.task_memory_mb`, §4.4) where exceeding it fails one job instead of the control plane.

* **Site tasks** (`TaskRunner`) run `restic backup`, and restores, next to the site's data.
* **Repository tasks** (`RepositoryTaskRunner`) run every group-level command: `init`, `cat config`, `snapshots`, `forget` and `prune`, `check` and `unlock`, `stats --mode raw-data` (the storage metrics), `dump` (export from a snapshot), and the state repository's commands (§5.10). `repository.Manager` builds the task, so its callers (`backup/restic`, `repomaint`, `metrics`, `statebackup`) keep their interfaces. The password and S3 credentials are secret files of the task (§4.4), never environment variables or arguments.
* **Exit codes keep their meaning:** restic's 10 (no repository), 11 (locked), and 12 (wrong password) become `ErrRepositoryMissing` (internal), `ErrLocked`, and `ErrWrongPassword` as before, and a repository whose concurrent `init`s left several key files is still reported as `ErrRepositoryDamaged`, with the repair (#188). Any other exit code is an error carrying restic's own words; 137 (killed) adds that the task probably exceeded its memory limit.
* **Initialization** is still single-flight per repository (#188). `init` runs in a task as the repository group (`RepositoryGID`), so the files it writes belong to that group's user with mode 0600; the control plane then shares them with the group (setgid directories, group permission bits), which is also what lets restic derive group-writable modes for everything later tasks write. A repository directory that does not exist is "no repository" without a task.
* **Repositories left by a state restore** (#277): a restored state database (§4.7, §5.10) lacks the record and password of a repository created after its backup was taken, so the next backup stores a new password that doesn't open the files. The record therefore says when its password first opened the repository (or `init` created it). Until then, the worker opens the repository before using it, even if its `config` is there, and a wrong password sets the repository aside and creates a new one: a local repository is renamed to `<data_dir>/backups/unopenable-<group>-<time>`; on S3, where a rename copies every object, the objects stay and the record moves to a new repository at `<prefix>/<group>-<time>`. Nothing is deleted, and the control plane log and the audit log say where the old files are. Once the password has opened the repository, a wrong password is an error for the operator and moves nothing, and backups keep their single restic command. The migration that added the marker set it for groups with a succeeded backup.
* **Standard input and output:** a task has no standard input, so the state backup's dump reaches `restic backup --stdin` as a file in the task's transfer directory. Output (`dump` of a site's files or database for an export) streams through `Task.Stdout` without a size limit.
* **Export of a repository** (`pressfleet repository export`) copies the repository into a directory below the data directory's `tmp/`, then runs `restic check` on the copy in a repository task that mounts the export directory, opened with the exported password file alone, and gives the files back to the control plane's user afterwards.
* **Concurrency:** the job runner bounds how many jobs run at once (`jobs.concurrency`), and a job runs one task at a time (a backup is one site task; prune, check, forget, and offboarding run their repository tasks one after the other), so jobs alone never hold more tasks than `jobs.concurrency`, whichever kind they are. What jobs do not bound is repository tasks started outside a job: the storage metrics collector (serially), and reading snapshots in an API request, such as the list a restore request starts with. The repository manager therefore holds a semaphore of `jobs.concurrency` slots for repository tasks, which bounds those at the same number. A driver-wide semaphore for site tasks too was rejected: a site task is held for as long as a backup lasts, so a restore request or the `wp-content` size collector would wait for the whole backup window behind four backups.
* **Latency:** a task costs about 0.5 s to start (0.6 s on S3), against about 40 ms for an in-process exec, once per command: +0.5 s on a restore request's snapshot list, +1 to 1.5 s on a prune or check job (`unlock`, `forget`, `prune`), +0.5 s per group every 30 minutes for the storage collector, and nothing on a backup.
* **What the control plane still runs:** `mariadb-dump` for the database dump, and the `docker` CLI that drives tasks and sites. The `restic` binary stays in the control plane image, because that image is also the task image (`task_image`) that every restic task runs; nothing in the control plane process executes it, which the end-to-end tests check by sampling the container's process list while it backs up, prunes, checks, and collects the storage metrics.

### 5.4. Restore Workflow
1. Takes an automatic **pre-restore snapshot** of the target site so the restore itself can be rolled back.
2. Enables maintenance mode via `Provisioner.SetMaintenance` (Traefik serves a maintenance page). In the Compose driver, higher-priority routes in Traefik's file provider send the site's domains to a static maintenance page service, so the page doesn't depend on the site's containers; the site record keeps the flag so re-renders preserve it.
3. Restores `wp-content` and the DB dump from the chosen snapshot.
4. Imports the DB dump via `DatabaseManager.Import`. If the snapshot was taken on a newer WordPress core version than the target site is running, the site is first moved to an image with at least that core version (a database schema cannot be safely run on an older core).
5. If the target domain differs from the snapshot's source (e.g., restoring into a different site), runs `wp search-replace` via WP-CLI.
6. Purges the FastCGI page cache and the site's object cache, then disables maintenance mode.
7. On failure at any step, restores the pre-restore snapshot and leaves the site in maintenance mode with an error on the job. If that fails too, the site is marked `failed`, and restoring a backup is the way out (below). The status message says what happened and what to do about the data, which stays true; it doesn't say whether the site is in maintenance mode, and turning that off doesn't clear it. The `maintenance` flag says that, and the dashboard shows the two together.

A `failed` site that finished provisioning can be restored, which is how to recover from a failure whose rollback failed too, whatever job it was. A site whose provisioning or clone failed has no secrets left and stays refused. Its containers may be up or missing and its data half replaced, so `site.restore` differs in four ways, in the same steps (decided 2026-10-31):

* Its first step makes sure the database, object cache user, and containers exist and are started, with the same idempotent operations as `site.recover` (§5.10). None of them touch data that is there.
* The pre-restore snapshot is still attempted, since the failed state may hold what happened since the last backup. If it can't be taken, the job goes on with a warning and no undo point, because the restore is what can repair the site. A running site's failed backup still stops its restore.
* If the site was in maintenance mode when the restore started, it stays in it, with a warning on the job. The flag can't tell an operator's choice from what a failed job left behind, so a person checks the site and turns it off, as in step 7. A site that wasn't serves visitors when the job finishes.
* If the job fails before the restore is complete, the site stays `failed`, however its containers look, because the state it would go back to is the one that failed. The status message says what state the data is in and keeps what it said before. Another restore can be started.

Refreshing a staging site and pushing it to production (§5.11) run these same steps, with the other site's snapshot as the source and a **scope**. `files` is `all` (all of `wp-content`), `code` (`plugins`, `themes`, `mu-plugins`, and `languages`), or `none`, and `database` is yes or no; at least one of the two is copied. A copied scope ends up identical to the snapshot's: a code directory the snapshot lacks is removed, and everything outside the scope is left alone. Without the database, steps 4 and 5 are skipped. `site.restore`, `site.refresh`, and `site.push` share the steps, and `site.restore` keeps its step names so a restore queued before an upgrade resumes after it.

### 5.5. Runtime Management (PHP & WordPress Core)

#### Site Image & Filesystem Layout
* The platform publishes two site images, both containing the same WordPress core release:
  * `site-php:php<version>-wp<version>` (e.g. `site-php:php8.4-wp6.9.1`): PHP-FPM with a fixed, documented set of extensions, plus WordPress core.
  * `site-nginx:wp<version>` (e.g. `site-nginx:wp6.9.1`): Nginx with the platform's WordPress configuration, plus WordPress core's static files (`wp-admin`, `wp-includes` assets) so they can be served without PHP. It depends only on the WordPress version, not the PHP version.
* A site's two containers must always run the same WordPress core version; the control plane updates them together.
* WordPress core is **read-only** inside both containers. This also prevents core files from being tampered with by a compromised plugin.
* `wp-config.php` is generated by the control plane from the site record (DB and Redis credentials, salts, platform constants) and mounted read-only. It is never edited by hand; agency-specific constants are set through the site's configuration in the platform.
* Platform must-use plugins (e.g. hiding core update notices, platform integration hooks) ship in the image and are loaded from outside `wp-content`, so they are versioned with the image and cannot be removed from within WordPress.
* The default plugins (§5.1 step 7) ship in the `site-php` image as release zips, pinned by version and checksum and verified at build time. Provisioning installs them into `wp-content` from the image, so it needs no network access; from then on they are regular plugins that WordPress updates (§5.6). The control plane never downgrades them, and reinstalls or reactivates one that was removed when it reconciles the site at startup, since the object cache and cache purging depend on them.
* `wp-content` is the only writable, persistent site volume.

#### PHP
* The agency selects a PHP version per site. Changing it runs `Provisioner.UpdateSite`, which recreates the site's `php` container on the new image (a brief interruption of a few seconds).
* The dashboard flags sites on PHP versions nearing or past upstream end-of-life.

#### WordPress Core
* WordPress's built-in core updater is disabled (`WP_AUTO_UPDATE_CORE` set to `false`, with core update notices hidden by a platform must-use plugin). Plugin and theme auto-updates remain enabled.
* When a new core release is published, the platform builds new site images and rolls them out:
  1. Takes a pre-update backup of the site.
  2. Recreates the site's `php` and `nginx` containers on the new images.
  3. Runs `wp core update-db` to apply any database schema changes.
  4. Runs a health check (HTTP status plus a WP-CLI sanity check). On failure, it reverts to the previous image and restores the pre-update backup if the schema was changed.
* **Rollout policy** (decided 2026-09-28):
  * **Minor and security releases** (x.y.Z) roll out automatically to every site, within the maintenance window if one is set.
  * **Major releases** (x.Y) wait for approval per site or per client group. An optional install-wide setting approves them automatically a set number of days after their images are published (off by default); an explicit approval or hold overrides it.
  * **Holds are per major branch, not per version.** A site or client group held on its current branch keeps receiving that branch's minor and security releases, so a hold never leaves a site unpatched. WordPress backports security fixes to older branches only as a courtesy, so the dashboard warns when a site has been held on an older branch for a long time and when its branch stops getting releases.

#### Image Updates
* When the platform ships image updates (PHP patch releases, WordPress core releases, security fixes, base OS updates), the control plane rolls them out by recreating site containers, optionally within a maintenance window.
* Rebuilt images keep their tags, so each site runs pinned builds (image digests), recorded per site and tag; a site moves to a new build only through the rollout, one site at a time with the same health check as a core update, and a failed update moves it back to the previous build. No pre-update backup is taken: the core, and so the database schema, doesn't change (#58).
* Site template changes from a platform upgrade (§4.7) roll out the same way (`site.template_update` jobs): the site's rendered files are copied aside, the site is re-rendered with the current templates and configuration (recreating only the containers whose definition changed), and health-checked. The previous release's templates aren't in the new binary, so a failed update puts the copied files back and brings the containers up on them. A failed site stays on its previous render until a release with other templates or an explicit site update.
* The image matrix is kept small: for each supported PHP version, the latest WordPress core release, plus the latest release of every older branch a site is held on or awaiting approval for.

### 5.6. WordPress Runtime Defaults
* **Plugin and theme updates:** Handled natively by WordPress (auto-updates or wp-admin), writing to the `wp-content` volume. The platform only reports pending updates, as part of site health (§9 Q2); it never runs them. The control plane checks each site twice a day (`wp plugin list --update=available`, and the same for themes, staggered across sites like WP-Cron) and keeps the latest result; the dashboard shows the count on the site list and the updates on the site's page, and can check a site again on demand. Pending updates make a site's health a warning, never a failure: the default alert rule *plugin or theme updates pending* (warning) fires once they have been pending for 3 days, which leaves WordPress's own auto-updates time to apply them first.
* **WP-Cron:** `DISABLE_WP_CRON` is set; cron is triggered by the control plane scheduler so it runs reliably on low-traffic sites.
* **Cache purging:** Nginx Helper purges the FastCGI cache using file-based purging against the per-site cache volume shared between the `nginx` and `php` containers. Both containers run as the site's UID so the cache files can be deleted. The platform configures it that way at provisioning and whenever it reconciles the site. A full purge (`POST /api/v1/sites/{id}/cache/purge`, and the purges in go-live and restore) runs as a job in the control plane rather than inside WordPress, so it works on stopped or broken sites: it flushes the site's object cache database as the object cache admin, then deletes the page cache files through the driver (`Provisioner.PurgeCache`).
* **Outbound email:** Sites send mail through a configurable SMTP relay. When `PRESSFLEET_DISABLE_MAIL` is set (staging sites, §5.11), the must-use plugin short-circuits `wp_mail()` and logs one line per suppressed message, without addresses.

### 5.7. Site Import Workflow

Existing sites are imported from a `wp-content` archive and a SQL dump (decided 2026-09-28). Other sources, such as pulling from a remote host or reading migration plugin formats, may follow after MVP based on user feedback.

1. **Upload:** The archive and dump are sent through a resumable, chunked upload API with a configurable size limit. Uploads are stored under the data directory, never backed up, and deleted when the import ends.
2. **Validation:** Before anything is created, the control plane checks the archive layout and the dump, and reads the table prefix, the source URL, and the core version from the dump. It rejects multisite installs, and cores newer than the newest site image.
3. **Site creation:** Like provisioning (§5.1 steps 2–5): the new site gets a slug, preview domain, credentials, database, object cache user, and containers on the newest core.
4. **Content:** Restores `wp-content` and imports the dump with `DatabaseManager.Import` under its own table prefix, then runs `wp core update-db`. Host-specific drop-ins (`object-cache.php`, `advanced-cache.php`, `db.php`) are replaced by the platform's; other hosts' must-use plugins are kept and reported as warnings on the job.
5. **URLs and platform setup:** Runs `wp search-replace` from the source URL to the preview URL, applies the default plugin stack and platform configuration (§5.1 step 7), and purges the caches. The site is then on its preview domain and goes live like any other (§5.2).

The source's `wp-config.php` is not imported: the platform generates it (§5.5). An import that fails permanently deletes the new site, as failed provisioning does.

#### Export
A site can be exported in the same two files import takes, so an export can be imported again here or moved to another host (decided 2026-10-02): `<slug>-wp-content.tar.gz` and `<slug>-database.sql.gz`. The archive leaves out the platform's host drop-ins, which point at its object cache and page cache. Agencies use it to hand a site to a client, move it, or offboard a client group without restic.
* **From the live site:** a job dumps the database (`DatabaseManager.Dump`) and archives `wp-content` with a task running as the site's UID with the volume mounted read-only, holding the site's lock.
* **From a backup snapshot:** the backup worker (§5.3) streams the snapshot's `wp-content` and dump out of the client group's repository (`restic dump`), without touching the live site. It runs in a repository task, like prune and check, rather than as a site task, since a deleted site has no containers to run one in. This also works for sites that have been deleted, whose snapshots stay in the repository until the group's repository is deleted; they are listed under their client group.
* Exports work like database exports (§5.8): the files wait in the data directory, can be downloaded (with resumable range requests) by the caller until they expire, and are then deleted. Starting an export and every download are audited. A live export first estimates its size (the database plus `wp-content`) and refuses to start if the data directory lacks the room.

### 5.8. Developer Access

Agency developers get files onto a site and reach its database through the platform, never with a shell in the site's containers or the site's own database password (decided 2026-09-28). Operators and Admins have this access (§4.6), and every session and deployment is audited.

#### SFTP Gateway
* One SSH gateway runs as a platform service on a single host port (default 2222), since SSH can't be routed by host name like HTTP. Developers connect with `sftp -P 2222 <site-slug>@<dashboard domain>`: the SSH username names the site, and the key names the platform user.
* Authentication is by SSH public keys that users register on their platform account; there are no per-site or shared passwords. Disabling a user, lowering their role to Viewer, or removing a key ends their access to every site at once.
* For each session, the driver starts a short-lived worker for that site that runs as the site's UID and mounts only its `wp-content` volume, and the gateway relays the SFTP session to it. Files written over SFTP are therefore owned by the site, and a session can't reach another site's files. On Kubernetes the worker maps to a short-lived Pod mounting the site's volume.
* The gateway offers no shell or command execution, and no port forwarding except the database tunnel below.

#### Deploy API
* `POST /api/v1/sites/{id}/deploy` deploys a theme or plugin package (zip or tar.gz) into `wp-content` as a job, for CI pipelines and scripts using API tokens.
* The job unpacks the package next to its target, swaps it in with a rename, and purges the caches. The previous version is kept until a health check passes; if the check fails, it is swapped back. It can optionally activate the theme or plugin.
* Git push deployment isn't offered: a CI pipeline builds the code and calls the deploy API.

#### Database Access
* **Exports:** a job dumps the site's database with `DatabaseManager.Dump` into a compressed file that the caller downloads with their session or token before it expires (default 1 hour). Replacing a site's database goes through the restore pipeline (§5.4).
* **Temporary database sessions:** for tools like TablePlus or DBeaver, a user opens a session through the API. The control plane creates a temporary MariaDB user limited to the site's database, read-only unless an Admin asks for write access, and returns its credentials once with an `ssh -L` command. The SFTP gateway forwards connections only to that site's database, and only for a user with an active session. At expiry (default 1 hour, at most 8), the control plane drops the MariaDB user and ends its connections.
* There is no web database UI in the MVP; an on-demand Adminer, behind the platform's authentication and using the same temporary users, can be added later if agencies ask for it.

### 5.9. Dashboard Login to WordPress

A button on a site's dashboard page logs the current platform user in to the site's wp-admin as an administrator, without a WordPress password (decided 2026-10-02). Operators and Admins can use it (§4.6), as they already have full control of a site through deployments and database access (§5.8); Viewers and `read-only` tokens can't.

* **Which user:** the administrator the platform created with the site (§5.1). If that user no longer exists or is no longer an administrator, or the site was imported and has no platform-created administrator (§5.7), the dashboard lists the site's administrators (read with WP-CLI) and the user picks one. They can also pick another administrator on purpose.
* **Login tokens:** the control plane signs a short-lived, single-use token with a platform Ed25519 key. The private key is stored encrypted in the state database with the master key (so the state database backup of §5.10 includes `platform_keys` and its secret), and the public key and the site's ID are written to every site's `wp-config.php` as `PRESSFLEET_LOGIN_PUBLIC_KEY` and `PRESSFLEET_SITE_ID`, so sites hold no new secret and logging in needs no WP-CLI call. Existing sites get the constants when the control plane reconciles them at startup. Key rotation isn't part of the MVP.
* **Token format (v1):** `v1.<payload>.<signature>`, both parts base64url without padding. The payload is JSON `{"site", "user", "actor", "jti", "iat", "exp"}`: the site ID, the WordPress user's login, the platform user's ID, a random 128-bit ID (hex), and Unix times 60 seconds apart. The signature is Ed25519 over the ASCII bytes `v1.<payload>`.
* **Flow:** `POST /api/v1/sites/{id}/admin-login` returns the token and the site's login URL: `wp-login.php?action=pressfleet-login` on the primary domain once the site is live (§5.2), otherwise on the preview domain. The dashboard submits the token as a form POST in a new tab, so it never appears in a URL, an access log (which only Operators and Admins can read, §7.4), a `Referer` header, or browser history. The site must be running and not in maintenance mode.
* **Site side:** a module of the platform must-use plugin checks the signature, the site ID, the expiry, and that the token hasn't been used (each `jti` is recorded in the site's database until it expires, not in the object cache, which can evict it). It then checks that the user is an administrator, sets the WordPress auth cookie for a browser session, and redirects to wp-admin. Any failure shows one generic error. The session is marked as a platform login with the actor and `jti`.
* **Two-factor:** the platform login counts as authentication. When the security set requires two-factor authentication (§6.1), a dashboard login asks for no second factor, and its session is exempt from the forced enrollment, so a shared administrator account isn't bound to one person's device. Password logins are unaffected. This is about wp-admin; dashboard sessions themselves can require a second factor (§6.2).
* **Audit:** every issued token is an audit event (§4.6) naming the WordPress user and the `jti` (never the token), so a WordPress session can be traced to the platform user who opened it.
* **Preview password protection:** when it is on (§5.1) and the site isn't live, Traefik asks for the preview credentials before the login reaches WordPress; the dashboard says so.

### 5.10. Disaster Recovery

The client group repositories (§5.3) and the master key aren't enough to rebuild a platform whose host is lost: which sites exist, their settings and domains, the users, and every secret, the repository passwords included, are in the control plane's state database. The control plane therefore backs up the state database too, and a new host is rebuilt from the master key, the backup location, and the install-time configuration (#96, decided 2026-10-20).

* **Format:** a logical, dialect-neutral export of every table (JSON lines, with the schema version in a header and an end record that makes a truncated backup fail to load). One format covers SQLite and PostgreSQL, loads into either, and needs no PostgreSQL client tools in the control plane image; native dumps (`VACUUM INTO`, `pg_dump`) would need one code path per driver, and `pg_dump` must match the server's major version. Loading recreates the schema at the backup's version from the binary's own migrations, inserts the rows, applies the newer migrations, and commits only if all of it succeeded, so a backup from an older release restores into a newer one, and a failed restore changes nothing.
* **Contents:** the whole state database, including `platform_keys` and its secret (the login signing key, §5.9) and the master key's check value. Secrets stay encrypted with the master key inside the backup.
* **Location:** a platform-level **state repository**, a Restic repository beside the client group repositories on the same backend (`<data_dir>/backups/state`, or `<prefix>/state` on S3). Restic encrypts it with a password derived from the master key with HKDF, since it can't be stored in the database it backs up. Its restic commands run like the group-level ones, in repository tasks for the state repository (§5.3), so its local directory belongs to the repository group like the group repositories do.
* **Schedule and retention:** hourly by default, as a job (`platform.state_backup`), followed by `restic forget --prune` with its own retention policy (`backup.state.*`); `pressfleet state backup` in the control plane container takes one on demand.
* **Restore:** `pressfleet init --force` on the new host, with the saved master key file and the same answers, installs an empty platform; `pressfleet recover` then stops the control plane and runs `pressfleet state recover` in a one-off control plane container (the host CLI never touches the state database, §4.7), and starts the control plane again. Before committing, the restore checks that the backup's master key check value matches and that the login key decrypts (an unusable one is discarded), fails the jobs the old host hadn't finished, and queues a `site.recover` job for every site that was running, stopped, or being updated. A site comes back stopped if it was stopped, including before the update in progress (the store records the status a site had when it went to `updating`), or if an unfinished stop job asked for it. It refuses to replace a state database that already has client groups or sites unless forced.
* **Site recovery:** `site.recover` recreates a site's database and object cache user with the restored credentials, renders and starts its containers with the current configuration, restores the newest snapshot in the site's client group repository (which may be newer than the state backup), and runs `wp core update-db`. The login key reaches the site through that render: a restored key keeps every site's public key, and a key generated because the backup had none (or an unusable one) is pushed to every site as it is recreated, so one-click login recovers on its own. Sites that were being provisioned or deleted are marked failed for the operator.
* Whatever the platform recorded after the last state backup is lost; metrics, uploads, and pending exports aren't backed up. An end-to-end test runs the whole drill: install, back up, delete everything but the backup location and the master key, reinstall, recover.
* **On Kubernetes** (§4.8), `helm install` with the saved values and master key installs the empty platform, and `pressfleet state recover` runs as a one-off Job with the control plane scaled to zero. `site.recover` then rebuilds the sites as above.
* **Staging sites** (§5.11) have no scheduled backups by default. A staging site without a snapshot of its own is recovered from its production site's newest snapshot, with its URLs rewritten to its preview domain, and the job carries a warning. If neither site has one, the staging site is marked failed.

### 5.11. Staging Sites

A staging site is a copy of a production site on its own preview domain, linked to it, for testing heavier changes (plugins, a theme, a PHP upgrade, a WordPress major release) before they reach production. It can be refreshed from production, and its changes can be pushed back (decided 2026-10-29, #196; plan: [staging-clones-plan.md](staging-clones-plan.md)).

* **Shape:** a staging site is a site whose parent is its production site. It belongs to the production site's client group, because copies go through the group's backup repository. Staging sites can't be nested, and a production site can have any number of them, limited only by host capacity (§4.1).
* **Copies go through the backup engine.** The source is backed up (kind `sync`) and the snapshot is restored into the target with the restore pipeline (§5.4), so every copy has an undo point and rolls back on failure. A clone or refresh can start from an existing backup of the production site instead. A job holds one lock key, so each copy is two jobs queued together: the source's backup under the source's lock key, and the copy under the target's, which waits for the backup to finish and fails, changing nothing, if the backup failed. A job can only wait for a job that already exists, so chains can't deadlock, and a queued job that is waiting doesn't block its lock key, so two copies in opposite directions run side by side instead of one after another.
* **Create** (`POST /api/v1/sites/{id}/staging-sites`): allocates the staging site like an import (§5.7), with a slug derived from production's (`acme-staging`), and the copy job then creates its database, object cache user, and stack, restores the snapshot, runs `wp core update-db`, rewrites production's URL to the staging preview URL, and configures the default plugins. It inherits production's PHP version, core, table prefix (the dump is imported verbatim), resources (which can be overridden), and initial admin user; no admin password is generated, because production's users come with the copy. A clone that fails rolls back like provisioning and leaves the staging site marked failed. The clone pulls the site images before pinning them (a failed pull is only a warning) and, before restoring any of production's data, checks that the image the staging site runs has the outbound mail switch (§5.6); if the image predates it, the clone fails permanently and rolls back, so a copy of production can't email real customers. A clone therefore requires a site image with the switch, and fails if the host can't get one.
* **Refresh** (`POST /api/v1/sites/{id}/refresh`) copies production into the staging site with a scope (§5.4). The staging site gets a pre-refresh backup. A refresh with the database rewrites production's URLs to the staging preview URL and converges the default plugin stack, because the dump brings production's plugin state; one without it leaves the database alone. The staging site keeps its settings: its mail switch, preview protection, domain, credentials, and environment are never read from the copy.
* **Push** (`POST /api/v1/sites/{id}/push`) copies the staging site into production with a scope. Production gets a pre-push backup, which the job result names so the push can be undone by restoring it, is in maintenance mode during the push, and is rolled back automatically if the push fails. If that rollback fails too, production is marked `failed`, and restoring the pre-push backup recovers it (§5.4); a push and a refresh still refuse a failed target. URLs are rewritten to production's primary domain (or preview domain before go-live), and the default plugin stack is converged afterwards. A database push replaces everything production recorded since the staging snapshot (orders, comments, form entries, users); the dashboard shows how old the staging site's database and uploads are and asks for the production site's slug before a database or all-files push starts. That age is the time of the snapshot each part was last copied from (`database_as_of`, `uploads_as_of`), not of the last refresh: a code-only refresh leaves both as they were, a push sets them to the pushed snapshot's time, and a restore of the staging site clears them, since the data it puts back has no known age. A database push from a staging site on a newer WordPress major branch than production's core policy allows is refused, because it would bypass the hold or approval (§5.5); a newer release of the same branch moves production up, as a restore does. Settings are never pushed: production keeps its PHP version, resources, domains, preview protection, and mail switch. Plugin activation lives in the database, so a code-only push updates active plugins and brings new ones in inactive. A push without the database doesn't converge the plugin stack, which would write to it, but does reinstall from the site image any default plugin (and the object cache drop-in) the pushed files removed: production depends on them (§5.5, §6.1), and the next reconciliation may be a long way off. WordPress failing to load the pushed code rolls the push back. The core guard is checked when the push is requested and again against the snapshot, before the job changes anything. A staging site deleted or marked failed before the job starts fails the push with production unchanged.
* **Defaults:** a staging site has only its preview domain (go-live and aliases are refused), `WP_ENVIRONMENT_TYPE` is `staging`, preview password protection is on, outbound email is off (it can be turned on per staging site), WP-Cron runs as on production, and scheduled backups are off (the site's backup schedule can turn them on; manual backups work). The admin bar shows a badge naming the environment. `blog_public` is left alone, since a database push would carry it to production; the preview domain's `noindex` header keeps staging out of search engines.
* **Everything else** works as on any site: restore, export, SFTP, the deploy API, database sessions, one-click login, plugin update checks, and core, image, and template rollouts under the staging site's own core policy. Updating staging sites before their production sites (a canary) is left for later.
* **Deletion:** deleting a staging site is an Operator action that keeps its backups; deleting its backups too stays Admin (§4.6). A production site with staging sites can't be deleted; the request is refused before anything is removed.
* **What a copy carries along:** production's users and passwords, customer data, plugin settings with live API keys, licenses, webhooks, and scheduled events. PressFleet turns off mail through `wp_mail()` and keeps the site behind preview protection, but can't make every plugin safe; the operator guide lists what to check.
* **Not supported:** copying selected tables or merging database changes, promoting a staging site to production or detaching it, custom domains on staging sites, staging sites in another client group, and pushing settings.

---

## 6. Security Baseline

* **Tenant isolation:** Each site runs as its own non-root UID with PHP `open_basedir` restricted to its own files. Sites must not be able to reach other sites' containers over the network.
* **Resource limits:** Every site container has a memory reservation, a memory limit, and a CPU limit (§4.1 Capacity Target).
* **Database:** Each site has its own MariaDB user with grants only on its own database.
* **Redis (shared):**
  * Each site has its own ACL user restricted to its own key prefix (`~<site-id>:*`) and its own logical database (`db=<n>`, Valkey 9). The prefix alone isn't enough: `SCAN` takes no key argument, so it would list every site's key names (which include user logins and email addresses), and the Redis Object Cache plugin needs `SCAN` for its selective and group flushes. With a database per site, `SCAN`, `DBSIZE`, `RANDOMKEY`, and `FLUSHDB` only see the site's own keys.
  * Dangerous and cross-tenant commands are denied to site users (`FLUSHALL`, `KEYS`, `CONFIG`, `MONITOR`, `CLIENT LIST`, `SWAPDB`, Pub/Sub, functions, etc.). `FLUSHDB` and `INFO` are allowed: the first only reaches the site's database, and the plugin requires the second.
  * Site users live only in the server's memory; the control plane recreates them from the state database when it starts and whenever the server restarts.
  * Redis runs with a `maxmemory` limit and an LRU eviction policy. One busy site can evict others' cache entries; this is an accepted trade-off for lower resource usage, and eviction rates are monitored (§7).
* **Backups:** Each client group's Restic repository has its own encryption key.
* **Secrets:** Database passwords, Redis ACL passwords, preview basic auth passwords, Restic repository keys, S3 credentials, and users' TOTP secrets (§6.2) are stored encrypted at rest in the control plane database using the platform master key, supplied via environment variable or file. Install-time secrets that `pressfleet init` collects before the state database exists (DNS provider credentials, the SMTP relay password) are encrypted with the same key and stored sealed in the configuration file; the control plane decrypts them at startup. Plaintext secrets are never logged. Losing the master key makes all backups unrecoverable; see the warning in §4.1.
* **Kubernetes:** a namespace per site with default-deny NetworkPolicies, Pod Security `restricted`, no service account tokens, and a control plane whose RBAC reaches only PressFleet's namespaces (§4.8).
* **Docker socket:** Access to the Docker socket is equivalent to root on the host. Traefik uses a read-only socket proxy, and only the control plane has write access. The proxy still lets Traefik inspect containers, which returns their environment, so no container the control plane creates gets a secret as an environment variable: backup tasks read theirs from files (§4.2).
* **Developer access:** SFTP and database tunnels authenticate with SSH keys tied to platform users, not shared passwords. SFTP runs as the site's UID with only its `wp-content` mounted, and database sessions use temporary MariaDB users that are dropped at expiry, so the site's own database password is never revealed (§5.8).
* **Dashboard login to WordPress:** platform users log in to wp-admin with signed tokens that expire after 60 seconds and work once, sent in a POST body rather than a URL. Sites hold only the platform's public key, and every login is audited (§5.9).
* **Control plane access:** All API endpoints require authentication (session for the UI, API tokens for automation) and role-based authorization (§4.6). Dashboard logins can require a second factor, and sensitive actions a fresh one (§6.2). State-changing actions and authorization denials are written to the audit log.

### 6.1. WordPress Security Set

The images already make WordPress core and `wp-config.php` read-only, disable core updates, deny `/xmlrpc.php` by default, and refuse to run PHP from `uploads`. On top of that, the platform ships a configurable security set. It is one install-wide setting (§4.1): each feature is on or off for every site, with no per-site overrides. Changing it updates existing sites.

PHP features are modules of the platform must-use plugin in the site images (`/opt/pressfleet/mu-plugins`), switched on by `PRESSFLEET_*` constants in the generated `wp-config.php`. They ship and update with the images, site admins can't deactivate them, and they add nothing to `wp-content`. Web server features are nginx settings in the `site-nginx` image.

| Feature | Default | Implementation |
| :--- | :---: | :--- |
| **Block user enumeration:** `?author=N` redirects, `/wp/v2/users` for anonymous requests, author data in oEmbed responses, the users sitemap, and login errors that reveal whether a username exists | On | Must-use plugin |
| **Password policy:** minimum length and a common-password list, checked on the server when a password is set (profile, reset, user creation, WP-CLI). WordPress's own strength meter only runs in the browser. | On | Must-use plugin |
| **Password hashing:** WordPress's bcrypt default, or Argon2id. Argon2id costs noticeable memory per login, which counts against the capacity target (§4.1). | bcrypt | Must-use plugin (`wp_hash_password_algorithm`) |
| **Login rate limiting:** per-IP request rate limit on `wp-login.php` | On | nginx `limit_req` |
| **Disable the theme and plugin file editor** (`DISALLOW_FILE_EDIT`). Plugin and theme updates keep working. | On | `wp-config.php` |
| **Security headers:** `Referrer-Policy`, `X-Frame-Options: SAMEORIGIN`, `Permissions-Policy`. HSTS on production domains only, without `includeSubDomains`, since a client's other subdomains may not serve HTTPS. | On | nginx; HSTS via a Traefik middleware on primary-domain routers |
| **Two-factor authentication:** the WordPress.org [Two Factor](https://wordpress.org/plugins/two-factor/) plugin, installed during provisioning, optionally required for administrators. When required, administrators without a second factor (including the one provisioning creates) are sent to set one up at their next login, and can't reach the rest of wp-admin or use application passwords until they have. Logins from the dashboard (§5.9) are exempt: the platform login stands in for the second factor. | Off | Regular plugin (updated by WordPress); enforcement in the must-use plugin |
| **Disable application passwords.** Left available by default because integrations rely on them. | Off | Must-use plugin |

All-in-one security suites (Wordfence and similar) are deliberately excluded: their memory use conflicts with the capacity target, they expect to write to `wp-config.php`, `.user.ini`, and core files (all read-only here), and they duplicate platform features.

### 6.2. Dashboard Two-Factor Authentication

Dashboard logins can require a second factor: an authenticator app (TOTP), or a security key or passkey (WebAuthn, which covers FIDO2 keys such as YubiKeys), with recovery codes as a fallback (decided 2026-10-18). Any user can turn it on for themselves; a platform setting can require it. This is separate from two-factor in sites' wp-admin (§6.1).

**Settings (§4.1):**

| Setting | Default | Meaning |
| :--- | :---: | :--- |
| `two_factor.required` | `off` | `off`: optional for everyone. `admins`: Admins must enroll. `all`: every user must. |
| `two_factor.reauthentication` | `on` | Ask for a fresh check before sensitive actions in the dashboard. |
| `two_factor.reauthentication_max_age` | `10m` | How long a check counts as fresh (1 minute to 1 hour). |

**Login:** a password login for a user with a second factor answers `401` with the problem type `second-factor-required`, listing the user's methods and the WebAuthn request options, and sets a short-lived (5 minutes), single-use `pressfleet_login` cookie. `POST /api/v1/auth/login/second-factor` checks the factor and starts the session. Users without a second factor log in as before. Failed factor checks count against the same per-account and per-address throttle as failed passwords, and are audited.

**Factors:**
* **TOTP:** RFC 6238 (SHA-1, 6 digits, 30-second steps, one step of skew), confirmed with a code at enrollment. The secret is sealed with the master key, since it can't be hashed; each time step's code works once. One per user.
* **WebAuthn:** security keys and passkeys, any number per user, named by the user. The relying party ID is `dashboard_domain` (`localhost` without one), and origins come from configuration, never from the request. Attestation is `none`. Registration prefers resident keys and records each credential's user verification and backup flags, and each user has a random WebAuthn user handle, so credentials registered now can later sign in without a password. A signature counter that goes backwards is audited, not refused, since synced passkeys report 0. Changing `dashboard_domain` makes every registered credential unusable; `pressfleet doctor` warns about credentials bound to another relying party ID.
* **Recovery codes:** ten single-use codes of 80 random bits, stored as SHA-256 hashes and shown once. Regenerating replaces the set. They stand in for a second factor at login but don't count as enrollment.

**Enforcement:** when `two_factor.required` covers a user's role and they have no second factor, a password login starts a restricted session that can only view the user, log out, and enroll; anything else is refused with `403` and the problem type `two-factor-enrollment-required`. It is checked on every request, so role and setting changes apply immediately (§4.6). A user can't remove their last second factor while one is required. API tokens and SSH keys of users who haven't enrolled keep working, so turning the setting on doesn't break automation.

**Reauthentication:** sessions record when the user last proved who they are (`verified_at`: at login, and on `POST /api/v1/auth/reauthenticate` with a second factor, or the password for users without one). With `two_factor.reauthentication` on, the authorization check refuses the following actions from a session verified longer ago than the maximum age, with `403` and the problem type `reauthentication-required`; the dashboard asks for the factor and retries:

| Action | Why |
| :--- | :--- |
| Add, remove, or reset second factors; regenerate recovery codes | They protect the account |
| Create API tokens; add SSH keys | Long-lived credentials that bypass two-factor |
| Create, update (including roles), and delete users; manage others' API tokens and SSH keys | Privilege changes |
| Open read-write database sessions | Direct write access to site data |
| Delete sites, delete client groups, offboard client groups | Destructive, and can delete backups |

Password changes already ask for the current password. Dashboard login to WordPress (§5.9) isn't on the list: it is frequent and scoped to one site. API tokens can't be asked to reauthenticate, so the checks don't apply to them, but tokens can never create API tokens (§4.6) and second factors are managed from sessions only. The exception is user management: an Admin's full-scope token can still create, change, and delete users without a fresh check, so it is as powerful as the Admin account (accepted risk, §4.6; guidance in `docs/configuration.md`, "API tokens").

**Resets:** removing a factor signs the user out of their other sessions. An Admin can reset another user's second factors, which removes them all and signs that user out everywhere; `pressfleet user reset-two-factor`, run inside the control plane container (§4.7), does the same for a locked-out last Admin. Every enrollment, removal, reset, reauthentication, and recovery-code login is audited (§4.6), without codes, secrets, or keys.

**Passwordless sign-in** with a passkey is planned after the MVP; the constraints above (resident keys, user handles, stored flags, and a challenge store that doesn't need to know the user) keep it from needing a migration or re-registration.

---

## 7. Monitoring, Metrics & Alerting

Monitoring is a core feature, surfaced natively in the dashboard rather than requiring a separate tool. Metrics are stored in VictoriaMetrics and exposed in Prometheus format, so agencies can optionally connect Grafana or other tooling: with `metrics.public_domain` set, Traefik serves only the read-only query paths on that domain behind HTTP basic auth, with a platform-generated credential that is shown once and can be rotated. Otherwise VictoriaMetrics is reachable on the platform network only. Retention is configurable and defaults to 90 days.

### 7.1. Per-Site Metrics

| Category | Metrics | Source |
| :--- | :--- | :--- |
| **Availability** | Uptime, HTTP check status, response time | Control plane HTTP checks |
| **Traffic** | Request rate, status code breakdown, latency percentiles | Traefik Prometheus metrics |
| **Page cache** | FastCGI cache hit/miss/bypass ratio | Nginx (`$upstream_cache_status`) |
| **PHP** | Active/idle FPM workers, max-children reached, slow requests | PHP-FPM status endpoint |
| **Resources** | CPU, memory, restarts | Docker stats API |
| **Storage** | `wp-content` volume size, database size | Control plane collector |
| **Object cache** | Key count, memory used, hit ratio (per prefix where feasible) | Redis |
| **TLS** | Certificate expiry per domain | Control plane checks |
| **Backups** | Last successful backup time, backup size, duration | Backup jobs |
| **Updates** | Pending plugin and theme updates | Control plane update checks (WP-CLI) |

### 7.2. Platform Metrics
* Host CPU, memory, disk usage, and disk I/O. On Kubernetes, per-node CPU and memory from `metrics.k8s.io`; disk metrics aren't available, so the built-in disk usage rule has nothing to evaluate, and the cluster's own monitoring covers them (§4.8).
* MariaDB connections, slow queries, and buffer pool usage.
* Redis memory usage, hit ratio, and evictions.
* Job queue depth and failure rate.
* Restic repository size per client group.

### 7.3. Alerting
* **Default alert rules:** site down, elevated 5xx rate, certificate expiring or failed to renew, backup failed or stale, disk nearly full, container restart loop, PHP-FPM worker saturation, Redis eviction spikes, plugin or theme updates pending (§5.6).
* **Channels (MVP):** email (SMTP) and generic webhook (covers Slack, Teams, etc. via incoming webhooks).
* **Staging sites (§5.11):** built-in site-scoped rules skip staging sites, which are disposable and often idle or stopped; a rule can include them (`include_staging`). Backup alerts already skip sites whose backup schedule is off. `pressfleet_site_info` carries `environment` and `parent_site_id` labels.
* **Limitation:** Uptime checks run from the PressFleet host itself and cannot detect a full host outage. External uptime checking is a future consideration.

### 7.4. Logs
* Site access and error logs, PHP-FPM slow logs, and job logs are viewable per site in the dashboard, read from container logs via the orchestrator driver.
* Reading a site's runtime logs (`listSiteLogs`, `streamSiteLogs`) is an Operator action (decided 2026-10-21, #181), and the dashboard hides the logs viewer from Viewers. The access log records `$request_uri` with its query string, and WordPress and plugins put one-time secrets there (password reset keys, magic-login links), so a Viewer who could read it could take over a WordPress account. The log format is unchanged, so Operators and Admins can still see those query strings; they already have wp-admin and database access (§5.8, §5.9). Viewers keep metrics and alerts, and a job's log lines. We may revisit this after the MVP.
* Log rotation limits are set on all containers to cap disk usage.

---

## 8. Implementation Milestones

```
[ Phase 1: Core Engine ] ──► [ Phase 2: Ingress & Caching ] ──► [ Phase 3: Backup & Files ] ──► [ Phase 4: Monitoring ] ──► [ Phase 5: Dashboard ]
                                                                                                                                  │
                                                                             [ Post-MVP: Staging Clones (priority) ] ◄────────────┘
                                                                             [ Post-MVP: Kubernetes ]
                                                                             [ Post-MVP: MCP Server ]
```

* **Phase 1: Core Engine & Compose Driver**
  * Repository scaffolding, MIT license, and GitHub Actions CI (lint, test, image builds to GHCR).
  * `pressfleet init` (including master key generation and warning) and `pressfleet doctor` preflight checks.
  * Initial OpenAPI spec with code generation for the Go server.
  * Establish the Go control plane API, job queue, and SQLite/PostgreSQL schema with forward-only migrations (users, client groups, sites, jobs; §4.7).
  * Implement `ComposeProvisioner` (per-site networks, two-container site stack, resource reservations/limits) and `MariaDBManager`.
  * Build the `site-nginx` and `site-php` images for each supported PHP version, with a generated, read-only `wp-config.php`.
  * API authentication (sessions and scoped tokens), role-based authorization, and the audit log (§4.6).
  * **Done when:** a site can be created in a client group via the API, reached over HTTP on the host, and deleted with no leftover containers, networks, volumes, or database, and two sites cannot reach each other over the network.
* **Phase 2: Ingress, Caching & Domain Management**
  * Integrate Traefik with the Docker provider via a socket proxy.
  * Preview domains with wildcard (DNS-01) or per-site (HTTP-01) certificates.
  * Go-live workflow with primary domains and aliases; automatic HTTP/2, HTTP/3, and Let's Encrypt certificates.
  * Shared Redis with per-site ACL users; Nginx `fastcgi_cache` defaults including cache purging.
  * Optional preview password protection and the WordPress security set (§6.1).
  * **Done when:** a new site is reachable over HTTPS on its preview domain within seconds of provisioning, go-live switches it to a custom domain with a valid certificate, and one site cannot read or flush another site's object cache.
* **Phase 3: Backup Engine & File Management**
  * Implement the Restic-based `BackupEngine` with per-client-group repositories, scheduled backups, retention, and integrity checks.
  * Implement the restore pipeline with pre-restore snapshots and search-replace.
  * WordPress core update rollout with major release approval and branch holds (§5.5).
  * Site import from a `wp-content` archive and SQL dump (§5.7).
  * Developer access: SFTP gateway, deploy API, database exports, and temporary database sessions (§5.8).
  * **Done when:** a site can be backed up, modified, and restored to its earlier state, a failed restore rolls back automatically, an existing site can be imported from an archive and dump, and a developer can deploy code and reach the database only for the site they're working on.
* **Phase 4: Monitoring & Alerting**
  * Deploy VictoriaMetrics as a platform service and implement the built-in metrics collector.
  * HTTP uptime checks, TLS expiry checks, and backup health metrics.
  * Alert rules engine with email and webhook channels.
  * Metrics and log query API endpoints for the dashboard.
  * **Done when:** every metric in §7.1–7.2 is collected and queryable via the API, and each default alert rule fires and resolves correctly in integration tests.
* **Phase 5: Dashboard**
  * Build the Nuxt management UI for client groups, provisioning, go-live, site status, job progress, backups, metrics charts, logs, alerts, users and roles, and the audit log, plus one-click login to a site's WordPress admin (§5.9).
  * Two-factor authentication for dashboard logins: TOTP, security keys and passkeys, recovery codes, an optional requirement, and reauthentication for sensitive actions (§6.2).
  * Embed the static UI build into the Go binary.
  * **Done when:** every Phase 1–4 operation can be performed from the UI.
* **MVP Release Gate**
  * Capacity test: 50 sites on an 8 GiB host stay healthy under light traffic (§4.1).
  * Forward-only `pressfleet upgrade` with a documented, tested manual recovery procedure, and enforcement of the version skew policy between the host CLI and the control plane (§4.7).
* **Post-MVP (High Priority): Staging Clones**
  * Clone a production site to a linked staging site on a preview domain, refresh it from production, and push its code, files, or database back to production with a pre-push backup and automatic rollback (§5.11).
* **Post-MVP: Kubernetes Driver**
  * A Kubernetes driver using `client-go`, with templates in `/templates/k8s`, that passes the same conformance suite as the Compose driver (§4.8).
  * A Helm chart for the platform; install, upgrade, and recovery on a cluster; doctor's cluster checks; and metrics on Kubernetes.
  * **Done when:** on a kind cluster installed from the chart, a site can be provisioned, reached over HTTPS on its preview domain, taken live on a custom domain, backed up and restored (with automatic rollback on failure), reached over SFTP, and deleted with nothing left behind; two sites can't reach each other or the cluster's API; and an upgrade between two releases and a disaster recovery drill succeed.
* **Post-MVP: Upgrade Rollback**
  * Automatic rollback of a failed `pressfleet upgrade`, and `pressfleet upgrade --rollback`, automating the manual recovery procedure (§4.7).
  * **Done when:** an upgrade whose health checks fail restores the previous platform and state on its own; `pressfleet upgrade --rollback` does the same on demand, and refuses (without `--force`) changes that would leave sites broken or orphaned; and a rollback that fails prints manual steps that recover the platform.
* **Post-MVP: MCP Server**
  * A Model Context Protocol server built into the control plane (Streamable HTTP, off by default) whose tools are derived from the OpenAPI spec and cover the dashboard's operations.
  * Every tool goes through the API's authorization check and audit log; `tools/list` shows only what the caller's token and role allow.

---

## 9. Open Questions

1. ~~**Security plugins:** Which plugins make up the default "agency security" set, if any?~~ **Resolved (2026-09-27):** a configurable security set, set install-wide with no per-site overrides: must-use plugin modules, nginx settings, and the optional Two Factor plugin. No all-in-one security suite. See §6.1.
2. ~~**Plugin/theme update visibility:** WordPress handles plugin and theme updates, but should the dashboard *report* pending updates across all sites (via WP-CLI), and should it take a backup before updates run?~~ **Resolved (2026-10-02):** report only. Pending plugin and theme updates count towards a site's health and are shown in the dashboard; the platform neither performs the updates nor takes a backup around them. See §5.6.
3. ~~**Core rollout policy:** Confirm the proposed default in §5.5 (security/minor releases rolled out automatically, major releases approved per site or client group).~~ **Resolved (2026-09-28):** the proposal, with holds per major branch that keep receiving minor and security releases, and an optional delay after which majors are approved automatically. See §5.5.
4. ~~**User roles:** Do agency staff need roles beyond full access (e.g. read-only, or access limited to certain client groups)?~~ **Resolved (2026-09-26):** fixed platform-wide Admin, Operator, and Viewer roles, only Admins can delete sites, and an audit log ships in the MVP. Client-group scoping is deferred. See §4.6.
5. ~~**Preview domain protection:** Is `noindex` sufficient, or should preview domains optionally require HTTP basic auth?~~ **Resolved (2026-09-27):** optional per-site basic auth on the preview domain via Traefik, off by default, with a platform-generated password stored encrypted. See §5.1.
6. ~~**Monitoring stack:** Confirm the proposed VictoriaMetrics + built-in collector approach (§3, §7).~~ **Resolved (2026-10-01):** confirmed: single-node VictoriaMetrics with a built-in Go collector, no Prometheus, cAdvisor, or per-site exporters. See §3 and §7.
7. ~~**Metrics retention:** How long should metrics be retained (e.g. 30 or 90 days)?~~ **Resolved (2026-10-01):** configurable, defaulting to 90 days. See §7.
8. ~~**Developer code deployment:** How do agency developers get custom themes/plugins onto a site: SFTP, git push, a CI deploy API endpoint, WP-CLI/SSH access? Currently only an optional SFTP/file browser is planned (Phase 3).~~ **Resolved (2026-09-28):** an SFTP gateway authenticated with platform users' SSH keys, plus a deploy API for CI. No git push or shell access. See §5.8.
9. ~~**Importing existing sites:** Is an import workflow (upload an archive + DB dump, or pull from a remote host) part of the MVP?~~ **Resolved (2026-09-28):** yes, from a `wp-content` archive and SQL dump; other sources may follow after MVP. See §5.7.
10. ~~**Database access:** Should agencies get a database UI (e.g. Adminer started on demand), WP-CLI only, or neither?~~ **Resolved (2026-09-28):** database exports through the platform, and temporary, read-only-by-default database sessions tunneled through the SFTP gateway. No web database UI in the MVP. See §5.8.
11. ~~**Platform upgrades:** How does an existing install upgrade to a new PressFleet release (control plane migrations, platform service upgrades, rollback)?~~ **Resolved (2026-09-26):** forward-only migrations applied at startup after a state backup; the host CLI upgrades the control plane to its own version; upgrades are forward-only with a documented manual rollback (automatic rollback revised to post-MVP on 2026-10-03); kubectl-style version skew policy between the host CLI and the control plane. See §4.7.

---

## 10. Next Steps for Implementation Agent

1. **Repository Setup:** Initialize a monorepo following the conventions in §3.1, containing `/api` (OpenAPI spec), `/control-plane` (Go backend), `/ui` (Nuxt frontend), `/images` (`site-nginx` and `site-php` images), and `/templates/compose` (site Compose templates). Reserve `/templates/k8s` for the future Kubernetes driver.
2. **Interface Implementation:** Write the OpenAPI spec for Phase 1 endpoints (§4.5), the core Go interfaces from §4.4 (`Provisioner`, `DatabaseManager`, `BackupEngine`), the domain model from §4.3, and the job queue.
3. **Integration Test Suite:** Build an integration test framework using Docker-in-Docker (dind) to validate site creation, WP-CLI execution, and teardown against the Compose driver.
4. **Scope Discipline:** Build what the current milestone's issues and accepted plan describe (§8). Features listed under Non-Goals (§1.1) stay out until a milestone takes them on, as staging clones (§5.11) and Kubernetes (§4.8) now have. Raise open questions, and resolve the milestone's decision issues, before building features that depend on them.
