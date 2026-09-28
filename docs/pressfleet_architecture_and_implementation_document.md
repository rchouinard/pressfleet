# Project Specification: PressFleet Architecture & Implementation Plan

**Project Codename:** PressFleet  
**Status:** Draft  
**Last Updated:** 2026-09-27  
**Target Audience:** Agencies specializing in WordPress development and management  
**Core Vision:** A self-hostable, containerized application server for WordPress—a smaller-scale, open-source alternative to WP Engine featuring one-click deployments, automatic TLS, page/object caching, built-in monitoring, file management, and automated backup/restore routines.

---

## 1. Executive Summary & Goals

PressFleet aims to simplify WordPress management for agencies by delivering:
* **One Agency, One Platform:** Each PressFleet install serves a single agency. Sites are organized into client groups.
* **One-Click Provisioning:** Spin up fully configured WordPress sites from a single API call or dashboard action, immediately reachable on a preview subdomain.
* **Docker Compose First:** Docker Compose is the default deployment target for single-host, small, and demo installations. The architecture must not preclude a future Kubernetes deployment target (see §4.2).
* **Low Resource Footprint:** Shared platform services (database, object cache, metrics storage) are preferred over per-site copies wherever isolation can be preserved.
* **Built-in Performance:** Pre-configured Nginx FastCGI page caching and Redis object caching for every site.
* **Monitoring & Metrics as a Core Feature:** Uptime, performance, resource usage, and backup health for every site, with alerting.
* **Integrated Backup Engine:** Encrypted, deduplicated backups to local or S3-compatible storage with automated restoration/rollback pipelines.
* **Platform-Managed Runtime:** The platform owns the PHP runtime (versions, extensions, security patches) and WordPress core, both shipped in immutable site images. Plugin and theme updates are left to WordPress's native update mechanisms. Site data is limited to `wp-content` and the database, which keeps backups small and focused.

### 1.1. Non-Goals (MVP)

* **Multi-agency / PaaS hosting.** One install serves one agency. A multi-agency PaaS may be considered in the future.
* **Client-group-scoped user access.** MVP roles apply platform-wide; restricting users to specific client groups is a future consideration that §4.6 keeps possible.
* **Client access to the platform.** Only agency staff use the dashboard and API. Exposing platform features to clients (e.g. via WordPress plugins that call the platform API) is a future consideration (§4.2).
* **Staging clones.** Post-MVP, but the **highest-priority** follow-up (§8). The MVP data model and restore pipeline should anticipate it.
* **Kubernetes deployment.** Planned for a later release; the MVP only has to keep the door open (§4.2).
* **WordPress Multisite.**
* **Managing plugin and theme updates.** WordPress handles these natively.
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
| **Site Stack** | The set of containers and volumes making up one site (one Compose project per site). |
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
| **Control Plane API** | **Go** | — | Fast execution, low memory, single static binary, native Docker Engine API and Kubernetes `client-go` SDKs for the future K8s driver. |
| **Control Plane UI** | **Nuxt (Vue 3) + Tailwind** | — | Dashboard for site management, client groups, monitoring, and job logs. Built as a static SPA (`nuxt generate`) and embedded into the Go binary via `embed`, so no Node.js runtime is required in production. |
| **Orchestration (MVP)** | **Docker Compose** | Docker Engine API directly | One Compose project per site, rendered from templates. Human-readable, easy to inspect and debug on the host. |
| **Orchestration (Future)** | **Kubernetes (`k8s.io/client-go`)** | — | Multi-node scale. Not an MVP goal. |
| **Control Plane State** | **SQLite** (default) | PostgreSQL | SQLite for Compose installs (zero extra services); PostgreSQL supported for larger installs and required for the future K8s target. |
| **Job Queue** | **Database-backed queue** | Redis-backed queue | Jobs are stored in the control plane database so the queue has no dependency on the shared Redis used by sites. |
| **Edge Proxy & TLS** | **Traefik** | Caddy | Dynamic container routing via Docker labels (and Kubernetes CRDs later) with automatic Let's Encrypt certificates, including DNS-01 wildcard certificates for preview domains. |
| **WP Web Server** | **Nginx + PHP-FPM** | OpenLiteSpeed | Web server tuned with `fastcgi_cache` pre-configured for WordPress page caching. |
| **Site Database** | **MariaDB LTS (11.4+)** | MySQL 8.4 LTS | Shared database server with per-site databases and users. |
| **Object Cache** | **Valkey** (Redis-compatible; shared, platform-managed) | Redis, Memcached, per-site Redis | A single server keeps memory usage low; per-site isolation is enforced with ACL users, per-site logical databases, and key prefixes (§6). Valkey is BSD-licensed (Redis 8 is AGPLv3/RSALv2/SSPLv1), publishes an official image, and speaks the Redis protocol, so the Redis Object Cache plugin works unchanged. Its per-database ACL rules (Valkey 9+), which Redis lacks, are what keep one site from listing another's keys (§6). "Redis" in PressFleet names the protocol and role, not the server. |
| **Metrics Storage** | **VictoriaMetrics (single-node)** *(proposed)* | Prometheus | Prometheus-compatible, markedly lower memory and disk usage, single binary. |
| **Metrics Collection** | **Built-in Go collector** *(proposed)* | cAdvisor, per-site exporter sidecars | The control plane scrapes Docker stats, Traefik, PHP-FPM, Nginx, MariaDB, and Redis directly, avoiding a sidecar per site. |
| **Backup Engine** | **Restic** | Borg, Duplicity | Encrypted, deduplicated snapshots of `wp-content` and DB dumps pushed to local disk or S3/MinIO. |

