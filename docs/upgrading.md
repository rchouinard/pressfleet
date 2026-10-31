# Upgrading

`pressfleet upgrade` moves the platform to the release of the `pressfleet` CLI that runs it (design doc §4.7). To upgrade, install the new release's CLI on the host and run:

```console
# pressfleet upgrade
```

Run it as root, like `init`. Upgrades are **forward-only**: there is no `--rollback`. If the upgraded platform doesn't come back healthy, `upgrade` prints the commands that return it to the previous release ([Recovering from a failed upgrade](#recovering-from-a-failed-upgrade)). Automatic rollback is planned after the MVP (#141).

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
6. **Applies** the new project: renders it into `<data_dir>/platform/`, recreates the changed services, and waits until every service is healthy and the control plane reports ready at the new version. The control plane migrates the state database as it starts; with SQLite it first backs the database up into `<data_dir>/state/backups/pressfleet-<timestamp>-schema<NNNN>.db` (NNNN is the schema version before the migration).

The host CLI never opens the state database itself. Running `upgrade` again when nothing changed does nothing. Run at the release the platform already runs, `upgrade` applies changes to the config file: see [Changing settings](configuration.md#changing-settings).

Sites aren't part of the platform project. If the release changes how sites are rendered, the upgraded control plane moves them to the new rendering one at a time, with a health check and a revert on failure ([Site template updates](configuration.md#site-template-updates)); `GET /api/v1/template-rollout` shows its progress.

## Notes for specific releases

The GitHub release notes list the commits since the previous release ([Releasing](releasing.md)). What an operator needs to know or do for a release is kept here, so it is versioned with the release.

### The release that adds staging sites

**New.** [Staging sites](staging.md): a copy of a production site on its own preview address, refreshed from production and pushed back to it, from the dashboard or the API. Nothing changes for existing sites until someone creates a staging site.

**New permissions.** Four actions, each for Operators and Admins: `site.staging.create`, `site.staging.refresh`, `site.staging.push`, and `site.staging.delete`. Deleting a staging site's backups with it stays Admin, as does deleting a production site. None asks for a fresh second factor. Roles are platform-wide, so any Operator can push any staging site to its production site: look at who holds the role ([Roles](first-steps.md#3-create-users-and-choose-roles), [Security](security.md#staging-sites)).

**Release order: site images first.** Staging sites turn off outbound email with a switch (`PRESSFLEET_DISABLE_MAIL`) that is part of the **site images**. Whoever cuts the release must publish site images that have the switch before publishing the control plane release that can create staging sites. An older image ignores the switch, and a staging site on it **can send real email** to the customers in the copied data. The same images add the admin bar badge `STAGING | email off`.

#### Staging sites need up-to-date site images

A site runs a pinned build of its image tag. A new site is pinned to the build the host already has for that tag, and it pulls the tag only if the host has none. So right after an upgrade, a staging site can start on a cached build from before the switch, until the image rollout moves it (a pass runs when the control plane starts and every 15 minutes, and pulls new builds every `updates.images.check_hours`, 6 hours by default; see [Platform image updates](configuration.md#platform-image-updates)). Before the first staging site on an upgraded host:

1. Check that the image rollout has finished: the Jobs page shows the `site.image_update` jobs, and `GET /api/v1/image-rollout` the newest builds it has seen and any failed sites. If you set `updates.images.check_hours` to `0`, or you don't want to wait, pull the tags in use on the host yourself (`docker pull ghcr.io/rchouinard/pressfleet/site-php:<tag>` and `site-nginx:<tag>`); builds that reach the host that way roll out too.
2. Create the staging site, open it, and look for the `STAGING | email off` badge in the admin bar. If it isn't there, the site runs an older image: stop it (it does no harm while stopped) and wait for the rollout to move it.

#### Other changes in this release

**CPU limits.** A site's CPU limit is now split between its `nginx` and `php` containers in multiples of 0.01 CPU. Docker's systemd cgroup driver rounds each container's limit up to a multiple of 10 ms per second, so a site set to 0.75 CPUs used to get 0.76. Existing sites keep their old split until they are next re-rendered, which includes the reconciliation the control plane runs for every site when it starts. A site whose limit isn't a multiple of 0.04 CPU (the default of 1 CPU is) gets its containers recreated by that reconciliation after this upgrade, a brief restart outside the maintenance window and without the health check of a rollout. The rest are unaffected.

**Database.** Migrations 0032 to 0035 add what staging sites keep: the job a job waits for (0032), a staging site's state and outbound mail switch (0033), the alert rule option `include_staging` (0034), and the ages of a staging site's database and uploads (0035). Like all migrations they are forward-only: the control plane can't be moved back to the previous release without restoring the state backup ([Recovering from a failed upgrade](#recovering-from-a-failed-upgrade)).

**Not changed.** No new settings. The site templates are unchanged, so no template rollout runs. The oldest release this one upgrades from is unchanged, and the host CLI has no changes since the previous release, so the CLI and control plane skew rules are unaffected ([Version skew](#version-skew)). The built-in alert rules skip staging sites ([Alerts](configuration.md#alerts)).

## PostgreSQL state databases

PressFleet backs up an SQLite state database before migrating it, but not PostgreSQL (see [State database](configuration.md#state-database)). Take a backup of the PostgreSQL database right before upgrading: returning to the previous release after a failed upgrade needs it. `upgrade` asks you to confirm that, or takes `--yes`.

## Recovering from a failed upgrade

If the platform doesn't come back healthy, `upgrade` exits non-zero and prints the error and the steps that return to the previous release, with the actual paths filled in. They look like this (with the default data directory):

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
- If the upgrade recreated a service only because a file it mounts changed, step 4 has a second command that recreates it on the previous files.
- Sites keep serving throughout, unless the upgrade recreated Traefik, MariaDB, or the object cache and one of them is what failed. Recovering from those is not covered by these steps; look at `docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/platform logs <service>` first.
- Restoring the state database loses whatever the control plane recorded after the upgrade started.

Alternatively, fix the cause and run `pressfleet upgrade` again. A retry keeps the platform project saved by the failed attempt, so the recovery steps still return to the release from before it. An end-to-end test runs these steps exactly as printed (#142).
