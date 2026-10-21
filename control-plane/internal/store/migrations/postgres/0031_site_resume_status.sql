-- The status a site had before a job set it to updating (#187), written by
-- Sites.SetStatus. Keep in sync with
-- migrations/sqlite/0031_site_resume_status.sql.
--
-- An update job reads the site's status back from its containers when it
-- finishes, so the status before the update was never stored. Recovering
-- the state database on a new host (#96), where there are no containers,
-- needs it to bring a site that was stopped back stopped. Empty unless the
-- site went to updating from running or stopped.
ALTER TABLE sites ADD COLUMN resume_status TEXT NOT NULL DEFAULT '';
