-- When a repository's stored password was first known to open it (#277).
-- Keep in sync with
-- migrations/sqlite/0036_backup_repository_password_verified.sql.
--
-- A state restore can drop a repository's record and password while its
-- files stay, and the next backup then stores a new password that doesn't
-- open them. password_verified_at tells such a repository from one in use:
-- package backup/repository sets it the first time the stored password
-- opens the repository or restic init creates it with that password. While
-- it is NULL, the repository is opened before it is used, and one the
-- password doesn't open is set aside (never deleted) for a new one. Once it
-- is set, a wrong password is an error for the operator, and backups run no
-- extra restic task.
--
-- Backfill: a group with a succeeded backup has a repository its stored
-- password has opened, since the backup and the password are recorded in
-- the same database, so it is marked and is never set aside. Every other
-- repository, including any already stuck after a state restore, is opened
-- once, which costs one restic task.
ALTER TABLE backup_repositories ADD COLUMN password_verified_at TIMESTAMPTZ;
UPDATE backup_repositories SET password_verified_at = created_at
WHERE EXISTS (
    SELECT 1 FROM backup_records
    WHERE backup_records.client_group_id = backup_repositories.client_group_id
      AND backup_records.status = 'succeeded'
);
