-- Display name on user profiles (#118). Keep in sync with
-- migrations/postgres/0025_user_display_name.sql.

-- Empty when the user hasn't set one; the UI then falls back to the email.
ALTER TABLE users ADD COLUMN display_name TEXT NOT NULL DEFAULT '';
