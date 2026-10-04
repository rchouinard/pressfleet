-- Pending plugin and theme updates (#125, design doc §5.6, §9 Q2), written
-- by package pluginupdates. Keep in sync with
-- migrations/postgres/0029_site_update_reports.sql.
--
-- Each site's latest successful check: when it ran, how many plugins and
-- themes had an update available, and the updates themselves as a JSON
-- array of {kind, name, title, version, update_version}. The platform only
-- reports these; WordPress applies plugin and theme updates itself. A site
-- without a row hasn't been checked yet.
CREATE TABLE site_update_reports (
    site_id        TEXT NOT NULL PRIMARY KEY REFERENCES sites (id) ON DELETE CASCADE,
    checked_at     TIMESTAMP NOT NULL,
    plugin_updates INTEGER NOT NULL DEFAULT 0,
    theme_updates  INTEGER NOT NULL DEFAULT 0,
    updates        TEXT NOT NULL DEFAULT '[]'
);