### 3.1. Project Conventions

| Area | Convention |
| :--- | :--- |
| **License** | MIT |
| **Hosting** | GitHub (`github.com/rchouinard/pressfleet`) |
| **Go** | Latest stable release (currently Go 1.27); module path `github.com/rchouinard/pressfleet` |
| **CI/CD** | GitHub Actions |
| **Container registry** | GitHub Container Registry (`ghcr.io/rchouinard/pressfleet/...`) |
| **Supported platforms** | Linux hosts on `amd64`. Other architectures (e.g. `arm64`) are not built or tested for the MVP. |

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
| MariaDB | ~768 MiB |
| Redis (`maxmemory`) | ~256 MiB |
| VictoriaMetrics | ~256 MiB |
| Traefik + control plane | ~256 MiB |
| OS and headroom | ~256 MiB |

* **Per-site memory:** each site has a **128 MiB reservation** (the guaranteed baseline, split between `nginx` and `php`) and a higher **hard limit** (default 512 MiB) that allows bursting. Bursting relies on not every site peaking at once. Reservations must not exceed host memory; `pressfleet doctor` and the dashboard warn when they do.
* **PHP-FPM:** runs with `pm = ondemand` so idle sites hold no worker processes. `pm.max_children` is derived from the site's memory limit.
* **Validation:** before MVP release, a capacity test provisions 50 sites on an 8 GiB host and verifies they stay healthy under light traffic.

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

### 4.2. Future Compatibility Constraints

Kubernetes and client-facing integrations are out of scope for the MVP. To keep them achievable, the MVP must follow these constraints:
* All orchestration goes through the `Provisioner` interface (§4.4); no Docker/Compose calls outside the Compose driver.
* Site configuration is expressed as data (`SiteConfig`), never as Compose-specific fields.
* Control plane state must work on PostgreSQL, not just SQLite.
* Site containers must be stateless apart from declared volumes (`wp-content`) so they can later run as Pods with PVCs.
* API tokens carry a scope so that site-scoped tokens for future client-facing WordPress plugins can be added without redesigning authentication. A token can never do more than its owner's role allows (§4.6).
* Authorization checks resolve every target to its client group, so client-group-scoped user access can be added later without redesigning authorization (§4.6).
* The site model supports a parent/child relationship between sites so a staging clone can later be linked to its production site.

### 4.3. Domain Model (MVP)

| Entity | Key Fields / Relationships |
| :--- | :--- |
| **User** | Email, password hash, role (Admin, Operator, or Viewer; §4.6), API tokens, SSH public keys (§5.8). |
| **ClientGroup** | Name, notes, backup repository reference. Has many Sites. |
| **Site** | Client group, slug, preview domain, primary domain (nullable until go-live), aliases, PHP version, WordPress core version, core branch hold and major release approval (§5.5), resource limits, preview password protection (off by default; §5.1), status, parent site (nullable, reserved for staging). |
| **Job** | Type, target, state, step log, timestamps. |
| **BackupRecord** | Site, snapshot ID, WordPress core version at backup time, size, status, timestamp. |
| **AlertRule / Alert** | Condition, target, channel; firing and resolved events. |
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
    ExecWPCLI(ctx context.Context, siteID string, args []string) (string, error)
    PurgeCache(ctx context.Context, siteID string) error // page cache; the object cache manager flushes the object cache
}

