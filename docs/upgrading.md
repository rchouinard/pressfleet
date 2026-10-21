# Upgrading

`pressfleet upgrade` moves the platform to the release of the `pressfleet` CLI that runs it (design doc §4.7). To upgrade, install the new release's CLI on the host and run:

```console
# pressfleet upgrade
```

The control plane checks for new releases once a day, and `pressfleet version` and the dashboard say when one is available ([Release check](configuration.md#release-check)).

Run it as root, like `init`. Upgrades are **forward-only**: there is no `--rollback`. If the upgraded platform doesn't come back healthy, `upgrade` prints the commands that return it to the previous release ([Recovering from a failed upgrade](#recovering-from-a-failed-upgrade)). Automatic rollback is planned after the MVP (#141).

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
