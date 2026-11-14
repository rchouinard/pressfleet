# Backups

PressFleet backs up every site every day, keeps them under a retention policy, checks them, and can restore them. This page is what an operator sets up and does. The mechanics are in [Backup repositories](configuration.md#backup-repositories), [Site backups](configuration.md#site-backups), and [Site restores](configuration.md#site-restores).

How it fits together:

- Each **client group** has its own encrypted [Restic](https://restic.net) repository. All of the group's sites back up into it, and they deduplicate against each other.
- Each repository has its own random password, stored in the state database **encrypted with the [master key](installation.md#master-key)**. Without the master key no backup can be read.
- A **snapshot** holds a site's `wp-content` and a SQL dump of its database. WordPress core and `wp-config.php` aren't in it: they are rebuilt from the site's WordPress version and settings.
- The control plane's own **state database** (which sites exist, users, settings, secrets) is backed up too, into a separate state repository, so a lost host can be rebuilt ([Disaster recovery](disaster-recovery.md)).

## Choosing where backups go

Choose this at install. It applies to repositories created afterwards; a client group's repository stays where it was created, and there is no tool to move one.

| | `local` (the default) | `s3` |
| --- | --- | --- |
| Where | `<data_dir>/backups/repositories/<client group ID>` | An S3-compatible bucket: Amazon S3, MinIO, or another S3-compatible service. |
| Survives losing the host | **No.** It is on the same disk as the sites. | Yes, if the bucket is elsewhere. |
| Setup | Nothing. | Endpoint, bucket, access key; see below. |

**Use `s3` for anything you can't afford to lose.** With `local`, you must copy `<data_dir>/backups/` off the host yourself (rsync, a backup agent, a mounted network volume), keeping ownership and modes (the client group repositories belong to group 60000). Local backups protect against mistakes, not against losing the server.

To use S3, give `init` these flags (or answer the prompts; the S3 questions are only asked for `s3`):

```sh
pressfleet init --backup-backend s3 \
  --backup-s3-endpoint https://s3.eu-west-1.amazonaws.com \
  --backup-s3-bucket agency-backups \
  --backup-s3-prefix pressfleet \
  --backup-s3-region eu-west-1 \
  --backup-s3-access-key-id AKIA... \
  --backup-s3-secret-access-key-file /root/s3-secret
```

- The endpoint is a base URL without a path. `--backup-s3-region` defaults to `us-east-1`; set it for Amazon buckets elsewhere.
- Create the bucket first, or let restic create it on first use if the key may.
- Give PressFleet its own bucket (or prefix) and an access key that can read, write, and delete only there. Backups run from the site's network, so the host needs outbound access to the endpoint.
- The secret key is read from the first line of the file, encrypted with the master key, and only the encrypted value is written to the config file. Delete the file afterwards.
- Each client group's repository is `<prefix>/<client group ID>`; the state repository is `<prefix>/state`. A group whose repository was set aside after a state restore gets a new one at `<prefix>/<client group ID>-<time>` ([Troubleshooting](troubleshooting.md#backups)).
- To run S3 on the same host (MinIO or RustFS), see [Self-hosted S3](configuration.md#self-hosted-s3). That protects against mistakes, not against losing the host.

To change S3 credentials later, see [Rotating secrets](security.md#rotating-secrets).

## Schedule

Every site is backed up once a day from the moment it is created, at a time inside the **backup window** that is derived from the site's ID, so backups spread out instead of all starting together. The window is `backup.schedule.window_start` (UTC, default `02:00`) for `window_hours` (default 4). Run it when your hosts are quietest.

Per site, an Operator or Admin can change the schedule on the site's **Backups** page (or `PUT /api/v1/sites/{id}/backup-schedule`): the interval (1, 2, 3, 4, 6, 8, or 12 hours, or whole days up to a week), the UTC hour to start in, or turn scheduled backups off. Manual backups always work: **Back up now** on the same page (`POST /api/v1/sites/{id}/backups`).

Before risky changes the platform takes its own backups: `pre_update` before a WordPress core update and `pre_restore` before a restore, a staging refresh, or a push to production. A `sync` backup (**Staging sync** in the dashboard) is the snapshot taken as the source of a staging clone, refresh, or push: production's for a clone or refresh, the staging site's for a push. They show in the same list with their `kind`, and the retention policy treats them like any other backup.

[Staging sites](staging.md) start with scheduled backups **off**, because they are disposable copies, so the **Backup stale** and **Backup failed** alerts don't apply to them. Turn a schedule on from the staging site's **Backups** page if one holds work you can't redo. Manual backups work, and every refresh and push backs the staging site up anyway. A staging site's backups sit in its production site's repository and deduplicate against production's.

A site backup that can't read some files still succeeds; the backup's message lists the files it skipped.

## Retention

Each night, after the window closes, a `backup.prune` job applies the retention policy to every site separately and frees the space. A snapshot is kept if any rule keeps it. The defaults:

| Setting | Default | Keeps |
| --- | --- | --- |
| `backup.retention.keep_last` | 3 | The newest 3 snapshots. |
| `backup.retention.keep_daily` | 7 | The newest of each of the last 7 days. |
| `backup.retention.keep_weekly` | 4 | The newest of each of the last 4 weeks. |
| `backup.retention.keep_monthly` | 6 | The newest of each of the last 6 months. |
| `keep_hourly`, `keep_yearly` | 0 | Off. |

If you set every `keep_*` to `0`, nothing is ever removed. Change retention in the config file and apply it with `pressfleet upgrade` ([Changing settings](configuration.md#changing-settings)). A deleted site's snapshots follow the same policy until you delete the repository.

## Integrity checks

Every `backup.check.interval_days` days (default 7), a `backup.check` job runs `restic check` on each repository and reads back `backup.check.read_data_percent` of its data (default 10%, chosen at random each time, so the whole repository is verified over time). On S3 that downloads the share. If a check finds damage, its job fails and logs the errors, and you repair it with restic (see [Troubleshooting](troubleshooting.md#backups)).

Run a prune or check now, in the control plane container ([Operating](operating.md#commands-that-run-in-the-control-plane-container)). Each queues a job and prints its ID:

```sh
pressfleet repository prune <client-group-id>
pressfleet repository check <client-group-id>
```

**Watch for failures.** The default alert rules **Backup failed** (critical, at once) and **Backup stale** (no successful backup for 36 hours) cover this. Make sure an [alert channel](operating.md#alert-channels) is set up, or a failing backup will go unnoticed. The **Jobs** page shows every `site.backup`, `backup.prune`, and `backup.check` job.

**Test a restore** now and then, into a site you can afford to overwrite. A backup nobody has restored is a hope.

## Restoring a site

On the site's **Backups** page, choose a backup and **Restore**. Via the API: `POST /api/v1/sites/{id}/restore` with `{"snapshot_id": "..."}`; add `"source_site_id"` to restore a snapshot of another site of the same client group. The site must be running, or [failed](#recovering-a-failed-site). The job:

1. takes a `pre_restore` backup of the site as it is now;
2. puts the site in maintenance mode (visitors see a `503` page);
3. replaces `wp-content` and the database with the snapshot's, restarts the site's containers, and updates the database for the site's WordPress core;
4. rewrites the database's URLs to the site's own if the snapshot came from another site or from before go-live;
5. purges the caches and takes the site out of maintenance mode.

If the restore fails after touching the site's data, the job puts the `pre_restore` backup back and leaves the site **in maintenance mode**, with what happened in its status message. The message stays after maintenance mode is off, as a record: the site's `maintenance` flag, which the site page shows, is what says whether visitors see the `503` page. Check the site, then turn maintenance mode off (the site page, or `PUT /api/v1/sites/{id}/maintenance` with `{"enabled": false}`). If even that can't be restored, the site is marked `failed` and the status message names the pre-restore backup; restore it once the cause is fixed.

To undo a push to production, restore the **Before a restore** backup that the push took of production ([Undoing a push](staging.md#undoing-a-push)).

### Recovering a failed site

A restore, refresh, push, or core update whose rollback also failed marks the site `failed`. So can other failures. Restoring a backup recovers the site, usually the pre-restore (or pre-push, or pre-update) backup its status message names, and the site can be restored whatever state it is in. Fix the cause first if it is still there, such as a full disk or an unreachable repository. A site whose provisioning or clone failed has nothing to recover and can only be deleted. A refresh or a push is refused until the target is running again.

On a failed site, the job differs from the one above:

- It first puts back what is missing and starts the site: the database, the object cache user, and the containers.
- It attempts the `pre_restore` backup, but goes on without it if a broken site can't be backed up. The job then carries a warning, the backup shows as failed, and the restore **can't be undone or rolled back**.
- A site that was in maintenance mode when the restore started, which a failed job usually leaves it in, **stays in maintenance mode**, and visitors see the `503` page. The site page and the job's log say so. Check the site, then turn maintenance mode off as above. A site that wasn't in maintenance mode serves visitors when the job finishes.
- If the restore fails too, the site stays `failed`, with a status message that says what state its data is in, whether it was rolled back to the state it failed in, left as the restore left it, or never touched. Fix the cause and restore again.

Restores stay within a client group, because each group has its own repository. To restore a site's backup into a site of another group, [export](#exporting-a-site) it and import it ([Site import](configuration.md#site-import)).

## Exporting a site

An export gives you a site as two files: `wp-content.tar.gz` and `database.sql.gz`, the same two files [site import](configuration.md#site-import) takes. Use it to move a site to another host, to hand it to a client, or to copy it into another client group.

- **A live site:** the site page's export card, or `POST /api/v1/sites/{id}/export`.
- **A site as it was in a backup:** `POST /api/v1/sites/{id}/backups/{backup id}/export`. The live site isn't touched.
- **A deleted site's backup:** on the client group's page ("Backups of deleted sites"), or `POST /api/v1/client-groups/{id}/backups/{backup id}/export`.

When the job succeeds, download the files from the export (`GET /api/v1/exports/{id}`) before they expire: `site_export.ttl_seconds`, a day by default. Downloads need a dashboard session or a full API token. The export leaves out WordPress core, `wp-config.php`, the platform's cache drop-ins, and symbolic links; constants you set through the platform must be set again elsewhere. For just the database, `POST /api/v1/sites/{id}/database/export` (an hour to download).

An export takes room in the data directory until it expires. A live-site export estimates its size first and fails, before writing anything, if the disk can't hold it.

## Offboarding a client

When a client leaves, you can hand over their backups and delete everything. Only Admins can do this.

1. **Delete the group's sites.** A client group can't be deleted, nor its repository, while it has sites. By default a deleted site's backups stay in the repository. Tick "Also delete this site's backups" only if the client doesn't need them; that can't be undone.
2. **Hand over the data**, either way:
   - **Site by site:** on the client group's page, **Backups of deleted sites** > export. Each is a site in the two files above, that the client can import anywhere.
   - **The whole repository**, with its password. In the control plane container:

     ```sh
     pressfleet repository export <client-group-id> /var/lib/pressfleet/tmp/acme
     ```

     This copies the repository (downloading it from S3 if needed), writes `password.txt` and a `README.txt` beside it, and checks that plain restic opens it. The directory must be below `<data_dir>/tmp/`, which is how the container shares files with the host. Move it off the host, and **delete it afterwards**: it holds the password in plain text. The client needs only restic:

     ```sh
     restic -r repository --password-file password.txt snapshots
     restic -r repository --password-file password.txt restore latest --tag site:<site-id> --target restored
     ```
3. **Delete the repository**, typing the group's name to confirm: on the client group's page, or tick "Also permanently delete the backup repository" when deleting the group. In the container: `pressfleet repository delete <client-group-id> --yes`. This removes the files (or S3 objects) and the stored password. While it runs, creating a site in the group is refused with a conflict, so a new site can't lose its repository; if the delete fails, the group accepts sites again and you can run the delete again.

Each step is in the audit log. Don't export while the repository is being pruned.

## Backing up the platform itself

The control plane backs up its **state database** every hour (`backup.state.interval_hours`) into the state repository, with its own retention (24 hourly, 14 daily, 8 weekly by default). It includes users, sites, domains, and all secrets, still encrypted with the master key. You can see them and take one now:

```sh
pressfleet state backup     # back up now and wait for it
pressfleet state backups    # list them
```

Two other things are *not* in any backup and are yours to keep:

- **The master key and the config file**, off-host ([Master key](installation.md#master-key)).
- **The backup location itself**, if it is `local`: copy `<data_dir>/backups/` off the host.

Metrics history, uploads, and exports waiting to be downloaded aren't backed up.

A state backup holds the repository passwords as they were when it was taken. Restoring one that is older than a client group's repository leaves that repository without its password; the group's next backup then moves the repository aside, keeping its files, and starts a new one ([Troubleshooting](troubleshooting.md#backups)).

### The disaster recovery drill

Rehearse rebuilding the platform before you need to: on a spare machine, install the same way (`pressfleet init --force` with the saved master key and config), run `pressfleet recover`, and check that the sites come back. The procedure, and what comes back and what doesn't, is in [Disaster recovery](disaster-recovery.md). Do it once before you put client sites on the platform, and again after big changes.

For PostgreSQL state databases, and the backup that `pressfleet upgrade` asks you to take beforehand, see [Upgrading](upgrading.md#postgresql-state-databases).
