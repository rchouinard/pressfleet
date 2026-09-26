-- Site UIDs and WordPress salts (#21, #24). Keep in sync with
-- migrations/sqlite/0005_site_uid.sql.
--
-- Every site runs as its own UID (§6), which store.AllocateSiteUID takes
-- from provisioner.MinSiteUID-MaxSiteUID (20000-59999; 19999 below is the
-- counter's value before the first allocation). site_uid_counter holds the
-- last UID handed out. It only grows, so a deleted site's UID is never given
-- to a new site, which could otherwise inherit files the old site left
-- behind. Allocating is an UPDATE of its single row, which serializes
-- concurrent allocations: a row lock on PostgreSQL, the write lock on SQLite.
CREATE TABLE site_uid_counter (
    id       INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    last_uid INTEGER NOT NULL
);

-- No release has created sites yet, but development databases may have
-- some: give them UIDs in ID order (IDs are ULIDs, so creation order) and
-- start the counter after them, so the column can be NOT NULL.
ALTER TABLE sites ADD COLUMN uid INTEGER;
UPDATE sites SET uid = 19999 + (SELECT COUNT(*) FROM sites s WHERE s.id <= sites.id);
INSERT INTO site_uid_counter (id, last_uid) SELECT 1, 19999 + COUNT(*) FROM sites;
ALTER TABLE sites ALTER COLUMN uid SET NOT NULL;
CREATE UNIQUE INDEX sites_uid ON sites (uid);

-- Reference to the site's keys and salts in secrets (wpconfig.MarshalSalts);
-- empty until provisioning generates them.
ALTER TABLE sites ADD COLUMN wp_salts_secret_ref TEXT NOT NULL DEFAULT '';
