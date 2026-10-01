-- Alert channels and their deliveries (#76, design doc §7.3), written by
-- package alertchannels. Keep in sync with migrations/sqlite/0024_alert_channels.sql.
--
-- A channel is somewhere alert events go: type "email" or "webhook". config
-- is a JSON object of the settings that aren't secret (SMTP host and
-- recipients, the webhook format); secret_ref names the encrypted secret
-- (secrets table) holding the rest as JSON: the SMTP password, a webhook's
-- URL (Slack and Teams URLs are credentials) and signing secret. It is
-- NULL for a channel with no secrets, and a foreign key so the secret can't
-- be deleted while the channel exists.
--
-- Routing: a channel receives events of alerts at or above min_severity
-- (info, warning, or critical), and if rule_id or client_group_id is set,
-- only those of that rule or client group. They aren't foreign keys: a
-- filter on a rule or group that has been deleted matches nothing, which
-- is safer than widening the channel to every alert.
--
-- A delivery is one alert event (alert_id, event_type) bound for one
-- channel. Rows are created when the notifier accepts the event, and the
-- delivery worker retries them with backoff: state is "pending" until the
-- channel accepted it ("delivered") or attempts reached the cap
-- ("failed"). event is the JSON snapshot of the event as it was when it
-- was raised, so a retry sends what the first attempt would have.
-- Deleting the alert or the channel deletes its deliveries.
CREATE TABLE alert_channels (
    id              TEXT NOT NULL PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    type            TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    min_severity    TEXT NOT NULL DEFAULT 'info',
    rule_id         TEXT,
    client_group_id TEXT,
    config          TEXT NOT NULL DEFAULT '{}',
    secret_ref      TEXT UNIQUE REFERENCES secrets (id) ON DELETE RESTRICT,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL
);

CREATE TABLE alert_deliveries (
    id              TEXT NOT NULL PRIMARY KEY,
    alert_id        TEXT NOT NULL REFERENCES alerts (id) ON DELETE CASCADE,
    event_type      TEXT NOT NULL, -- firing, resolved
    channel_id      TEXT NOT NULL REFERENCES alert_channels (id) ON DELETE CASCADE,
    state           TEXT NOT NULL, -- pending, delivered, failed
    attempts        INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT NOT NULL DEFAULT '',
    next_attempt_at TIMESTAMPTZ NOT NULL,
    delivered_at    TIMESTAMPTZ,
    event           TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL,
    UNIQUE (alert_id, event_type, channel_id)
);
CREATE INDEX alert_deliveries_due ON alert_deliveries (state, next_attempt_at);
CREATE INDEX alert_deliveries_channel_id ON alert_deliveries (channel_id, id);
