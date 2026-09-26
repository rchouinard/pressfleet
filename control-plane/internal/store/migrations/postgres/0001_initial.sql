-- Initial schema. Keep in sync with migrations/sqlite/0001_initial.sql.
--
-- IDs are lowercase ULIDs from internal/id. Timestamps are UTC. Enumerated
-- values (role, scope, status, state, result) are validated in Go rather than
-- with CHECK constraints; see the package documentation.

CREATE TABLE users (
    id            TEXT NOT NULL PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE, -- normalized to lowercase by the application
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL,        -- admin, operator, viewer (§4.6)
    disabled_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);

CREATE TABLE sessions (
    id           TEXT NOT NULL PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    source_ip    TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL,
    last_seen_at TIMESTAMPTZ NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL
);
CREATE INDEX sessions_user_id ON sessions (user_id);
CREATE INDEX sessions_expires_at ON sessions (expires_at);

CREATE TABLE api_tokens (
    id           TEXT NOT NULL PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    token_hash   TEXT NOT NULL UNIQUE,
    scope        TEXT NOT NULL, -- full, read-only (§4.6)
    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX api_tokens_user_id ON api_tokens (user_id);

-- Audit events deliberately have no foreign keys: the history must survive
-- the deletion of the users, tokens, and targets it refers to.
CREATE TABLE audit_events (
    id             TEXT NOT NULL PRIMARY KEY,
    occurred_at    TIMESTAMPTZ NOT NULL,
    actor_user_id  TEXT,
    actor_token_id TEXT,
    action         TEXT NOT NULL,
    target_type    TEXT NOT NULL DEFAULT '',
    target_id      TEXT NOT NULL DEFAULT '',
    result         TEXT NOT NULL, -- success, denied, failure
    source_ip      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_events_occurred_at ON audit_events (occurred_at);
CREATE INDEX audit_events_actor ON audit_events (actor_user_id, occurred_at);
CREATE INDEX audit_events_target ON audit_events (target_type, target_id, occurred_at);

CREATE TABLE client_groups (
    id                TEXT NOT NULL PRIMARY KEY,
    name              TEXT NOT NULL UNIQUE,
    notes             TEXT NOT NULL DEFAULT '',
    backup_repository TEXT NOT NULL DEFAULT '', -- empty until the repository is initialized
    created_at        TIMESTAMPTZ NOT NULL,
    updated_at        TIMESTAMPTZ NOT NULL
);

CREATE TABLE sites (
    id                    TEXT NOT NULL PRIMARY KEY,
    client_group_id       TEXT NOT NULL REFERENCES client_groups (id) ON DELETE RESTRICT,
    -- Reserved for staging clones (§4.2); a parent can't be deleted while it has children.
    parent_site_id        TEXT REFERENCES sites (id) ON DELETE RESTRICT,
    name                  TEXT NOT NULL,
    slug                  TEXT NOT NULL UNIQUE,
    status                TEXT NOT NULL,
    preview_domain        TEXT NOT NULL UNIQUE,
    primary_domain        TEXT UNIQUE, -- NULL until go-live
    php_version           TEXT NOT NULL,
    wp_core_version       TEXT NOT NULL,
    wp_core_pinned        BOOLEAN NOT NULL DEFAULT FALSE,
    enable_cache          BOOLEAN NOT NULL DEFAULT TRUE,
    cpu_limit             TEXT NOT NULL DEFAULT '', -- e.g. "1.0"; empty means the platform default
    memory_reservation_mb INTEGER NOT NULL DEFAULT 128,
    memory_limit_mb       INTEGER NOT NULL DEFAULT 512,
    created_at            TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL
);
CREATE INDEX sites_client_group_id ON sites (client_group_id);
CREATE INDEX sites_parent_site_id ON sites (parent_site_id);

CREATE TABLE site_aliases (
    domain     TEXT NOT NULL PRIMARY KEY,
    site_id    TEXT NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX site_aliases_site_id ON site_aliases (site_id);

-- The job target is not a foreign key: a job that deletes its target must
-- keep its history.
CREATE TABLE jobs (
    id                 TEXT NOT NULL PRIMARY KEY,
    type               TEXT NOT NULL,
    target_type        TEXT NOT NULL,
    target_id          TEXT NOT NULL,
    state              TEXT NOT NULL,
    params             TEXT NOT NULL DEFAULT '{}', -- JSON; never contains secrets
    error              TEXT NOT NULL DEFAULT '',
    attempts           INTEGER NOT NULL DEFAULT 0,
    created_by_user_id TEXT REFERENCES users (id) ON DELETE SET NULL,
    run_after          TIMESTAMPTZ NOT NULL,
    lease_owner        TEXT,
    lease_expires_at   TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL,
    started_at         TIMESTAMPTZ,
    finished_at        TIMESTAMPTZ,
    updated_at         TIMESTAMPTZ NOT NULL
);
CREATE INDEX jobs_state_run_after ON jobs (state, run_after);
CREATE INDEX jobs_target ON jobs (target_type, target_id, created_at);

CREATE TABLE job_steps (
    id          TEXT NOT NULL PRIMARY KEY,
    job_id      TEXT NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    seq         INTEGER NOT NULL,
    name        TEXT NOT NULL,
    state       TEXT NOT NULL,
    error       TEXT NOT NULL DEFAULT '',
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    UNIQUE (job_id, seq)
);

CREATE TABLE job_logs (
    id        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    job_id    TEXT NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    step_seq  INTEGER,
    logged_at TIMESTAMPTZ NOT NULL,
    level     TEXT NOT NULL,
    message   TEXT NOT NULL
);
CREATE INDEX job_logs_job_id ON job_logs (job_id, id);

-- Secrets are encrypted with the platform master key before they reach the
-- database (§6).
CREATE TABLE secrets (
    id         TEXT NOT NULL PRIMARY KEY,
    kind       TEXT NOT NULL,
    ciphertext BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);
