package redis

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const (
	testSiteID  = "01k5z3m8q9r7t6v5w4x3y2z1ab"
	otherSiteID = "01k5z3m8q9r7t6v5w4x3y2z1cd"
)

// fakeUser is a user on the fake server.
type fakeUser struct {
	db    int
	hash  string
	rules []string
}

// fakeServer is an in-memory cache server.
type fakeServer struct {
	mu       sync.Mutex
	accounts map[string]fakeUser
	data     map[int]int // database -> key count
	flushes  []int
	run      string
	calls    int
	// fail, if set, is consulted before each operation.
	fail func(op string) error
	// hook, if set, runs after each ensureUser, outside the lock.
	hook func(name string)
}

func newFakeServer() *fakeServer {
	return &fakeServer{accounts: map[string]fakeUser{}, data: map[int]int{}, run: "run-1"}
}

func (f *fakeServer) begin(op string) error {
	f.calls++
	if f.fail != nil {
		return f.fail(op)
	}
	return nil
}

func (f *fakeServer) ensureUser(_ context.Context, u siteUser, db int, hash string) error {
	f.mu.Lock()
	if err := f.begin("ensureUser"); err != nil {
		f.mu.Unlock()
		return err
	}
	if _, ok := f.accounts[u.name]; !ok {
		delete(f.data, db)
		f.flushes = append(f.flushes, db)
	}
	f.accounts[u.name] = fakeUser{db: db, hash: hash, rules: siteRules(u, db, hash)}
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook(u.name)
	}
	return nil
}

func (f *fakeServer) userDB(_ context.Context, name string) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("userDB"); err != nil {
		return 0, false, err
	}
	u, ok := f.accounts[name]
	return u.db, ok, nil
}

func (f *fakeServer) deleteUser(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("deleteUser"); err != nil {
		return err
	}
	delete(f.accounts, name)
	return nil
}

func (f *fakeServer) flushDB(_ context.Context, db int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("flushDB"); err != nil {
		return err
	}
	delete(f.data, db)
	f.flushes = append(f.flushes, db)
	return nil
}

func (f *fakeServer) users(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("users"); err != nil {
		return nil, err
	}
	names := []string{"default", "healthcheck", "pressfleet"}
	for n := range f.accounts {
		names = append(names, n)
	}
	slices.Sort(names)
	return names, nil
}

func (f *fakeServer) runID(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("runID"); err != nil {
		return "", err
	}
	return f.run, nil
}

// restart forgets every runtime user and all data, like a server restart.
func (f *fakeServer) restart(run string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts, f.data, f.run = map[string]fakeUser{}, map[int]int{}, run
}

func (f *fakeServer) user(name string) (fakeUser, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.accounts[name]
	return u, ok
}

type fixture struct {
	m       *Manager
	srv     *fakeServer
	db      *store.DB
	secrets secrets.Store
}

