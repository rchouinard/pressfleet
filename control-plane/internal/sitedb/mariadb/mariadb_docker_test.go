package mariadb_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/mariadb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const testRootPassword = "test-root-pw"

// startMariaDB runs the pinned platform MariaDB image, published on a free
// loopback port, and returns its address. It is removed when the test ends.
func startMariaDB(t *testing.T) string {
	t.Helper()
	images, err := platform.DefaultImages("test")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, port, _ := net.SplitHostPort(addr)

	dir := t.TempDir()
	composeFile := fmt.Sprintf(`services:
  mariadb:
    image: %q
    environment:
      MARIADB_ROOT_PASSWORD: %q
    command: ["--max-connections=150", "--skip-name-resolve"]
    ports:
      - "127.0.0.1:%s:3306"
    healthcheck:
      test: ["CMD", "healthcheck.sh", "--connect", "--innodb_initialized"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 60s
      start_interval: 1s
`, images.MariaDB, testRootPassword, port)
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(composeFile), 0o600); err != nil {
		t.Fatal(err)
	}
	p := compose.Project{Name: fmt.Sprintf("pftest-sitedb-%d", time.Now().UnixNano()), Dir: dir}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := p.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
			t.Errorf("down: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := p.Up(ctx, compose.UpOptions{Wait: true, WaitTimeout: 3 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	return addr
}

