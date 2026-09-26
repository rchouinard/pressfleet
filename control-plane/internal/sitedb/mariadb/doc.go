// Package mariadb implements [sitedb.Manager] against the platform's shared
// MariaDB server (design doc §4.4, §5.1 step 3, §6).
//
// # Objects
//
// Each site gets a database and a user, both named pf_<site-id>. The user is
// 'pf_<site-id>'@'%' with a random password, ALL PRIVILEGES on its own
// database and nothing else: no global privileges and no GRANT OPTION. The
// database name in the GRANT has its _ escaped, because GRANT treats _ and %
// as wildcards.
//
// Each user also gets MAX_USER_CONNECTIONS (default
// [DefaultMaxUserConnections]) so one busy or misbehaving site can't take
// all of the server's max_connections (150; §4.1). With PHP-FPM in
// ondemand mode, a site holds at most one connection per PHP worker, and
// pm.max_children is single digits for the default 512 MiB memory limit
// (images/README.md), so the limit leaves room for WP-CLI and cron. It is
// not a reservation: idle sites hold no connections, so 50 sites share 150
// comfortably, while a runaway site stops at 20.
//
// # SQL safety
//
// No input is interpolated into SQL unchecked. Site IDs are validated with
// [id.Valid] (26 lowercase base32 characters) before any identifier is built,
// and identifiers are quoted anyway (backticks for names, single quotes for
// the account). MariaDB can't bind a password as a parameter in CREATE USER
// or ALTER USER, so the manager sends IDENTIFIED BY PASSWORD with the
// mysql_native_password hash computed in Go: the plaintext never appears in
// SQL text, and the hash is 41 characters of [*0-9A-F].
//
// # State and idempotency
//
// The site_databases table in the state database records each site's
// database, user, and a reference to the secret holding its
// [sitedb.Credentials] (encoded with [sitedb.MarshalCredentials]). The row
// and the secret are written in one transaction and deleted in one
// transaction, so a credentials secret is never orphaned and a row never
// points at a missing secret (secret_ref is a foreign key).
//
// CreateDatabase:
//
//  1. If the site has no row, generate a password and store the secret and
//     the row in one transaction.
//  2. Read the credentials from the secret.
//  3. Converge MariaDB: CREATE DATABASE IF NOT EXISTS, CREATE USER IF NOT
//     EXISTS, ALTER USER (password and connection limit), GRANT. Every
//     statement is idempotent, so this also repairs a database or user that
//     was dropped behind PressFleet's back (e.g. a MariaDB volume reset) or a
//     password that drifted, using the stored password.
//  4. Return the secret reference.
//
// DropDatabase:
//
//  1. DROP USER IF EXISTS, then DROP DATABASE IF EXISTS. Names derive from
//     the site ID, so this works without a row.
//  2. Delete the row and the secret in one transaction, if there is a row.
//
// Crash and retry analysis (jobs retry failed steps, §5):
//
//   - Crash in CreateDatabase before step 1 commits: nothing exists, or the
//     transaction rolled back. A retry starts over.
//   - Crash after step 1, before or during step 3: the row and secret exist
//     and MariaDB objects are missing or partial. A retry finds the row and
//     converges MariaDB with the same password, returning the same ref.
//   - Crash in DropDatabase after step 1: MariaDB objects are gone and the
//     row remains. A retry's drops are no-ops and it deletes the row.
//     Dropping MariaDB first means a row always exists while MariaDB objects
//     might, so a later CreateDatabase reuses the stored password.
//   - A permanently failed provisioning job rolls back with DropDatabase,
//     which removes whatever part of the above had been created.
//   - MariaDB objects without a row (e.g. the state database restored from
//     an older backup) are adopted by CreateDatabase: it stores new
//     credentials and ALTER USER sets the new password, keeping the data.
//   - Two concurrent CreateDatabase calls for one site: the second insert
//     fails on the primary key, its transaction (secret included) rolls
//     back, and it uses the winner's credentials. Concurrent create and drop
//     for one site are not supported; jobs for a site run one at a time.
//
// # Admin connection
//
// The manager connects as the admin user (root by default) with the password
// read from a file, by default the platform's Compose secret at
// /run/secrets/mariadb_root_password. It connects lazily and re-reads the
// file for every new connection, so the control plane starts while MariaDB
// is down and picks up a changed password without a restart. Its pool is
// small, since admin connections count against max_connections too.
//
// Dump and Import are not implemented yet (#49).
package mariadb
