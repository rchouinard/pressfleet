package redis_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/redis"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/resp"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// cacheServer is the platform's object cache, run from the configuration
// the platform renders.
type cacheServer struct {
	project      compose.Project
	addr         string
	passwordFile string
	password     redact.Secret
}

// startCache renders the platform project to get the real valkey.conf and
// admin password, and runs the pinned image with them, published on a free
// loopback port. It is removed when the test ends.
func startCache(t *testing.T) cacheServer {
	t.Helper()
	images, err := platform.DefaultImages("test")
	if err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	platformDir := filepath.Join(dataDir, "platform")
	password := redact.Secret(rand26())
	if err := platform.Render(platformDir, platform.Config{
		DataDir:             dataDir,
		Images:              images,
		MariaDBRootPassword: "unused",
		RedisAdminPassword:  password,
		MasterKeyFile:       filepath.Join(t.TempDir(), "master.key"),
	}); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, port, _ := net.SplitHostPort(addr)

	// The service definition mirrors the platform's, plus a published port.
	composeFile := fmt.Sprintf(`services:
  redis:
    image: %q
    command: ["valkey-server", "/etc/valkey/valkey.conf"]
    user: "999:999"
    read_only: true
    cap_drop: [ALL]
    tmpfs:
      - /data:size=8m,mode=0700,uid=999,gid=999
    volumes:
      - type: bind
        source: %q
        target: /etc/valkey/valkey.conf
        read_only: true
    ports:
      - "127.0.0.1:%s:6379"
    healthcheck:
      test: ["CMD", "valkey-cli", "--no-auth-warning", "--user", "healthcheck", "--pass", "nopass", "-e", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5
      start_period: 30s
      start_interval: 1s
`, images.Redis, filepath.Join(platformDir, platform.RedisConfigFile), port)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(composeFile), 0o600); err != nil {
		t.Fatal(err)
	}
	p := compose.Project{Name: fmt.Sprintf("pftest-objcache-%d", time.Now().UnixNano()), Dir: dir}
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
	s := cacheServer{project: p, addr: addr, passwordFile: filepath.Join(platformDir, platform.RedisAdminPasswordSecret), password: password}
	s.waitReachable(t)
	return s
}

