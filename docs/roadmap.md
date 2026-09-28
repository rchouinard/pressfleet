# PressFleet MVP Roadmap

This roadmap breaks the design in [pressfleet_architecture_and_implementation_document.md](pressfleet_architecture_and_implementation_document.md) into milestones and issues.
Live status is tracked in the [PressFleet MVP project](https://github.com/users/rchouinard/projects/3); GitHub is the source of truth, and this file is a snapshot of the original plan (2026-09-26), updated when decisions add or change issues.

## Conventions

- **Milestones** map to the implementation phases in design doc §8. Each ends with an integration-test issue that automates its "done when" criteria.
- **Priority:** P0 = critical path for the milestone, P1 = required for MVP, P2 = optional / nice to have.
- **Size:** S ≈ ≤1 day, M ≈ 2–3 days, L ≈ ~1 week, XL = break down before starting.
- **Labels:** `area:*` for subsystem, `type:feature|chore|decision|test|docs` for kind of work, `post-mvp` for deferred items.
- **Decisions:** `type:decision` issues come from the design doc's open questions (§9). Resolve them before starting the work they block.
- **Dependencies:** each issue lists its prerequisites under "Depends on" and links them as GitHub "blocked by" relationships.

## Milestones

### M1: Core Engine & Compose Driver

Goal: the control plane can provision and tear down isolated WordPress sites through the API using the Compose driver.

Done when: a site can be created in a client group via the API, reached over HTTP on the host, and deleted with no leftover containers, networks, volumes, or database, and two sites cannot reach each other over the network. (Design doc §8 Phase 1)

| # | Issue | Priority | Size | Depends on |
| --- | --- | --- | --- | --- |
| [#1](https://github.com/rchouinard/pressfleet/issues/1) | Scaffold monorepo layout, MIT license, and Go module | P0 | S | — |
| [#2](https://github.com/rchouinard/pressfleet/issues/2) | GitHub Actions CI: lint, test, and build | P0 | S | #1 |
| [#3](https://github.com/rchouinard/pressfleet/issues/3) | Define core adapter interfaces and domain types | P0 | S | #1 |
| [#4](https://github.com/rchouinard/pressfleet/issues/4) | Control plane binary skeleton: CLI commands and configuration | P0 | S | #1 |
| [#5](https://github.com/rchouinard/pressfleet/issues/5) | Control plane database layer (SQLite + PostgreSQL) with migrations | P0 | L | #4, #2 |
| [#6](https://github.com/rchouinard/pressfleet/issues/6) | Secrets encryption with the platform master key | P0 | M | #5 |
| [#7](https://github.com/rchouinard/pressfleet/issues/7) | Platform services Compose project | P0 | M | #4 |
| [#8](https://github.com/rchouinard/pressfleet/issues/8) | `pressfleet init`: install configuration and master key generation | P0 | M | #6, #7 |
| [#9](https://github.com/rchouinard/pressfleet/issues/9) | `pressfleet doctor` host preflight checks | P0 | M | #4 |
| [#10](https://github.com/rchouinard/pressfleet/issues/10) | ~~Decision: user roles beyond full access?~~ Resolved | P1 | S | — |
| [#11](https://github.com/rchouinard/pressfleet/issues/11) | Initial OpenAPI 3.1 spec and Go server code generation | P0 | M | #1, #2 |
| [#12](https://github.com/rchouinard/pressfleet/issues/12) | HTTP API server foundation | P0 | M | #11, #4 |
| [#13](https://github.com/rchouinard/pressfleet/issues/13) | Authentication: users, sessions, and scoped API tokens | P0 | L | #5, #12, #10 |
| [#14](https://github.com/rchouinard/pressfleet/issues/14) | Database-backed job queue and worker pool | P0 | L | #5, #12 |
| [#15](https://github.com/rchouinard/pressfleet/issues/15) | Recurring task scheduler | P1 | M | #14 |
| [#16](https://github.com/rchouinard/pressfleet/issues/16) | Client groups API (CRUD) | P0 | S | #13, #103 |
| [#17](https://github.com/rchouinard/pressfleet/issues/17) | Build `site-php` image (PHP-FPM + WordPress core) | P0 | L | #1 |
| [#18](https://github.com/rchouinard/pressfleet/issues/18) | Build `site-nginx` image | P0 | M | #17 |
| [#19](https://github.com/rchouinard/pressfleet/issues/19) | CI pipeline to build and push images to GHCR | P1 | S | #2, #18 |
| [#20](https://github.com/rchouinard/pressfleet/issues/20) | Site image build matrix and version manifest | P1 | M | #19 |
| [#21](https://github.com/rchouinard/pressfleet/issues/21) | Generate read-only `wp-config.php` from the site record | P0 | M | #6, #3 |
| [#22](https://github.com/rchouinard/pressfleet/issues/22) | Design: driver interface extensions for logs, stats, and volume access | P1 | S | #3 |
| [#23](https://github.com/rchouinard/pressfleet/issues/23) | MariaDBManager: create/drop per-site databases and users | P0 | M | #3, #6, #7 |
| [#24](https://github.com/rchouinard/pressfleet/issues/24) | Site Compose templates and renderer | P0 | M | #3, #21, #18 |
| [#25](https://github.com/rchouinard/pressfleet/issues/25) | ComposeProvisioner: site lifecycle | P0 | L | #24, #23 |
| [#26](https://github.com/rchouinard/pressfleet/issues/26) | ComposeProvisioner: `ExecWPCLI` | P0 | S | #25 |
| [#27](https://github.com/rchouinard/pressfleet/issues/27) | Site provisioning workflow (`POST /api/v1/sites`) | P0 | L | #14, #26, #16 |
| [#28](https://github.com/rchouinard/pressfleet/issues/28) | Sites API: list, get, update, start/stop | P0 | M | #27, #20 |
| [#29](https://github.com/rchouinard/pressfleet/issues/29) | Site deletion workflow | P0 | M | #27 |
| [#30](https://github.com/rchouinard/pressfleet/issues/30) | Control-plane-driven WP-Cron | P1 | S | #15, #26 |
| [#31](https://github.com/rchouinard/pressfleet/issues/31) | Integration test harness (Docker-in-Docker) | P0 | M | #2, #25 |
| [#32](https://github.com/rchouinard/pressfleet/issues/32) | Integration tests: M1 acceptance criteria | P0 | M | #31, #29, #28 |
| [#103](https://github.com/rchouinard/pressfleet/issues/103) | Authorization: roles and permission checks | P0 | M | #13 |
| [#104](https://github.com/rchouinard/pressfleet/issues/104) | Audit log for state-changing actions | P1 | M | #103, #14 |

**Status: complete (2026-09-27).** All issues are closed and the E2E workflow automates the "done when" criteria. Until Traefik lands in M2, "reached over HTTP on the host" means an HTTP client container on the host's Docker engine, attached to the site network (see [testing.md](testing.md)); #36 moved this check to the host itself, through Traefik.

Follow-ups carried out of M1:

- ~~Pass `dashboard_domain` to the control plane container~~ (done in #33) ~~and narrow `NGINX_TRUSTED_PROXIES` to the site subnet~~ (done in #36: site-nginx trusts only its own network by default, `NGINX_TRUSTED_PROXIES=local`).
- ~~Attach Traefik and Redis to site networks alongside MariaDB~~ (done in #37 and #36, through the provisioner's list of shared services).
- Record the actor's email on audit events so they stay readable after a user is deleted (needs a migration).
- Prune expired sessions, job history, and state database backups on a schedule.
- Give `pressfleet user create` a structured "user exists" exit code; `init --force` matches stderr today.
- Move igbinary off its release candidate once 3.2.17 is out.

### M2: Ingress, Caching & Domains

Goal: every site is served over HTTPS with page and object caching, and can go live on its own domain.

Done when: a new site is reachable over HTTPS on its preview domain within seconds of provisioning, go-live switches it to a custom domain with a valid certificate, and one site cannot read or flush another site's object cache. (Design doc §8 Phase 2)

| # | Issue | Priority | Size | Depends on |
| --- | --- | --- | --- | --- |
| [#33](https://github.com/rchouinard/pressfleet/issues/33) | Traefik platform service with read-only Docker socket proxy | P0 | M | #7 |
| [#34](https://github.com/rchouinard/pressfleet/issues/34) | Preview domain TLS: DNS-01 wildcard with HTTP-01 fallback | P0 | M | #33, #8 |
| [#35](https://github.com/rchouinard/pressfleet/issues/35) | ~~Decision: preview domain protection~~ Resolved | P2 | S | — |
| [#36](https://github.com/rchouinard/pressfleet/issues/36) | Traefik routing labels for site stacks | P0 | S | #34, #25, #35 |
| [#37](https://github.com/rchouinard/pressfleet/issues/37) | Shared Redis platform service | P0 | S | #7 |
| [#38](https://github.com/rchouinard/pressfleet/issues/38) | Per-site Redis ACL users | P0 | M | #37, #29 |
| [#39](https://github.com/rchouinard/pressfleet/issues/39) | Configure Redis Object Cache per site | P0 | S | #38, #21 |
| [#40](https://github.com/rchouinard/pressfleet/issues/40) | Nginx `fastcgi_cache` defaults | P0 | M | #18, #24 |
| [#41](https://github.com/rchouinard/pressfleet/issues/41) | Nginx Helper integration and `PurgeCache` | P0 | S | #40, #39, #26 |
| [#42](https://github.com/rchouinard/pressfleet/issues/42) | ~~Decision: default agency security plugin set~~ Resolved | P1 | S | — |
| [#43](https://github.com/rchouinard/pressfleet/issues/43) | Default plugin stack activation during provisioning | P0 | S | #41, #42, #108 |
| [#44](https://github.com/rchouinard/pressfleet/issues/44) | `SetMaintenance`: Traefik-served maintenance page | P1 | M | #36 |
| [#45](https://github.com/rchouinard/pressfleet/issues/45) | Go-live workflow: primary domain switch | P0 | L | #36, #41 |
| [#46](https://github.com/rchouinard/pressfleet/issues/46) | Domain alias management | P1 | S | #45 |
| [#47](https://github.com/rchouinard/pressfleet/issues/47) | Outbound email via configurable SMTP relay | P1 | S | #21, #17 |
| [#48](https://github.com/rchouinard/pressfleet/issues/48) | Integration tests: M2 acceptance criteria | P0 | M | #32, #45, #43, #107, #109 |
| [#107](https://github.com/rchouinard/pressfleet/issues/107) | Preview domain password protection | P1 | M | #36 |
| [#108](https://github.com/rchouinard/pressfleet/issues/108) | WordPress security set: configuration and must-use plugin modules | P1 | M | #21 |
| [#109](https://github.com/rchouinard/pressfleet/issues/109) | site-nginx: login rate limiting and security headers | P1 | S | #108 |

**Status: complete (2026-09-27).** All issues are implemented, and `TestM2Acceptance` automates the "done when" criteria against Pebble (see [testing.md](testing.md)). The shared object cache runs Valkey, which isolates each site in its own ACL user and logical database; that closes the `SCAN` leak listed under the design document gaps below.

Follow-ups carried out of M2:

- Rebuild and publish the site images from main: e2e pulls them from GHCR unless `images/` changed, and the published images predate the bundled plugins (#43).
- Refresh the §4.1 memory budget: Traefik now needs 96 MiB, the socket proxy 24 MiB, the maintenance page 24 MiB, and Valkey 64 MiB of overhead above `maxmemory`; `pressfleet doctor`'s budget doesn't include them yet.
- Watch the Traefik container (like the object cache manager does for Valkey) so a recreated Traefik rejoins site networks without a control plane restart; MariaDB has the same limitation.
- Enforce domain uniqueness across primary domains and aliases in the database (a shared domains table); today it's checked in the application only.
- Let operators change DNS provider credentials and SMTP settings without `pressfleet init --force` (with `pressfleet upgrade`, #95).
- Move preview protection routing to Traefik's file provider so toggling it doesn't recreate nginx.
- Trusted proxies for IPv6 site networks (`NGINX_TRUSTED_PROXIES=local` covers IPv4 only).
- Decide on permalinks for new sites: with plain permalinks, `/wp-json/` and sitemap paths redirect.
- Cover the DNS-01 wildcard path end to end (needs a propagation-check setting and an exec-provider script for challtestsrv).
- Password policy gaps: WP-CLI paths that call `wp_set_password()` directly are caught only after saving, and custom forms (e.g. WooCommerce) aren't covered.

### M3: Backup Engine & File Management

Goal: encrypted, deduplicated backups per client group with safe, automatically rolled-back restores, and platform-managed core updates.

Done when: a site can be backed up, modified, and restored to its earlier state, and a failed restore rolls back automatically. (Design doc §8 Phase 3)

| # | Issue | Priority | Size | Depends on |
| --- | --- | --- | --- | --- |
| [#49](https://github.com/rchouinard/pressfleet/issues/49) | MariaDBManager: `Dump` and `Import` | P0 | M | #23 |
| [#50](https://github.com/rchouinard/pressfleet/issues/50) | Backup worker runtime environment | P0 | M | #22, #25 |
| [#51](https://github.com/rchouinard/pressfleet/issues/51) | Per-client-group Restic repository management | P0 | M | #16, #6, #50 |
| [#52](https://github.com/rchouinard/pressfleet/issues/52) | ResticBackupEngine: `Backup` and `ListSnapshots` | P0 | L | #49, #51, #14 |
| [#53](https://github.com/rchouinard/pressfleet/issues/53) | Scheduled backups | P0 | S | #52, #15 |
| [#54](https://github.com/rchouinard/pressfleet/issues/54) | Retention, prune, and integrity checks | P0 | M | #53 |
| [#55](https://github.com/rchouinard/pressfleet/issues/55) | Restore pipeline with pre-restore snapshot and rollback | P0 | XL | #52, #44, #41 |
| [#56](https://github.com/rchouinard/pressfleet/issues/56) | Decision: WordPress core rollout policy | P1 | S | — |
| [#57](https://github.com/rchouinard/pressfleet/issues/57) | WordPress core update rollout | P1 | L | #55, #20, #56 |
| [#58](https://github.com/rchouinard/pressfleet/issues/58) | Platform image update rollout | P1 | M | #57 |
| [#59](https://github.com/rchouinard/pressfleet/issues/59) | Decision: importing existing sites in MVP? | P1 | S | — |
| [#60](https://github.com/rchouinard/pressfleet/issues/60) | Decision: developer code deployment | P1 | S | — |
| [#61](https://github.com/rchouinard/pressfleet/issues/61) | Decision: database access for agencies | P2 | S | — |
| [#62](https://github.com/rchouinard/pressfleet/issues/62) | Per-site file management (SFTP / web file browser) | P2 | L | #60, #50 |
| [#63](https://github.com/rchouinard/pressfleet/issues/63) | Integration tests: M3 acceptance criteria | P0 | M | #55, #48 |

### M4: Monitoring & Alerting

Goal: built-in metrics, uptime checks, logs, and alerting for every site and the platform.

Done when: every metric in §7.1–7.2 is collected and queryable via the API, and each default alert rule fires and resolves correctly in integration tests. (Design doc §8 Phase 4)

| # | Issue | Priority | Size | Depends on |
| --- | --- | --- | --- | --- |
| [#64](https://github.com/rchouinard/pressfleet/issues/64) | Decision: confirm VictoriaMetrics + built-in collector | P0 | S | — |
| [#65](https://github.com/rchouinard/pressfleet/issues/65) | Decision: metrics retention period | P1 | S | — |
| [#66](https://github.com/rchouinard/pressfleet/issues/66) | VictoriaMetrics platform service | P0 | S | #7, #64, #65 |
| [#67](https://github.com/rchouinard/pressfleet/issues/67) | Metrics collector framework | P0 | M | #66, #22 |
| [#68](https://github.com/rchouinard/pressfleet/issues/68) | Collector: container resource metrics | P0 | S | #67 |
| [#69](https://github.com/rchouinard/pressfleet/issues/69) | Collector: Traefik traffic metrics | P0 | S | #67, #33 |
| [#70](https://github.com/rchouinard/pressfleet/issues/70) | Collector: page cache and PHP-FPM metrics | P0 | M | #67, #40 |
| [#71](https://github.com/rchouinard/pressfleet/issues/71) | Collector: MariaDB and Redis metrics | P0 | M | #67, #37 |
| [#72](https://github.com/rchouinard/pressfleet/issues/72) | Collector: host and storage metrics | P0 | M | #67, #51 |
| [#73](https://github.com/rchouinard/pressfleet/issues/73) | HTTP uptime and TLS expiry checks | P0 | M | #67, #45 |
| [#74](https://github.com/rchouinard/pressfleet/issues/74) | Backup health and job queue metrics | P0 | S | #67, #53 |
| [#75](https://github.com/rchouinard/pressfleet/issues/75) | Alert rules engine with default rules | P0 | L | #66 |
| [#76](https://github.com/rchouinard/pressfleet/issues/76) | Alert channels: SMTP email and webhook | P0 | M | #75 |
| [#77](https://github.com/rchouinard/pressfleet/issues/77) | Metrics query API for the dashboard | P0 | M | #67 |
| [#78](https://github.com/rchouinard/pressfleet/issues/78) | Logs API via the orchestrator driver | P0 | M | #22, #25, #14 |
| [#79](https://github.com/rchouinard/pressfleet/issues/79) | Integration tests: M4 acceptance criteria | P0 | L | #63, #68, #69, #70, #71, #72, #73, #74, #76, #77, #78 |

### M5: Dashboard

Goal: a Nuxt dashboard, embedded in the Go binary, that covers all platform operations.

Done when: every Phase 1–4 operation can be performed from the UI. (Design doc §8 Phase 5)

| # | Issue | Priority | Size | Depends on |
| --- | --- | --- | --- | --- |
| [#80](https://github.com/rchouinard/pressfleet/issues/80) | Nuxt UI scaffold, generated TS client, and embedding in the Go binary | P0 | M | #11, #12 |
| [#81](https://github.com/rchouinard/pressfleet/issues/81) | UI: app shell, navigation, and job progress | P0 | M | #80, #14 |
| [#82](https://github.com/rchouinard/pressfleet/issues/82) | UI: login, session, users, and API tokens | P0 | S | #81, #13 |
| [#83](https://github.com/rchouinard/pressfleet/issues/83) | UI: client groups | P0 | S | #81, #16 |
| [#84](https://github.com/rchouinard/pressfleet/issues/84) | UI: site list, detail, provisioning, and deletion | P0 | L | #83, #28, #29 |
| [#85](https://github.com/rchouinard/pressfleet/issues/85) | UI: site settings | P0 | M | #84, #57, #44, #41, #107 |
| [#86](https://github.com/rchouinard/pressfleet/issues/86) | UI: go-live and domain management | P0 | M | #84, #46 |
| [#87](https://github.com/rchouinard/pressfleet/issues/87) | UI: backups and restore | P0 | M | #84, #55 |
| [#88](https://github.com/rchouinard/pressfleet/issues/88) | UI: site and platform metrics charts | P0 | L | #84, #77 |
| [#89](https://github.com/rchouinard/pressfleet/issues/89) | UI: logs viewer | P1 | M | #84, #78 |
| [#90](https://github.com/rchouinard/pressfleet/issues/90) | UI: alerts, rules, and channels | P0 | M | #81, #76 |
| [#91](https://github.com/rchouinard/pressfleet/issues/91) | UI: platform overview and capacity warnings | P1 | M | #81, #9, #72 |
| [#92](https://github.com/rchouinard/pressfleet/issues/92) | Decision: plugin/theme update visibility | P2 | S | — |
| [#93](https://github.com/rchouinard/pressfleet/issues/93) | UI end-to-end tests (Playwright) | P1 | L | #82, #85, #86, #87, #88, #89, #90, #91 |
| [#105](https://github.com/rchouinard/pressfleet/issues/105) | UI: audit log viewer | P1 | S | #81, #104 |

### MVP Release Gate

Goal: PressFleet is installable, upgradable, documented, secure, and meets the capacity target.

Done when: 50 sites on an 8 GiB host stay healthy under light traffic (§4.1), and install docs, security review, and the release pipeline are complete.

| # | Issue | Priority | Size | Depends on |
| --- | --- | --- | --- | --- |
| [#94](https://github.com/rchouinard/pressfleet/issues/94) | ~~Decision: platform upgrade process~~ Resolved | P0 | S | — |
| [#95](https://github.com/rchouinard/pressfleet/issues/95) | Platform upgrade command and procedure | P0 | L | #94, #8, #106, #58 |
| [#96](https://github.com/rchouinard/pressfleet/issues/96) | Disaster recovery: restore control plane state on a new host | P1 | M | #52, #94 |
| [#97](https://github.com/rchouinard/pressfleet/issues/97) | Security review: tenant isolation and secrets | P0 | M | #79 |
| [#98](https://github.com/rchouinard/pressfleet/issues/98) | Capacity test: 50 sites on an 8 GiB host | P0 | L | #79 |
| [#99](https://github.com/rchouinard/pressfleet/issues/99) | Installation and operator documentation | P0 | M | #95 |
| [#100](https://github.com/rchouinard/pressfleet/issues/100) | Release pipeline: versioned binaries and images | P0 | M | #19, #80 |
| [#106](https://github.com/rchouinard/pressfleet/issues/106) | Host CLI ↔ control plane admin interface and version skew enforcement | P0 | M | #12 |

### Post-MVP: Staging Clones

Highest-priority follow-up after MVP (§1.1, §8). Do not implement before MVP ships.

| # | Issue | Priority | Size | Depends on |
| --- | --- | --- | --- | --- |
| [#101](https://github.com/rchouinard/pressfleet/issues/101) | Staging clones (tracking) | P2 | XL | #55 |

### Post-MVP: Kubernetes Driver

KubernetesProvisioner using client-go (§4.2, §8). Do not implement before MVP ships.

| # | Issue | Priority | Size | Depends on |
| --- | --- | --- | --- | --- |
| [#102](https://github.com/rchouinard/pressfleet/issues/102) | Kubernetes driver (tracking) | P2 | XL | #3 |

## Open decisions

These must be resolved before the work they block.

| # | Decision | Milestone | Priority |
| --- | --- | --- | --- |
| [#10](https://github.com/rchouinard/pressfleet/issues/10) | ~~user roles beyond full access?~~ **Resolved:** fixed Admin/Operator/Viewer roles, audit log in MVP (design doc §4.6) | M1: Core Engine & Compose Driver | P1 |
| [#22](https://github.com/rchouinard/pressfleet/issues/22) | Design: driver interface extensions for logs, stats, and volume access | M1: Core Engine & Compose Driver | P1 |
| [#35](https://github.com/rchouinard/pressfleet/issues/35) | ~~preview domain protection~~ **Resolved:** optional per-site basic auth on the preview domain, off by default (design doc §5.1) | M2: Ingress, Caching & Domains | P2 |
| [#42](https://github.com/rchouinard/pressfleet/issues/42) | ~~default agency security plugin set~~ **Resolved:** install-wide configurable security set of must-use plugin modules, nginx settings, and optional Two Factor; no per-site overrides (design doc §6.1) | M2: Ingress, Caching & Domains | P1 |
| [#56](https://github.com/rchouinard/pressfleet/issues/56) | WordPress core rollout policy | M3: Backup Engine & File Management | P1 |
| [#59](https://github.com/rchouinard/pressfleet/issues/59) | importing existing sites in MVP? | M3: Backup Engine & File Management | P1 |
| [#60](https://github.com/rchouinard/pressfleet/issues/60) | developer code deployment | M3: Backup Engine & File Management | P1 |
| [#61](https://github.com/rchouinard/pressfleet/issues/61) | database access for agencies | M3: Backup Engine & File Management | P2 |
| [#64](https://github.com/rchouinard/pressfleet/issues/64) | confirm VictoriaMetrics + built-in collector | M4: Monitoring & Alerting | P0 |
| [#65](https://github.com/rchouinard/pressfleet/issues/65) | metrics retention period | M4: Monitoring & Alerting | P1 |
| [#92](https://github.com/rchouinard/pressfleet/issues/92) | plugin/theme update visibility | M5: Dashboard | P2 |
| [#94](https://github.com/rchouinard/pressfleet/issues/94) | ~~platform upgrade process~~ **Resolved:** forward-only migrations, CLI upgrades control plane to its own version, kubectl-style version skew (design doc §4.7) | MVP Release Gate | P0 |

## Gaps in the design document

Planning surfaced these items, which the design document doesn't yet cover. Each has an issue:

- **Driver interface extensions ([#22](https://github.com/rchouinard/pressfleet/issues/22)):** the `Provisioner` interface (§4.4) has no methods for container logs (§7.4), resource stats (§7.1), or read-only volume access for backups (§5.3). §4.2 forbids Docker calls outside the driver, so the interface must grow in a Kubernetes-compatible way.
- **Redis `SCAN` leakage ([#38](https://github.com/rchouinard/pressfleet/issues/38)):** `SCAN` takes no key argument, so a key-prefix ACL alone likely doesn't stop one site from listing another site's key names. Check what the Redis Object Cache plugin's selective flush requires. **Resolved:** the plugin's selective and group flushes are an `EVAL` script looping over `SCAN MATCH <prefix>*`, and script commands are checked against the caller's ACL, so `SCAN` can't simply be denied. Each site gets its own logical database as well as its key prefix (Valkey 9 `db=` ACL rules), which confines `SCAN`, `DBSIZE`, `RANDOMKEY`, and `FLUSHDB` to the site's own keys (design doc §6).
- **Disaster recovery ([#96](https://github.com/rchouinard/pressfleet/issues/96)):** the design never covers restoring control plane state (site records, secrets) on a new host. Restic repositories plus the master key aren't enough on their own to rebuild the platform.
- **Unphased features:** WordPress core rollout (§5.5) is placed in M3 ([#57](https://github.com/rchouinard/pressfleet/issues/57)) because it needs backups. WP-Cron triggering (§5.6) is in M1 ([#30](https://github.com/rchouinard/pressfleet/issues/30)) and SMTP relay setup is in M2 ([#47](https://github.com/rchouinard/pressfleet/issues/47)).

## Where to start

M1 and M2 are complete; continue with M3, starting with the open decisions it depends on (#56, #59, #60, #61). Original guidance:

Begin with [#1](https://github.com/rchouinard/pressfleet/issues/1) (repo scaffolding). After it lands, CI ([#2](https://github.com/rchouinard/pressfleet/issues/2)), core interfaces ([#3](https://github.com/rchouinard/pressfleet/issues/3)), the CLI skeleton ([#4](https://github.com/rchouinard/pressfleet/issues/4)), and the `site-php` image ([#17](https://github.com/rchouinard/pressfleet/issues/17)) can proceed in parallel.