func newFixture(t *testing.T, db *store.DB, databases int) fixture {
	t.Helper()
	srv := newFakeServer()
	st := secrets.NewStore(db, secretstest.Keyring(t, 1))
	return fixture{m: newManager(srv, db, st, databases, storetest.Logger()), srv: srv, db: db, secrets: st}
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

func (f fixture) creds(t *testing.T, ref string) objectcache.Credentials {
	t.Helper()
	plain, err := f.secrets.Get(context.Background(), secrets.Ref(ref))
	if err != nil {
		t.Fatal(err)
	}
	c, err := objectcache.UnmarshalCredentials(plain)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// checkServer asserts the site's user exists on the fake server with the
// stored password and database, and returns the credentials.
func (f fixture) checkServer(t *testing.T, siteID, ref string) objectcache.Credentials {
	t.Helper()
	c := f.creds(t, ref)
	if c.Username != "pf_"+siteID || c.KeyPrefix != siteID+":" || c.Database <= 0 {
		t.Errorf("credentials = %+v", c)
	}
	u, ok := f.srv.user(c.Username)
	if !ok {
		t.Fatalf("user %s missing on the server", c.Username)
	}
	if u.hash != passwordHash(c.Password) || u.db != c.Database {
		t.Errorf("server user = db %d, hash match %v; want db %d", u.db, u.hash == passwordHash(c.Password), c.Database)
	}
	return c
}

func TestCreateUserIdempotent(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, 16)

		ref, err := f.m.CreateUser(ctx, testSiteID)
		if err != nil {
			t.Fatal(err)
		}
		c := f.checkServer(t, testSiteID, ref)
		if c.Database != 1 {
			t.Errorf("first site's database = %d, want 1", c.Database)
		}
		// A new user's database is flushed before the user gets it.
		if !slices.Equal(f.srv.flushes, []int{1}) {
			t.Errorf("flushes = %v", f.srv.flushes)
		}

		f.srv.data[1] = 5 // the site caches something
		again, err := f.m.CreateUser(ctx, testSiteID)
		if err != nil || again != ref {
			t.Fatalf("second CreateUser = %q, %v; want %q", again, err, ref)
		}
		f.checkServer(t, testSiteID, ref)
		// Converging an existing user keeps its data.
		if f.srv.data[1] != 5 || len(f.srv.flushes) != 1 {
			t.Errorf("existing user's database was flushed: %v", f.srv.flushes)
		}
		if n := f.count(t, "secrets"); n != 1 {
			t.Errorf("secrets = %d, want 1", n)
		}

		// A server restart loses the user but not the record; the next
		// create restores it with the stored password and database.
		f.srv.restart("run-2")
		again, err = f.m.CreateUser(ctx, testSiteID)
		if err != nil || again != ref {
			t.Fatalf("CreateUser after restart = %q, %v; want %q", again, err, ref)
		}
		if f.checkServer(t, testSiteID, ref) != c {
			t.Error("credentials changed across a restart")
		}
	})
}

func TestCreateUserAllocatesDatabases(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, 4) // databases 1-3 for sites
		ids := []string{
			"01k5z3m8q9r7t6v5w4x3y2z1a1", "01k5z3m8q9r7t6v5w4x3y2z1a2",
			"01k5z3m8q9r7t6v5w4x3y2z1a3", "01k5z3m8q9r7t6v5w4x3y2z1a4",
		}
		dbOf := func(ref string) int { return f.creds(t, ref).Database }
		for i, id := range ids[:3] {
			ref, err := f.m.CreateUser(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got := dbOf(ref); got != i+1 {
				t.Errorf("site %d database = %d, want %d", i, got, i+1)
			}
		}
		// All taken.
		if _, err := f.m.CreateUser(ctx, ids[3]); !errors.Is(err, ErrNoFreeDatabase) {
			t.Fatalf("err = %v, want ErrNoFreeDatabase", err)
		}
		if n := f.count(t, "secrets"); n != 3 {
			t.Errorf("secrets after refused create = %d, want 3", n)
		}
		// A deleted site's database is reused, lowest first, and flushed
		// before the new user gets it.
		if err := f.m.DeleteUser(ctx, ids[1]); err != nil {
			t.Fatal(err)
		}
		f.srv.data[2] = 9 // say the delete's flush didn't happen
		ref, err := f.m.CreateUser(ctx, ids[3])
		if err != nil {
			t.Fatal(err)
		}
		if got := dbOf(ref); got != 2 {
			t.Errorf("reused database = %d, want 2", got)
		}
		if _, ok := f.srv.data[2]; ok {
			t.Error("reused database not flushed")
		}
	})
}

func TestLowestFree(t *testing.T) {
	tests := []struct {
		used      []int
		databases int
		want      int
	}{
		{nil, 16, 1},
		{[]int{1, 2, 3}, 16, 4},
		{[]int{1, 3}, 16, 2},
		{[]int{2, 3}, 16, 1},
		{[]int{1, 2, 3}, 4, 0},
		{nil, 1, 0},
	}
	for _, tt := range tests {
		if got := lowestFree(tt.used, tt.databases); got != tt.want {
			t.Errorf("lowestFree(%v, %d) = %d, want %d", tt.used, tt.databases, got, tt.want)
		}
	}
}

