-- Dashboard two-factor authentication (#149, design doc §6.2). Keep in sync
-- with migrations/sqlite/0030_two_factor.sql.
--
-- user_totp is a user's authenticator app, at most one per user. The server
-- needs the plaintext secret to check codes, so it can't be hashed: it is an
-- encrypted secret (secrets table, kind "totp-secret"), ON DELETE RESTRICT
-- like other secret references, and the code that deletes a row deletes its
-- secret in the same transaction (store.UserTOTPs.Delete, store.Users.Delete).
-- confirmed_at is NULL until the user enters a valid code; unconfirmed rows
-- expire. last_used_step is the RFC 6238 time step of the last accepted code,
-- so a code from that step or earlier can't be replayed.
CREATE TABLE user_totp (
    user_id        TEXT NOT NULL PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    secret_id      TEXT NOT NULL UNIQUE REFERENCES secrets (id) ON DELETE RESTRICT,
    confirmed_at   TIMESTAMPTZ,
    last_used_step BIGINT NOT NULL DEFAULT 0,
    created_at     TIMESTAMPTZ NOT NULL
);

-- Security keys and passkeys, any number per user. credential_id is the
-- authenticator's raw credential ID and public_key its COSE key; algorithm is
-- the COSE algorithm identifier (-7 for ES256). transports is a JSON array of
-- the transports the authenticator reported. The flags are those of the
-- registration, with backup_state updated at each use (a credential can start
-- or stop syncing). rp_id is the relying party ID it was registered for, so
-- pressfleet doctor can warn when dashboard_domain changes.
CREATE TABLE webauthn_credentials (
    id              TEXT NOT NULL PRIMARY KEY,
    user_id         TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    credential_id   BYTEA NOT NULL UNIQUE,
    public_key      BYTEA NOT NULL,
    algorithm       INTEGER NOT NULL,
    sign_count      BIGINT NOT NULL DEFAULT 0,
    aaguid          BYTEA NOT NULL,
    transports      TEXT NOT NULL DEFAULT '[]',
    user_verified   BOOLEAN NOT NULL DEFAULT FALSE,
    backup_eligible BOOLEAN NOT NULL DEFAULT FALSE,
    backup_state    BOOLEAN NOT NULL DEFAULT FALSE,
    rp_id           TEXT NOT NULL,
    name            TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL,
    last_used_at    TIMESTAMPTZ
);
CREATE INDEX webauthn_credentials_user_id ON webauthn_credentials (user_id);

-- The random 64-byte WebAuthn user handle, generated at first registration,
-- so credentials don't carry the user ID and passwordless sign-in can find the
-- user by it. Unique through an index, as on SQLite.
ALTER TABLE users ADD COLUMN webauthn_user_handle BYTEA;
CREATE UNIQUE INDEX users_webauthn_user_handle ON users (webauthn_user_handle);

-- Single-use recovery codes, stored as SHA-256 hashes like session tokens.
-- used_at is set when a code is spent; regenerating replaces the whole set.
CREATE TABLE recovery_codes (
    id         TEXT NOT NULL PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,
    UNIQUE (user_id, code_hash)
);

-- When the session's user last proved who they are: at login, and on
-- reauthentication. Existing sessions were verified when they were created.
ALTER TABLE sessions ADD COLUMN verified_at TIMESTAMPTZ;
UPDATE sessions SET verified_at = created_at;
ALTER TABLE sessions ALTER COLUMN verified_at SET NOT NULL;
