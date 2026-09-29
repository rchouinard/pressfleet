-- Temporary database sessions (#115, design doc §5.8), written by package
-- dbsession. Keep in sync with migrations/sqlite/0021_database_sessions.sql.
--
-- One row per session a user opened on a site's database. The row is
-- inserted (status active) before the session's temporary MariaDB user,
-- pft_<id>, is created, so every such user has a row. A session ends when
-- a user ends it (ended), when expires_at passes (expired), or when its
-- user couldn't be created (failed); ended_at says when. dropped_at is
-- when the MariaDB user was dropped and its connections ended: a session
-- that has ended but whose user isn't dropped yet is retried by the sweep.
-- The row stays as the history of sessions. The password is never stored.
--
-- site_id and user_id are not foreign keys, like database_exports: the
-- history outlives deleted sites and users. ended_by_user_id is who ended
-- the session through the API; empty when it expired or was revoked.
CREATE TABLE database_sessions (
    id               TEXT NOT NULL PRIMARY KEY,
    site_id          TEXT NOT NULL,
    user_id          TEXT NOT NULL,
    access           TEXT NOT NULL,
    status           TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL,
    expires_at       TIMESTAMPTZ NOT NULL,
    ended_at         TIMESTAMPTZ,
    ended_by_user_id TEXT,
    end_reason       TEXT NOT NULL DEFAULT '',
    dropped_at       TIMESTAMPTZ
);
CREATE INDEX database_sessions_site_id ON database_sessions (site_id, status);
CREATE INDEX database_sessions_expiry ON database_sessions (status, expires_at);
CREATE INDEX database_sessions_undropped ON database_sessions (dropped_at, status);
