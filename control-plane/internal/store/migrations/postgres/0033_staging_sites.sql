-- Per-site state of staging sites (#199). Keep in sync with
-- migrations/sqlite/0033_staging_sites.sql.
--
-- A staging site is a site with parent_site_id set (0001); these columns
-- are the state its workflows keep. mail_disabled is the outbound mail
-- switch (§5.11): a staging site starts with mail off, and the flag is
-- what renders the site's mail-disabling mu-plugin. staging_snapshot_id
-- names the production snapshot the site was last cloned or refreshed
-- from. The timestamps are NULL until the site has been refreshed or
-- pushed.
ALTER TABLE sites ADD COLUMN mail_disabled BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE sites ADD COLUMN staging_refreshed_at TIMESTAMPTZ;
ALTER TABLE sites ADD COLUMN staging_snapshot_id TEXT;
ALTER TABLE sites ADD COLUMN staging_pushed_at TIMESTAMPTZ;
