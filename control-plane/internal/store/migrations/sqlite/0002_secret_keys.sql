-- Master key bookkeeping for encrypted secrets (§6). Keep in sync with
-- migrations/postgres/0002_secret_keys.sql.
--
-- master_keys records the platform master keys used with this installation,
-- identified by a key ID derived from the key. check_value lets the control
-- plane recognize a wrong key at startup instead of failing on the first
-- decryption. At most one key is current: the partial unique index stops two
-- control planes with different keys from both registering on first start.
-- Master key rotation will retire the current key and register a new one.
CREATE TABLE master_keys (
    id          TEXT NOT NULL PRIMARY KEY,
    check_value BLOB NOT NULL,
    is_current  BOOLEAN NOT NULL,
    created_at  TIMESTAMP NOT NULL
);
CREATE UNIQUE INDEX master_keys_current ON master_keys (is_current) WHERE is_current;

-- The master key each secret is encrypted with. Nothing has written secrets
-- before this migration, so the empty default never applies; SQLite needs a
-- default to add a NOT NULL column.
ALTER TABLE secrets ADD COLUMN key_id TEXT NOT NULL DEFAULT '';
CREATE INDEX secrets_key_id ON secrets (key_id);
