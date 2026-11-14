-- When a repository's deletion started (#351). Keep in sync with
-- migrations/sqlite/0037_backup_repository_deleting.sql.
--
-- Package backup/repository sets deleting_at, in the transaction that
-- checks the group has no sites, before it deletes the repository's data,
-- and clears it if the deletion fails. While it is set, creating a site in
-- the group is refused (store.ErrRepositoryDeleting), so no site can be
-- created for a repository whose data is going. The control plane clears
-- any left set by a crash when it starts. NULL for every existing row.
ALTER TABLE backup_repositories ADD COLUMN deleting_at TIMESTAMPTZ;
