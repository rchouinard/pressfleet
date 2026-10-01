-- Alert rules and alerts (#75, design doc §4.3 AlertRule / Alert, §7.3),
-- written by package alerts. Keep in sync with migrations/postgres/0023_alerts.sql.
--
-- An alert rule is a PromQL-style expression evaluated against
-- VictoriaMetrics on a schedule. kind says how the result becomes a
-- condition: "threshold" compares each returned sample's value with
-- threshold using operator (so a built-in rule's threshold can be edited
-- without touching the expression), and "expression" treats every returned
-- sample as true (the expression filters, as in "up == 0"). scope is
-- "platform" or "site"; a site-scoped rule may be limited to one client
-- group with client_group_id. for_seconds is how long the condition must
-- hold before a pending alert fires. labels and annotations are JSON
-- objects of strings; annotations hold the summary and description
-- templates. Rows with a builtin_key are the default rules seeded at
-- startup: they can be edited and disabled but not deleted, and seeding
-- never overwrites them once they exist.
--
-- An alert is one incident of one rule for one series (fingerprint is a
-- hash of the series' labels): pending while the condition has held for
-- less than the rule's duration, firing from then until the condition
-- stops, and resolved after that. A partial unique index allows only one
-- unresolved alert per rule and series, which is what deduplicates: a
-- condition that keeps holding updates its alert instead of creating
-- another. A rule that is deleted takes its alerts with it. site_id and
-- client_group_id are copies of the series' labels for filtering and are
-- not foreign keys, so the history outlives deleted sites and groups.
-- firing_notified_at and resolved_notified_at say when the notifier
-- accepted each event, so an event is sent exactly once, and one that
-- wasn't accepted before a restart is sent after it.
CREATE TABLE alert_rules (
    id              TEXT NOT NULL PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    description     TEXT NOT NULL DEFAULT '',
    scope           TEXT NOT NULL,
    client_group_id TEXT REFERENCES client_groups (id) ON DELETE CASCADE,
    kind            TEXT NOT NULL,
    expr            TEXT NOT NULL,
    operator        TEXT NOT NULL DEFAULT '',
    threshold       DOUBLE PRECISION NOT NULL DEFAULT 0,
    for_seconds     INTEGER NOT NULL DEFAULT 0,
    severity        TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    labels          TEXT NOT NULL DEFAULT '{}',
    annotations     TEXT NOT NULL DEFAULT '{}',
    builtin_key     TEXT UNIQUE, -- NULL for rules users created
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL
);
CREATE INDEX alert_rules_client_group_id ON alert_rules (client_group_id);

CREATE TABLE alerts (
    id                   TEXT NOT NULL PRIMARY KEY,
    rule_id              TEXT NOT NULL REFERENCES alert_rules (id) ON DELETE CASCADE,
    fingerprint          TEXT NOT NULL,
    labels               TEXT NOT NULL DEFAULT '{}',
    site_id              TEXT,
    client_group_id      TEXT,
    state                TEXT NOT NULL, -- pending, firing, resolved
    severity             TEXT NOT NULL,
    summary              TEXT NOT NULL DEFAULT '',
    description          TEXT NOT NULL DEFAULT '',
    value                DOUBLE PRECISION NOT NULL DEFAULT 0,
    started_at           TIMESTAMP NOT NULL,
    fired_at             TIMESTAMP,
    resolved_at          TIMESTAMP,
    updated_at           TIMESTAMP NOT NULL,
    firing_notified_at   TIMESTAMP,
    resolved_notified_at TIMESTAMP
);
CREATE UNIQUE INDEX alerts_unresolved ON alerts (rule_id, fingerprint) WHERE state <> 'resolved';
CREATE INDEX alerts_rule_id ON alerts (rule_id, id);
CREATE INDEX alerts_state ON alerts (state, id);
CREATE INDEX alerts_site_id ON alerts (site_id, id);
