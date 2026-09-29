package mariadb

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
)

// server is the MariaDB side of the manager. Both methods must be
// idempotent: the manager calls them again on every retry. Tests substitute
// an in-memory fake to exercise the state database side and crash points.
type server interface {
	// ensure makes the database and user exist, with the user's password set
	// to the given mysql_native_password hash, its connection limit set, and
	// all privileges on its own database.
	ensure(ctx context.Context, name siteName, passwordHash string, maxUserConnections int) error
	// drop removes the user and the database if they exist.
	drop(ctx context.Context, name siteName) error
	// empty drops the database if it exists, keeping the user and its
	// grants, so ensure can recreate it empty.
	empty(ctx context.Context, name siteName) error

	// ensureTemporary makes a temporary user exist with the given password
	// hash and connection limit, and with exactly the grants of access on
	// the site's database.
	ensureTemporary(ctx context.Context, user tempName, db siteName, passwordHash string, access sitedb.Access, maxUserConnections int) error
	// dropTemporary drops a temporary user if it exists and ends its
	// connections.
	dropTemporary(ctx context.Context, user tempName) error
	// listTemporary returns the names of every user whose name starts with
	// the temporary prefix.
	listTemporary(ctx context.Context) ([]string, error)
}

// Privileges of temporary users on the site's database, by access. A
// read-only user may read tables and views; EXECUTE is left out because a
// stored routine can write with its definer's privileges. A read-write
// user has what the site's own user has.
var tempPrivileges = map[sitedb.Access]string{
	sitedb.AccessReadOnly:  "SELECT, SHOW VIEW",
	sitedb.AccessReadWrite: "ALL PRIVILEGES",
}

// ensureTemporary converges like ensure. REVOKE first, so a retry with a
// different access leaves only the grants asked for.
func (s sqlServer) ensureTemporary(ctx context.Context, user tempName, db siteName, passwordHash string, access sitedb.Access, maxUserConnections int) error {
	if !validNativeHash(passwordHash) {
		return fmt.Errorf("ensure %s: invalid password hash", user)
	}
	privs, ok := tempPrivileges[access]
	if !ok {
		return fmt.Errorf("ensure %s: unknown access %q", user, access)
	}
	userOpts := fmt.Sprintf("IDENTIFIED BY PASSWORD '%s' WITH MAX_USER_CONNECTIONS %d", passwordHash, maxUserConnections)
	stmts := []struct{ what, sql string }{
		{"creating user", "CREATE USER IF NOT EXISTS " + user.account() + " " + userOpts},
		{"updating user", "ALTER USER " + user.account() + " " + userOpts},
		{"revoking privileges", "REVOKE ALL PRIVILEGES, GRANT OPTION FROM " + user.account()},
		{"granting privileges", "GRANT " + privs + " ON " + db.grantIdent() + ".* TO " + user.account()},
	}
	for _, st := range stmts {
		if _, err := s.db.ExecContext(ctx, st.sql); err != nil {
			return fmt.Errorf("%s %s: %w", st.what, user, err)
		}
	}
	return nil
}

// dropTemporary drops the user before ending its connections, so none can
// open in between. DROP USER alone leaves open connections running with
// the privileges they had.
func (s sqlServer) dropTemporary(ctx context.Context, user tempName) error {
	if _, err := s.db.ExecContext(ctx, "DROP USER IF EXISTS "+user.account()); err != nil {
		return fmt.Errorf("dropping user %s: %w", user, err)
	}
	if _, err := s.db.ExecContext(ctx, "KILL HARD USER "+user.quoted()); err != nil {
		return fmt.Errorf("ending connections of %s: %w", user, err)
	}
	return nil
}

// listTemporary reads mysql.user, which the admin user can read. In LIKE,
// "\_" is a literal underscore.
func (s sqlServer) listTemporary(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT User FROM mysql.user WHERE User LIKE 'pft\_%'`)
	if err != nil {
		return nil, fmt.Errorf("listing temporary users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, fmt.Errorf("listing temporary users: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing temporary users: %w", err)
	}
	return out, nil
}

// sqlServer runs the statements against a MariaDB server as the admin user.
type sqlServer struct {
	db *sql.DB
}

// ensure converges rather than checking first: every statement is
// idempotent, so the same sequence creates missing objects, repairs a
// database dropped behind PressFleet's back, resets a password that no
// longer matches the stored credentials, and does nothing harmful when
// everything already exists.
//
// Only validated identifiers (siteName), the hex password hash, and an
// integer are spliced into these statements; nothing else comes from input.
func (s sqlServer) ensure(ctx context.Context, name siteName, passwordHash string, maxUserConnections int) error {
	if !validNativeHash(passwordHash) {
		return fmt.Errorf("ensure %s: invalid password hash", name)
	}
	userOpts := fmt.Sprintf("IDENTIFIED BY PASSWORD '%s' WITH MAX_USER_CONNECTIONS %d", passwordHash, maxUserConnections)
	stmts := []struct{ what, sql string }{
		{"creating database", "CREATE DATABASE IF NOT EXISTS " + name.ident() +
			" CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"},
		{"creating user", "CREATE USER IF NOT EXISTS " + name.account() + " " + userOpts},
		// CREATE USER IF NOT EXISTS leaves an existing user alone; ALTER
		// brings its password and limit in line with the stored values.
		{"updating user", "ALTER USER " + name.account() + " " + userOpts},
		{"granting privileges", "GRANT ALL PRIVILEGES ON " + name.grantIdent() + ".* TO " + name.account()},
	}
	for _, st := range stmts {
		if _, err := s.db.ExecContext(ctx, st.sql); err != nil {
			return fmt.Errorf("%s %s: %w", st.what, name, err)
		}
	}
	return nil
}

// drop removes the user first, so no new connection can open while the
// database is being dropped.
func (s sqlServer) drop(ctx context.Context, name siteName) error {
	if _, err := s.db.ExecContext(ctx, "DROP USER IF EXISTS "+name.account()); err != nil {
		return fmt.Errorf("dropping user %s: %w", name, err)
	}
	if _, err := s.db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name.ident()); err != nil {
		return fmt.Errorf("dropping database %s: %w", name, err)
	}
	return nil
}

// empty drops only the database. Grants on a database are stored apart
// from it and survive the drop.
func (s sqlServer) empty(ctx context.Context, name siteName) error {
	if _, err := s.db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+name.ident()); err != nil {
		return fmt.Errorf("dropping database %s: %w", name, err)
	}
	return nil
}

// validNativeHash reports whether h has the form nativePasswordHash
// produces.
func validNativeHash(h string) bool {
	if len(h) != 41 || h[0] != '*' {
		return false
	}
	for _, c := range h[1:] {
		if (c < '0' || c > '9') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
