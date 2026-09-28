-- Per-client-group Restic repositories (#51, design doc §5.3 "Repository
-- Layout", §6), managed by package backup/repository. Keep in sync with
-- migrations/sqlite/0012_backup_repositories.sql.
--
-- A row records that a client group's repository password exists and where
-- the repository lives. It is written in the same transaction as the secret
-- holding the password, so the password is never orphaned, and the
-- repository is (re)initialized with it. client_groups.backup_repository
-- mirrors location for display; neither holds credentials.
--
-- backend is local or s3. location is the Restic repository as the control
-- plane opens it: an absolute path under the data directory, or an s3: URL.
-- Changing the configured backend later doesn't move existing repositories.
--
-- client_group_id is a foreign key: a client group can't be deleted while its
-- repository (and so its backups) still exists; offboarding exports or
-- deletes the repository first. secret_ref is a foreign key, so the password
-- can't be deleted while the row still points at it.
CREATE TABLE backup_repositories (
    client_group_id TEXT NOT NULL PRIMARY KEY REFERENCES client_groups (id) ON DELETE RESTRICT,
    backend         TEXT NOT NULL,
    location        TEXT NOT NULL UNIQUE,
    secret_ref      TEXT NOT NULL UNIQUE REFERENCES secrets (id) ON DELETE RESTRICT,
    created_at      TIMESTAMPTZ NOT NULL
);
