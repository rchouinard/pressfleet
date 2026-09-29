-- Resumable uploads of large files (#111, design doc §5.7, §5.8), managed by
-- package uploads. Keep in sync with migrations/sqlite/0016_uploads.sql.
--
-- One row per upload of a large file (a site import's wp-content archive or
-- SQL dump, or a theme or plugin package to deploy). The content lives in
-- <data_dir>/uploads/<id>, never in the database and never in backups.
--
-- purpose is the job the upload is for: site_import or site_deploy. It
-- decides which permission the upload's operations need, and a job only
-- takes an upload made for it. client_group_id is the group the imported
-- site will join, or the deploy target's group; site_id is the deploy
-- target (NULL for imports). Both cascade: an upload for a deleted site or
-- group is useless, and the cleanup job removes files without a row.
-- created_by_user_id cascades too: only the user who created an upload can
-- use it.
--
-- size_bytes is the size declared when the upload was created and
-- received_bytes how much of it has arrived. A chunk is only counted once
-- all of it is on disk, so an interrupted chunk is sent again from
-- received_bytes. hash_state is the SHA-256 state after received_bytes
-- (NULL before the first chunk), so completing an upload doesn't read the
-- file again; sha256 is the checksum the client declared, in hex, or empty
-- until it declares one.
--
-- status is uploading, complete (checksum verified), or in_use (claimed by
-- job_id, whose end removes the upload). expires_at is when an upload that
-- isn't in use counts as abandoned and is removed; NULL while in use.
CREATE TABLE uploads (
    id                 TEXT NOT NULL PRIMARY KEY,
    purpose            TEXT NOT NULL,
    client_group_id    TEXT NOT NULL REFERENCES client_groups (id) ON DELETE CASCADE,
    site_id            TEXT REFERENCES sites (id) ON DELETE CASCADE,
    created_by_user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    filename           TEXT NOT NULL DEFAULT '',
    size_bytes         BIGINT NOT NULL,
    received_bytes     BIGINT NOT NULL DEFAULT 0,
    sha256             TEXT NOT NULL DEFAULT '',
    hash_state         BYTEA,
    status             TEXT NOT NULL,
    job_id             TEXT REFERENCES jobs (id) ON DELETE SET NULL,
    created_at         TIMESTAMPTZ NOT NULL,
    updated_at         TIMESTAMPTZ NOT NULL,
    expires_at         TIMESTAMPTZ
);
CREATE INDEX uploads_client_group_id ON uploads (client_group_id);
CREATE INDEX uploads_site_id ON uploads (site_id);
CREATE INDEX uploads_created_by_user_id ON uploads (created_by_user_id);
CREATE INDEX uploads_job_id ON uploads (job_id);
CREATE INDEX uploads_expires_at ON uploads (expires_at);