// A crash or failure after the credentials are stored leaves a record the
// retry picks up, instead of an orphaned secret or a second password.
func TestCreateUserRetryAfterServerFailure(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, 16)
		injected := errors.New("cache down")
		f.srv.fail = func(string) error { return injected }
		if _, err := f.m.CreateUser(ctx, testSiteID); !errors.Is(err, injected) {
			t.Fatalf("err = %v, want injected", err)
		}
		rec, err := getRecord(ctx, db, testSiteID)
		if err != nil {
			t.Fatalf("record after failed create: %v", err)
		}
		f.srv.fail = nil
		ref, err := f.m.CreateUser(ctx, testSiteID)
		if err != nil {
			t.Fatal(err)
		}
		if ref != string(rec.SecretRef) {
			t.Errorf("retry ref = %q, want the stored %q", ref, rec.SecretRef)
		}
		f.checkServer(t, testSiteID, ref)
		if n := f.count(t, "secrets"); n != 1 {
			t.Errorf("secrets = %d, want 1", n)
		}
	})
}

func TestCreateUserConcurrent(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db, 16)
		const n = 4
		refs := make([]string, 2*n)
		errs := make([]error, 2*n)
		var wg sync.WaitGroup
		for i := range 2 * n {
			id := testSiteID
			if i%2 == 1 {
				id = otherSiteID
			}
			wg.Go(func() { refs[i], errs[i] = f.m.CreateUser(context.Background(), id) })
		}
		wg.Wait()
		for i := range 2 * n {
			if errs[i] != nil {
				t.Fatalf("CreateUser %d: %v", i, errs[i])
			}
			if refs[i] != refs[i%2] {
				t.Errorf("refs for one site differ: %q vs %q", refs[i], refs[i%2])
			}
		}
		if got := f.count(t, "secrets"); got != 2 {
			t.Errorf("secrets = %d, want 2", got)
		}
		a, b := f.checkServer(t, testSiteID, refs[0]), f.checkServer(t, otherSiteID, refs[1])
		if a.Database == b.Database {
			t.Errorf("both sites got database %d", a.Database)
		}
	})
}

func TestDeleteUser(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, 16)
		name := "pf_" + testSiteID

		// Deleting what never existed succeeds.
		if err := f.m.DeleteUser(ctx, testSiteID); err != nil {
			t.Fatalf("DeleteUser before create: %v", err)
		}
		ref, err := f.m.CreateUser(ctx, testSiteID)
		if err != nil {
			t.Fatal(err)
		}
		f.srv.data[1] = 3
		f.srv.flushes = nil

		// The server is down: the record and secret are gone already, the
		// user and data remain.
		injected := errors.New("cache down")
		f.srv.fail = func(op string) error {
			if op == "deleteUser" {
				return injected
			}
			return nil
		}
		if err := f.m.DeleteUser(ctx, testSiteID); !errors.Is(err, injected) {
			t.Fatalf("err = %v, want injected", err)
		}
		if n := f.count(t, "site_object_caches"); n != 0 {
			t.Fatalf("records after failed delete = %d, want 0", n)
		}
		f.srv.fail = nil

		// The retry finds the database from the user on the server.
		for i := range 2 {
			if err := f.m.DeleteUser(ctx, testSiteID); err != nil {
				t.Fatalf("DeleteUser %d: %v", i, err)
			}
		}
		if _, ok := f.srv.user(name); ok {
			t.Error("user remains")
		}
		if _, ok := f.srv.data[1]; ok || !slices.Contains(f.srv.flushes, 1) {
			t.Errorf("database 1 not flushed: flushes %v", f.srv.flushes)
		}
		for _, table := range []string{"site_object_caches", "secrets"} {
			if n := f.count(t, table); n != 0 {
				t.Errorf("%s rows = %d, want 0", table, n)
			}
		}
		if _, err := f.secrets.Get(ctx, secrets.Ref(ref)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("secret after delete: err = %v, want ErrNotFound", err)
		}
	})
}