// waitReachable waits until the admin can log in over the published port,
// which can lag behind the container's health.
func (s cacheServer) waitReachable(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		c, err := s.admin(t.Context())
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cache server not reachable: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (s cacheServer) admin(ctx context.Context) (*resp.Conn, error) {
	return resp.Dial(ctx, resp.Options{Addr: s.addr, Username: platform.RedisAdminUser, Password: s.password})
}

func rand26() string { return strings.ToLower(id.New()) }

// site is a connection as a site user, with its credentials.
type site struct {
	id    string
	creds objectcache.Credentials
	conn  *resp.Conn
}

func (s site) key(k string) string { return s.creds.KeyPrefix + k }

// do runs a command and returns the reply or error.
func (s site) do(t *testing.T, args ...string) (any, error) {
	t.Helper()
	return s.conn.Do(t.Context(), args...)
}

// must runs a command that must succeed.
func (s site) must(t *testing.T, args ...string) any {
	t.Helper()
	v, err := s.do(t, args...)
	if err != nil {
		t.Fatalf("%s: %q: %v", s.id, args, err)
	}
	return v
}

// denied asserts a command fails with an ACL error or a failed script.
func (s site) denied(t *testing.T, args ...string) {
	t.Helper()
	v, err := s.do(t, args...)
	var e *resp.Error
	if !errors.As(err, &e) || (e.Code() != "NOPERM" && !strings.Contains(e.Message, "ACL failure") && !strings.Contains(e.Message, "No permissions")) {
		t.Errorf("%s: %q = %v, %v; want permission denied", s.id, args, v, err)
	}
}

func connectSite(t *testing.T, srv cacheServer, secretStore secrets.Store, siteID, ref string) site {
	t.Helper()
	plain, err := secretStore.Get(t.Context(), secrets.Ref(ref))
	if err != nil {
		t.Fatal(err)
	}
	creds, err := objectcache.UnmarshalCredentials(plain)
	if err != nil {
		t.Fatal(err)
	}
	c, err := resp.Dial(t.Context(), resp.Options{Addr: srv.addr, Username: creds.Username, Password: creds.Password, DB: creds.Database})
	if err != nil {
		t.Fatalf("site %s can't connect: %v", siteID, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return site{id: siteID, creds: creds, conn: c}
}

// The Redis Object Cache plugin's flush scripts (includes/object-cache.php,
// lua_flush_closure and lua_flush_extended_closure), with the key prefix
// substituted as the plugin does.
const (
	pluginFlushScript = `local cur = 0
local i = 0
local tmp
repeat
    tmp = redis.call('SCAN', cur, 'MATCH', '%s*')
    cur = tonumber(tmp[1])
    if tmp[2] then
        for _, v in pairs(tmp[2]) do
            redis.call('del', v)
            i = i + 1
        end
    end
until 0 == cur
return i`
)

// TestObjectCacheIsolation is the acceptance test of #38: against the real
// server and configuration, site A can use its own keys but can't read,
// list, or flush site B's, and site users survive a server restart.
func TestObjectCacheIsolation(t *testing.T) {
	if os.Getenv("PRESSFLEET_DOCKER_TESTS") != "1" {
		t.Skip("set PRESSFLEET_DOCKER_TESTS=1 to run tests against Docker")
	}
	srv := startCache(t)
	db := storetest.Open(t, store.SQLite)
	secretStore := secrets.NewStore(db, secretstest.Keyring(t, 1))
	m, err := redis.New(redis.Config{
		Addr: srv.addr, User: platform.RedisAdminUser, PasswordFile: srv.passwordFile,
		Databases: platform.RedisDatabases, PollInterval: 200 * time.Millisecond,
	}, db, secretStore, storetest.Logger())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	idA, idB := id.New(), id.New()
	refA, err := m.CreateUser(ctx, idA)
	if err != nil {
		t.Fatal(err)
	}
	refB, err := m.CreateUser(ctx, idB)
	if err != nil {
		t.Fatal(err)
	}
	a := connectSite(t, srv, secretStore, idA, refA)
	b := connectSite(t, srv, secretStore, idB, refB)
	if a.creds.Database == b.creds.Database {
		t.Fatalf("both sites got database %d", a.creds.Database)
	}

	b.must(t, "SET", b.key("wp:useremail:owner@example.com"), "42")
	b.must(t, "SET", b.key("wp:posts:1"), "secret post")

	t.Run("site A uses its own keys", func(t *testing.T) {
		a.must(t, "SET", a.key("wp:posts:1"), "hello")
		if v := a.must(t, "GET", a.key("wp:posts:1")); v != "hello" {
			t.Errorf("GET = %v", v)
		}
		a.must(t, "MSET", a.key("x"), "1", a.key("y"), "2")
		a.must(t, "INCRBY", a.key("n"), "5")
		a.must(t, "EXPIRE", a.key("x"), "60")
		a.must(t, "ZADD", a.key("wp:redis-cache:metrics"), "1", "m")
		// MULTI/EXEC, as phpredis pipelines use.
		a.must(t, "MULTI")
		a.must(t, "SET", a.key("tx"), "1")
		if v := a.must(t, "EXEC"); len(v.([]any)) != 1 {
			t.Errorf("EXEC = %v", v)
		}
		// The plugin runs INFO when it connects.
		if v, _ := a.must(t, "INFO").(string); !strings.Contains(v, "redis_version") {
			t.Error("INFO lacks redis_version")
		}
		a.must(t, "PING")
	})

	t.Run("site A can't read or write site B's keys", func(t *testing.T) {
		a.denied(t, "GET", b.key("wp:posts:1"))
		a.denied(t, "SET", b.key("wp:posts:1"), "defaced")
		a.denied(t, "DEL", b.key("wp:posts:1"))
		a.denied(t, "EXISTS", b.key("wp:posts:1"))
		a.denied(t, "GET", "unprefixed")
		a.denied(t, "EVAL", "return redis.call('GET', KEYS[1])", "1", b.key("wp:posts:1"))
		a.denied(t, "EVAL", "return redis.call('GET', '"+b.key("wp:posts:1")+"')", "0")
		// Even the same key name in another database is out of reach.
		a.denied(t, "SELECT", strconv.Itoa(b.creds.Database))
		a.denied(t, "SELECT", "0")
		a.denied(t, "MOVE", a.key("x"), strconv.Itoa(b.creds.Database))
		a.denied(t, "COPY", a.key("x"), a.key("x2"), "DB", strconv.Itoa(b.creds.Database))
		a.denied(t, "SWAPDB", strconv.Itoa(a.creds.Database), strconv.Itoa(b.creds.Database))
		a.denied(t, "EVAL", "redis.call('SELECT', ARGV[1]); return redis.call('GET', KEYS[1])", "1", b.key("wp:posts:1"), strconv.Itoa(b.creds.Database))
		a.denied(t, "SORT", a.key("list"), "BY", b.key("*"), "GET", b.key("*"))
	})

	t.Run("site A can't list site B's keys", func(t *testing.T) {
		v := a.must(t, "SCAN", "0", "MATCH", "*", "COUNT", "10000")
		keys, _ := resp.Strings(v.([]any)[1])
		for _, k := range keys {
			if !strings.HasPrefix(k, a.creds.KeyPrefix) {
				t.Errorf("SCAN returned %q", k)
			}
		}
		if len(keys) == 0 {
			t.Error("SCAN returned none of site A's keys")
		}
		// SCAN inside a script sees the same.
		v = a.must(t, "EVAL", "return redis.call('SCAN', '0', 'MATCH', '*', 'COUNT', '10000')[2]", "0")
		keys, _ = resp.Strings(v)
		for _, k := range keys {
			if !strings.HasPrefix(k, a.creds.KeyPrefix) {
				t.Errorf("SCAN in a script returned %q", k)
			}
		}
		for range 20 {
			if k, _ := a.must(t, "RANDOMKEY").(string); !strings.HasPrefix(k, a.creds.KeyPrefix) {
				t.Errorf("RANDOMKEY returned %q", k)
			}
		}
		if n, _ := a.must(t, "DBSIZE").(int64); n != int64(len(keys)) {
			t.Errorf("DBSIZE = %d, SCAN found %d", n, len(keys))
		}
		a.denied(t, "KEYS", "*")
		a.denied(t, "CLIENT", "LIST")
		a.denied(t, "MONITOR")
		a.denied(t, "CLIENT", "TRACKING", "on", "BCAST")
		a.denied(t, "SUBSCRIBE", "__keyspace@"+strconv.Itoa(b.creds.Database)+"__:*")
		a.denied(t, "PSUBSCRIBE", "*")
		a.denied(t, "SCRIPT", "SHOW", "0000000000000000000000000000000000000000")
		a.denied(t, "FUNCTION", "LIST")
		a.denied(t, "CONFIG", "GET", "*")
		a.denied(t, "ACL", "LIST")
		a.denied(t, "MEMORY", "STATS")
	})

	t.Run("site A can't flush site B's keys", func(t *testing.T) {
		a.denied(t, "FLUSHALL")
		a.denied(t, "FLUSHALL", "ASYNC")
		a.denied(t, "FUNCTION", "FLUSH")
		a.denied(t, "SCRIPT", "FLUSH")
		a.denied(t, "SCRIPT", "KILL")
		a.denied(t, "EVAL", "return redis.call('FLUSHALL')", "0")

		// The plugin's selective flush deletes only site A's keys.
		a.must(t, "SET", a.key("wp:options:alloptions"), "x")
		n, _ := a.must(t, "EVAL", fmt.Sprintf(pluginFlushScript, a.creds.KeyPrefix), "0").(int64)
		if n == 0 {
			t.Error("selective flush deleted nothing")
		}
		// A group flush, as wp_cache_flush_group runs it.
		a.must(t, "SET", a.key("wp:posts:2"), "x")
		a.must(t, "EVAL", fmt.Sprintf(pluginFlushScript, a.key("wp:posts:")), "0")
		if v, _ := a.do(t, "GET", a.key("wp:posts:2")); v != nil {
			t.Errorf("group flush left %v", v)
		}
		// A full flush (selective flush off) is FLUSHDB: only A's database.
		a.must(t, "SET", a.key("again"), "x")
		a.must(t, "FLUSHDB")
		if n, _ := a.must(t, "DBSIZE").(int64); n != 0 {
			t.Errorf("site A's DBSIZE after FLUSHDB = %d", n)
		}
		if v := b.must(t, "GET", b.key("wp:posts:1")); v != "secret post" {
			t.Errorf("site B's key after site A's flushes = %v", v)
		}
	})

	// Every command site users may run that takes no key, so that the key
	// pattern doesn't confine it, is one known not to reach other sites.
	// New server versions add commands; this fails until they are vetted.
	t.Run("keyless commands are vetted", func(t *testing.T) {
		allowed := keylessAllowed(t, srv, a.creds.Username)
		// The audit sees allowed commands and not denied ones.
		if !slices.Contains(allowed, "info") || !slices.Contains(allowed, "ping") ||
			slices.Contains(allowed, "keys") || slices.Contains(allowed, "client|list") {
			t.Fatalf("keyless audit is broken: allowed %v", allowed)
		}
		t.Logf("keyless commands site users may run: %v", allowed)
		var unvetted []string
		for _, c := range allowed {
			if !vettedKeyless[c] {
				unvetted = append(unvetted, c)
			}
		}
		if len(unvetted) > 0 {
			t.Errorf("site users may run keyless commands that aren't vetted; deny them in siteRules or add them to vettedKeyless: %v", unvetted)
		}
	})

	t.Run("converging a user keeps its connections", func(t *testing.T) {
		if _, err := m.CreateUser(ctx, idA); err != nil {
			t.Fatal(err)
		}
		a.must(t, "SET", a.key("still"), "here")
	})

	t.Run("the server's restart is repaired", func(t *testing.T) {
		watchCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { defer close(done); m.Watch(watchCtx) }()
		defer func() { cancel(); <-done }()

		restart(t, srv)
		// The restarted server forgets site users until Watch restores them
		// with the stored passwords.
		deadline := time.Now().Add(30 * time.Second)
		for {
			c, err := resp.Dial(ctx, resp.Options{Addr: srv.addr, Username: a.creds.Username, Password: a.creds.Password, DB: a.creds.Database})
			if err == nil {
				if _, err := c.Do(ctx, "SET", a.key("after-restart"), "1"); err != nil {
					t.Errorf("site A after restart: %v", err)
				}
				_ = c.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("site A's user not restored after a restart: %v", err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	t.Run("deleting a user disconnects it and empties its database", func(t *testing.T) {
		b = connectSite(t, srv, secretStore, idB, refB)
		b.must(t, "SET", b.key("doomed"), "1")
		if err := m.DeleteUser(ctx, idB); err != nil {
			t.Fatal(err)
		}
		if _, err := b.do(t, "PING"); err == nil {
			t.Error("deleted user's connection still works")
		}
		if _, err := resp.Dial(ctx, resp.Options{Addr: srv.addr, Username: b.creds.Username, Password: b.creds.Password}); !resp.IsCode(err, "WRONGPASS") {
			t.Errorf("deleted user login: err = %v, want WRONGPASS", err)
		}
		c, err := srv.admin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = c.Close() }()
		if _, err := c.Do(ctx, "SELECT", strconv.Itoa(b.creds.Database)); err != nil {
			t.Fatal(err)
		}
		// FLUSHDB ASYNC empties the database at once.
		if n, err := c.Do(ctx, "DBSIZE"); err != nil || n != int64(0) {
			t.Errorf("site B's database after delete: DBSIZE = %v, %v", n, err)
		}
		if err := m.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		users, err := c.Do(ctx, "ACL", "USERS")
		if err != nil {
			t.Fatal(err)
		}
		names, _ := resp.Strings(users)
		if slices.Contains(names, b.creds.Username) || !slices.Contains(names, a.creds.Username) {
			t.Errorf("users after deleting B = %v", names)
		}
	})
}

// restart stops and starts the server container, which loses every runtime
// ACL user and all data.
func restart(t *testing.T, srv cacheServer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if err := srv.project.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if err := srv.project.Start(ctx, compose.StartOptions{Wait: true, WaitTimeout: time.Minute}); err != nil {
		t.Fatal(err)
	}
	srv.waitReachable(t)
}

// vettedKeyless are the keyless commands site users may run. Each only
// concerns the caller's connection or its own database, or reports
// nothing about other sites beyond server aggregates (INFO).
var vettedKeyless = map[string]bool{
	// Connection and protocol.
	"auth": true, "hello": true, "ping": true, "echo": true, "quit": true, "reset": true, "select": true,
	"readonly": true, "readwrite": true, "asking": true,
	"client|id": true, "client|getname": true, "client|setname": true, "client|info": true, "client|setinfo": true,
	// Transactions and scripts (commands inside are checked as usual).
	"multi": true, "exec": true, "discard": true, "unwatch": true,
	"script|load": true, "script|exists": true,
	// The caller's own database.
	"dbsize": true, "randomkey": true, "scan": true, "flushdb": true,
	// Replication acknowledgements; they wait, they don't read.
	"wait": true, "waitaof": true,
	// Server information without key names or values.
	"info": true, "time": true, "lolwut": true,
	"command|count": true, "command|docs": true, "command|getkeys": true, "command|getkeysandflags": true,
	"command|help": true, "command|info": true, "command|list": true,
	"acl|cat": true, "acl|genpass": true, "acl|help": true, "acl|whoami": true,
	// Help texts.
	"commandlog|help": true, "config|help": true, "latency|help": true, "object|help": true,
	"pubsub|help": true, "script|help": true, "slowlog|help": true, "xgroup|help": true, "xinfo|help": true,
	"module|help": true,
}

// keylessAllowed lists the commands and subcommands without key arguments
// that user may run, using COMMAND and ACL DRYRUN as the admin. DRYRUN
// checks against database 0, which site users can't use, so commands that
// act on the current database (SCAN, DBSIZE, FLUSHDB, ...) come out denied:
// the database rule confines them whatever the command rules say.
func keylessAllowed(t *testing.T, srv cacheServer, user string) []string {
	t.Helper()
	ctx := t.Context()
	c, err := srv.admin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	v, err := c.Do(ctx, "COMMAND")
	if err != nil {
		t.Fatal(err)
	}
	var allowed []string
	var visit func(cmd []any)
	visit = func(cmd []any) {
		// COMMAND reply: name, arity, flags, first key, last key, step,
		// ACL categories, tips, key specs, subcommands.
		name, _ := cmd[0].(string)
		if subs, ok := cmd[9].([]any); ok && len(subs) > 0 {
			for _, s := range subs {
				visit(s.([]any))
			}
			return
		}
		if specs, ok := cmd[8].([]any); ok && len(specs) > 0 {
			return
		}
		arity, _ := cmd[1].(int64)
		args := append([]string{"ACL", "DRYRUN", user}, strings.Split(name, "|")...)
		for len(args)-3 < int(max(arity, -arity)) {
			args = append(args, "0")
		}
		res, err := c.Do(ctx, args...)
		if err != nil {
			t.Fatalf("ACL DRYRUN %s: %v", name, err)
		}
		if res == "OK" {
			allowed = append(allowed, name)
		}
	}
	for _, cmd := range v.([]any) {
		visit(cmd.([]any))
	}
	slices.Sort(allowed)
	return allowed
}
