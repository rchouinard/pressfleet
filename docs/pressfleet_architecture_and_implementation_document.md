# Project Specification: PressFleet Architecture & Implementation Plan

**Project Codename:** PressFleet  
**Status:** Draft  
**Last Updated:** 2026-09-26  
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
| **Platform Services** | Shared infrastructure run once per install: Traefik, MariaDB, Redis, metrics storage, control plane. |
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
| **Object Cache** | **Redis** (shared, platform-managed) | Valkey (BSD-licensed drop-in), Memcached, per-site Redis | A single Redis instance keeps memory usage low; per-site isolation is enforced with ACLs and key prefixes (§6). |
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

* **Platform Services** (Traefik, control plane, MariaDB, Redis, VictoriaMetrics) run as a single Compose project installed once per host.
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
* **Required:** Linux on amd64, Docker Engine with Compose v2, cgroup v2 (for memory reservations and limits), ports 80 and 443 available, dashboard and wildcard preview DNS records resolving to the host.
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
| **User** | Email, password hash, role (Admin, Operator, or Viewer; §4.6), API tokens. |
| **ClientGroup** | Name, notes, backup repository reference. Has many Sites. |
| **Site** | Client group, slug, preview domain, primary domain (nullable until go-live), aliases, PHP version, WordPress core version and pin/rollout setting, resource limits, status, parent site (nullable, reserved for staging). |
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
    PurgeCache(ctx context.Context, siteID string) error
}

// DatabaseManager manages per-site databases and users on the shared MariaDB server.
type DatabaseManager interface {
    CreateDatabase(ctx context.Context, siteID string) (secretRef string, err error)
    DropDatabase(ctx context.Context, siteID string) error
    Dump(ctx context.Context, siteID string, destPath string) error
    Import(ctx context.Context, siteID string, srcPath string) error
}

type SnapshotID string

