-- Site import (#112, design doc §5.7). Keep in sync with
-- migrations/sqlite/0020_site_table_prefix.sql.
--
-- The WordPress table prefix of the site's database, which the control
-- plane renders into wp-config.php. Provisioned sites use WordPress's
-- default; an imported site keeps the prefix of its dump, because WordPress
-- also stores it in option names and user meta keys.
ALTER TABLE sites ADD COLUMN table_prefix TEXT NOT NULL DEFAULT 'wp_';
