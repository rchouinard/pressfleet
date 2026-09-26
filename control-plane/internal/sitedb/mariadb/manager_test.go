package mariadb

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// fakeUser is a user on the fake server.
type fakeUser struct {
	hash     string
	maxConns int
	grant    string // database the user has privileges on
}

// fakeServer is an in-memory MariaDB side.
type fakeServer struct {
	mu    sync.Mutex
	dbs   map[string]bool
	users map[string]fakeUser
	calls int
	// fail, if set, is consulted before each operation ("ensure", "drop").
	fail func(op string) error
}

func newFakeServer() *fakeServer {
	return &fakeServer{dbs: map[string]bool{}, users: map[string]fakeUser{}}
}

func (f *fakeServer) ensure(_ context.Context, name siteName, hash string, maxConns int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		if err := f.fail("ensure"); err != nil {
			return err
		}
	}
	f.dbs[name.String()] = true
	f.users[name.String()] = fakeUser{hash: hash, maxConns: maxConns, grant: name.String()}
	return nil
}

func (f *fakeServer) drop(_ context.Context, name siteName) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		if err := f.fail("drop"); err != nil {
			return err
		}
	}
	delete(f.users, name.String())
	delete(f.dbs, name.String())
	return nil
}

func (f *fakeServer) state(name string) (db bool, u fakeUser, userOK bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, userOK = f.users[name]
	return f.dbs[name], u, userOK
}

type fixture struct {
	m       *Manager
	srv     *fakeServer
	db      *store.DB
	secrets secrets.Store
}

func newFixture(t *testing.T, db *store.DB) fixture {
	t.Helper()
	srv := newFakeServer()
	st := secrets.NewStore(db, secretstest.Keyring(t, 1))
	return fixture{m: newManager(srv, db, st, 0, storetest.Logger()), srv: srv, db: db, secrets: st}
}

func (f fixture) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	// table is a constant from the test.
	if err := f.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// creds decodes the credentials behind ref.
func (f fixture) creds(t *testing.T, ref string) sitedb.Credentials {
	t.Helper()
	plain, err := f.secrets.Get(context.Background(), secrets.Ref(ref))
	if err != nil {
		t.Fatal(err)
	}
	c, err := sitedb.UnmarshalCredentials(plain)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// checkServer asserts the fake server has the site's database and a user
// whose password matches ref's credentials.
func (f fixture) checkServer(t *testing.T, ref string) {
	t.Helper()
	c := f.creds(t, ref)
	name := "pf_" + testSiteID
	if c.Database != name || c.Username != name {
		t.Errorf("credentials = %s/%s, want %s", c.Database, c.Username, name)
	}
	db, u, ok := f.srv.state(name)
	if !db || !ok {
		t.Fatalf("server objects: database %v, user %v", db, ok)
	}
	if u.hash != nativePasswordHash(c.Password) {
		t.Error("user password doesn't match stored credentials")
	}
	if u.maxConns != DefaultMaxUserConnections || u.grant != name {
		t.Errorf("user = max conns %d, grant %q", u.maxConns, u.grant)
	}
}

func TestCreateDatabaseIdempotent(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db)

		ref, err := f.m.CreateDatabase(ctx, testSiteID)
		if err != nil {
			t.Fatal(err)
		}
		f.checkServer(t, ref)

		again, err := f.m.CreateDatabase(ctx, testSiteID)
		if err != nil || again != ref {
			t.Fatalf("second CreateDatabase = %q, %v; want %q", again, err, ref)
		}
		f.checkServer(t, ref)
		if n := f.count(t, "secrets"); n != 1 {
			t.Errorf("secrets = %d, want 1", n)
		}

		// A MariaDB volume reset loses the objects but not the record; the
		// next create restores them with the stored password.
		f.srv.dbs, f.srv.users = map[string]bool{}, map[string]fakeUser{}
		again, err = f.m.CreateDatabase(ctx, testSiteID)
		if err != nil || again != ref {
			t.Fatalf("CreateDatabase after reset = %q, %v; want %q", again, err, ref)
		}
		f.checkServer(t, ref)
	})
}

// A crash or failure after the credentials are stored leaves a record the
// retry picks up, instead of an orphaned secret or a second password.
func TestCreateDatabaseRetryAfterServerFailure(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db)
		injected := errors.New("mariadb down")
		f.srv.fail = func(string) error { return injected }

		if _, err := f.m.CreateDatabase(ctx, testSiteID); !errors.Is(err, injected) {
			t.Fatalf("err = %v, want injected", err)
		}
		if n := f.count(t, "site_databases"); n != 1 {
			t.Fatalf("records after failed create = %d, want 1", n)
		}
		rec, err := getRecord(ctx, db, testSiteID)
		if err != nil {
			t.Fatal(err)
		}

		f.srv.fail = nil
		ref, err := f.m.CreateDatabase(ctx, testSiteID)
		if err != nil {
			t.Fatal(err)
		}
		if ref != string(rec.SecretRef) {
			t.Errorf("retry ref = %q, want the stored %q", ref, rec.SecretRef)
		}
		f.checkServer(t, ref)
		if n := f.count(t, "secrets"); n != 1 {
			t.Errorf("secrets = %d, want 1", n)
		}
	})
}

