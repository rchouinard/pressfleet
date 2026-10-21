# Disaster recovery

This guide rebuilds a platform whose host is lost: the disk died, the server was deleted, or the data directory is gone. The client group backup repositories hold the sites' files and databases, but they aren't enough on their own. Which sites exist, their settings and domains, the users, and every secret (the repositories' own passwords included) are in the control plane's state database. So the control plane backs the state database up too, next to the site backups, and a new host is rebuilt from three things (design doc §4.1, §5.3):

1. **The master key**, saved off-host when the platform was installed. Without it no backup can be read: not the state backups, not the site backups.
2. **The backup location**: `<data_dir>/backups/` with the local backend, or the S3 bucket with `s3`.
3. **The answers given to `pressfleet init`**: the domains, the data directory, the backup settings (including the S3 credentials), and the rest. The config file (`/etc/pressfleet/pressfleet.yaml` by default) holds them; keep a copy with the master key. Its secrets are encrypted with the master key.

Anything the platform recorded after the last state backup is lost: a site created since then has to be imported or created again, and settings changed since then go back. The sites themselves come back from their own newest backups, which can be newer than the state backup.

## State backups

The control plane backs up the state database every hour (`backup.state.interval_hours`) into the **state repository**, a Restic repository beside the client group repositories:

- `local`: `<data_dir>/backups/state`
- `s3`: `<prefix>/state` in the backup bucket

A state backup is a copy of the whole state database in a format that is the same for SQLite and PostgreSQL (a JSON export of every table), so it also covers PostgreSQL state databases, and a backup from one loads into the other. It includes every secret, still encrypted with the master key: the client group repository passwords, the site credentials, and the platform login key with its secret (`platform_keys`, the key that signs one-click logins to WordPress, design doc §5.9). Restic encrypts the backup again, with a password derived from the master key, which is how a new host opens the repository with nothing but the key.

Retention keeps the last day's hourly backups, then one a day for two weeks and one a week for two months (`backup.state.retention`, see [Settings](configuration.md#settings)). Each backup is a `platform.state_backup` job, which shows on the dashboard's jobs page; a failed one is retried like any job.

Inside the control plane container (`docker compose --project-name pressfleet --project-directory /var/lib/pressfleet/platform exec control-plane …`, shortened to `docker compose exec control-plane` here):

```sh
docker compose exec control-plane pressfleet state backup     # back up now and wait for it
docker compose exec control-plane pressfleet state backups    # list the backups
```

**Keep the backup location off the host.** With `s3` on another machine or a cloud bucket, it already is. With `local`, the backups are on the same disk as everything else: copy `<data_dir>/backups/` elsewhere regularly (rsync, a backup agent, a mounted network volume), keeping file ownership and modes. With a self-hosted S3 service on the same host ([Self-hosted S3](configuration.md#self-hosted-s3)), copy its data off the host.

## Rebuilding on a new host

Run these steps as root on the new host. Use the same data directory path as before: local repositories are recorded by path.

1. **Prepare the host** as for a first installation ([Installation](installation.md)): Docker, the address pools, and the DNS records, pointed at the new host's address.

2. **Put the backups and the key back.** With the local backend, copy the saved `backups/` directory to `<data_dir>/backups/` with its ownership and modes (`cp -a`, `rsync -a`); the client group repositories must stay in group 60000. With `s3`, nothing to copy. Put the master key in its file, outside the data directory, readable only by root:

   ```sh
   install -d -m 0700 /etc/pressfleet
   (umask 077; cat > /etc/pressfleet/master.key)   # paste the key, then Ctrl-D
   ```

3. **Install** with `pressfleet init`, giving the same answers as the first installation (or prefilled from the saved config file), and `--force`, because the master key file is already there. `init` reuses the key; it never replaces one. It starts an empty platform and creates an admin user, which the restore replaces with the old users.

   ```sh
   pressfleet init --force
   ```

4. **Recover the state:**

   ```sh
   pressfleet recover
   ```

   `recover` stops the control plane, restores the newest state backup in a one-off control plane container (the host CLI never touches the state database, design doc §4.7), and starts the control plane again. `--snapshot <id>` restores an older backup; list them with `docker compose exec control-plane pressfleet state backups`. The restore checks, before anything changes, that the backup opens with this master key and that the platform login key decrypts with it. It reports what it did:

   ```
   Restoring state backup 1a2b3c4d, taken 2026-10-20T03:00:12Z (schema version 30, sqlite, release v0.4.0), from /var/lib/pressfleet/backups/state
   Restored 5310 rows in 42 tables; the state database is at schema version 30.
   The backup opens with this installation's master key.
   The platform login key was restored and decrypts with the master key; sites keep their public key.
   Failed 3 jobs the old host hadn't finished.
   12 sites will be recovered from their newest backups when the control plane starts.
   ```

   If anything fails (the wrong master key, a damaged backup, a backup from a newer release than the CLI), nothing changes and the control plane is started again as it was. `recover` refuses to replace a state database that already has client groups or sites, as an existing platform does; `--force` replaces it anyway.

5. **Log in** to the dashboard with your old account, and watch the sites come back on the jobs page. The control plane recreates every site that was running or stopped with one `site.recover` job per site: it creates the site's database and object cache user with the restored credentials, starts its containers with the current configuration, restores the newest snapshot of the site in its client group repository (which may be newer than the state backup), and brings the database up to date with the site's WordPress core. A site that was stopped is stopped again. Each site is `updating` until its job finishes, and `running` (or `stopped`) afterwards.

6. **Check** a few sites, and take a state backup (`docker compose exec control-plane pressfleet state backup`). Copy the backup location off the host again if it is local.

### What comes back, and what doesn't

- **Sites** that were running or stopped come back from their newest backup. A site without any backup can't be recovered: its job fails and the site is marked `failed` with "the site has no backup to recover from"; delete it, or import it again if you have its files.
- **Sites that were being provisioned or deleted** when the state backup was taken are marked `failed`, with a message saying so. Delete them, or create them again. Sites that had already failed stay failed.
- **Jobs** the old host hadn't finished (queued, running, or rolling back) are marked `failed`: their work was lost with the host. Scheduled work (backups, WP-Cron, update checks, maintenance) carries on from the schedules.
- **One-click login to WordPress** keeps working: the platform login key is part of the state backup, and sites keep their public key. If the backup had no login key, or one that didn't decrypt, the control plane generates a new one when it starts, and each site gets the new public key when it is recreated, so one-click login recovers on its own either way.
- **Domains and certificates:** the routes are rendered from the site records; Traefik requests new certificates once DNS points at the new host.
- **Dashboard sessions and API tokens** are restored with the users. Sessions from before the loss stay valid until they expire. If the old host may have been compromised, reset the users' passwords and revoke their API tokens.
- **Metrics** (VictoriaMetrics) aren't backed up. History starts again on the new host.
- **Uploads** and **exports** waiting to be downloaded aren't backed up.
- **Failed recoveries:** a site whose `site.recover` job failed for good is marked `failed` with the reason, and what the job created is left in place. Fix the cause and restore the site from one of its backups once it runs, or delete it.

## Rehearsing

Run the drill before you need it: on a spare machine or VM, follow [Rebuilding on a new host](#rebuilding-on-a-new-host) with a copy of the backup location, the master key, and the config file, and check that the sites come back. The end-to-end test `TestDisasterRecovery` runs the same drill on every change: it installs a platform with a site, takes its backups, deletes everything but the backup location and the master key (containers, volumes, data directory, config file), and rebuilds it with `pressfleet init --force` and `pressfleet recover` (see [Testing](testing.md#disaster-recovery)).

## PostgreSQL state databases

State backups cover PostgreSQL state databases the same way. `recover` replaces every table in the state database's schema, so the database must be dedicated to PressFleet. Keep your own PostgreSQL backups too: the pre-migration backup before an upgrade, which `pressfleet upgrade` asks for, is still yours to take ([Upgrading](upgrading.md#postgresql-state-databases)).
