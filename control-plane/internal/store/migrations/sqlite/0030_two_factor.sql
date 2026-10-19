-- Dashboard two-factor authentication (#149, design doc §6.2). Keep in sync
-- with migrations/postgres/0030_two_factor.sql.
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
    confirmed_at   TIMESTAMP,
    last_used_step INTEGER NOT NULL DEFAULT 0,
    created_at     TIMESTAMP NOT NULL
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
    credential_id   BLOB NOT NULL UNIQUE,
    public_key      BLOB NOT NULL,
    algorithm       INTEGER NOT NULL,
    sign_count      INTEGER NOT NULL DEFAULT 0,
    aaguid          BLOB NOT NULL,
    transports      TEXT NOT NULL DEFAULT '[]',
    user_verified   BOOLEAN NOT NULL DEFAULT FALSE,
    backup_eligible BOOLEAN NOT NULL DEFAULT FALSE,
    backup_state    BOOLEAN NOT NULL DEFAULT FALSE,
    rp_id           TEXT NOT NULL,
    name            TEXT NOT NULL,
    created_at      TIMESTAMP NOT NULL,
    last_used_at    TIMESTAMP
);
CREATE INDEX webauthn_credentials_user_id ON webauthn_credentials (user_id);

-- The random 64-byte WebAuthn user handle, generated at first registration,
-- so credentials don't carry the user ID and passwordless sign-in can find the
-- user by it. SQLite can't add a UNIQUE column, so the index enforces it.
ALTER TABLE users ADD COLUMN webauthn_user_handle BLOB;
CREATE UNIQUE INDEX users_webauthn_user_handle ON users (webauthn_user_handle);

-- Single-use recovery codes, stored as SHA-256 hashes like session tokens.
-- used_at is set when a code is spent; regenerating replaces the whole set.
CREATE TABLE recovery_codes (
    id         TEXT NOT NULL PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL,
    used_at    TIMESTAMP,
    created_at TIMESTAMP NOT NULL,
    UNIQUE (user_id, code_hash)
);

-- When the session's user last proved who they are: at login, and on
-- reauthentication. Existing sessions were verified when they were created.
ALTER TABLE sessions ADD COLUMN verified_at TIMESTAMP;
UPDATE sessions SET verified_at = created_at;

-- SQLite can't add a NOT NULL column without a default (see 0005), so these
-- triggers enforce it, with the message the real constraint would give.
CREATE TRIGGER sessions_verified_at_not_null_insert BEFORE INSERT ON sessions
WHEN NEW.verified_at IS NULL
BEGIN
    SELECT RAISE(ABORT, 'NOT NULL constraint failed: sessions.verified_at');
END;
CREATE TRIGGER sessions_verified_at_not_null_update BEFORE UPDATE OF verified_at ON sessions
WHEN NEW.verified_at IS NULL
BEGIN
    SELECT RAISE(ABORT, 'NOT NULL constraint failed: sessions.verified_at');
END;
