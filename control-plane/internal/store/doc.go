// Package store is the control plane state database. It runs on SQLite (the
// default, zero extra services) and PostgreSQL (larger installs, and required
// for the future Kubernetes target), per design doc §3 and §4.2.
//
// # Drivers
//
// SQLite uses modernc.org/sqlite, a pure-Go port, so the binary stays a
// static CGO_ENABLED=0 build. PostgreSQL uses github.com/jackc/pgx/v5 through
// its database/sql adapter, so the rest of the code sees one *sql.DB API.
//
// # Queries
//
// Queries are hand-written SQL in the portable subset both dialects share,
// with ? placeholders. [Querier] rewrites them to $1, $2, ... for PostgreSQL.
// A code generator such as sqlc would need two copies of every query and a
// generated package per dialect; the portable subset covers everything the
// control plane needs, and the tests run every query on both dialects.
//
// Rules for queries:
//   - Use ? placeholders only, and never the PostgreSQL ?-family JSON
//     operators (they would be rewritten).
//   - Use RETURNING, ON CONFLICT, and standard SQL; avoid dialect-specific
//     functions. Pass the current time as an argument instead of using
//     CURRENT_TIMESTAMP or now().
//   - Scan timestamps into time.Time (or sql.NullTime). Both drivers are
//     configured to return UTC, and [Querier] truncates time arguments to
//     microseconds in UTC, which is PostgreSQL's precision, so values round-trip
//     identically on both dialects.
//   - Map driver errors with [Querier] results through the sentinel errors
//     ([ErrNotFound], [ErrConflict], [ErrReference]); callers never see
//     driver-specific error types.
//
// Repositories are small types that wrap a [Querier], so they work on a *DB
// or inside a transaction ([DB.InTx]). [ClientGroups] is the pattern to copy.
//
// # Migrations
//
// Migrations are forward-only SQL files embedded in the binary (design doc
// §4.7), one directory per dialect:
//
//	migrations/sqlite/NNNN_description.sql
//	migrations/postgres/NNNN_description.sql
//
// To change the schema, add the next NNNN file to both directories; a test
// fails if the two directories disagree, and another compares the resulting
// tables and columns when PostgreSQL tests are enabled. Never edit a released
// migration. Each file runs in its own transaction together with its row in
// schema_migrations.
//
// The runner is a small in-house one instead of a library such as goose:
// PressFleet needs forward-only migrations, a pre-migration backup, and a
// refusal to run against a newer schema, and a library would bring down
// migrations and extra dependencies without covering the rest.
//
// Enumerations (roles, token scopes, statuses, job states) are validated in
// Go, not with CHECK constraints: SQLite cannot alter a CHECK constraint
// without rebuilding the table, which would make adding a value expensive.
//
// # Backups and restore
//
// [DB.Migrate] backs up the database before applying pending migrations.
// For SQLite this is VACUUM INTO a timestamped file, and [Restore] puts such
// a file back (used by upgrade rollback). SQLite databases are protected by a
// lock file: every [Open] holds a shared lock and [Restore] needs an
// exclusive one, so a database is never replaced under a running process.
//
// For PostgreSQL, backups are the operator's responsibility and [DB.Migrate]
// only logs a warning. Running pg_dump from the control plane would require
// shipping PostgreSQL client tools in the image, matched to the server's
// major version (pg_dump refuses to dump a newer server), and it would
// duplicate what PostgreSQL operators already have: managed snapshots,
// point-in-time recovery, or their own pg_dump schedule. For the same reason
// [Restore] supports SQLite only.
//
// Disaster recovery backups are different: [DB.Dump] and [Load] (dump.go)
// copy the whole database in a dialect-neutral format, for both drivers,
// which package backup/statebackup keeps in the state repository (#96).
package store
