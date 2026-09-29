-- WordPress core update rollout (#57, design doc §5.5). Keep in sync with
-- migrations/sqlite/0019_core_updates.sql.
--
-- A site's or client group's core update policy (package coreupdate): a
-- hold keeps the site on its current major branch, which still gets its
-- minor and security releases (hold_since is when it was placed), and
-- approved_branch is the newest major branch ("7.2") it may move to, or ''
-- for none. A site without a row follows its group, and a group without one
-- the platform's defaults.
CREATE TABLE site_core_policies (
    site_id         TEXT NOT NULL PRIMARY KEY REFERENCES sites (id) ON DELETE CASCADE,
    hold            BOOLEAN NOT NULL DEFAULT FALSE,
    hold_since      TIMESTAMPTZ,
    approved_branch TEXT NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ NOT NULL
);

CREATE TABLE client_group_core_policies (
    client_group_id TEXT NOT NULL PRIMARY KEY REFERENCES client_groups (id) ON DELETE CASCADE,
    hold            BOOLEAN NOT NULL DEFAULT FALSE,
    hold_since      TIMESTAMPTZ,
    approved_branch TEXT NOT NULL DEFAULT '',
    updated_at      TIMESTAMPTZ NOT NULL
);

-- A rollout moves sites to new images one site at a time: "core" for
-- WordPress core releases. It counts consecutive failed site updates and
-- halts at a limit until an operator resumes it, or until the releases it
-- rolls out change (fingerprint).
CREATE TABLE rollouts (
    name                 TEXT NOT NULL PRIMARY KEY,
    fingerprint          TEXT NOT NULL DEFAULT '',
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    halted_at            TIMESTAMPTZ,
    halt_reason          TEXT NOT NULL DEFAULT '',
    updated_at           TIMESTAMPTZ NOT NULL
);

-- A site's last failed update in a rollout, to target (a version). The
-- rollout doesn't try that target on the site again; a newer one, or an
-- update started by hand, does.
CREATE TABLE rollout_failures (
    rollout   TEXT NOT NULL,
    site_id   TEXT NOT NULL REFERENCES sites (id) ON DELETE CASCADE,
    target    TEXT NOT NULL,
    message   TEXT NOT NULL DEFAULT '',
    failed_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (rollout, site_id)
);
CREATE INDEX rollout_failures_site_id ON rollout_failures (site_id);

-- wp_core_pinned (pinned to the exact core version) is replaced by holds on
-- the major branch, which keep getting security releases (decision #56).
INSERT INTO site_core_policies (site_id, hold, hold_since, updated_at)
    SELECT id, TRUE, updated_at, updated_at FROM sites WHERE wp_core_pinned;
ALTER TABLE sites DROP COLUMN wp_core_pinned;