// BackupEngine creates and restores site snapshots.
// Each client group has its own Restic repository; snapshots are tagged by site ID.
type BackupEngine interface {
    Backup(ctx context.Context, siteID string) (SnapshotID, error)
    ListSnapshots(ctx context.Context, siteID string) ([]SnapshotID, error)
    Restore(ctx context.Context, siteID string, snap SnapshotID, targetSiteID string) error
    Prune(ctx context.Context, clientGroupID string) error
}
```

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
| Create sites; change site settings (PHP version, core pin, resources); go-live and domains; start/stop; maintenance mode; cache purge | ✓ | ✓ | |
| Take backups, restore, and approve core updates | ✓ | ✓ | |
| Create and edit client groups; manage alert rules | ✓ | ✓ | |
| Delete sites | ✓ | | |
| Delete or offboard client groups (export or delete the backup repository and its key) | ✓ | | |
| Manage users, roles, and other users' API tokens; view the audit log | ✓ | | |
| Change platform settings (DNS and S3 credentials, SMTP, alert channels, backup location, retention) | ✓ | | |
| Manage their own password and API tokens | ✓ | ✓ | ✓ |

**Rules:**
* **Single check point:** Every API handler calls one authorization check with the caller, the action, and the target before doing anything. The UI hides actions the user can't take, but the server is authoritative.
* **Tokens:** A token's effective permissions are the intersection of its scope and its owner's *current* role. MVP scopes are `full` (everything the owner's role allows) and `read-only`. Future site-scoped tokens (§4.2) plug into the same check.
* **Immediate effect:** Role changes apply immediately to the user's existing sessions and tokens.
* **At least one Admin:** The last Admin can't be demoted, disabled, or deleted. `pressfleet init` creates the first user as an Admin, and `pressfleet user create --role` exists for recovery.
* **Client groups:** The check resolves every target (site, backup, job, alert) to its client group. Client-group-scoped access (post-MVP) only needs a user↔group membership table on top of this.

**Audit log:** Every authenticated state-changing request and every authorization denial is recorded as an `AuditEvent`. Each event holds the actor (user and token), action, target, result, source IP, and timestamp, and never includes secrets. Admins can view and filter the audit log in the dashboard. Retention is configurable.

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
7. **Plugin Auto-Configuration:** Activates the default plugin stack: Nginx Helper, Redis Object Cache (configured with the site's ACL credentials, key prefix, and selective flush), and the agency security plugin set (TBD).
8. **Preview Ready:** The site is immediately reachable over HTTPS on its preview domain. Preview domains send `X-Robots-Tag: noindex` to keep them out of search engines.

### 5.2. Go-Live (Domain Switch) Workflow
1. **User Request:** Agency user sets the primary domain (and optional aliases) on the site.
2. **DNS Check:** Control plane verifies the domain resolves to the host and reports clearly if not. The user may retry once DNS has propagated.
3. **Routing & TLS:** `Provisioner.UpdateSite` adds Traefik routes for the primary domain and aliases; Traefik obtains certificates via HTTP-01.
4. **URL Update:** Runs `wp search-replace` from the preview URL to the primary URL and updates `home`/`siteurl`.
5. **Cache Purge:** Purges the FastCGI page cache and the site's object cache.
6. **Preview Redirect:** The preview domain permanently redirects to the primary domain.

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
2. Enables maintenance mode via `Provisioner.SetMaintenance` (Traefik serves a maintenance page).
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
* **Proposed rollout policy:** security and minor releases roll out automatically; major releases are approved per site or per client group. Each site can also be pinned to its current core version.

#### Image Updates
* When the platform ships image updates (PHP patch releases, WordPress core releases, security fixes, base OS updates), the control plane rolls them out by recreating site containers, optionally within a maintenance window.
* The image matrix is kept small: only the latest WordPress core release is built for each supported PHP version, plus the images sites are currently pinned to.

### 5.6. WordPress Runtime Defaults
* **Plugin and theme updates:** Handled natively by WordPress (auto-updates or wp-admin), writing to the `wp-content` volume.
* **WP-Cron:** `DISABLE_WP_CRON` is set; cron is triggered by the control plane scheduler so it runs reliably on low-traffic sites.
* **Cache purging:** Nginx Helper purges the FastCGI cache using file-based purging against the per-site cache volume shared between the `nginx` and `php` containers. Both containers run as the site's UID so the cache files can be deleted.
* **Outbound email:** Sites send mail through a configurable SMTP relay.

---

## 6. Security Baseline

* **Tenant isolation:** Each site runs as its own non-root UID with PHP `open_basedir` restricted to its own files. Sites must not be able to reach other sites' containers over the network.
* **Resource limits:** Every site container has a memory reservation, a memory limit, and a CPU limit (§4.1 Capacity Target).
* **Database:** Each site has its own MariaDB user with grants only on its own database.
* **Redis (shared):**
  * Each site has its own ACL user restricted to its own key prefix (`~<site-id>:*`).
  * Dangerous and cross-tenant commands are denied to site users (`FLUSHALL`, `FLUSHDB`, `KEYS`, `CONFIG`, etc.). Sites use the Redis Object Cache plugin's selective (prefix-based) flush.
  * Redis runs with a `maxmemory` limit and an LRU eviction policy. One busy site can evict others' cache entries; this is an accepted trade-off for lower resource usage, and eviction rates are monitored (§7).
* **Backups:** Each client group's Restic repository has its own encryption key.
* **Secrets:** Database passwords, Redis ACL passwords, Restic repository keys, DNS provider credentials, and S3 credentials are stored encrypted at rest in the control plane database using the platform master key, supplied via environment variable or file. Plaintext secrets are never logged. Losing the master key makes all backups unrecoverable; see the warning in §4.1.
* **Docker socket:** Access to the Docker socket is equivalent to root on the host. Traefik uses a read-only socket proxy, and only the control plane has write access.
* **Control plane access:** All API endpoints require authentication (session for the UI, API tokens for automation) and role-based authorization (§4.6). State-changing actions and authorization denials are written to the audit log.

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
  * Establish the Go control plane API, job queue, and SQLite/PostgreSQL schema with migrations (users, client groups, sites, jobs).
  * Implement `ComposeProvisioner` (per-site networks, two-container site stack, resource reservations/limits) and `MariaDBManager`.
  * Build the `site-nginx` and `site-php` images for each supported PHP version, with a generated, read-only `wp-config.php`.
  * API authentication (sessions and scoped tokens), role-based authorization, and the audit log (§4.6).
  * **Done when:** a site can be created in a client group via the API, reached over HTTP on the host, and deleted with no leftover containers, networks, volumes, or database, and two sites cannot reach each other over the network.
* **Phase 2: Ingress, Caching & Domain Management**
  * Integrate Traefik with the Docker provider via a socket proxy.
  * Preview domains with wildcard (DNS-01) or per-site (HTTP-01) certificates.
  * Go-live workflow with primary domains and aliases; automatic HTTP/2, HTTP/3, and Let's Encrypt certificates.
  * Shared Redis with per-site ACL users; Nginx `fastcgi_cache` defaults including cache purging.
  * **Done when:** a new site is reachable over HTTPS on its preview domain within seconds of provisioning, go-live switches it to a custom domain with a valid certificate, and one site cannot read or flush another site's object cache.
* **Phase 3: Backup Engine & File Management**
  * Implement the Restic-based `BackupEngine` with per-client-group repositories, scheduled backups, retention, and integrity checks.
  * Implement the restore pipeline with pre-restore snapshots and search-replace.
  * Optional SFTP / web-based file browser per site.
  * **Done when:** a site can be backed up, modified, and restored to its earlier state, and a failed restore rolls back automatically.
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
* **Post-MVP (High Priority): Staging Clones**
  * Clone a production site to a linked staging site (on a preview domain), and push staging changes back to production.
* **Post-MVP: Kubernetes Driver**
  * Implement `KubernetesProvisioner` using `client-go`, with templates in `/templates/k8s`.
  * Test against kind or k3d.

---

## 9. Open Questions

1. **Security plugins:** Which plugins make up the default "agency security" set, if any?
2. **Plugin/theme update visibility:** WordPress handles plugin and theme updates, but should the dashboard *report* pending updates across all sites (via WP-CLI), and should it take a backup before updates run?
3. **Core rollout policy:** Confirm the proposed default in §5.5 (security/minor releases rolled out automatically, major releases approved per site or client group).
4. ~~**User roles:** Do agency staff need roles beyond full access (e.g. read-only, or access limited to certain client groups)?~~ **Resolved (2026-09-26):** fixed platform-wide Admin, Operator, and Viewer roles, only Admins can delete sites, and an audit log ships in the MVP. Client-group scoping is deferred. See §4.6.
5. **Preview domain protection:** Is `noindex` sufficient, or should preview domains optionally require HTTP basic auth?
6. **Monitoring stack:** Confirm the proposed VictoriaMetrics + built-in collector approach (§3, §7).
7. **Metrics retention:** How long should metrics be retained (e.g. 30 or 90 days)?
8. **Developer code deployment:** How do agency developers get custom themes/plugins onto a site: SFTP, git push, a CI deploy API endpoint, WP-CLI/SSH access? Currently only an optional SFTP/file browser is planned (Phase 3).
9. **Importing existing sites:** Is an import workflow (upload an archive + DB dump, or pull from a remote host) part of the MVP?
10. **Database access:** Should agencies get a database UI (e.g. Adminer started on demand), WP-CLI only, or neither?
11. **Platform upgrades:** How does an existing install upgrade to a new PressFleet release (control plane migrations, platform service upgrades, rollback)?

---

## 10. Next Steps for Implementation Agent

1. **Repository Setup:** Initialize a monorepo following the conventions in §3.1, containing `/api` (OpenAPI spec), `/control-plane` (Go backend), `/ui` (Nuxt frontend), `/images` (`site-nginx` and `site-php` images), and `/templates/compose` (site Compose templates). Reserve `/templates/k8s` for the future Kubernetes driver.
2. **Interface Implementation:** Write the OpenAPI spec for Phase 1 endpoints (§4.5), the core Go interfaces from §4.4 (`Provisioner`, `DatabaseManager`, `BackupEngine`), the domain model from §4.3, and the job queue.
3. **Integration Test Suite:** Build an integration test framework using Docker-in-Docker (dind) to validate site creation, WP-CLI execution, and teardown against the Compose driver.
4. **Scope Discipline:** Do not implement Kubernetes support, staging clones, or any feature listed under Non-Goals (§1.1). Raise items in Open Questions (§9) before building features that depend on them.
