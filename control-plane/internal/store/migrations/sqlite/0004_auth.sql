-- Authentication (#13). Keep in sync with migrations/postgres/0004_auth.sql.

-- Shown to admins; NULL until the user first logs in.
ALTER TABLE users ADD COLUMN last_login_at TIMESTAMP;

-- The first characters of the token (e.g. "pft_3kq9"), so users can tell
-- tokens apart. The plaintext token is never stored.
ALTER TABLE api_tokens ADD COLUMN prefix TEXT NOT NULL DEFAULT '';
