-- Database exports (#114, design doc §5.8), written by package dbexport.
-- Keep in sync with migrations/postgres/0017_database_exports.sql.
--
-- One row per export of a site's database that a user asked for. The
-- export job inserts it (status running), dumps and compresses the
-- database into <data_dir>/exports/<id>.sql.gz, and marks it ready with the
-- file's size and when it expires, or failed with a message that holds no
-- secrets. Once expires_at has passed, the file is deleted and the row is
-- marked expired (expired_at); the row stays as the history of exports.
--
-- site_id is not a foreign key, like backup_records: the history outlives
-- a deleted site, whose exports can't be downloaded any more. job_id is
-- the export job.
CREATE TABLE database_exports (
    id          TEXT NOT NULL PRIMARY KEY,
    site_id     TEXT NOT NULL,
    status      TEXT NOT NULL,
    size_bytes  INTEGER,
    message     TEXT NOT NULL DEFAULT '',
    job_id      TEXT REFERENCES jobs (id) ON DELETE SET NULL,
    created_at  TIMESTAMP NOT NULL,
    ready_at    TIMESTAMP,
    expires_at  TIMESTAMP,
    expired_at  TIMESTAMP
);
CREATE INDEX database_exports_site_id ON database_exports (site_id, id);
CREATE INDEX database_exports_expiry ON database_exports (status, expires_at);
CREATE INDEX database_exports_job_id ON database_exports (job_id);
