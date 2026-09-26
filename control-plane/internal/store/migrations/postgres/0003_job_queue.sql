-- Job queue and recurring schedules (#14, #15). Keep in sync with
-- migrations/sqlite/0003_job_queue.sql. Package internal/jobs documents
-- the semantics.

-- seq orders jobs for FIFO per lock key. It is assigned on insert.
ALTER TABLE jobs ADD COLUMN seq BIGINT NOT NULL DEFAULT 0;
-- Jobs with the same lock key run one at a time, in seq order.
ALTER TABLE jobs ADD COLUMN lock_key TEXT;
ALTER TABLE jobs ADD COLUMN max_attempts INTEGER NOT NULL DEFAULT 5;
-- State that steps pass forward to later steps. JSON; never contains secrets.
ALTER TABLE jobs ADD COLUMN data TEXT NOT NULL DEFAULT '{}';
-- Non-secret outputs for API clients. JSON; secrets are referenced by ID.
ALTER TABLE jobs ADD COLUMN result TEXT NOT NULL DEFAULT '{}';
-- The schedule that enqueued the job and the slot it ran for. Not a foreign
-- key: deleting a schedule keeps the history of its jobs.
ALTER TABLE jobs ADD COLUMN schedule_id TEXT;
ALTER TABLE jobs ADD COLUMN scheduled_for TIMESTAMPTZ;

CREATE INDEX jobs_lock_key ON jobs (lock_key, seq);
CREATE INDEX jobs_seq ON jobs (seq);
CREATE INDEX jobs_schedule_id ON jobs (schedule_id, state);

ALTER TABLE job_steps ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;

CREATE TABLE schedules (
    id                TEXT NOT NULL PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE,       -- e.g. wp-cron:<site-id>
    job_type          TEXT NOT NULL,
    target_type       TEXT NOT NULL,
    target_id         TEXT NOT NULL,
    payload           TEXT NOT NULL DEFAULT '{}', -- JSON copied into each job; never contains secrets
    lock_key          TEXT,
    max_attempts      INTEGER NOT NULL,
    interval_seconds  INTEGER NOT NULL,
    offset_seconds    INTEGER NOT NULL DEFAULT 0,
    stagger_seconds   INTEGER NOT NULL,
    missed_run_policy TEXT NOT NULL,              -- skip, coalesce, catch_up
    next_run_at       TIMESTAMPTZ NOT NULL,
    last_run_at       TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);
CREATE INDEX schedules_next_run_at ON schedules (next_run_at);
CREATE INDEX schedules_target ON schedules (target_type, target_id);
