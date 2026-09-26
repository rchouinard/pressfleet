-- Per-site databases on the shared MariaDB server (§4.4, §6), managed by
-- package sitedb/mariadb. Keep in sync with
-- migrations/postgres/0006_site_databases.sql.
--
-- A row records that a site's database credentials exist; it is written in
-- the same transaction as the secret holding them, so a credentials secret is
-- never orphaned, and the MariaDB objects are (re)created from it.
--
-- site_id is not a foreign key to sites: provisioning may create the
-- database before the site row is committed, and deletion drops the database
-- in a job step that must not depend on the site row still existing.
-- secret_ref is a foreign key, so the credentials can't be deleted while the
-- row still points at them.
CREATE TABLE site_databases (
    site_id       TEXT NOT NULL PRIMARY KEY,
    database_name TEXT NOT NULL UNIQUE,
    username      TEXT NOT NULL UNIQUE,
    secret_ref    TEXT NOT NULL UNIQUE REFERENCES secrets (id) ON DELETE RESTRICT,
    created_at    TIMESTAMP NOT NULL
);
