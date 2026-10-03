-- The platform's signing keys (#128, design doc §5.9), written by package
-- adminlogin. Keep in sync with migrations/sqlite/0026_platform_login_key.sql.
--
-- One row per named key; "login" is the Ed25519 key that signs one-click
-- WordPress login tokens. The private key is an encrypted secret (secrets
-- table, ON DELETE RESTRICT so it can't be deleted from under the key);
-- public_key is its base64 (standard alphabet) public half, which every
-- site's wp-config.php carries. The primary key makes first-start
-- generation safe when two control planes race. Rotation isn't supported.
CREATE TABLE platform_keys (
    name       TEXT NOT NULL PRIMARY KEY,
    public_key TEXT NOT NULL,
    secret_ref TEXT NOT NULL UNIQUE REFERENCES secrets (id) ON DELETE RESTRICT,
    created_at TIMESTAMPTZ NOT NULL
);
