package mariadb

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// fakeClient is a shell script standing in for mariadb-dump or mariadb. It
// records its arguments, a copy of its option file (and whether that file
// had mode 0600), and its standard input, then prints canned output and
// exits with a canned status.
type fakeClient struct {
	path string
	dir  string
}

func newFakeClient(t *testing.T, name, stdout, stderr string, exit int) fakeClient {
	t.Helper()
	dir := t.TempDir()
	for file, content := range map[string]string{"stdout": stdout, "stderr": stderr} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
d='` + dir + `'
opt=${1#--defaults-file=}
cp "$opt" "$d/options"
find "$opt" -perm 600 > "$d/mode"
printf '%s\n' "$@" > "$d/args"
cat > "$d/stdin"
cat "$d/stdout"
cat "$d/stderr" >&2
exit ` + string(rune('0'+exit)) + "\n"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return fakeClient{path: path, dir: dir}
}

func (c fakeClient) read(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.dir, file))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ran reports whether the client was run.
func (c fakeClient) ran() bool {
	_, err := os.Stat(filepath.Join(c.dir, "args"))
	return err == nil
}

// checkRun checks how the client was invoked for the site: credentials
// only in a 0600 option file, the given options, and the site's database.
func (c fakeClient) checkRun(t *testing.T, creds sitedb.Credentials, wantArgs []string) {
	t.Helper()
	args := strings.Split(strings.TrimSuffix(c.read(t, "args"), "\n"), "\n")
	if len(args) == 0 || !strings.HasPrefix(args[0], "--defaults-file=") {
		t.Fatalf("first argument isn't --defaults-file: %q", args)
	}
	want := append(append([]string{args[0]}, wantArgs...), creds.Database)
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("args = %q, want %q", args, want)
	}
	if strings.Contains(c.read(t, "args"), creds.Password.Reveal()) {
		t.Error("password in the client's arguments")
	}
	if strings.TrimSpace(c.read(t, "mode")) == "" {
		t.Error("option file mode isn't 0600")
	}
	wantOpts := "[client]\nhost=\"db.internal\"\nport=\"3307\"\nprotocol=\"TCP\"\nuser=\"" + creds.Username +
		"\"\npassword=\"" + creds.Password.Reveal() + "\"\n"
	if got := c.read(t, "options"); got != wantOpts {
		t.Errorf("option file = %q, want %q", got, wantOpts)
	}
}

// dumpFixture is a fixture whose manager runs fake clients and writes its
// option files to a directory the test checks is left empty.
type dumpFixture struct {
	fixture
	optDir string
}

func newDumpFixture(t *testing.T, db *store.DB) dumpFixture {
	t.Helper()
	f := dumpFixture{fixture: newFixture(t, db), optDir: t.TempDir()}
	f.m.addr = "db.internal:3307"
	f.m.tempDir = f.optDir
	t.Cleanup(func() {
		entries, err := os.ReadDir(f.optDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("option files left behind: %v", entries)
		}
	})
	return f
}

func (f dumpFixture) create(t *testing.T) sitedb.Credentials {
	t.Helper()
	ref, err := f.m.CreateDatabase(context.Background(), testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	return f.creds(t, ref)
}

// dirNames lists dir, to check that no temporary files are left.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

const fakeDump = "/*M!999999\\- enable the sandbox mode */ \n" +
	"INSERT INTO `wp_options` VALUES\n" +
	"(1,'siteurl','https://example.com','yes');\n" +
	"/*!50003 CREATE*/ /*!50017 DEFINER=`pf_other`@`%`*/ /*!50003 TRIGGER t BEFORE INSERT ON wp_posts FOR EACH ROW SET NEW.post_title = NEW.post_title */;;\n" +
	"/*!50001 CREATE ALGORITHM=UNDEFINED */\n" +
	"/*!50013 DEFINER=`pf_other`@`%` SQL SECURITY DEFINER */\n" +
	"/*!50001 VIEW `v` AS select 1 AS `x` */;\n"

const fakeDumpStripped = "/*M!999999\\- enable the sandbox mode */ \n" +
	"INSERT INTO `wp_options` VALUES\n" +
	"(1,'siteurl','https://example.com','yes');\n" +
	"/*!50003 CREATE*/ /*!50003 TRIGGER t BEFORE INSERT ON wp_posts FOR EACH ROW SET NEW.post_title = NEW.post_title */;;\n" +
	"/*!50001 CREATE ALGORITHM=UNDEFINED */\n" +
	"/*!50013 SQL SECURITY DEFINER */\n" +
	"/*!50001 VIEW `v` AS select 1 AS `x` */;\n"

func TestDump(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newDumpFixture(t, db)
		creds := f.create(t)
		client := newFakeClient(t, "mariadb-dump", fakeDump, "", 0)
		f.m.dumpProgram = client.path

		dir := t.TempDir()
		dest := filepath.Join(dir, "db.sql")
		// A dump from an earlier attempt is replaced.
		if err := os.WriteFile(dest, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := f.m.Dump(context.Background(), testSiteID, dest); err != nil {
			t.Fatal(err)
		}
		client.checkRun(t, creds, dumpArgs)
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != fakeDumpStripped {
			t.Errorf("dump = %q, want %q", got, fakeDumpStripped)
		}
		if fi, err := os.Stat(dest); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("dump file mode = %v, %v; want 0600", fi.Mode(), err)
		}
		if names := dirNames(t, dir); len(names) != 1 {
			t.Errorf("destination directory holds %q, want only db.sql", names)
		}
	})
}

func TestDumpClientFails(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newDumpFixture(t, db)
		creds := f.create(t)
		stderr := "mariadb-dump: Got error: 1045: \"Access denied for user (using password: " + creds.Password.Reveal() + ")\"\n"
		f.m.dumpProgram = newFakeClient(t, "mariadb-dump", "-- partial\n", stderr, 2).path

		dir := t.TempDir()
		dest := filepath.Join(dir, "db.sql")
		if err := os.WriteFile(dest, []byte("previous"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := f.m.Dump(context.Background(), testSiteID, dest)
		if err == nil {
			t.Fatal("Dump succeeded")
		}
		if !strings.Contains(err.Error(), "Got error: 1045") || !strings.Contains(err.Error(), "exit status 2") {
			t.Errorf("err = %v, want the client's error and status", err)
		}
		if strings.Contains(err.Error(), creds.Password.Reveal()) {
			t.Errorf("err contains the password: %v", err)
		}
		if got, _ := os.ReadFile(dest); string(got) != "previous" {
			t.Errorf("destination = %q after a failed dump, want it untouched", got)
		}
		if names := dirNames(t, dir); len(names) != 1 {
			t.Errorf("destination directory holds %q after a failed dump", names)
		}
	})
}

func TestImport(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := newDumpFixture(t, db)
		creds := f.create(t)
		client := newFakeClient(t, "mariadb", "", "", 0)
		f.m.clientProgram = client.path
		var ops []string
		f.srv.fail = func(op string) error { ops = append(ops, op); return nil }

		src := filepath.Join(t.TempDir(), "db.sql")
		if err := os.WriteFile(src, []byte(fakeDumpStripped), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := f.m.Import(context.Background(), testSiteID, src); err != nil {
			t.Fatal(err)
		}
		client.checkRun(t, creds, importArgs)
		if got := client.read(t, "stdin"); got != fakeDumpStripped {
			t.Errorf("client read %q, want the dump", got)
		}
		// The database is emptied, then recreated with the user intact.
		if strings.Join(ops, ",") != "empty,ensure" {
			t.Errorf("server operations = %q, want empty then ensure", ops)
		}
		f.checkServer(t, string(mustRecord(t, db).SecretRef))
	})
}

func TestImportFailures(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newDumpFixture(t, db)
		creds := f.create(t)
		src := filepath.Join(t.TempDir(), "db.sql")
		if err := os.WriteFile(src, []byte("SELECT 1;\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		// A missing dump file is found before anything is dropped.
		client := newFakeClient(t, "mariadb", "", "", 0)
		f.m.clientProgram = client.path
		var ops []string
		f.srv.fail = func(op string) error { ops = append(ops, op); return nil }
		if err := f.m.Import(ctx, testSiteID, src+".missing"); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("import of a missing file: err = %v, want ErrNotExist", err)
		}
		if len(ops) != 0 || client.ran() {
			t.Errorf("import of a missing file ran the client (%v) or changed the server: %q", client.ran(), ops)
		}

		// So is a missing client.
		f.m.clientProgram = filepath.Join(t.TempDir(), "mariadb")
		if err := f.m.Import(ctx, testSiteID, src); !errors.Is(err, ErrClientNotFound) {
			t.Errorf("import without a client: err = %v, want ErrClientNotFound", err)
		}
		if len(ops) != 0 {
			t.Errorf("import without a client changed the server: %q", ops)
		}

		// A failed statement is reported with the client's message.
		stderr := "ERROR 1064 (42000) at line 1: You have an error in your SQL syntax; " + creds.Password.Reveal() + strings.Repeat("x", 2*maxStderr)
		f.m.clientProgram = newFakeClient(t, "mariadb", "", stderr, 1).path
		err := f.m.Import(ctx, testSiteID, src)
		if err == nil || !strings.Contains(err.Error(), "ERROR 1064") || !strings.HasSuffix(err.Error(), "[truncated]") {
			t.Errorf("err = %v, want the client's truncated error", err)
		}
		if err != nil && strings.Contains(err.Error(), creds.Password.Reveal()) {
			t.Errorf("err contains the password: %v", err)
		}

		// A server failure stops the import before the client runs.
		client = newFakeClient(t, "mariadb", "", "", 0)
		f.m.clientProgram = client.path
		injected := errors.New("mariadb down")
		f.srv.fail = func(string) error { return injected }
		if err := f.m.Import(ctx, testSiteID, src); !errors.Is(err, injected) {
			t.Errorf("err = %v, want injected", err)
		}
		if client.ran() {
			t.Error("client ran after the server failed")
		}
	})
}

func mustRecord(t *testing.T, db *store.DB) record {
	t.Helper()
	rec, err := getRecord(context.Background(), db, testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestDumpImportPreconditions(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newDumpFixture(t, db)
		dumpClient := newFakeClient(t, "mariadb-dump", "", "", 0)
		importClient := newFakeClient(t, "mariadb", "", "", 0)
		f.m.dumpProgram, f.m.clientProgram = dumpClient.path, importClient.path
		dir := t.TempDir()
		dest := filepath.Join(dir, "db.sql")
		src := filepath.Join(dir, "src.sql")
		if err := os.WriteFile(src, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		// A site without a database has nothing to dump or import into,
		// and no credentials are created for it.
		if err := f.m.Dump(ctx, testSiteID, dest); !errors.Is(err, sitedb.ErrDatabaseNotFound) {
			t.Errorf("Dump: err = %v, want ErrDatabaseNotFound", err)
		}
		if err := f.m.Import(ctx, testSiteID, src); !errors.Is(err, sitedb.ErrDatabaseNotFound) {
			t.Errorf("Import: err = %v, want ErrDatabaseNotFound", err)
		}
		if n := f.count(t, "site_databases"); n != 0 {
			t.Errorf("records = %d, want 0", n)
		}
		if f.srv.calls != 0 || dumpClient.ran() || importClient.ran() {
			t.Error("server or clients used for a site without a database")
		}
		if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("dump file: %v, want none", err)
		}

		if err := f.m.Dump(ctx, "Not-An-ID", dest); !errors.Is(err, ErrInvalidSiteID) {
			t.Errorf("Dump: err = %v, want ErrInvalidSiteID", err)
		}
		if err := f.m.Import(ctx, "Not-An-ID", src); !errors.Is(err, ErrInvalidSiteID) {
			t.Errorf("Import: err = %v, want ErrInvalidSiteID", err)
		}

		f.create(t)
		f.m.dumpProgram = "pressfleet-no-such-mariadb-dump"
		if err := f.m.Dump(ctx, testSiteID, dest); !errors.Is(err, ErrClientNotFound) {
			t.Errorf("Dump without a client: err = %v, want ErrClientNotFound", err)
		}
		if names := dirNames(t, dir); len(names) != 1 {
			t.Errorf("files after failed dumps: %q", names)
		}
	})
}

func TestStripDefiners(t *testing.T) {
	long := "(1,'" + strings.Repeat("a", 200<<10) + "/*!50013 DEFINER=`x`@`%` SQL SECURITY DEFINER */')\n"
	tests := []struct{ name, in, want string }{
		{"no definers", "INSERT INTO t VALUES\n(1);\n", "INSERT INTO t VALUES\n(1);\n"},
		{"view and trigger", fakeDump, fakeDumpStripped},
		{
			"quoted account",
			"/*!50013 DEFINER=`we``ird`@`local``host` SQL SECURITY INVOKER */\n",
			"/*!50013 SQL SECURITY INVOKER */\n",
		},
		{"no trailing newline", "/*!50013 DEFINER=`a`@`%` SQL SECURITY DEFINER */", "/*!50013 SQL SECURITY DEFINER */"},
		// Only line starts are rewritten: data mentioning a DEFINER, even
		// past the read buffer, passes through.
		{"in data", "(1,'/*!50013 DEFINER=`a`@`%` SQL')\n", "(1,'/*!50013 DEFINER=`a`@`%` SQL')\n"},
		{"long line", long, long},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := stripDefiners(&out, strings.NewReader(tt.in)); err != nil {
				t.Fatal(err)
			}
			if out.String() != tt.want {
				t.Errorf("got %q, want %q", out.String(), tt.want)
			}
		})
	}
}

func TestOptionValue(t *testing.T) {
	tests := []struct {
		in, want string
		ok       bool
	}{
		{"ABCDEF234567", `"ABCDEF234567"`, true},
		{`pa"ss\word #x`, `"pa\"ss\\word #x"`, true},
		{"", "", false},
		{"two\nlines", "", false},
		{"nul\x00", "", false},
	}
	for _, tt := range tests {
		got, ok := optionValue(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("optionValue(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
