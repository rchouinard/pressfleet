-- Site backups (#52, design doc §4.3 BackupRecord, §5.3), written by package
-- backup/sitebackup. Keep in sync with
-- migrations/sqlite/0013_backup_records.sql.
--
-- One row per backup the platform took or tried to take of a site. The row
-- is inserted when the backup job starts working on it (status running) and
-- finished with the snapshot's ID and sizes (succeeded) or with an error
-- message that holds no secrets (failed). kind says what asked for it:
-- manual, scheduled, or pre_restore. The client group's Restic repository
-- is the source of truth for what can be restored; these rows are the
-- platform's history of backups, failures included, and what the API lists.
--
-- site_id is not a foreign key: a site's snapshots stay in its client
-- group's repository after the site is deleted (they can still be restored
-- into another site), so its records stay too. client_group_id cascades: a
-- group can only be deleted once its repository, and so every snapshot, is
-- gone (backup_repositories restricts it), and deleting the repository
-- deletes the group's records in the same transaction. job_id is the job
-- that took the backup.
--
-- snapshot_id, size_bytes, added_bytes, and file_count are NULL until the
-- backup succeeded. size_bytes is the size of the files backed up (what a
-- restore writes), added_bytes what the backup added to the repository
-- after deduplication and compression. created_at is when the job began
-- the backup, started_at when its latest attempt at the snapshot began.
CREATE TABLE backup_records (
    id              TEXT NOT NULL PRIMARY KEY,
    site_id         TEXT NOT NULL,
    client_group_id TEXT NOT NULL REFERENCES client_groups (id) ON DELETE CASCADE,
    kind            TEXT NOT NULL,
    status          TEXT NOT NULL,
    snapshot_id     TEXT,
    wp_core_version TEXT NOT NULL,
    size_bytes      BIGINT,
    added_bytes     BIGINT,
    file_count      BIGINT,
    message         TEXT NOT NULL DEFAULT '',
    job_id          TEXT REFERENCES jobs (id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    started_at      TIMESTAMPTZ NOT NULL,
    finished_at     TIMESTAMPTZ
);
CREATE INDEX backup_records_site_id ON backup_records (site_id, id);
CREATE INDEX backup_records_client_group_id ON backup_records (client_group_id);
CREATE INDEX backup_records_job_id ON backup_records (job_id);
