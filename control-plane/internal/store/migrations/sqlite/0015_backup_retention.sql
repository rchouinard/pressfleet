-- Backup retention (#54, design doc §5.3 step 4). Keep in sync with
-- migrations/postgres/0015_backup_retention.sql.
--
-- expired_at is when a backup's snapshot was found to be gone from its
-- client group's repository, normally because the retention policy removed
-- it (restic forget); NULL while the snapshot can still be restored. The
-- record itself stays, as the history of the site's backups.
ALTER TABLE backup_records ADD COLUMN expired_at TIMESTAMP;
