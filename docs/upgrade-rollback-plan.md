# Upgrade Rollback: Implementation Plan

**Status:** Proposed, 2026-10-29. The decisions below wait on [#245](https://github.com/rchouinard/pressfleet/issues/245). Milestone: [Post-MVP: Upgrade Rollback](https://github.com/rchouinard/pressfleet/milestone/10); tracking issue: [#141](https://github.com/rchouinard/pressfleet/issues/141).

MVP upgrades are forward-only. When `pressfleet upgrade` fails, it prints the commands that return the platform to the previous release, and the operator runs them by hand (design doc §4.7, #95). This plan makes the upgrade run those steps itself when it fails, and adds `pressfleet upgrade --rollback` for an upgrade that succeeded but is regretted. The manual steps stay as the fallback when a rollback can't finish.

## What the MVP already provides

The upgrade already keeps everything a rollback needs, and #142 tests the manual steps verbatim:

- **The previous platform project**, copied to `<data_dir>/upgrade/previous-platform/` before anything changes.
- **The upgrade record**, `<data_dir>/upgrade/upgrade.json`. It holds the versions, the start time, the status (in progress, failed, or succeeded), the image each service ran (by digest), and the services recreated, including those recreated only because a mounted file changed.
- **A pre-migration backup.** With SQLite, the new control plane backs up the state database before migrating it (`pressfleet-<timestamp>-schema<NNNN>.db`), and `state restore`, run with the previous control plane image, puts it back. The restore first saves the current database, so it can be undone.
- **The recovery steps template** (`recoveryTemplate`), which fills in the paths and images, covers the PostgreSQL and no-migration cases, and is kept in sync with `docs/upgrading.md` by a test.
- **Retries.** A failed upgrade's record tells a retry to keep the project the first attempt saved.

What a rollback needs that isn't there:

- **A reliable way to know a migration ran.** The CLI finds the backup by file name and time. That works only for SQLite, so with PostgreSQL the CLI can't tell whether the previous control plane would still start on the database.
- **Protection for sites while an upgrade is unconfirmed.** A failed upgrade's control plane can start the core, image, and template rollouts while the upgrade waits for it.
- **Knowing what the control plane did since an upgrade.** Rolling back the state database undoes those records, but not their effects on sites. The CLI never reads the database (§4.7), so the control plane has to say.
- **Care with runtime files.** The platform project includes Traefik's dynamic directory, which the control plane writes at runtime: sites' domain and maintenance routes. Copying the saved project back would revert live routes.

## Decisions

Proposed. Each one is confirmed or changed in [#245](https://github.com/rchouinard/pressfleet/issues/245), which lists the alternatives.

| Topic | Proposal |
| --- | --- |
| Default | A failed upgrade rolls back on its own. `--no-rollback` prints the manual steps instead. Exit codes: `0` upgraded; `2` failed and rolled back; `1` failed and not rolled back. |
| Triggers | Any failure after the platform starts to change, including SIGINT and SIGTERM. An interrupted upgrade can be retried or rolled back, but not replaced by an upgrade to another release. |
| State database | SQLite: restore the pre-migration backup. PostgreSQL: roll back the images if no migration ran; otherwise print the steps for the operator's backup. A marker file records every migration. |
| `--rollback` | Shows what changed since the upgrade, and refuses changes that would leave sites broken or orphaned unless `--force` is given. Only the most recent upgrade can be rolled back, once. |
| Rollout hold | While an upgrade is in progress or failed, the core, image, and template rollouts don't start. |
| Platform project | Restore the rendered files exactly. Leave Traefik's dynamic directory to the previous control plane's reconcile. |
| MariaDB | Never roll back across a series change. |
| Kubernetes | Out of scope; that's Helm's job and the operator's database restore. |

## Scope

**In scope:**

- the migration marker
- the rollout hold
- the admin command that reports changes since an upgrade
- automatic rollback
- `--rollback`
- end-to-end tests
- docs

**Out of scope, for later:**

- a logical pre-migration backup of PostgreSQL databases (the §5.10 format), which would let PostgreSQL installs roll back after a migration on their own
- rolling back more than one upgrade
- rolling back across a MariaDB series change, once series changes are supported
- Kubernetes installs, which use `helm rollback` and the operator's database restore (#212, #230)

## The rollback sequence

Automatic rollback and `--rollback` share one sequence ([#249](https://github.com/rchouinard/pressfleet/issues/249)). Each step is idempotent, and the record's status moves from `rolling_back` to `rolled_back` or `rollback_failed`, so a rollback that stops can be run again:

1. **Stop the control plane.**
2. **Restore the state database, if the upgrade migrated it.** The CLI reads the migration marker ([#246](https://github.com/rchouinard/pressfleet/issues/246)).
   - **SQLite:** run `state restore <backup>` with the previous control plane image, as recovery step 2 does.
   - **PostgreSQL:** stop here, and print the steps for the operator's own backup. The previous control plane would refuse the migrated schema.

   Without a migration, the database stays as it is, and nothing recorded since the upgrade is lost.
3. **Restore the platform project.** Copy the rendered files back from `previous-platform`, and remove the files the new release added. Traefik's dynamic directory, which the control plane writes at runtime, is left alone.
4. **Start the previous release's services** with `--remove-orphans`. The services recreated only because a mounted file changed are force-recreated on the previous files.
5. **Wait for the control plane** to report ready at the previous version.

If a step fails, the CLI stops and prints the manual recovery steps, filled in for where the rollback stopped. The steps are designed to work after a partial rollback: a repeated `state restore` first saves the database it replaces, and copying the project back again gives the same files. The end-to-end test checks this ([#251](https://github.com/rchouinard/pressfleet/issues/251)).

When the previous control plane starts, its startup reconcile re-renders every site with its own templates and the restored records. That puts back image pins and site templates, rewrites the domain and maintenance routes in Traefik's dynamic directory, and reconnects platform services.

## Automatic rollback

[#249](https://github.com/rchouinard/pressfleet/issues/249) changes what `pressfleet upgrade` does after the platform starts to change. Today a failure prints the manual steps. With this change:

- **Triggers.** Errors from rendering the new project, from `up`, from the health checks, or from the version check start the rollback sequence. So do SIGINT and SIGTERM during the apply; a second signal stops the rollback and prints the manual steps instead.
- **Output and exit codes.** The output names the cause and what was rolled back. Exit code `2` means the upgrade failed and was rolled back; `1` means it wasn't (`--no-rollback`, or a failed rollback), with the manual steps printed.
- **The CLI afterwards.** The CLI is still the newer release, so if the previous control plane is now outside the version skew policy, the output says which CLI to install again.
- **An interrupted upgrade.** If the CLI was killed outright, the record says the upgrade is in progress. The next `pressfleet upgrade` offers to retry the same release or roll back, and won't start an upgrade to a different release over it.
- **MariaDB.** The record keeps the MariaDB series from before the upgrade, and a rollback refuses to cross a series change. Upgrades can't change the series today (§4.7), so this is a guard for later. A patch release within a series rolls back with everything else.

### Holding rollouts

A failed upgrade's control plane may run for minutes while the upgrade waits for it to become healthy. [#247](https://github.com/rchouinard/pressfleet/issues/247) keeps it from changing sites in that time. While the upgrade record's status is in progress, failed, or rolling back, the core, image, and template rollouts start no site jobs. User-requested jobs and the startup reconcile run as usual. When the CLI marks the upgrade succeeded, the rollouts resume, without a restart.

The rollout endpoints and the dashboard show `held` with the reason, and doctor warns. The record's statuses become a contract between the CLI and the control plane, versioned like the admin interface (§4.7).

## Rolling back a successful upgrade

`pressfleet upgrade --rollback` ([#250](https://github.com/rchouinard/pressfleet/issues/250)) runs the same sequence for the most recent upgrade, if it hasn't already been rolled back. The new control plane may have worked for a while by then, and restoring the state database undoes its records but not what it did to sites. So before anything changes, the CLI asks the running control plane what changed since the upgrade started, with `pressfleet admin changes --since <time>` ([#248](https://github.com/rchouinard/pressfleet/issues/248)). The control plane answers from its jobs and audit log, and the CLI sorts the changes into three groups:

| Group | Changes | Why |
| --- | --- | --- |
| Blocking | Sites created, imported, or deleted; WordPress core updates; client groups deleted or offboarded | A created site's containers, database, and object cache user would be orphaned. A deleted site would be recreated empty. A core-updated site would move back to an older core than its database. A deleted repository is gone. |
| Converged | Site template and image updates, settings changes | The previous control plane's reconcile moves the sites back to match the restored records. |
| Lost | Backup records, users, alert rules, jobs, and the like | Their records go with the restore; backup snapshots stay in the repositories. |

`--rollback` lists all three groups. It refuses blocking changes unless `--force` is given, saying what `--force` would leave behind, and otherwise asks for confirmation (or `--yes`). The confirmation names the upgrade's time, and says whether the state database will be restored. If the control plane doesn't answer, as after an upgrade that failed with `--no-rollback`, the CLI skips the changes check and says why that is safe: the rollout hold kept rollouts away from the sites.

`--rollback` is exempt from the version skew check, as `upgrade` is (§4.7). Only one saved project exists, so only the most recent upgrade can be rolled back, and only once.

## PostgreSQL

PressFleet doesn't back up PostgreSQL state databases before migrating them (§4.7). With PostgreSQL:

- **No migration:** both automatic rollback and `--rollback` restore the images and project alone, and lose nothing.
- **A migration:** they stop before changing anything, and print the steps that use the operator's own backup.

The marker ([#246](https://github.com/rchouinard/pressfleet/issues/246)) is what lets the CLI tell the two cases apart. Taking a logical backup of PostgreSQL before migrating, in the §5.10 format, would let these installs roll back on their own too. That reverses a design doc decision, so it is left for later.

## Compatibility and release

- **Default.** `pressfleet upgrade` rolls back by default, which the release notes say. `--no-rollback` keeps today's behavior.
- **Admin interface.** `admin changes` is a new command, so `adminInterfaceVersion` stays the same.
- **Bootstrapping.** The control plane being upgraded to writes the marker and honors the rollout hold, and the CLI running the upgrade is that same release. Rollback therefore already works for an upgrade to the first release that has it, from any older release. The CLI falls back to today's file-name lookup if a marker is missing.
- **Manual recovery.** `TestUpgradeRecovery` (#142) runs with `--no-rollback`, and the docs keep the manual steps.

## Issues

Sizes and priorities follow the [roadmap](roadmap.md#conventions). All are in the [Post-MVP: Upgrade Rollback](https://github.com/rchouinard/pressfleet/milestone/10) milestone and are sub-issues of [#141](https://github.com/rchouinard/pressfleet/issues/141).

| # | Issue | Priority | Size | Depends on |
| --- | --- | --- | --- | --- |
| [#245](https://github.com/rchouinard/pressfleet/issues/245) | Decision: upgrade rollback behavior | P0 | S | — |
| [#246](https://github.com/rchouinard/pressfleet/issues/246) | Control plane: record state database migrations in a marker file | P0 | S | #245 |
| [#247](https://github.com/rchouinard/pressfleet/issues/247) | Control plane: hold rollouts while an upgrade is unconfirmed | P1 | S | #245 |
| [#248](https://github.com/rchouinard/pressfleet/issues/248) | Admin interface: what changed since an upgrade | P1 | S | #245 |
| [#249](https://github.com/rchouinard/pressfleet/issues/249) | Upgrade: roll back a failed upgrade automatically | P0 | L | #245, #246 |
| [#250](https://github.com/rchouinard/pressfleet/issues/250) | Upgrade: `pressfleet upgrade --rollback` | P0 | M | #248, #249 |
| [#251](https://github.com/rchouinard/pressfleet/issues/251) | E2E: automatic and on-demand upgrade rollback | P0 | M | #247, #250 |
| [#252](https://github.com/rchouinard/pressfleet/issues/252) | Docs: upgrade rollback | P1 | S | #250 |

### Order

1. **Decisions (#245).** Then the three control plane pieces can be built in parallel: the marker (#246), the rollout hold (#247), and `admin changes` (#248).
2. **Automatic rollback (#249)**, the core of the milestone.
3. **`--rollback` (#250)**, on the same sequence.
4. **End-to-end tests (#251) and docs (#252).**

The critical path is #245 → #246 → #249 → #250 → #251.

### Working

Each issue is worked on its own branch and merged through a pull request. PRs that touch the upgrade add the `e2e` label, since the upgrade tests run there.

### Testing

- **Unit:** the fake `UpgradeProject`, for a failure at each step of the upgrade and of the rollback; with and without a migration; SQLite and PostgreSQL; interrupts; forced services; and the refusals of `--rollback`.
- **Store:** markers on SQLite and PostgreSQL, and `admin changes` with jobs and audit events of each kind.
- **End to end** ([#251](https://github.com/rchouinard/pressfleet/issues/251)):
  - upgrade to a control plane whose healthcheck always fails, which rolls back with a migration
  - a SIGTERM during the apply, which rolls back
  - `--rollback` after a successful upgrade, and its refusal when a site was created since
  - a failed rollback, whose printed manual steps recover the platform
  - the manual recovery test (#142) with `--no-rollback`

  If the tree has no migration since the previous release, the test image adds a no-op one, so the restore path always runs.

## Decision log

Recorded here once [#245](https://github.com/rchouinard/pressfleet/issues/245) is resolved.