func TestFlushCache(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, 16)

		// A site without a user has nothing to flush.
		if err := f.m.FlushCache(ctx, testSiteID); err != nil {
			t.Fatalf("FlushCache before create: %v", err)
		}
		if f.srv.calls != 0 {
			t.Errorf("server calls = %d, want none", f.srv.calls)
		}
		ref, err := f.m.CreateUser(ctx, testSiteID)
		if err != nil {
			t.Fatal(err)
		}
		c := f.checkServer(t, testSiteID, ref)
		f.srv.data[c.Database] = 3
		f.srv.data[c.Database+1] = 5 // another site's
		f.srv.flushes = nil
		if err := f.m.FlushCache(ctx, testSiteID); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.srv.data[c.Database]; ok || !slices.Equal(f.srv.flushes, []int{c.Database}) {
			t.Errorf("flushes = %v, data %v; want database %d flushed", f.srv.flushes, f.srv.data, c.Database)
		}
		if f.srv.data[c.Database+1] != 5 {
			t.Error("another database was flushed")
		}
		// The user is untouched.
		f.checkServer(t, testSiteID, ref)

		injected := errors.New("cache down")
		f.srv.fail = func(op string) error {
			if op == "flushDB" {
				return injected
			}
			return nil
		}
		if err := f.m.FlushCache(ctx, testSiteID); !errors.Is(err, injected) {
			t.Errorf("err = %v, want injected", err)
		}
	})
}

func TestReconcile(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, 16)
		refA, err := f.m.CreateUser(ctx, testSiteID)
		if err != nil {
			t.Fatal(err)
		}
		refB, err := f.m.CreateUser(ctx, otherSiteID)
		if err != nil {
			t.Fatal(err)
		}
		f.srv.restart("run-2")
		// A site user without a record, e.g. left by a failed deletion.
		orphan, _ := newSiteUser("01k5z3m8q9r7t6v5w4x3y2z1zz")
		f.srv.accounts[orphan.name] = fakeUser{db: 9}

		if err := f.m.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		f.checkServer(t, testSiteID, refA)
		f.checkServer(t, otherSiteID, refB)
		if _, ok := f.srv.user(orphan.name); ok {
			t.Error("orphaned user not deleted")
		}
		names, _ := f.srv.users(ctx)
		if !slices.Contains(names, "pressfleet") || !slices.Contains(names, "healthcheck") {
			t.Errorf("non-site users touched: %v", names)
		}
	})
}

// A deletion that runs while Reconcile recreates users doesn't leave the
// deleted site's user behind.
func TestReconcileRacingDelete(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, 16)
		if _, err := f.m.CreateUser(ctx, testSiteID); err != nil {
			t.Fatal(err)
		}
		name := "pf_" + testSiteID
		// The deletion completes right after Reconcile has read the record
		// and just before it recreates the user.
		var once sync.Once
		f.srv.hook = func(n string) {
			if n != name {
				return
			}
			once.Do(func() {
				f.srv.hook = nil
				if err := f.m.DeleteUser(ctx, testSiteID); err != nil {
					t.Errorf("DeleteUser: %v", err)
				}
				// Reconcile's ensureUser for the old record lands after it.
				f.srv.mu.Lock()
				f.srv.accounts[name] = fakeUser{db: 1}
				f.srv.mu.Unlock()
			})
		}
		if err := f.m.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.srv.user(name); ok {
			t.Error("deleted site's user was left on the server")
		}
	})
}

func TestWatchReconcilesOnRestart(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newFixture(t, db, 16)
		f.m.pollInterval = 5 * time.Millisecond
		f.m.resyncInterval = time.Hour
		ref, err := f.m.CreateUser(context.Background(), testSiteID)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Each reconcile ensures the one site's user; count those calls.
		// Not the users listing that starts a reconcile: the user is back
		// only once ensureUser ran, after the state database was read, so
		// checkServer below would race it. ensureUser holds the fake's lock
		// while it creates the user, and checkServer takes the lock.
		thens := make(chan struct{}, 10)
		f.srv.mu.Lock()
		f.srv.fail = func(op string) error {
			if op == "ensureUser" {
				thens <- struct{}{}
			}
			return nil
		}
		f.srv.mu.Unlock()
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.m.Watch(ctx)
		}()
		waitFor := func(what string) {
			t.Helper()
			select {
			case <-thens:
			case <-time.After(10 * time.Second):
				t.Fatalf("no reconcile %s", what)
			}
		}
		waitFor("at start")

		// Unreachable, then back after a restart.
		f.srv.mu.Lock()
		counting := f.srv.fail
		f.srv.fail = func(string) error { return errors.New("down") }
		f.srv.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		f.srv.restart("run-" + strconv.Itoa(2))
		f.srv.mu.Lock()
		f.srv.fail = counting
		f.srv.mu.Unlock()
		waitFor("after the restart")
		f.checkServer(t, testSiteID, ref)

		// Nothing changed: no further reconcile.
		select {
		case <-thens:
			t.Error("reconciled without a restart")
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
		<-done
	})
}