// MariaDB objects left without a record (say, the state database was
// restored from an older backup) are adopted with new credentials.
func TestCreateDatabaseAdoptsUnrecordedObjects(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db)
		name := "pf_" + testSiteID
		f.srv.dbs[name] = true
		f.srv.users[name] = fakeUser{hash: nativePasswordHash("old"), grant: name}

		ref, err := f.m.CreateDatabase(context.Background(), testSiteID)
		if err != nil {
			t.Fatal(err)
		}
		f.checkServer(t, ref)
	})
}

func TestCreateDatabaseConcurrent(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db)
		const n = 4
		refs := make([]string, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() { refs[i], errs[i] = f.m.CreateDatabase(context.Background(), testSiteID) })
		}
		wg.Wait()
		for i := range n {
			if errs[i] != nil {
				t.Fatalf("CreateDatabase %d: %v", i, errs[i])
			}
			if refs[i] != refs[0] {
				t.Errorf("refs differ: %q vs %q", refs[i], refs[0])
			}
		}
		if got := f.count(t, "secrets"); got != 1 {
			t.Errorf("secrets = %d, want 1", got)
		}
		f.checkServer(t, refs[0])
	})
}

func TestDropDatabase(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db)
		name := "pf_" + testSiteID

		// Dropping what never existed succeeds.
		if err := f.m.DropDatabase(ctx, testSiteID); err != nil {
			t.Fatalf("DropDatabase before create: %v", err)
		}

		ref, err := f.m.CreateDatabase(ctx, testSiteID)
		if err != nil {
			t.Fatal(err)
		}

		// A failed server drop keeps the record, so the retry still knows
		// the site had a database, and the stored password stays usable.
		injected := errors.New("mariadb down")
		f.srv.fail = func(op string) error {
			if op == "drop" {
				return injected
			}
			return nil
		}
		if err := f.m.DropDatabase(ctx, testSiteID); !errors.Is(err, injected) {
			t.Fatalf("err = %v, want injected", err)
		}
		if n := f.count(t, "site_databases"); n != 1 {
			t.Fatalf("records after failed drop = %d, want 1", n)
		}
		f.srv.fail = nil

		for i := range 2 {
			if err := f.m.DropDatabase(ctx, testSiteID); err != nil {
				t.Fatalf("DropDatabase %d: %v", i, err)
			}
		}
		if db, _, userOK := f.srv.state(name); db || userOK {
			t.Errorf("server objects remain: database %v, user %v", db, userOK)
		}
		for _, table := range []string{"site_databases", "secrets"} {
			if n := f.count(t, table); n != 0 {
				t.Errorf("%s rows = %d, want 0", table, n)
			}
		}
		if _, err := f.secrets.Get(ctx, secrets.Ref(ref)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("secret after drop: err = %v, want ErrNotFound", err)
		}

		// A new database after a drop gets fresh credentials.
		ref2, err := f.m.CreateDatabase(ctx, testSiteID)
		if err != nil || ref2 == ref {
			t.Errorf("CreateDatabase after drop = %q, %v; want a new ref", ref2, err)
		}
	})
}

func TestInvalidSiteIDTouchesNothing(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db)
		bad := "x`; DROP DATABASE mysql; --"
		if _, err := f.m.CreateDatabase(ctx, bad); !errors.Is(err, ErrInvalidSiteID) {
			t.Errorf("CreateDatabase err = %v, want ErrInvalidSiteID", err)
		}
		if err := f.m.DropDatabase(ctx, bad); !errors.Is(err, ErrInvalidSiteID) {
			t.Errorf("DropDatabase err = %v, want ErrInvalidSiteID", err)
		}
		if f.srv.calls != 0 || f.count(t, "site_databases") != 0 {
			t.Error("an invalid site ID reached the server or the state database")
		}
	})
}

func TestDumpImportUnsupported(t *testing.T) {
	m := newManager(newFakeServer(), nil, secrets.Store{}, 0, storetest.Logger())
	if err := m.Dump(context.Background(), testSiteID, "/tmp/x"); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Dump err = %v", err)
	}
	if err := m.Import(context.Background(), testSiteID, "/tmp/x"); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Import err = %v", err)
	}
}

func TestNewValidates(t *testing.T) {
	for _, cfg := range []Config{
		{User: "root", PasswordFile: "/pw"},
		{Addr: "mariadb:3306", PasswordFile: "/pw"},
		{Addr: "mariadb:3306", User: "root"},
		{Addr: "mariadb:3306", User: "root", PasswordFile: "/pw", MaxUserConnections: -1},
	} {
		if _, err := New(cfg, nil, secrets.Store{}, storetest.Logger()); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
	// New doesn't connect, so it succeeds with nothing listening.
	m, err := New(Config{Addr: "127.0.0.1:1", User: "root", PasswordFile: "/nonexistent"}, nil, secrets.Store{}, storetest.Logger())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ping(context.Background()); err == nil {
		t.Error("Ping succeeded with no server")
	}
	if err := m.Close(); err != nil {
		t.Error(err)
	}
}
