package mariadb_test

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/mariadb"
)

// runTemporaryUserScenario checks database session users (§5.8) on a real
// server: a read-only one reads its site's database and can't change it, a
// read-write one can, neither reaches another site's database, and
// dropping one ends its open connections.
func runTemporaryUserScenario(ctx context.Context, t *testing.T, addr string, admin *sql.DB, m *mariadb.Manager, st secrets.Store) {
	siteA, siteB := id.New(), id.New()
	var site [2]sitedb.Credentials
	for i, siteID := range []string{siteA, siteB} {
		ref, err := m.CreateDatabase(ctx, siteID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.DropDatabase(context.Background(), siteID) })
		plain, err := st.Get(ctx, secrets.Ref(ref))
		if err != nil {
			t.Fatal(err)
		}
		if site[i], err = sitedb.UnmarshalCredentials(plain); err != nil {
			t.Fatal(err)
		}
	}
	a, b := site[0], site[1]
	mustExec := func(db *sql.DB, q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(connect(t, addr, a.Username, a.Password.Reveal(), a.Database), "CREATE TABLE posts (id INT PRIMARY KEY)")
	mustExec(connect(t, addr, a.Username, a.Password.Reveal(), a.Database), "INSERT INTO posts VALUES (1)")
	mustExec(connect(t, addr, b.Username, b.Password.Reveal(), b.Database), "CREATE TABLE secrets (v TEXT)")

	roID, rwID := id.New(), id.New()
	ro, err := m.CreateTemporaryUser(ctx, siteA, roID, sitedb.AccessReadOnly)
	if err != nil {
		t.Fatalf("CreateTemporaryUser read-only: %v", err)
	}
	rw, err := m.CreateTemporaryUser(ctx, siteA, rwID, sitedb.AccessReadWrite)
	if err != nil {
		t.Fatalf("CreateTemporaryUser read-write: %v", err)
	}
	if ro.Database != a.Database || ro.Username != "pft_"+roID {
		t.Errorf("read-only credentials = %s on %s", ro.Username, ro.Database)
	}

	roDB := connect(t, addr, ro.Username, ro.Password.Reveal(), ro.Database)
	var n int
	if err := roDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM posts").Scan(&n); err != nil || n != 1 {
		t.Errorf("read-only SELECT: %d, %v", n, err)
	}
	for _, q := range []string{
		"INSERT INTO posts VALUES (2)", "UPDATE posts SET id = 3", "DELETE FROM posts",
		"CREATE TABLE evil (x INT)", "DROP TABLE posts", "ALTER TABLE posts ADD COLUMN x INT",
	} {
		if _, err := roDB.ExecContext(ctx, q); !denied(err) {
			t.Errorf("read-only %q: err = %v, want access denied", q, err)
		}
	}
	rwDB := connect(t, addr, rw.Username, rw.Password.Reveal(), rw.Database)
	mustExec(rwDB, "INSERT INTO posts VALUES (2)")
	mustExec(rwDB, "CREATE TABLE scratch (x INT)")

	// Only the site's database.
	for _, db := range []*sql.DB{roDB, rwDB} {
		var v string
		if err := db.QueryRowContext(ctx, "SELECT v FROM `"+b.Database+"`.secrets").Scan(&v); !denied(err) {
			t.Errorf("reading B's table: err = %v, want access denied", err)
		}
		if got := queryStrings(ctx, t, db, "SHOW DATABASES"); strings.Join(got, ",") != "information_schema,"+a.Database {
			t.Errorf("sees databases %v", got)
		}
	}
	if err := connect(t, addr, ro.Username, ro.Password.Reveal(), b.Database).PingContext(ctx); mysqlErrNo(err) != errDBAccessDenied {
		t.Errorf("connecting to B's database: err = %v, want access denied", err)
	}

	got, err := m.ListTemporaryUsers(ctx)
	slices.Sort(got)
	want := []string{roID, rwID}
	slices.Sort(want)
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("ListTemporaryUsers = %v, %v; want %v", got, err, want)
	}

	// Dropping ends open connections, not only new ones.
	conn, err := roDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := m.DropTemporaryUser(ctx, roID); err != nil {
			t.Fatalf("DropTemporaryUser: %v", err)
		}
	}
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.QueryRowContext(qctx, "SELECT COUNT(*) FROM posts").Scan(&n); err == nil {
		t.Error("an open connection of a dropped user still works")
	}
	if err := connect(t, addr, ro.Username, ro.Password.Reveal(), ro.Database).PingContext(ctx); err == nil {
		t.Error("a dropped user can connect")
	}
	if err := admin.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.user WHERE User = ?", ro.Username).Scan(&n); err != nil || n != 0 {
		t.Errorf("users named %s after drop: %d, %v", ro.Username, n, err)
	}
	if err := m.DropTemporaryUser(ctx, rwID); err != nil {
		t.Fatal(err)
	}
	if got, err := m.ListTemporaryUsers(ctx); err != nil || len(got) != 0 {
		t.Errorf("ListTemporaryUsers after drops = %v, %v", got, err)
	}
}
