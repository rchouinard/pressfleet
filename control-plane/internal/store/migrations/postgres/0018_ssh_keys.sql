-- SSH keys and audit event details for the SFTP gateway (#62, design doc
-- §5.8). Keep in sync with migrations/sqlite/0018_ssh_keys.sql.
--
-- A platform user's SSH public keys. The gateway identifies the user by the
-- key alone (the SSH username names the site), so a key belongs to at most
-- one user: fingerprint is unique across users. public_key is the key in
-- authorized_keys format, without a comment. Deleting a key, or its user,
-- ends its access at once; audit events keep its ID and fingerprint.
CREATE TABLE ssh_keys (
    id           TEXT NOT NULL PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    public_key   TEXT NOT NULL,
    fingerprint  TEXT NOT NULL UNIQUE, -- SHA256:<base64>, as ssh-keygen -l prints it
    created_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ
);
CREATE INDEX ssh_keys_user_id ON ssh_keys (user_id);

-- Extra facts about an event, as a JSON object of strings: for SFTP
-- sessions the key fingerprint, the session ID, and the bytes transferred.
-- NULL for events without any. Never holds secrets.
ALTER TABLE audit_events ADD COLUMN details TEXT;
