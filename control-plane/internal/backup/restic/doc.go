// Package restic implements [backup.Engine] with Restic (design doc §4.4,
// §5.3): each site's snapshots go into its client group's repository,
// which package backup/repository creates and opens.
//
// # Taking a backup
//
// Engine.Backup, for one site:
//
//  1. looks up the site record for its client group and WordPress core
//     version;
//  2. ensures the group's repository exists (repository.Manager.TaskAccess);
//  3. creates a transfer directory under the data directory's tmp/
//     (backup-<site ID>-<backup ID>), and dumps the site's database into it
//     as database.sql, from the control plane process
//     (sitedb.Manager.Dump);
//  4. runs `restic backup` in a site task (provisioner.TaskRunner) that
//     mounts the site's wp-content read-only, the transfer directory, and,
//     for a local repository, the repository directory;
//  5. reads the snapshot's ID and sizes from restic's JSON summary;
//  6. removes the transfer directory, whether or not the backup worked.
//
// WordPress core and wp-config.php are never in a snapshot: the task can
// only see wp-content and the transfer directory, and those are the only
// paths restic is given. Both are reproducible from the site image and the
// site record (§5.5).
//
// # Restoring
//
// Engine.Restore, of a snapshot of one site into a target site (the same
// site or another):
//
//  1. checks that the target exists and is in the same client group as
//     the snapshot's site (backup.ErrCrossGroup otherwise: a task reaches
//     one repository, with one key, so restores stay within a group), and
//     that the snapshot is one of the site's (backup.ErrSnapshotNotFound);
//  2. creates a transfer directory (restore-<target ID>-<short snapshot
//     ID>, so a retry replaces what a crashed attempt left);
//  3. extracts the dump into it in a task without wp-content (`restic
//     restore <snap>:/transfer --include /database.sql`), which changes
//     nothing on the site, so a snapshot without a dump fails first;
//  4. replaces the target's wp-content in a task that mounts it read-write
//     (`restic restore <snap>:/site/wp-content --target /site/wp-content
//     --delete`): afterwards it holds exactly the snapshot's files, owned
//     by the target site's UID, which the task runs as;
//  5. imports the dump into the target's database (sitedb.Manager.Import,
//     which drops and recreates it first);
//  6. removes the transfer directory, whether or not the restore worked.
//
// Every run converges on the snapshot's state, so a retried restore just
// restores again. Any restic error fails the restore, including files it
// couldn't write: the caller (the restore job in package sites) rolls the
// site back to its pre-restore snapshot. Restore changes nothing but
// wp-content and the database; maintenance mode, the site's URLs, its core
// version, and its caches are the caller's to handle.
//
// # Snapshot layout
//
// Restore (#55) and anyone reading an exported repository depend on this;
// change it only together with them. Every snapshot of a site holds
// exactly two paths, the same for every site, so sites deduplicate against
// each other and a snapshot restores into any site the same way:
//
//	/site/wp-content/        the site's wp-content (WPContentPath)
//	/transfer/database.sql   the database dump (DatabasePath): plain SQL
//	                         from mariadb-dump, without CREATE DATABASE or
//	                         USE and with DEFINER clauses stripped, so it
//	                         imports into any site's database
//
// and these tags ("key:value"; TagSite, TagWPCore, TagKind, TagBackup):
//
//	site:<site ID>              the site the snapshot is of
//	wp-core:<version>           the site's WordPress core version at backup
//	                            time; a restore needs at least this version
//	kind:<kind>                 manual, scheduled, pre_restore, or pre_update
//	                            (store.BackupKind)
//	backup:<backup record ID>   the platform's record of the backup
//	                            (store.BackupRecord), when there is one
//
// The snapshot's host name is site-<site ID> (Host). restic picks a
// backup's parent snapshot, which lets it skip unchanged files, and groups
// snapshots for `restic forget` retention policies by host and paths, so a
// per-site host makes both work per site without extra flags.
//
// # Idempotency
//
// A backup job step may run again after its result was lost (a crash, or a
// database write that failed). With backup.Options.BackupID set and
// backup.Options.Retry, which says that an earlier attempt ran, Backup first
// looks for a snapshot tagged backup:<ID> and returns it instead of taking
// another. A first attempt skips the look, which is a restic task of its own
// (#191): a backup of a group whose repository exists runs one restic
// command, its task, and TaskAccess does not open the repository either.
// The transfer directory's name comes from the same ID, so a retry replaces
// what a crashed attempt left behind.
//
// # Locking and concurrency
//
// `restic backup` takes a non-exclusive repository lock, so backups of
// different sites in one repository run at the same time (their jobs have
// different lock keys, site:<id>). Two backups of the same site must not run
// at once: they would share a transfer directory; the site's lock key
// prevents it. Prune and Check take restic's exclusive lock: their jobs
// use the lock key repo:<client group ID> (package backup/repomaint), which
// serializes them with each other; backups and restores meet them only at
// restic's own lock, which every command here waits for with --retry-lock
// (up to LockWait), so a backup that starts during a prune waits for it
// rather than failing, and the other way around. Prune and Check first run
// `restic unlock`, which removes only stale locks (those of restic
// processes that died, such as a killed backup task's), so a crash doesn't
// block them.
//
// # Retention and checks
//
// Prune applies Options.Retention with `restic forget --group-by host`: a
// site's snapshots share a host (Host), so each site keeps its own last n
// snapshots, days, and so on, whatever the snapshots' tags. It then runs
// `restic prune` even if nothing was forgotten, so a retry finishes what a
// failed attempt started. Check runs `restic check`, reading back
// Options.CheckReadDataPercent of the data, and reports damage as
// backup.ErrRepositoryDamaged. Both run in repository tasks
// (repository.Manager.Restic), outside the control plane (#178, #191).
//
// # Listing
//
// ListSnapshots asks the repository (`restic snapshots --tag site:<id>`,
// run in a repository task): the repository, not the state
// database, says what can be restored, since retention (#54) removes
// snapshots and a repository may have been exported and restored by hand.
// The platform's history of backups, failures included, is in the
// backup_records table (package backup/sitebackup), which the API lists
// without running restic. A site's snapshots stay listable after the site
// is deleted: its client group is then found from its backup records.
//
// # S3 repositories
//
// A task runs on the site's network, which is a plain bridge network, so
// it reaches the Internet (and so Amazon S3 and other public S3 services)
// like the site does; the control plane image it runs has CA certificates.
// Tasks for S3 repositories also join the platform's backup network
// (repository.TaskAccess.BackupNetwork, platform.BackupNetwork), which the
// control plane is on too, so an S3 service on this host attached to it is
// reached by the same container name from both. Tasks never join the
// platform network itself.
package restic
