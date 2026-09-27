-- Maintenance mode (#44, design doc §5.4). Keep in sync with
-- migrations/postgres/0010_site_maintenance.sql.
--
-- Whether the site's visitors get the platform's maintenance page. It is
-- the site's desired state, like its settings, so that re-rendering the site
-- (updates, the startup reconciliation) keeps it.
ALTER TABLE sites ADD COLUMN maintenance BOOLEAN NOT NULL DEFAULT FALSE;