func TestInvalidSiteIDTouchesNothing(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db, 16)
		bad := "x y ~* +@all"
		if _, err := f.m.CreateUser(ctx, bad); !errors.Is(err, ErrInvalidSiteID) {
			t.Errorf("CreateUser err = %v, want ErrInvalidSiteID", err)
		}
		if err := f.m.DeleteUser(ctx, bad); !errors.Is(err, ErrInvalidSiteID) {
			t.Errorf("DeleteUser err = %v, want ErrInvalidSiteID", err)
		}
		if err := f.m.FlushCache(ctx, bad); !errors.Is(err, ErrInvalidSiteID) {
			t.Errorf("FlushCache err = %v, want ErrInvalidSiteID", err)
		}
		if f.srv.calls != 0 || f.count(t, "site_object_caches") != 0 {
			t.Error("an invalid site ID reached the server or the state database")
		}
	})
}

func TestSiteRules(t *testing.T) {
	u, err := newSiteUser(testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	hash := passwordHash("pw")
	rules := siteRules(u, 7, hash)
	for _, want := range []string{"#" + hash, "~" + testSiteID + ":*", "db=7", "-@dangerous", "+info", "+flushdb"} {
		if !slices.Contains(rules, want) {
			t.Errorf("rules lack %q: %v", want, rules)
		}
	}
	if rules[0] != "reset" {
		t.Errorf("rules must start with reset: %v", rules)
	}
	// Only the hash goes to the server, never the password.
	if slices.Contains(rules, ">pw") || !validHash(hash) || validHash("pw") {
		t.Error("password handling")
	}
	if id, ok := siteIDFromUser(u.name); !ok || id != testSiteID {
		t.Errorf("siteIDFromUser(%q) = %q, %v", u.name, id, ok)
	}
	for _, n := range []string{"pressfleet", "default", "pf_nope", "healthcheck"} {
		if _, ok := siteIDFromUser(n); ok {
			t.Errorf("%q taken for a site user", n)
		}
	}
}

func TestParseUserDB(t *testing.T) {
	tests := []struct {
		reply any
		want  int
		err   bool
	}{
		{[]any{"flags", []any{"on"}, "databases", "db=5"}, 5, false},
		{[]any{"databases", "alldbs"}, 0, false},
		{[]any{"databases", "db=1,2"}, 0, false},
		{[]any{"flags", []any{"on"}}, 0, true},
		{"nope", 0, true},
	}
	for _, tt := range tests {
		got, err := parseUserDB(tt.reply)
		if got != tt.want || (err != nil) != tt.err {
			t.Errorf("parseUserDB(%v) = %d, %v", tt.reply, got, err)
		}
	}
	if got := infoField("# Server\r\nredis_version:7.2.4\r\nrun_id:abc\r\n", "run_id"); got != "abc" {
		t.Errorf("infoField = %q", got)
	}
}

func TestNewValidates(t *testing.T) {
	for _, cfg := range []Config{
		{User: "u", PasswordFile: "/pw", Databases: 16},
		{Addr: "redis:6379", PasswordFile: "/pw", Databases: 16},
		{Addr: "redis:6379", User: "u", Databases: 16},
		{Addr: "redis:6379", User: "u", PasswordFile: "/pw", Databases: 1},
		{Addr: "redis:6379", User: "u", PasswordFile: "/pw", Databases: 16, PollInterval: -1},
	} {
		if _, err := New(cfg, nil, secrets.Store{}, storetest.Logger()); err == nil {
			t.Errorf("New(%+v) succeeded", cfg)
		}
	}
	// New doesn't connect, so it succeeds with nothing listening.
	m, err := New(Config{Addr: "127.0.0.1:1", User: "u", PasswordFile: "/nonexistent", Databases: 16}, nil, secrets.Store{}, storetest.Logger())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ping(context.Background()); err == nil {
		t.Error("Ping succeeded with no server")
	}
}
