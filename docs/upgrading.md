# Upgrading

`pressfleet upgrade` moves the platform to the release of the `pressfleet` CLI that runs it (design doc §4.7). To upgrade, install the new release's CLI on the host and run:

```console
# pressfleet upgrade
```

Run it as root, like `init`. If the upgraded platform doesn't come back healthy, `upgrade` **rolls back** to the previous release on its own ([Automatic rollback](#automatic-rollback)). If it can't, or with `--no-rollback`, it prints the commands that return the platform to the previous release ([Recovering from a failed upgrade](#recovering-from-a-failed-upgrade)). `upgrade` exits with `0` when the platform is upgraded, `2` when the upgrade failed and was rolled back, and `1` when it failed and wasn't.

To return to the previous release after an upgrade that succeeded, run `pressfleet upgrade --rollback` ([Rolling back an upgrade](#rolling-back-an-upgrade)).

## Release check

The control plane asks GitHub for new releases when it starts and once a day. When a newer stable release exists (not a draft or pre-release):

- `pressfleet version` prints a notice under the control plane's version, saying to install the new release's CLI and run `pressfleet upgrade`;
- the dashboard flags the version under the menu with "Update available", and the About dialog links to the release notes;
- `GET /api/v1/platform` reports it as `release`.

The request carries no credentials or details about your install, only a `User-Agent` of `PressFleet/<version>`. If the host shouldn't contact GitHub (an air-gapped install), set `updates.release_check: false` ([Release check](configuration.md#release-check)); then nothing reports a release, and you must watch the [releases page](https://github.com/rchouinard/pressfleet/releases) yourself. A failed check is silent and changes nothing. Development builds never report one.

## Version skew

The host CLI and the control plane must be compatible: patch versions of one minor version always are; before 1.0 the minor versions must match, and from 1.0 the CLI may be one minor version ahead of or behind the control plane in the same major version. Host commands (`init`, `recover`, `doctor`, `metrics-access`) refuse to run outside that, with no override. `version` and `upgrade` are exempt. Details: [CLI and control plane versions](installation.md#cli-and-control-plane-versions).

In practice: install the new CLI, then run `pressfleet upgrade` straight away, so the pair is matched again. Don't downgrade the CLI below the control plane: the control plane can't be downgraded, and an older CLI may be unable to manage it.

## Before you upgrade

1. Read the release notes for the version, above all anything about renamed settings, the oldest release it upgrades from, and MariaDB.
2. Take a state backup: run `pressfleet state backup` in the control plane container ([Operating](operating.md#commands-that-run-in-the-control-plane-container)). With PostgreSQL, take your own database backup ([below](#postgresql-state-databases)).
3. Run `pressfleet doctor` and fix anything it fails other than the in-use ports.
4. Pick a quiet time if the release recreates Traefik, MariaDB, or the object cache; `upgrade` tells you, and asks first. Sites go down for about a minute, usually less.
5. Have the master key and config file safe off-host, as always.

Then:

```sh
# install the new CLI (see Installation > Getting the CLI), then
pressfleet version --client
pressfleet upgrade
pressfleet version            # both versions should now match
pressfleet doctor
```

Afterwards, watch the **Jobs** page: the control plane moves sites to new site images and templates one at a time, in your maintenance window if you set `updates.window_start` ([Platform image updates](configuration.md#platform-image-updates), [Site template updates](configuration.md#site-template-updates)).

## What it does

1. **Checks** that the upgrade can start, changing nothing:
   - the control plane is installed, and runs a release no newer than the CLI (the control plane can't be downgraded) and no older than the oldest release this CLI upgrades from (the release notes name it; older control planes go through an intermediate release first);
   - the host passes the runtime [preflight checks](configuration.md#preflight-checks);
   - the config file loads with the new release ([renamed and removed settings](configuration.md#renamed-and-removed-settings) are accepted with a warning);
   - MariaDB stays in its release series (X.Y). Each PressFleet release pins one series, and moving to another isn't supported yet: MariaDB upgrades its data files in place, one way.
2. **Renders** the new platform project into `<data_dir>/upgrade/next-platform/` and compares it with the running one, to find the services to recreate: those whose definition changed, and those mounting a configuration file whose content changed.
3. **Asks for confirmation** when it recreates Traefik, MariaDB, or the object cache, which interrupts every site until they are back (usually under a minute), and when the state database is PostgreSQL (see below). `--yes` confirms without asking. Upgrading only the control plane doesn't interrupt sites: they don't depend on it to serve traffic.
4. **Pulls** the new images. A failed pull leaves everything as it was.
5. **Saves** what a recovery needs: a copy of the platform project in `<data_dir>/upgrade/previous-platform/`, and in `<data_dir>/upgrade/upgrade.json` the versions, the time the upgrade started, and the images the services ran (by digest).
6. **Applies** the new project: renders it into `<data_dir>/platform/`, recreates the changed services, and waits until every service is healthy and the control plane reports ready at the new version. The control plane migrates the state database as it starts; with SQLite it first backs the database up into `<data_dir>/state/backups/pressfleet-<timestamp>-schema<NNNN>.db` (NNNN is the schema version before the migration). With either database it records the migration in `<data_dir>/state/backups/migration-<timestamp>-schema<NNNN>.json`: when it ran, the release, the schema versions, and the backup, if any. That is how the CLI learns whether the upgrade migrated the state database without opening it.

7. **Rolls back** if the apply fails or is interrupted ([Automatic rollback](#automatic-rollback)).

The host CLI never opens the state database itself. Running `upgrade` again when nothing changed does nothing. Run at the release the platform already runs, `upgrade` applies changes to the config file: see [Changing settings](configuration.md#changing-settings).

Sites aren't part of the platform project. If the release changes how sites are rendered, the upgraded control plane moves them to the new rendering one at a time, with a health check and a revert on failure ([Site template updates](configuration.md#site-template-updates)); `GET /api/v1/template-rollout` shows its progress.

## Held rollouts

An upgraded control plane that isn't confirmed healthy must not start changing sites. While the upgrade record `<data_dir>/upgrade/upgrade.json` has the status `in-progress`, `failed`, `rolling-back`, or `rollback-failed`, the control plane starts no site updates from the WordPress core, image, and site template rollouts. Updates you request by hand, and the reconciliation at startup, run as usual. When `upgrade` marks the record `succeeded` after the platform is healthy, or `rolled-back` after a rollback, the rollouts resume at their next pass (at most 15 minutes), without a restart.

`GET /api/v1/core-rollout`, `/api/v1/image-rollout`, and `/api/v1/template-rollout` report `held: true` with a `hold_reason`, the dashboard's platform overview shows it, and `pressfleet doctor` warns (`upgrade-rollouts`). To lift the hold, run `pressfleet upgrade` again until it succeeds or rolls back, or return to the previous release with `pressfleet upgrade --rollback` ([Rolling back an upgrade](#rolling-back-an-upgrade)) or, if that can't finish, the steps in [Recovering from a failed upgrade](#recovering-from-a-failed-upgrade).

An installation that never upgraded has no record and no hold. A record the control plane can't read or parse holds the rollouts too, with a warning in its log, because it can't tell that the upgrade was confirmed.

### The record as a contract

The CLI writes the record and the control plane reads its `status` (and `to`, for the message). The statuses are a contract between the two, versioned like the admin interface (design doc §4.7; version 1, `upgradestate.ContractVersion`):

| Status | Meaning | Rollouts |
| --- | --- | --- |
| `in-progress` | `upgrade` is applying the release, or was killed doing so | held |
| `failed` | the apply failed and nothing has retried or rolled it back | held |
| `rolling-back` | a rollback is running | held |
| `rollback-failed` | a rollback stopped | held |
| `succeeded` | the platform was healthy at the new version | run |
| `rolled-back` | the platform is back on the previous release | run |

A status a control plane doesn't know holds the rollouts. Fields can be added to the record; changing the meaning of a status, or what the control plane reads, raises the contract version.

## Notes for specific releases

The GitHub release notes list the commits since the previous release ([Releasing](releasing.md)). What an operator needs to know or do for a release is kept here, so it is versioned with the release.

### The next release (after 1.1.0)

**Automatic rollback (#249).** `pressfleet upgrade` now rolls back a failed upgrade on its own ([Automatic rollback](#automatic-rollback)), and exits with `2` when it did. Scripts that took any non-zero exit for a platform left failed should accept `2` as "failed, and back on the previous release". `--no-rollback` keeps the previous behavior: the platform is left as it failed, and the recovery steps are printed. The upgrade record's statuses `rolling-back`, `rolled-back`, and `rollback-failed` are spelled with hyphens; a record with underscores is an unknown status, which holds the rollouts.

**`pressfleet upgrade --rollback` (#250).** Returns the platform to the release before the last upgrade, after listing what the control plane changed since, and refusing changes it would leave behind unless `--force` is given ([Rolling back an upgrade](#rolling-back-an-upgrade)). It exits with `2` when it rolled back.

**Backup repositories after a state restore (#277).** After a state restore that is older than a client group's backup repository, the group's backups used to fail for good with `wrong password or no key found`. The control plane now moves such a repository aside, keeping its files, and creates a new one ([Troubleshooting](troubleshooting.md#backups)). It only does this while a repository's stored password has never opened it.

**Migration markers (#246).** The control plane now records each run of state database migrations in a small JSON file beside the pre-migration backups (`<data_dir>/state/backups/migration-<timestamp>-schema<NNNN>.json`), for PostgreSQL too. The CLI reads them to find a failed upgrade's backup, and falls back to the backups' file names for an upgrade from a control plane that doesn't write them. They take a few hundred bytes each; delete old ones with the backups they name. The control plane refuses to migrate if it can't write the marker, so `<data_dir>/state/backups` must be writable with PostgreSQL as well.

**Database.** Migration 0036 records when each repository's password first opened it. Repositories of client groups with a successful backup are marked as opened. Each other repository is opened once, by its group's next backup or restore, which costs that job one short restic task.

### The release that adds staging sites

**New.** [Staging sites](staging.md): a copy of a production site on its own preview address, refreshed from production and pushed back to it, from the dashboard or the API. Nothing changes for existing sites until someone creates a staging site.

**New permissions.** Four actions, each for Operators and Admins: `site.staging.create`, `site.staging.refresh`, `site.staging.push`, and `site.staging.delete`. Deleting a staging site's backups with it stays Admin, as does deleting a production site. None asks for a fresh second factor. Roles are platform-wide, so any Operator can push any staging site to its production site: look at who holds the role ([Roles](first-steps.md#3-create-users-and-choose-roles), [Security](security.md#staging-sites)).

**Release order: site images first.** Staging sites turn off outbound email with a switch (`PRESSFLEET_DISABLE_MAIL`) that is part of the **site images**. Whoever cuts the release must publish site images that have the switch before publishing the control plane release that can create staging sites. Every published image tag now includes it, and the same images add the admin bar badge `STAGING | email off`.

#### A staging site is only created on an image with the mail switch

A site runs a pinned build of its image tag, and a new site is pinned to the build the host already has for that tag. So a host that cached a build from before the switch could otherwise pin a new staging site to it. The clone job guards against that: it pulls the site's image tags before it starts the staging site (a failed pull is only a warning in the job log), and then, before any of production's data is restored, it checks that the running site has the switch. If it doesn't, the job fails and rolls back, and nothing of production is copied. The job's error says "this build of the site image ... doesn't include the outbound mail switch, so the clone was stopped to keep a copy of production from sending real email", names the pull error if there was one, and the staging site is left `failed`.

To fix it, get a current build of that image onto the host, and create the staging site again (delete the failed one first): pull it (`docker pull ghcr.io/rchouinard/pressfleet/site-php:<tag>`, with the tag named in the error), or let the host reach the registry. Updating the production site to a newer WordPress release also gets it a current image. The check runs when a staging site is created; turning outbound email on for a staging site later is your choice and isn't checked.

#### Other changes in this release

**CPU limits.** A site's CPU limit is now split between its `nginx` and `php` containers in multiples of 0.01 CPU. Docker's systemd cgroup driver rounds each container's limit up to a multiple of 10 ms per second, so a site set to 0.75 CPUs used to get 0.76. After the upgrade, a templates rollout goes through every site, one at a time with a health check. Only containers whose split changed are recreated, so a site on a limit that divides cleanly, such as the default of 1 CPU, isn't restarted.

**Host CLI.** When a failed upgrade's recovery steps can't read the state backup directory on the host, the CLI now lists it through Docker, instead of wrongly saying that the state database needs no restore. Install the new CLI before upgrading, as usual ([Version skew](#version-skew)).

**Database.** Migrations 0032 to 0035 add what staging sites keep: the job a job waits for (0032), a staging site's state and outbound mail switch (0033), the alert rule option `include_staging` (0034), and the ages of a staging site's database and uploads (0035). Like all migrations they are forward-only: the control plane can't be moved back to the previous release without restoring the state backup ([Recovering from a failed upgrade](#recovering-from-a-failed-upgrade)).

**Not changed.** No new settings. The oldest release this one upgrades from is unchanged, and so are the CLI and control plane skew rules ([Version skew](#version-skew)). The built-in alert rules skip staging sites ([Alerts](configuration.md#alerts)).

## PostgreSQL state databases

PressFleet backs up an SQLite state database before migrating it, but not PostgreSQL (see [State database](configuration.md#state-database)). Take a backup of the PostgreSQL database right before upgrading: returning to the previous release after a failed upgrade needs it. `upgrade` asks you to confirm that, or takes `--yes`.

## Automatic rollback

If anything fails after the platform starts to change (rendering the new project, recreating the services, their health checks, or the control plane's version check), or `upgrade` gets SIGINT or SIGTERM (Ctrl-C) while it applies the release, it returns the platform to the release before the upgrade. Failures before that, in the checks or the pull, change nothing and need no rollback. The rollback takes the same steps as the [recovery steps](#recovering-from-a-failed-upgrade):

1. Stops the control plane. Sites keep serving.
2. Restores the state database, if the upgrade migrated it (the control plane's migration markers tell, see [What it does](#what-it-does)): with SQLite, from the backup the control plane took before migrating, with the previous control plane image (`state restore`, which first saves the database it replaces). Anything the control plane recorded since the upgrade started is lost. Without a migration, the database stays as it is, and nothing is lost.
3. Puts the platform project from before the upgrade back, and removes the files the new release added. Traefik's dynamic directory keeps the sites' routes the control plane wrote; the previous control plane rewrites them when it starts.
4. Starts the previous release's services, recreating those the upgrade recreated only because a file they mount changed, and waits until the control plane reports ready at the previous version.

`upgrade` then says what it rolled back and exits with `2`. If this CLI is outside the [version skew](#version-skew) policy with the previous control plane, it says which CLI to install again. Fix the cause and run `pressfleet upgrade` to try again.

The rollback runs to the end even though the interrupt that started it canceled the upgrade. Interrupting it again stops it, as a failed step does; then `upgrade` prints the recovery steps, which work from wherever the rollback stopped, and exits with `1`. The upgrade record (`<data_dir>/upgrade/upgrade.json`) keeps the rollback's progress, with the status `rolling-back`, then `rolled-back` or `rollback-failed`, and the next `pressfleet upgrade` offers to resume it.

The rollback refuses to start, and changes nothing, when it can't finish safely:

- **PostgreSQL and a migration.** PressFleet doesn't back up PostgreSQL state databases, and the previous control plane would refuse the migrated schema. `upgrade` prints the recovery steps, whose step 2 restores [your own backup](#postgresql-state-databases). Without a migration, a PostgreSQL install rolls back like an SQLite one.
- **Not knowing whether the upgrade migrated the state database**, when neither the host nor a container can read the state backups directory.
- **A MariaDB release series change**, which can't be undone. Upgrades refuse to change the series, so this only guards against later releases.

`--no-rollback` leaves a failed upgrade as it is, for debugging, and prints the recovery steps instead. A later `pressfleet upgrade` retries it.

### An interrupted upgrade

If `upgrade` was killed while it applied the release or rolled it back (`kill -9`, a reboot, a lost SSH session without `nohup`), its record still says `in-progress` or `rolling-back`. The next `pressfleet upgrade` asks whether to retry the upgrade or roll it back; `--yes` retries an interrupted upgrade and resumes an interrupted rollback. It won't upgrade to another release over an interrupted upgrade: with the CLI of another release, it can only roll it back, after which you can upgrade. `pressfleet upgrade --rollback` rolls it back, or resumes the rollback, without asking which.

## Rolling back an upgrade

If an upgrade succeeded but you regret it, run, with the CLI that upgraded:

```console
# pressfleet upgrade --rollback
```

It returns the platform to the release before the upgrade, with the same steps as the [automatic rollback](#automatic-rollback). Only the most recent upgrade can be rolled back, and only once: `upgrade` keeps one platform project from before an upgrade, and the rollback puts it back. Like `upgrade`, `--rollback` is exempt from the [version skew](#version-skew) check. It also rolls back an upgrade that failed with `--no-rollback`, or that was interrupted, and resumes a rollback that stopped.

The new control plane may have worked for a while since the upgrade, and restoring the state database undoes what it recorded, but not what it did to sites. So when the upgrade migrated the state database, `--rollback` first asks the running control plane what changed since the upgrade started (`pressfleet admin changes`, run in its container), and lists the changes in three groups:

| Group | Changes | After the rollback |
| --- | --- | --- |
| Blocking | Sites created, imported, or deleted, staging sites included; WordPress core updates; go-lives; client groups deleted or offboarded | A created site's containers, database, and object cache user are orphaned; a deleted site is recreated empty; a core-updated site runs an older core than its database; a site that went live keeps the live URLs in its database while its record routes the old domains; a deleted group's repository is gone. |
| Converged | Site template and image updates, settings changes | The previous control plane's reconcile moves the sites back to match the restored records. |
| Lost | Backup records, users, alert rules, jobs, staging refreshes and pushes, restores, and the like | Their records go with the restore. Backup snapshots stay in the repositories, and content copied to sites stays. |

`--rollback` refuses blocking changes, says what each would leave behind, and changes nothing. Undo them first where you can (delete a site created since the upgrade, for instance), or pass `--force` to roll back anyway and clean up after it. Otherwise it says what it will do (the upgrade's start time, and whether the state database will be restored and from which backup) and asks for confirmation; `--yes` gives it.

- **No migration.** If the upgrade didn't migrate the state database, the rollback keeps it, with everything recorded since, so there is nothing to check, and nothing is lost. This holds for PostgreSQL too.
- **PostgreSQL and a migration.** `--rollback` stops before changing anything and prints the [recovery steps](#recovering-from-a-failed-upgrade), whose step 2 restores [your own backup](#postgresql-state-databases).
- **A control plane that doesn't answer.** After an upgrade that failed with `--no-rollback`, or was interrupted, the rollouts were [held](#held-rollouts), so the control plane changed sites only when asked: `--rollback` skips the check and says so. After a succeeded upgrade, it refuses until the control plane answers, or with `--force`.

`--rollback` exits like a failed upgrade: `2` when the platform is back on the previous release, and `1` when it isn't, because it refused (nothing changed) or the rollback stopped (with the recovery steps printed, as for the automatic rollback). Afterwards, install the previous release's CLI again if this one is outside the [version skew](#version-skew) policy with it; `--rollback` says so. To upgrade again, run `pressfleet upgrade`.

## Recovering from a failed upgrade

If the platform doesn't come back healthy and the [automatic rollback](#automatic-rollback) can't finish, or with `--no-rollback`, `upgrade` exits with `1` and prints the error and the steps that return to the previous release, with the actual paths filled in. They look like this (with the default data directory):

Sites keep serving while the control plane is down. To return to <previous version>, run these commands on this host as root:

1. Stop the control plane:

    docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/platform stop control-plane

2. Restore the state database from the backup the control plane took before migrating it, with the previous control plane image (<previous control plane image>):

    docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/upgrade/previous-platform run --rm --no-deps control-plane state restore /var/lib/pressfleet/state/backups/pressfleet-<timestamp>-schema<NNNN>.db

3. Put the previous platform project back:

    cp -a /var/lib/pressfleet/upgrade/previous-platform/. /var/lib/pressfleet/platform/

4. Start the previous release's services:

    docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/platform up --detach --wait --remove-orphans

Then use the pressfleet CLI of <previous version> again. Anything recorded in the control plane after the upgrade started is lost with the restore.

The previous control plane image is named by digest when it was pulled from a registry. Notes on the steps:

- With PostgreSQL, step 2 says to restore your own backup instead.
- If the control plane didn't migrate the state database (no pre-migration backup was written after the upgrade started), step 2 says no restore is needed, and nothing is lost.
- The backups directory belongs to root, so `upgrade` run by an operator in the docker group can't read it and lists it with a container instead; see below.
- If the upgrade recreated a service only because a file it mounts changed, step 4 has a second command that recreates it on the previous files.
- Sites keep serving throughout, unless the upgrade recreated Traefik, MariaDB, or the object cache and one of them is what failed. Recovering from those is not covered by these steps; look at `docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/platform logs <service>` first.
- Restoring the state database loses whatever the control plane recorded after the upgrade started.

The backups directory (`<data_dir>/state/backups`) belongs to root, because the control plane's container creates it. When the user running `upgrade` can't read it, `upgrade` lists it with a container of the previous control plane image, which needs Docker as the rest of the recovery does. Only if that fails too does step 2 not say whether a restore is needed. It then reads as follows, and you must do the check: skipping a needed restore leaves the previous release's control plane crash-looping with `database schema is newer than this pressfleet binary supports`.

2. This command couldn't check whether the control plane migrated the state database, so check it yourself by listing the state backups directory:

    docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/upgrade/previous-platform run --rm --no-deps --no-TTY --entrypoint ls control-plane -1 /var/lib/pressfleet/state/backups

If no file is named pressfleet-<timestamp>-schemaNNNN.db with a timestamp of 20260101T120000.000Z or later (when the upgrade started), the control plane didn't migrate the database, and nothing needs restoring: go on to step 3. Otherwise restore the first such file, the backup from before the migration, with the previous control plane image (<previous control plane image>):

    docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/upgrade/previous-platform run --rm --no-deps control-plane state restore /var/lib/pressfleet/state/backups/<file name from the listing>

Don't skip the restore when there is such a file: <previous version> would crash-loop with "database schema is newer than this pressfleet binary supports".

A client group that had no backup repository before the upgrade and got one while the new control plane ran loses that repository's password in the restore, but not its files (#277). The files only hold what the failed upgrade backed up. A control plane from a release newer than 1.1.0 handles it on its own: the group's next backup moves the old repository aside, keeping its files, and creates a new one ([Troubleshooting](troubleshooting.md#backups)). If the release you went back to is 1.1.0 or older, that group's backups fail with `stored password doesn't open the repository: Fatal: wrong password or no key found` instead. Move the repository aside yourself and retry the backup, which creates a fresh repository:

    docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/platform exec control-plane mv /var/lib/pressfleet/backups/repositories/<client-group-id> /var/lib/pressfleet/backups/unopenable-<client-group-id>

With S3 backups on those releases, move or delete the objects under the group's ID in the bucket instead.

Alternatively, fix the cause and run `pressfleet upgrade` again. A retry keeps the platform project saved by the failed attempt, so the recovery steps still return to the release from before it. An end-to-end test runs these steps exactly as printed (#142).
