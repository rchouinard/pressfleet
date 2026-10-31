-- How old each part of a staging site is (#208). Keep in sync with
-- migrations/sqlite/0035_staging_copy_times.sql.
--
-- staging_refreshed_at says when a job ran, not how old the data is: a
-- code-only refresh leaves the database as old as the last time it was
-- copied. These are the times of the snapshots the database and the
-- uploads were last copied from, so a push can say what it would lose.
-- NULL means unknown: never copied, or restored from data of unknown age.
-- No backfill: no release has had staging sites.
ALTER TABLE sites ADD COLUMN staging_database_as_of TIMESTAMPTZ;
ALTER TABLE sites ADD COLUMN staging_uploads_as_of TIMESTAMPTZ;
