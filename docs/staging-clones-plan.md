# Staging Clones: Implementation Plan

**Status:** Accepted, 2026-10-29. Every decision in [#196](https://github.com/rchouinard/pressfleet/issues/196) was confirmed as proposed; see the decision log. The design doc describes the result in §5.11. Milestone: [Post-MVP: Staging Clones](https://github.com/rchouinard/pressfleet/milestone/7); tracking issue: [#101](https://github.com/rchouinard/pressfleet/issues/101).

This plan adds staging sites. A staging site is a copy of a production site on its own preview domain, linked to the production site. It can be refreshed from production, and its changes can be pushed back. The main use is testing heavier changes without affecting the production site: new or updated plugins, a theme rebuild, a PHP upgrade, or a WordPress major release. The design doc lists staging clones as the highest-priority follow-up to the MVP (§1.1, §8).

## What the MVP already provides

The MVP was built to anticipate this feature (§1.1, §4.2), so most of the pieces exist:

- **Parent link.** `sites.parent_site_id` is in the initial schema, indexed, with `ON DELETE RESTRICT`. The state database dump already loads parent rows before child rows (`store/dump.go`, `parentsFirst`).
- **Restore into another site.** `backup.Engine.Restore(siteID, snap, targetSiteID)` accepts any target in the same client group. The `site.restore` job (`sites/restore.go`) is a complete copy pipeline: a pre-restore backup, maintenance mode, moving the core up when needed, the restore, a restart, `wp core update-db`, a URL rewrite (`rewrite_urls`, from whatever host the restored database names to the target's host), cache purges, and rollback to the pre-restore backup. The restore API already accepts `source_site_id`.
- **Creating a site whose content comes from elsewhere.** Import's `allocate` step (`sites/import.go`) creates a site like provisioning does, but without installing WordPress or generating an admin password.
- **Backups that can be off.** Each site's backup schedule has `enabled`, and backup health metrics already skip sites whose schedule is off (`metrics/backupjobs.go`), so the built-in backup alerts can't fire for them.
- **Preview domains** send `X-Robots-Tag: noindex` and can require a password (§5.1).

These pieces are missing:

- **The model.** `store.Site` doesn't read `parent_site_id`; the API reports it as always null.
- **Locking across two sites.** A job holds one lock key. Any copy between two sites must back up the source under the source's lock key and restore under the target's. `backup/sitebackup` says so: a job can't queue a backup job and wait for it.
- **Partial copies.** `Restore` always replaces both wp-content and the database.
- **Staging behavior.** wp-config.php has no environment type, sites have no way to turn off outbound mail, and the alert engine can't tell a staging site from a production site.
- **Deletion guard.** Deleting a parent would fail only at the job's last step (`delete_record`, on the foreign key), after its stack and database were already removed. The request has to be refused up front.

## Decisions

Confirmed in [#196](https://github.com/rchouinard/pressfleet/issues/196), which also lists the alternatives that were considered.

| Topic | Decision |
| --- | --- |
| Shape | A staging site has `parent_site_id` set, and is in the same client group as its production site. Staging sites can't be nested, and a production site can have several. |
| Copy mechanism | The backup engine. The source is backed up (backup kind `sync`) and the snapshot is restored into the target through the restore pipeline. A clone or refresh can also start from an existing production backup. |
| Scope | `files` is `all`, `code` (`plugins`, `themes`, `mu-plugins`, `languages`), or `none`, and `database` is yes or no; at least one is copied. No table-level selection. |
| Push guards | A pre-push backup and automatic rollback. A database push is refused when it would move production to a WordPress major branch that production's core policy doesn't allow. Settings are never pushed. In the dashboard, a database push needs the production site's slug typed in. |
| Staging defaults | Preview domain only, `WP_ENVIRONMENT_TYPE=staging`, preview protection on, outbound email off (can be turned on per site), WP-Cron on, scheduled backups off. PHP version, core, resources, table prefix, and the initial admin user come from production, and no admin password is generated. |
| Alerts | Built-in site-scoped rules skip staging sites. A rule can opt in with `include_staging`. |
| Permissions | Create, refresh, push, and delete staging sites: Operator. Deleting a staging site's backups, and deleting production sites, stay Admin. Nothing new needs reauthentication. |
| Rollouts | Staging sites take part like any other site. Updating staging sites before production (a canary) comes later. |
| Release | A minor release (1.1.0): additive API, forward-only migrations, no change to the admin interface, so the version skew rules are unaffected (§4.7). |

## Scope

**In scope:** the parent/child model and its guards; creating a staging site from a fresh or existing production backup; refreshing a staging site from production; pushing a staging site to production; scoped copies; staging defaults (environment type, mail switch, preview protection, no scheduled backups); alerts and metrics; disaster recovery of staging sites; the dashboard; end-to-end tests; operator docs.

**Out of scope, for later:**

- copying selected database tables, or merging database changes
- making a staging site the new production site (a swap), or detaching a staging site into a standalone site
- custom domains on staging sites
- staging sites in another client group
- canary rollouts that update staging sites before their production sites
- copying settings (PHP version, resources) along with a push
- a preview of what a push will change (a diff of plugin and theme versions)

## Terms

| Term | Meaning |
| --- | --- |
| **Production site** | A site with no parent. Every site the MVP creates is one. |
| **Staging site** | A site whose `parent_site_id` names its production site. |
| **Clone** | Creating a staging site from a snapshot of its production site. |
| **Refresh** | Copying production into an existing staging site. |
| **Push** | Copying a staging site into its production site. |
| **Scope** | What a refresh or push copies: some or all files, the database, or both. |

## Design

### Data model

| Change | Issue |
| --- | --- |
| `store.Site.ParentSiteID` (column exists). New `sites` columns: `mail_disabled`, `staging_refreshed_at`, `staging_snapshot_id`, `staging_pushed_at` | [#199](https://github.com/rchouinard/pressfleet/issues/199) |
| `jobs.after_job_id` | [#197](https://github.com/rchouinard/pressfleet/issues/197) |
| `alert_rules.include_staging` | [#205](https://github.com/rchouinard/pressfleet/issues/205) |
| Backup kind `sync`. `backup_records.kind` is free text, so this needs no migration | [#200](https://github.com/rchouinard/pressfleet/issues/200) |
| `provisioner.SiteConfig.Environment` and `DisableMail`, which are data rather than Compose fields (§4.2) | [#198](https://github.com/rchouinard/pressfleet/issues/198) |

Every migration exists for both SQLite and PostgreSQL and is forward-only (§4.7). A staging site's table prefix is always its production site's, because the dump is imported verbatim and wp-config.php must name the same prefix.

### Job chains

Each copy is two jobs, queued together in one transaction. The source's backup runs under the source's lock key. The copy runs under the target's lock key, and waits for the backup to finish:

```
POST /sites/{production}/staging-sites
  site.backup (kind sync)   target: production   lock site:production
  site.clone                target: staging      lock site:staging      after the backup

POST /sites/{staging}/refresh
  site.backup (kind sync)   target: production   lock site:production
  site.refresh              target: staging      lock site:staging      after the backup

POST /sites/{staging}/push
  site.backup (kind sync)   target: staging      lock site:staging
  site.push                 target: production   lock site:production   after the backup
```

With an existing snapshot (`snapshot_id`), the copy job is queued on its own. The waiting job's first step reads the awaited job's outcome. If the backup failed, the step fails permanently and the job rolls back, which changes nothing on the target.

A waiting job must not hold up its lock key. For example, a push and a refresh of the same pair of sites, queued together, wait on each other's lock keys in opposite directions. A job can only wait for a job that already exists, so waits never form a cycle and the queue can't deadlock, but with plain first-in-first-out lock keys the four jobs would run one after another. The rule from [#197](https://github.com/rchouinard/pressfleet/issues/197) lets them run two at a time: a queued job that hasn't started doesn't block its lock key while it waits. A job that has started keeps its key until it is final, as today.

### Creating a staging site

`POST /api/v1/sites/{site_id}/staging-sites` takes an optional name, resources, and `snapshot_id`. In one transaction it allocates the staging site, the way import's `allocate` does, with status `provisioning`:

| Field | Staging value |
| --- | --- |
| Slug and preview domain | Derived from production's slug (`acme-staging`, `acme-staging-2`), unique and within slug limits |
| Name | "<production name> (staging)" unless given |
| PHP version, core, table prefix, resources | Production's (resources can be overridden) |
| Initial admin username and email | Production's, so one-click login (§5.9) picks the same administrator |
| Admin password | None. Production's users come with the copy |
| Salts, database and object cache credentials, UID | The staging site's own |
| Preview protection | On, with generated credentials that are shown once |
| Outbound mail | Off |
| Backup schedule | Off |
| Cron and plugin-update schedules | As for any site |

The same transaction queues the `sync` backup of production (unless a snapshot was given) and the `site.clone` job. The `site.clone` steps are: prepare (resolve the snapshot), create_database, create_object_cache, start_stack, restore, restart_site, upgrade_database, rewrite_urls (production's URL to the staging preview URL), configure_plugins, the cache purges, and finish (record the snapshot and time). A clone that fails for good rolls back like provisioning. Like a failed provisioning, it leaves the staging row marked failed, so the user sees what happened on the production site's page, where they started the clone.

### Refresh and push

Both are the restore pipeline (§5.4) with a scope. [#203](https://github.com/rchouinard/pressfleet/issues/203) changes the restore handler so that `site.restore`, `site.refresh`, and `site.push` share their steps instead of copying them. `site.restore` keeps its step names, because queued jobs resume by step name. Both directions get the same safety:

- **Undo point.** The target gets a pre-restore backup. For a push, this is production's pre-push backup, and the job's result names it so the push can be undone by restoring it.
- **Maintenance mode.** The target is in maintenance mode from before the restore until it is finished.
- **Core.** The target moves up to at least the snapshot's core when the database is copied (§5.4 step 4).
- **URLs.** When the database is copied, URLs are rewritten from the source's host to the target's: production's primary domain, once live, on a push, and the staging preview domain on a refresh.
- **Rollback.** A failure rolls the target back to its pre-restore backup, in full, whatever the scope.

Restored files restart the target, so PHP's opcode cache doesn't keep running replaced code. The cache purges run for every scope. A refresh with the database also converges the default plugin stack, because the dump brings production's plugin state, which may not match the staging site's code. A refresh without it can't: converging writes to the database, which that scope leaves alone.

On top of that, a push:

- **Guards the core policy.** If the staging site runs a newer major branch than production's core policy allows, a database push would bypass the hold or approval (§5.5), so it is refused with a validation error: approve the branch for production first. A newer release of the same branch is allowed, since the rollout would bring production there anyway.
- **Never pushes settings.** Production keeps its PHP version, resources, domains, preview protection, and mail switch. The dashboard points out differences so they can be applied by hand.
- **Converges the default plugin stack** on production after the restore, as import does (§5.1 step 7).

### Scope

| `files` | What is restored from the snapshot |
| --- | --- |
| `all` | All of `wp-content`, which ends up matching the snapshot |
| `code` | `plugins`, `themes`, `mu-plugins`, and `languages`, each made to match the snapshot (a directory the snapshot lacks is removed). `uploads` and everything else stay as they are |
| `none` | Nothing in `wp-content` |

`database: true` replaces the database with the snapshot's dump. `database: false` leaves the database alone and skips the URL rewrite.

Plugin activation and theme selection live in the database. A code-only push therefore updates the code of plugins and themes that are already active, and brings new ones in inactive. WordPress skips an active plugin whose files are gone, and deactivates it when the Plugins page loads. The dashboard and the operator guide explain this.

### Staging behavior

| Area | Staging site |
| --- | --- |
| Domains | Preview domain only. Go-live and aliases are refused |
| Environment | `WP_ENVIRONMENT_TYPE=staging` in wp-config.php. WordPress and well-behaved plugins (Jetpack and WooPayments, among others) use it to turn off live-only behavior. The admin bar shows a badge naming the environment ([#201](https://github.com/rchouinard/pressfleet/issues/201)) |
| Search engines | Preview domains already send `noindex`. `blog_public` is left alone, because a database push would carry it to production |
| Outbound email | Off: `PRESSFLEET_DISABLE_MAIL` makes the must-use plugin short-circuit `wp_mail()` and log the suppressed message. It can be turned on per staging site. Plugins that send mail some other way aren't covered (documented) |
| WP-Cron | Runs, so staging behaves like production. The operator guide covers scheduled work that shouldn't run on a copy |
| Backups | No schedule; manual backups work. Every refresh and push takes a backup anyway, and restic deduplicates staging snapshots against production's in the group repository |
| Restore, export, SFTP, deploy API, database sessions and exports, one-click login | As for any site |
| Core, image, and template rollouts | As for any site, under the staging site's own core policy |
| Plugin update checks | Run. The built-in "updates pending" rule skips staging sites |
| Alerts | Built-in site-scoped rules skip staging sites unless a rule sets `include_staging` |
| Metrics | `pressfleet_site_info` gets `environment` and `parent_site_id` labels |
| Deletion | An Operator action, which keeps the staging site's backups. Deleting a production site is refused while it has staging sites |
| Disaster recovery | The staging site's newest snapshot, or its production site's newest if it has none ([#206](https://github.com/rchouinard/pressfleet/issues/206)) |
| Capacity | Counts like any site against host memory (§4.1). Staging sites can be stopped when idle |
| Kubernetes | Nothing driver-specific beyond the two `SiteConfig` fields |

### What a copy carries along

A staging site starts as a full copy of production's wp-content and database. It therefore carries production's users and their passwords, customer data, plugin settings with live API keys (payment gateways, mail and CRM services), license keys, webhooks, and scheduled events. PressFleet turns off outbound mail through `wp_mail()`, keeps search engines away, and puts the site behind preview protection. It can't make every plugin safe. The operator guide ([#210](https://github.com/rchouinard/pressfleet/issues/210)) says what to check, such as switching payment gateways to test mode.

### API

| Operation | Notes |
| --- | --- |
| `POST /api/v1/sites/{site_id}/staging-sites` (`createStagingSite`) | Production sites only. `202` with the site and the job, like `createSite` |
| `POST /api/v1/sites/{site_id}/refresh` (`refreshStagingSite`) | Staging sites only. Scope and an optional `snapshot_id` of a production backup |
| `POST /api/v1/sites/{site_id}/push` (`pushStagingSite`) | Staging sites only. Scope |
| `GET /api/v1/sites?parent_site_id=…&environment=…` | Filters on `listSites` |
| `PATCH /api/v1/sites/{site_id}` | `outbound_mail`, staging sites only |
| `Site` | `parent_site_id` (now real) and `environment`; on staging sites, a `staging` object with the production site's ID, the last refresh and its snapshot, the last push, and outbound mail |
| `Job` | `after_job_id` |
| `AlertRule` | `include_staging` |
| `BackupKind` | Adds `sync` |

Each issue updates `api/openapi.yaml` first and regenerates the Go server and the TypeScript client.

### Authorization

New actions, all Operator and above. Each resolves to the client group (§4.6), which the staging site and its production site share:

| Action | Operation |
| --- | --- |
| `site.staging.create` | Create a staging site |
| `site.staging.refresh` | Refresh a staging site |
| `site.staging.push` | Push to production. Checked against both sites, so it stays correct once access is scoped by client group |
| `site.staging.delete` | `DELETE` on a staging site. With `delete_backups` it stays `site.delete_with_backups` (Admin) |

§4.6 gets one row: "Create, refresh, push, and delete staging sites", for Admin and Operator.

### Dashboard

- **Production site page:** a Staging card listing the staging sites, and a "Create staging site" dialog.
- **Staging site page:** a banner linking to the production site, the refresh and push dialogs with the scope and its warnings, and the outbound email toggle. Domains and go-live are hidden.
- **Site list:** staging badges, staging sites grouped with their production site, and an environment filter.
- **Jobs:** show what a job is waiting for.

See [#207](https://github.com/rchouinard/pressfleet/issues/207) and [#208](https://github.com/rchouinard/pressfleet/issues/208).

## Compatibility and release

- **Release.** Ships as a minor release. The admin interface the host CLI uses doesn't change, so the n−1 to n+1 skew policy (§4.7) is unaffected.
- **Templates.** Production sites render byte-identical files, so the change needs no render version bump and starts no template rollout ([#198](https://github.com/rchouinard/pressfleet/issues/198)).
- **Images.** The mail switch needs rebuilt site images ([#201](https://github.com/rchouinard/pressfleet/issues/201)). Older images ignore `PRESSFLEET_DISABLE_MAIL`, so the images must be published before the control plane release that creates staging sites.
- **Upgrades.** Existing `site.restore` jobs keep their step names, so a restore queued before the upgrade resumes after it.

## Issues

Sizes and priorities follow the [roadmap](roadmap.md#conventions). All are in the [Post-MVP: Staging Clones](https://github.com/rchouinard/pressfleet/milestone/7) milestone and are sub-issues of [#101](https://github.com/rchouinard/pressfleet/issues/101).

| # | Issue | Priority | Size | Depends on |
| --- | --- | --- | --- | --- |
| [#196](https://github.com/rchouinard/pressfleet/issues/196) | Decision: staging site behavior and defaults | P0 | S | — |
| [#197](https://github.com/rchouinard/pressfleet/issues/197) | Jobs: run a job after another job finishes | P0 | M | — |
| [#198](https://github.com/rchouinard/pressfleet/issues/198) | Driver: render a site's environment type and outbound mail switch | P1 | S | #196 |
| [#199](https://github.com/rchouinard/pressfleet/issues/199) | Sites: staging site model, API fields, and guards | P0 | M | #196, #198 |
| [#200](https://github.com/rchouinard/pressfleet/issues/200) | Backup engine: restore part of a snapshot, and the sync backup kind | P0 | M | #196 |
| [#201](https://github.com/rchouinard/pressfleet/issues/201) | Site images: outbound mail switch and environment badge | P1 | S | #196 |
| [#202](https://github.com/rchouinard/pressfleet/issues/202) | Create a staging site from a production site | P0 | L | #197, #199 |
| [#203](https://github.com/rchouinard/pressfleet/issues/203) | Refresh a staging site from production | P0 | M | #202, #200 |
| [#204](https://github.com/rchouinard/pressfleet/issues/204) | Push a staging site to production | P0 | L | #203 |
| [#205](https://github.com/rchouinard/pressfleet/issues/205) | Alerts and metrics: staging sites | P1 | S | #199 |
| [#206](https://github.com/rchouinard/pressfleet/issues/206) | Disaster recovery and rollouts: staging sites | P1 | M | #202 |
| [#207](https://github.com/rchouinard/pressfleet/issues/207) | Dashboard: create and manage staging sites | P0 | M | #202 |
| [#208](https://github.com/rchouinard/pressfleet/issues/208) | Dashboard: refresh and push staging sites | P0 | M | #207, #203, #204 |
| [#209](https://github.com/rchouinard/pressfleet/issues/209) | E2E: staging clones acceptance tests | P0 | L | #201, #204, #208 |
| [#210](https://github.com/rchouinard/pressfleet/issues/210) | Docs: staging sites operator guide and release notes | P1 | S | #204, #208 |

### Order

1. **Decisions and generic groundwork (#196, #197).** #197 doesn't depend on the decisions and can start now.
2. **Foundations (#198, #200, #201, then #199).** These can run in parallel once #196 is resolved. Nothing is visible to users yet.
3. **Clone (#202).** The first usable slice through the API, followed by its dashboard (#207), recovery (#206), and alerts (#205).
4. **Refresh, then push (#203, #204).** Refresh builds the shared restore steps on a target where little is at stake, and push adds production's guards.
5. **Dashboard sync dialogs, tests, and docs (#208, #209, #210).**

The critical path is #196 → #198 → #199 → #202 → #203 → #204 → #208 → #209.

### Working

From 1.0 on, work is done on a branch per issue and merged through a pull request; nothing is committed straight to `main`. PRs that touch site workflows add the `e2e` label so the end-to-end workflow runs.

### Testing

- **Unit:** the provisioner, database, and backup fakes (`provisionertest`, `backuptest`) for every job, including rollback paths and a failed source backup. No Docker.
- **Store:** each change on SQLite and PostgreSQL, including the job lease query with `after_job_id`, and crossing chains that must not deadlock.
- **Docker tests** (`PRESSFLEET_DOCKER_TESTS=1`): a code-only restic restore that leaves `uploads` untouched.
- **Images:** `images/test.sh` checks that Mailpit receives nothing from a site with the mail switch set.
- **End to end** ([#209](https://github.com/rchouinard/pressfleet/issues/209)): clone a live site, push code only and then the database, refresh, a failed push that rolls back, the deletion guards, and the dashboard flows in Playwright. [#206](https://github.com/rchouinard/pressfleet/issues/206) adds a staging site to the disaster recovery drill.

## Decision log

**2026-10-29 ([#196](https://github.com/rchouinard/pressfleet/issues/196)):** all eight decisions were accepted as recommended, with no changes:

1. **Shape:** a staging site has `parent_site_id` set and is in its production site's client group. No nesting, and no per-site limit beyond host capacity.
2. **Copy mechanism:** the backup engine (a `sync` backup restored through the restore pipeline), not a live volume copy.
3. **Scope:** `files` is `all`, `code`, or `none`, plus `database`. A copied scope ends up identical to the source's. No table-level selection.
4. **Push:** database pushes are allowed, with a pre-push backup, automatic rollback, the production slug typed in, and the core policy guard. Settings are never pushed.
5. **Defaults:** preview domain only, `WP_ENVIRONMENT_TYPE=staging`, preview protection on, outbound email off, WP-Cron on (not off), scheduled backups off. PHP version, core, resources, table prefix, and the initial admin user come from production.
6. **Alerts:** built-in site-scoped rules skip staging sites unless a rule sets `include_staging`.
7. **Permissions:** create, refresh, push, and delete staging sites are Operator actions without reauthentication. Deleting backups and deleting production sites stay Admin, and a production site with staging sites can't be deleted.
8. **Rollouts:** staging sites take part like any site. Canary rollouts come later.

The design doc records them in §1.1, §4.2, §4.3, §4.6, §5.4, §5.6, §5.10, the new §5.11, §7.3, §8, and §10.