// connect opens a pool for user on addr, optionally selecting a database.
func connect(t *testing.T, addr, user, password, dbName string) *sql.DB {
	t.Helper()
	mc := mysql.NewConfig()
	mc.Net, mc.Addr, mc.User, mc.Passwd, mc.DBName = "tcp", addr, user, password, dbName
	mc.Timeout = 5 * time.Second
	mc.Logger = &mysql.NopLogger{}
	c, err := mysql.NewConnector(mc)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// mysqlErrNo returns the MariaDB error number of err, or 0.
func mysqlErrNo(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

// Error numbers MariaDB uses for access denied to a database or table.
const (
	errDBAccessDenied    = 1044
	errTableAccessDenied = 1142
	errUserLimitReached  = 1226
)

func denied(err error) bool {
	n := mysqlErrNo(err)
	return n == errDBAccessDenied || n == errTableAccessDenied
}

// TestMariaDB runs the manager against the pinned platform MariaDB image,
// with the state database on each dialect.
func TestMariaDB(t *testing.T) {
	if os.Getenv("PRESSFLEET_DOCKER_TESTS") != "1" {
		t.Skip("set PRESSFLEET_DOCKER_TESTS=1 to run tests against Docker")
	}
	addr := startMariaDB(t)
	admin := connect(t, addr, "root", testRootPassword, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for {
		// The published port can lag behind the container's health.
		if err := admin.PingContext(ctx); err == nil {
			break
		} else if ctx.Err() != nil {
			t.Fatalf("MariaDB not reachable: %v", err)
		}
		time.Sleep(time.Second)
	}

	pwFile := filepath.Join(t.TempDir(), "mariadb_root_password")
	if err := os.WriteFile(pwFile, []byte(testRootPassword+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dumpProg, clientProg := clientPrograms()

	storetest.Run(t, func(t *testing.T, state *store.DB) {
		st := secrets.NewStore(state, secretstest.Keyring(t, 1))
		m, err := mariadb.New(mariadb.Config{
			Addr: addr, User: "root", PasswordFile: pwFile,
			DumpProgram: dumpProg, ClientProgram: clientProg,
		}, state, st, storetest.Logger())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.Close() })
		if err := m.Ping(ctx); err != nil {
			t.Fatalf("Ping: %v", err)
		}
		runMariaDBScenario(ctx, t, addr, admin, m, state, st)
		t.Run("DumpImport", func(t *testing.T) {
			for _, p := range []string{dumpProg, clientProg} {
				if _, err := exec.LookPath(p); err != nil {
					t.Skipf("%s isn't installed; install the MariaDB client or set %s to a directory with mariadb and mariadb-dump", p, envClientDir)
				}
			}
			runDumpImportScenario(ctx, t, addr, m, st)
		})
	})
}

// envClientDir names a directory holding the mariadb and mariadb-dump
// programs for the dump and import test. Without it they are looked up in
// PATH, and the test is skipped if they aren't there.
const envClientDir = "PRESSFLEET_TEST_MARIADB_CLIENT_DIR"

func clientPrograms() (dump, client string) {
	dir := os.Getenv(envClientDir)
	if dir == "" {
		return mariadb.DefaultDumpProgram, mariadb.DefaultClientProgram
	}
	return filepath.Join(dir, mariadb.DefaultDumpProgram), filepath.Join(dir, mariadb.DefaultClientProgram)
}

func runMariaDBScenario(ctx context.Context, t *testing.T, addr string, admin *sql.DB, m *mariadb.Manager, state *store.DB, st secrets.Store) {
	siteA, siteB := id.New(), id.New()
	refA, err := m.CreateDatabase(ctx, siteA)
	if err != nil {
		t.Fatalf("CreateDatabase A: %v", err)
	}
	refB, err := m.CreateDatabase(ctx, siteB)
	if err != nil {
		t.Fatalf("CreateDatabase B: %v", err)
	}
	t.Cleanup(func() { _ = m.DropDatabase(context.Background(), siteB) })

	creds := func(ref string) sitedb.Credentials {
		t.Helper()
		plain, err := st.Get(ctx, secrets.Ref(ref))
		if err != nil {
			t.Fatal(err)
		}
		c, err := sitedb.UnmarshalCredentials(plain)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := creds(refA), creds(refB)
	if a.Database != "pf_"+siteA || a.Username != "pf_"+siteA {
		t.Errorf("credentials A = %s/%s", a.Database, a.Username)
	}
	exec := func(db *sql.DB, q string) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	// Each site user can connect and work in its own database.
	dbA := connect(t, addr, a.Username, a.Password.Reveal(), a.Database)
	dbB := connect(t, addr, b.Username, b.Password.Reveal(), b.Database)
	exec(dbA, "CREATE TABLE posts (id INT PRIMARY KEY)")
	exec(dbA, "INSERT INTO posts VALUES (1)")
	exec(dbB, "CREATE TABLE secrets (v TEXT)")
	exec(dbB, "INSERT INTO secrets VALUES ('b-only')")

	// Site A can't reach site B's database.
	bTable := "`" + b.Database + "`.secrets"
	var v string
	if err := dbA.QueryRowContext(ctx, "SELECT v FROM "+bTable).Scan(&v); !denied(err) {
		t.Errorf("A reading B's table: err = %v, want access denied", err)
	}
	if _, err := dbA.ExecContext(ctx, "CREATE TABLE `"+b.Database+"`.evil (x INT)"); !denied(err) {
		t.Errorf("A creating a table in B's database: err = %v, want access denied", err)
	}
	if _, err := dbA.ExecContext(ctx, "INSERT INTO "+bTable+" VALUES ('from a')"); !denied(err) {
		t.Errorf("A writing B's table: err = %v, want access denied", err)
	}
	if _, err := dbA.ExecContext(ctx, "CREATE DATABASE `pf_other`"); !denied(err) {
		t.Errorf("A creating a database: err = %v, want access denied", err)
	}
	if err := connect(t, addr, a.Username, a.Password.Reveal(), b.Database).PingContext(ctx); mysqlErrNo(err) != errDBAccessDenied {
		t.Errorf("A connecting to B's database: err = %v, want access denied", err)
	}
	if got := queryStrings(ctx, t, dbA, "SHOW DATABASES"); strings.Join(got, ",") != "information_schema,"+a.Database {
		t.Errorf("A sees databases %v", got)
	}

	// A's grants: usage (no privileges) globally with the connection limit,
	// and all privileges on exactly its own database, with no GRANT OPTION.
	grants := queryStrings(ctx, t, dbA, "SHOW GRANTS")
	if len(grants) != 2 {
		t.Fatalf("A has %d grants, want 2: %q", len(grants), grants)
	}
	escaped := strings.ReplaceAll(a.Database, "_", `\_`)
	for _, g := range grants {
		switch {
		case strings.HasPrefix(g, "GRANT USAGE ON *.* TO `"+a.Username+"`@`%`"):
			if !strings.Contains(g, "WITH MAX_USER_CONNECTIONS 20") {
				t.Errorf("global grant lacks the connection limit: %s", g)
			}
		case g == "GRANT ALL PRIVILEGES ON `"+escaped+"`.* TO `"+a.Username+"`@`%`":
		default:
			t.Errorf("unexpected grant: %s", g)
		}
		if strings.Contains(g, "GRANT OPTION") {
			t.Errorf("grant includes GRANT OPTION: %s", g)
		}
	}
	var maxConns int
	if err := admin.QueryRowContext(ctx, "SELECT max_user_connections FROM mysql.user WHERE user = ? AND host = '%'", a.Username).Scan(&maxConns); err != nil || maxConns != mariadb.DefaultMaxUserConnections {
		t.Errorf("max_user_connections = %d, %v", maxConns, err)
	}

	// The connection limit is enforced.
	checkConnectionLimit(ctx, t, addr, b)

	// Create is idempotent: same reference, and the password still works.
	again, err := m.CreateDatabase(ctx, siteA)
	if err != nil || again != refA {
		t.Fatalf("second CreateDatabase A = %q, %v; want %q", again, err, refA)
	}
	var n int
	if err := connect(t, addr, a.Username, a.Password.Reveal(), a.Database).
		QueryRowContext(ctx, "SELECT COUNT(*) FROM posts").Scan(&n); err != nil || n != 1 {
		t.Errorf("A after second create: rows %d, %v", n, err)
	}

	// Objects dropped behind PressFleet's back come back with the stored
	// password.
	exec(admin, "DROP USER `"+a.Username+"`@`%`")
	exec(admin, "DROP DATABASE `"+a.Database+"`")
	again, err = m.CreateDatabase(ctx, siteA)
	if err != nil || again != refA {
		t.Fatalf("CreateDatabase A after external drop = %q, %v; want %q", again, err, refA)
	}
	exec(connect(t, addr, a.Username, a.Password.Reveal(), a.Database), "CREATE TABLE restored (id INT)")

	// Drop is idempotent and removes the user, database, record, and secret.
	for i := range 2 {
		if err := m.DropDatabase(ctx, siteA); err != nil {
			t.Fatalf("DropDatabase A (%d): %v", i, err)
		}
	}
	if err := admin.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.user WHERE user = ?", a.Username).Scan(&n); err != nil || n != 0 {
		t.Errorf("users named %s after drop: %d, %v", a.Username, n, err)
	}
	if err := admin.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = ?", a.Database).Scan(&n); err != nil || n != 0 {
		t.Errorf("databases named %s after drop: %d, %v", a.Database, n, err)
	}
	if err := state.QueryRowContext(ctx, "SELECT COUNT(*) FROM site_databases WHERE site_id = ?", siteA).Scan(&n); err != nil || n != 0 {
		t.Errorf("records for A after drop: %d, %v", n, err)
	}
	if _, err := st.Get(ctx, secrets.Ref(refA)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("A's secret after drop: err = %v, want ErrNotFound", err)
	}
	if err := connect(t, addr, a.Username, a.Password.Reveal(), "").PingContext(ctx); err == nil {
		t.Error("A can still connect after drop")
	}

	// B is untouched.
	if err := connect(t, addr, b.Username, b.Password.Reveal(), b.Database).
		QueryRowContext(ctx, "SELECT v FROM secrets").Scan(&v); err != nil || v != "b-only" {
		t.Errorf("B after dropping A: %q, %v", v, err)
	}
}

// checkConnectionLimit opens connections as c's user until MariaDB refuses
// one with "user has exceeded max_user_connections".
func checkConnectionLimit(ctx context.Context, t *testing.T, addr string, c sitedb.Credentials) {
	t.Helper()
	pool := connect(t, addr, c.Username, c.Password.Reveal(), c.Database)
	var conns []*sql.Conn
	defer func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}()
	for range mariadb.DefaultMaxUserConnections + 5 {
		conn, err := pool.Conn(ctx)
		if err == nil {
			err = conn.PingContext(ctx)
		}
		if err != nil {
			if mysqlErrNo(err) != errUserLimitReached {
				t.Fatalf("connection %d: err = %v, want max_user_connections error", len(conns)+1, err)
			}
			if len(conns) > mariadb.DefaultMaxUserConnections {
				t.Errorf("opened %d connections, limit is %d", len(conns), mariadb.DefaultMaxUserConnections)
			}
			return
		}
		conns = append(conns, conn)
	}
	t.Errorf("opened %d connections without hitting the limit of %d", len(conns), mariadb.DefaultMaxUserConnections)
}

func queryStrings(ctx context.Context, t *testing.T, db *sql.DB, q string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
