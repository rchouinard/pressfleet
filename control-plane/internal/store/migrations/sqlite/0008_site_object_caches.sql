-- Per-site users on the shared object cache (§5.1 step 4, §6), managed by
-- package objectcache/redis. Keep in sync with
-- migrations/postgres/0008_site_object_caches.sql.
--
-- A row records that a site's object cache credentials exist; it is written
-- in the same transaction as the secret holding them, so a credentials
-- secret is never orphaned, and the server's ACL user is (re)created from
-- it, including after the cache server restarts and forgets its users.
--
-- db_index is the site's logical database. The unique constraint settles
-- two sites picking the same free index at once. Indexes are reused after a
-- site is deleted; the manager flushes a database before a new user gets it.
--
-- site_id is not a foreign key to sites, for the same reasons as in
-- site_databases: provisioning and deletion steps must not depend on the
-- site row.
CREATE TABLE site_object_caches (
    site_id    TEXT NOT NULL PRIMARY KEY,
    username   TEXT NOT NULL UNIQUE,
    db_index   INTEGER NOT NULL UNIQUE CHECK (db_index > 0),
    secret_ref TEXT NOT NULL UNIQUE REFERENCES secrets (id) ON DELETE RESTRICT,
    created_at TIMESTAMP NOT NULL
);
