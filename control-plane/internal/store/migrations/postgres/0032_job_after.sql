-- The job a job waits for (#197): it isn't leased until that job is final.
-- Keep in sync with migrations/sqlite/0032_job_after.sql. Package
-- internal/jobs documents the semantics, including how waiting jobs and
-- lock keys interact.
--
-- Not a foreign key, like schedule_id: enqueuing checks that the awaited
-- job exists, and deleting a job's history shouldn't be blocked by another
-- job's. The index finds a job's dependents.
ALTER TABLE jobs ADD COLUMN after_job_id TEXT;
CREATE INDEX jobs_after_job_id ON jobs (after_job_id);
