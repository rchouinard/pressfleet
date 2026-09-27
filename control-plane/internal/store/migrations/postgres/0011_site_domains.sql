-- Go-live (#45, design doc §5.2). Keep in sync with
-- migrations/sqlite/0011_site_domains.sql.
--
-- The host names whose URLs going live still has to rewrite in the site's
-- WordPress database, space-separated: the preview domain and any earlier
-- primary domain. A go-live request adds to them, and its job clears them
-- once `wp search-replace` has rewritten every one, so the next go-live
-- finishes one that stopped half way.
ALTER TABLE sites ADD COLUMN url_rewrite_from TEXT NOT NULL DEFAULT '';
