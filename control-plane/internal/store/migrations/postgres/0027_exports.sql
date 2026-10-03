-- Site exports (#133, #134, design doc §5.7 "Export"), written by package
-- export. Keep in sync with migrations/sqlite/0027_exports.sql.
--
-- Database exports (0017) become one kind of export among several, all
-- with the same life: the export job inserts a row (status running),
-- writes the export's files into <data_dir>/exports/<id>.<file name>, and
-- marks it ready with the files and when they expire, or failed with a
-- message that holds no secrets. Once expires_at has passed, the files are
-- deleted and the row is marked expired (expired_at); the row stays as the
-- history of exports.
--
-- kind is what was exported:
--   database  a site's database (database.sql.gz), §5.8
--   site      a live site's wp-content and database (wp-content.tar.gz,
--             database.sql.gz), the two files site import takes
--   snapshot  the same two files, from a backup snapshot (backup_id,
--             snapshot_id); the site may have been deleted since
--
-- site_id is not a foreign key, like backup_records: the history outlives
-- a deleted site. site_slug names the downloads and client_group_id
-- authorizes them, so both are copied in rather than looked up. site_url,
-- table_prefix, wp_core_version, php_version, and taken_at describe what
-- was exported, for whoever imports it: the site's URL, its WordPress
-- table prefix, core and PHP versions (empty when unknown, such as the PHP
-- version of a snapshot), and when its content was read.
CREATE TABLE exports (
    id              TEXT NOT NULL PRIMARY KEY,
    kind            TEXT NOT NULL,
    site_id         TEXT NOT NULL,
    site_slug       TEXT NOT NULL DEFAULT '',
    client_group_id TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL,
    size_bytes      BIGINT,
    message         TEXT NOT NULL DEFAULT '',
    job_id          TEXT REFERENCES jobs (id) ON DELETE SET NULL,
    backup_id       TEXT NOT NULL DEFAULT '',
    snapshot_id     TEXT NOT NULL DEFAULT '',
    site_url        TEXT NOT NULL DEFAULT '',
    table_prefix    TEXT NOT NULL DEFAULT '',
    wp_core_version TEXT NOT NULL DEFAULT '',
    php_version     TEXT NOT NULL DEFAULT '',
    taken_at        TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL,
    ready_at        TIMESTAMPTZ,
    expires_at      TIMESTAMPTZ,
    expired_at      TIMESTAMPTZ
);
CREATE INDEX exports_site_id ON exports (site_id, id);
CREATE INDEX exports_client_group_id ON exports (client_group_id, id);
CREATE INDEX exports_expiry ON exports (status, expires_at);
CREATE INDEX exports_job_id ON exports (job_id);

-- The files of a ready export, by name (e.g. wp-content.tar.gz).
CREATE TABLE export_files (
    export_id  TEXT NOT NULL REFERENCES exports (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    PRIMARY KEY (export_id, name)
);

-- Database exports carry over. Their files were named <id>.sql.gz, which
-- no export file is any more: a ready one's download is gone, and the
-- cleanup job deletes the file when it expires, within the hour.
INSERT INTO exports (id, kind, site_id, site_slug, client_group_id, status, size_bytes, message, job_id,
                     created_at, ready_at, expires_at, expired_at)
SELECT e.id, 'database', e.site_id, COALESCE(s.slug, ''), COALESCE(s.client_group_id, ''), e.status, e.size_bytes,
       e.message, e.job_id, e.created_at, e.ready_at, e.expires_at, e.expired_at
FROM database_exports e LEFT JOIN sites s ON s.id = e.site_id;
DROP TABLE database_exports;

-- A deleted site's backups stay listed under its client group, so they
-- keep the site's name and slug from when they were taken. Backups taken
-- before this migration get them from the site, if it still exists.
ALTER TABLE backup_records ADD COLUMN site_name TEXT NOT NULL DEFAULT '';
ALTER TABLE backup_records ADD COLUMN site_slug TEXT NOT NULL DEFAULT '';
UPDATE backup_records SET
    site_name = COALESCE((SELECT name FROM sites WHERE sites.id = backup_records.site_id), ''),
    site_slug = COALESCE((SELECT slug FROM sites WHERE sites.id = backup_records.site_id), '');
CREATE INDEX backup_records_client_group_site ON backup_records (client_group_id, id);
