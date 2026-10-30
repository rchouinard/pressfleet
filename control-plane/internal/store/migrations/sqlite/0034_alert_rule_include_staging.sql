-- Alert rules can opt in to staging sites (#205). Keep in sync with
-- migrations/postgres/0034_alert_rule_include_staging.sql.
--
-- Staging sites are disposable and often idle or stopped, so a site-scoped
-- rule skips them unless include_staging is set (§7.3). Existing rules,
-- the built-in ones included, keep skipping them.
ALTER TABLE alert_rules ADD COLUMN include_staging BOOLEAN NOT NULL DEFAULT FALSE;