// DatabaseManager manages per-site databases and users on the shared MariaDB server.
type DatabaseManager interface {
    CreateDatabase(ctx context.Context, siteID string) (secretRef string, err error)
    DropDatabase(ctx context.Context, siteID string) error
    Dump(ctx context.Context, siteID string, destPath string) error
    Import(ctx context.Context, siteID string, srcPath string) error
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
    Restore(ctx context.Context, siteID string, snap SnapshotID, targetSiteID string) error
    Prune(ctx context.Context, clientGroupID string) error
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
```

Site components are named by role (`nginx`, `php`), not by orchestrator object. Backups use `TaskRunner`: the control plane dumps the database into a transfer directory under its data directory, then runs `restic` in a task that mounts `wp-content` read-only plus the transfer directory. In the Compose driver, a task is a one-off container on the site's network. The data directory is mounted at the same path on the host and in the control plane container, so transfer directories can be bind-mounted into tasks.

| Interface | Compose driver | Future Kubernetes driver |
| :--- | :--- | :--- |
| `LogReader` | Container logs | Pod logs API, per container |
| `StatsReader` | Docker stats (cumulative CPU) and container restart count | Kubelet summary API (cumulative CPU) and Pod status restart counts |
| `TaskRunner` | One-off container mounting the site volume and a bind-mounted transfer directory | Job whose Pod mounts the site PVC and a shared transfer volume |

### 4.5. API Design

* **Spec-first:** The API is defined in an OpenAPI 3.1 document (`/api/openapi.yaml`), which is the source of truth. Go server interfaces and request/response types, and the TypeScript client used by the Nuxt UI, are generated from it. CI fails if generated code is out of date with the spec.
* **Versioned paths:** All endpoints live under `/api/v1/`. Breaking changes require a new version prefix; additive changes do not.
* **Conventions:** JSON request/response bodies, standard HTTP status codes, `202 Accepted` + job ID for long-running operations, and a consistent error body (RFC 9457 Problem Details).
* **Authentication:** Session cookies for the embedded UI; bearer API tokens (with scopes, §4.2) for automation.
* **Authorization:** Role-based, enforced by the server on every endpoint (§4.6). `403 Forbidden` responses use the standard Problem Details body.

### 4.6. Authorization & Roles

Each user has one platform-wide role. The roles are fixed in the MVP; there are no custom roles.

| Action | Admin | Operator | Viewer |
| :--- | :---: | :---: | :---: |
| View sites, client groups, jobs, backups, metrics, logs, and alerts | ✓ | ✓ | ✓ |
| Create and import sites; change site settings (PHP version, core branch hold, resources, preview password protection); go-live and domains; start/stop; maintenance mode; cache purge | ✓ | ✓ | |
| Deploy code; connect over SFTP; export the database; open read-only database sessions (§5.8) | ✓ | ✓ | |
| Open read-write database sessions | ✓ | | |
| Take backups, restore, and approve core updates | ✓ | ✓ | |
| Create and edit client groups; manage alert rules | ✓ | ✓ | |
| Delete sites | ✓ | | |
| Delete or offboard client groups (export or delete the backup repository and its key) | ✓ | | |
| Manage users, roles, and other users' API tokens; view the audit log | ✓ | | |
| Change platform settings (DNS and S3 credentials, SMTP, alert channels, backup location, retention, WordPress security set) | ✓ | | |
| Manage their own password, API tokens, and SSH keys | ✓ | ✓ | ✓ |

**Rules:**
* **Single check point:** Every API handler calls one authorization check with the caller, the action, and the target before doing anything. The UI hides actions the user can't take, but the server is authoritative.
* **Tokens:** A token's effective permissions are the intersection of its scope and its owner's *current* role. MVP scopes are `full` (everything the owner's role allows) and `read-only`. Future site-scoped tokens (§4.2) plug into the same check.
* **Immediate effect:** Role changes apply immediately to the user's existing sessions and tokens.
* **At least one Admin:** The last Admin can't be demoted, disabled, or deleted. `pressfleet init` creates the first user as an Admin, and `pressfleet user create --role` (run inside the control plane container, §4.7) exists for recovery.
* **Client groups:** The check resolves every target (site, backup, job, alert) to its client group. Client-group-scoped access (post-MVP) only needs a user↔group membership table on top of this.

**Audit log:** Every authenticated state-changing request and every authorization denial is recorded as an `AuditEvent`. Each event holds the actor (user and token), action, target, result, source IP, and timestamp, and never includes secrets. Admins can view and filter the audit log in the dashboard. Retention is configurable.


### 4.7. Versioning, Upgrades & Version Skew

PressFleet uses semantic versioning. Each release bundles the control plane image, the host CLI, and the platform definition: the platform Compose templates plus the exact image digests of every platform service.

**Responsibilities:**
* The **control plane** owns its state database: migrations, pre-migration backups, and restoring those backups.
* The **host CLI** owns the platform definition and renders the platform Compose project from templates embedded in its binary. It never reads or writes the state database directly. Commands that operate on state (e.g. `pressfleet user create`) run inside the control plane container (`docker compose exec control-plane pressfleet …`), so their version always matches the database schema.

**Migrations:**
* Migrations are forward-only; there are no down migrations.
* The control plane applies pending migrations automatically at startup, after backing up the state database.
* The control plane refuses to start if the database schema is newer than the versions it supports.
* One-off data migrations must be safe to run more than once.

**Upgrades (`pressfleet upgrade`):**
* The host CLI upgrades the control plane to the CLI's own version. To upgrade, install the new CLI and run `pressfleet upgrade`.
* Upgrading directly from any older version is supported, provided it is at or above the minimum version that release supports upgrading from.
* Steps: run preflight checks; record the current platform definition and image digests; render the new platform Compose project; pull images; recreate only the services that changed; wait for the control plane (which migrates at startup) and platform services to report healthy.
* If health checks fail, the upgrade rolls back automatically. It restores the recorded platform definition and images, then restores the pre-migration state backup using the previous control plane version. `pressfleet upgrade --rollback` does the same by hand.
* Rollback is for failed or immediately regretted upgrades. Anything recorded in the state database after the upgrade (e.g. new sites) is lost on rollback.
* Upgrading only the control plane doesn't interrupt site traffic. Recreating Traefik, MariaDB, or Redis briefly interrupts every site.
* MariaDB major version upgrades are one-way. They only ship in major PressFleet releases, after a backup of every site database.
* When site Compose templates change, sites are re-rendered and recreated one at a time with health checks, using the image rollout mechanism (§5.5).

**Version skew between the host CLI and the control plane:**
* Patch versions within the same minor version are always compatible.
* From 1.0, the host CLI's minor version must be within one of the control plane's (n−1, n, or n+1, where n is the control plane's minor version). Before 1.0, the minor versions must match.
* Before every command, the CLI checks the control plane's version. Outside the policy it refuses to run and explains how to fix it; there is no override. `pressfleet version` and `pressfleet upgrade` are exempt.
* The host CLI talks to the control plane only through a small, versioned admin interface. From 1.0, changes to that interface must stay backward compatible for at least one minor release, and CI tests the CLI against control planes at n−1, n, and n+1.

---

## 5. Core Operational Workflows

All long-running operations run as **Jobs**. API endpoints that start them return `202 Accepted` with a job ID; clients poll `GET /api/v1/jobs/{id}` (or receive updates via the UI). Each job step must be idempotent so a failed job can be retried safely, and a job that fails permanently must roll back resources it created.

### 5.1. One-Click Provisioning Workflow
1. **User Request:** Agency user calls `POST /api/v1/sites` with client group, site name, and admin email. Returns `202` + job ID.
2. **Resource Allocation:** Control plane assigns a site ID and slug, derives the preview domain (`<slug>.<preview-base-domain>`), and generates database credentials, a Redis ACL user, and a WordPress admin password (stored encrypted; the admin password is shown to the user once).
3. **Database Creation:** `DatabaseManager.CreateDatabase` creates the database and a user whose grants are limited to that database.
4. **Object Cache Credentials:** Creates the site's Redis ACL user, restricted to its own key prefix (§6).
5. **Container Provisioning:** `Provisioner.CreateSite` creates the site network and renders the site's Compose project (`nginx` and `php` containers from the images for the selected PHP version and current WordPress core, `wp-content` and cache volumes, generated `wp-config.php`, Traefik labels for the preview domain, resource limits) and brings it up.
6. **WordPress Installation:** Runs `wp core install` against the image's WordPress core with the preview domain as the site URL via `ExecWPCLI`.
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
2. The backup worker (which includes the `mariadb-dump` client and `restic`) calls `DatabaseManager.Dump` over the network to a temporary directory.
3. Runs `restic backup` over the site's `wp-content` volume (mounted read-only) and the dump file into the site's **client group repository**, tagged with the site ID and the site's WordPress core version. WordPress core and `wp-config.php` are not backed up; both are reproducible from the site image and the control plane's site record.
4. Applies the retention policy (`restic forget --prune`) and periodically verifies repository integrity (`restic check`). Pruning takes an exclusive repository lock, so prune/check jobs are serialized per repository and scheduled away from backup windows.
5. Records the result as a `BackupRecord` and emits backup metrics (§7); failed or stale backups raise alerts.

#### Repository Layout
* One Restic repository per client group, each with its own encryption key.
* Sites within a group deduplicate against each other (shared plugins, themes, and media are common).
* Offboarding a client is a matter of handing over or deleting one repository and key.

### 5.4. Restore Workflow
1. Takes an automatic **pre-restore snapshot** of the target site so the restore itself can be rolled back.
2. Enables maintenance mode via `Provisioner.SetMaintenance` (Traefik serves a maintenance page). In the Compose driver, higher-priority routes in Traefik's file provider send the site's domains to a static maintenance page service, so the page doesn't depend on the site's containers; the site record keeps the flag so re-renders preserve it.
3. Restores `wp-content` and the DB dump from the chosen snapshot.
4. Imports the DB dump via `DatabaseManager.Import`. If the snapshot was taken on a newer WordPress core version than the target site is running, the site is first moved to an image with at least that core version (a database schema cannot be safely run on an older core).
5. If the target domain differs from the snapshot's source (e.g., restoring into a different site), runs `wp search-replace` via WP-CLI.
6. Purges the FastCGI page cache and the site's object cache, then disables maintenance mode.
7. On failure at any step, restores the pre-restore snapshot and leaves the site in maintenance mode with an error on the job.

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
* The image matrix is kept small: for each supported PHP version, the latest WordPress core release, plus the latest release of every older branch a site is held on or awaiting approval for.

### 5.6. WordPress Runtime Defaults
* **Plugin and theme updates:** Handled natively by WordPress (auto-updates or wp-admin), writing to the `wp-content` volume.
* **WP-Cron:** `DISABLE_WP_CRON` is set; cron is triggered by the control plane scheduler so it runs reliably on low-traffic sites.
* **Cache purging:** Nginx Helper purges the FastCGI cache using file-based purging against the per-site cache volume shared between the `nginx` and `php` containers. Both containers run as the site's UID so the cache files can be deleted. The platform configures it that way at provisioning and whenever it reconciles the site. A full purge (`POST /api/v1/sites/{id}/cache/purge`, and the purges in go-live and restore) runs as a job in the control plane rather than inside WordPress, so it works on stopped or broken sites: it flushes the site's object cache database as the object cache admin, then deletes the page cache files through the driver (`Provisioner.PurgeCache`).
* **Outbound email:** Sites send mail through a configurable SMTP relay.

### 5.7. Site Import Workflow

Existing sites are imported from a `wp-content` archive and a SQL dump (decided 2026-09-28). Other sources, such as pulling from a remote host or reading migration plugin formats, may follow after MVP based on user feedback.

1. **Upload:** The archive and dump are sent through a resumable, chunked upload API with a configurable size limit. Uploads are stored under the data directory, never backed up, and deleted when the import ends.
2. **Validation:** Before anything is created, the control plane checks the archive layout and the dump, and reads the table prefix, the source URL, and the core version from the dump. It rejects multisite installs, and cores newer than the newest site image.
3. **Site creation:** Like provisioning (§5.1 steps 2–5): the new site gets a slug, preview domain, credentials, database, object cache user, and containers on the newest core.
4. **Content:** Restores `wp-content` and imports the dump with `DatabaseManager.Import` under its own table prefix, then runs `wp core update-db`. Host-specific drop-ins (`object-cache.php`, `advanced-cache.php`, `db.php`) are replaced by the platform's; other hosts' must-use plugins are kept and reported as warnings on the job.
5. **URLs and platform setup:** Runs `wp search-replace` from the source URL to the preview URL, applies the default plugin stack and platform configuration (§5.1 step 7), and purges the caches. The site is then on its preview domain and goes live like any other (§5.2).

The source's `wp-config.php` is not imported: the platform generates it (§5.5). An import that fails permanently deletes the new site, as failed provisioning does.

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
* **Secrets:** Database passwords, Redis ACL passwords, preview basic auth passwords, Restic repository keys, and S3 credentials are stored encrypted at rest in the control plane database using the platform master key, supplied via environment variable or file. Install-time secrets that `pressfleet init` collects before the state database exists (DNS provider credentials, the SMTP relay password) are encrypted with the same key and stored sealed in the configuration file; the control plane decrypts them at startup. Plaintext secrets are never logged. Losing the master key makes all backups unrecoverable; see the warning in §4.1.
* **Docker socket:** Access to the Docker socket is equivalent to root on the host. Traefik uses a read-only socket proxy, and only the control plane has write access.
* **Developer access:** SFTP and database tunnels authenticate with SSH keys tied to platform users, not shared passwords. SFTP runs as the site's UID with only its `wp-content` mounted, and database sessions use temporary MariaDB users that are dropped at expiry, so the site's own database password is never revealed (§5.8).
* **Control plane access:** All API endpoints require authentication (session for the UI, API tokens for automation) and role-based authorization (§4.6). State-changing actions and authorization denials are written to the audit log.

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
| **Two-factor authentication:** the WordPress.org [Two Factor](https://wordpress.org/plugins/two-factor/) plugin, installed during provisioning, optionally required for administrators. When required, administrators without a second factor (including the one provisioning creates) are sent to set one up at their next login, and can't reach the rest of wp-admin or use application passwords until they have. | Off | Regular plugin (updated by WordPress); enforcement in the must-use plugin |
| **Disable application passwords.** Left available by default because integrations rely on them. | Off | Must-use plugin |

All-in-one security suites (Wordfence and similar) are deliberately excluded: their memory use conflicts with the capacity target, they expect to write to `wp-config.php`, `.user.ini`, and core files (all read-only here), and they duplicate platform features.

---

## 7. Monitoring, Metrics & Alerting

Monitoring is a core feature, surfaced natively in the dashboard rather than requiring a separate tool. Metrics are stored in VictoriaMetrics and exposed in Prometheus format, so agencies can optionally connect Grafana or other tooling.

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

### 7.2. Platform Metrics
* Host CPU, memory, disk usage, and disk I/O.
* MariaDB connections, slow queries, and buffer pool usage.
* Redis memory usage, hit ratio, and evictions.
* Job queue depth and failure rate.
* Restic repository size per client group.

### 7.3. Alerting
* **Default alert rules:** site down, elevated 5xx rate, certificate expiring or failed to renew, backup failed or stale, disk nearly full, container restart loop, PHP-FPM worker saturation, Redis eviction spikes.
* **Channels (MVP):** email (SMTP) and generic webhook (covers Slack, Teams, etc. via incoming webhooks).
* **Limitation:** Uptime checks run from the PressFleet host itself and cannot detect a full host outage. External uptime checking is a future consideration.

### 7.4. Logs
* Site access and error logs, PHP-FPM slow logs, and job logs are viewable per site in the dashboard, read from container logs via the orchestrator driver.
* Log rotation limits are set on all containers to cap disk usage.

---

## 8. Implementation Milestones

```
[ Phase 1: Core Engine ] ──► [ Phase 2: Ingress & Caching ] ──► [ Phase 3: Backup & Files ] ──► [ Phase 4: Monitoring ] ──► [ Phase 5: Dashboard ]
                                                                                                                                  │
                                                                             [ Post-MVP: Staging Clones (priority) ] ◄────────────┘
                                                                             [ Post-MVP: Kubernetes ]
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
  * Build the Nuxt management UI for client groups, provisioning, go-live, site status, job progress, backups, metrics charts, logs, alerts, users and roles, and the audit log.
  * Embed the static UI build into the Go binary.
  * **Done when:** every Phase 1–4 operation can be performed from the UI.
* **MVP Release Gate**
  * Capacity test: 50 sites on an 8 GiB host stay healthy under light traffic (§4.1).
  * `pressfleet upgrade` with automatic rollback, and enforcement of the version skew policy between the host CLI and the control plane (§4.7).
* **Post-MVP (High Priority): Staging Clones**
  * Clone a production site to a linked staging site (on a preview domain), and push staging changes back to production.
* **Post-MVP: Kubernetes Driver**
  * Implement `KubernetesProvisioner` using `client-go`, with templates in `/templates/k8s`.
  * Test against kind or k3d.

---

## 9. Open Questions

1. ~~**Security plugins:** Which plugins make up the default "agency security" set, if any?~~ **Resolved (2026-09-27):** a configurable security set, set install-wide with no per-site overrides: must-use plugin modules, nginx settings, and the optional Two Factor plugin. No all-in-one security suite. See §6.1.
2. **Plugin/theme update visibility:** WordPress handles plugin and theme updates, but should the dashboard *report* pending updates across all sites (via WP-CLI), and should it take a backup before updates run?
3. ~~**Core rollout policy:** Confirm the proposed default in §5.5 (security/minor releases rolled out automatically, major releases approved per site or client group).~~ **Resolved (2026-09-28):** the proposal, with holds per major branch that keep receiving minor and security releases, and an optional delay after which majors are approved automatically. See §5.5.
4. ~~**User roles:** Do agency staff need roles beyond full access (e.g. read-only, or access limited to certain client groups)?~~ **Resolved (2026-09-26):** fixed platform-wide Admin, Operator, and Viewer roles, only Admins can delete sites, and an audit log ships in the MVP. Client-group scoping is deferred. See §4.6.
5. ~~**Preview domain protection:** Is `noindex` sufficient, or should preview domains optionally require HTTP basic auth?~~ **Resolved (2026-09-27):** optional per-site basic auth on the preview domain via Traefik, off by default, with a platform-generated password stored encrypted. See §5.1.
6. **Monitoring stack:** Confirm the proposed VictoriaMetrics + built-in collector approach (§3, §7).
7. **Metrics retention:** How long should metrics be retained (e.g. 30 or 90 days)?
8. ~~**Developer code deployment:** How do agency developers get custom themes/plugins onto a site: SFTP, git push, a CI deploy API endpoint, WP-CLI/SSH access? Currently only an optional SFTP/file browser is planned (Phase 3).~~ **Resolved (2026-09-28):** an SFTP gateway authenticated with platform users' SSH keys, plus a deploy API for CI. No git push or shell access. See §5.8.
9. ~~**Importing existing sites:** Is an import workflow (upload an archive + DB dump, or pull from a remote host) part of the MVP?~~ **Resolved (2026-09-28):** yes, from a `wp-content` archive and SQL dump; other sources may follow after MVP. See §5.7.
10. ~~**Database access:** Should agencies get a database UI (e.g. Adminer started on demand), WP-CLI only, or neither?~~ **Resolved (2026-09-28):** database exports through the platform, and temporary, read-only-by-default database sessions tunneled through the SFTP gateway. No web database UI in the MVP. See §5.8.
11. ~~**Platform upgrades:** How does an existing install upgrade to a new PressFleet release (control plane migrations, platform service upgrades, rollback)?~~ **Resolved (2026-09-26):** forward-only migrations applied at startup after a state backup; the host CLI upgrades the control plane to its own version, with automatic rollback; kubectl-style version skew policy between the host CLI and the control plane. See §4.7.

---

## 10. Next Steps for Implementation Agent

1. **Repository Setup:** Initialize a monorepo following the conventions in §3.1, containing `/api` (OpenAPI spec), `/control-plane` (Go backend), `/ui` (Nuxt frontend), `/images` (`site-nginx` and `site-php` images), and `/templates/compose` (site Compose templates). Reserve `/templates/k8s` for the future Kubernetes driver.
2. **Interface Implementation:** Write the OpenAPI spec for Phase 1 endpoints (§4.5), the core Go interfaces from §4.4 (`Provisioner`, `DatabaseManager`, `BackupEngine`), the domain model from §4.3, and the job queue.
3. **Integration Test Suite:** Build an integration test framework using Docker-in-Docker (dind) to validate site creation, WP-CLI execution, and teardown against the Compose driver.
4. **Scope Discipline:** Do not implement Kubernetes support, staging clones, or any feature listed under Non-Goals (§1.1). Raise items in Open Questions (§9) before building features that depend on them.
