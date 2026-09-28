package mariadb_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/mariadb"
)

// wpSchema is a cut-down WordPress schema: the column types, charsets,
// keys, and zero dates of wp_options, wp_posts, and wp_postmeta, plus a
// binary table, a view, and a trigger.
var wpSchema = []string{
	`CREATE TABLE wp_options (
		option_id bigint(20) unsigned NOT NULL AUTO_INCREMENT,
		option_name varchar(191) NOT NULL DEFAULT '',
		option_value longtext NOT NULL,
		autoload varchar(20) NOT NULL DEFAULT 'yes',
		PRIMARY KEY (option_id),
		UNIQUE KEY option_name (option_name),
		KEY autoload (autoload)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci`,
	`CREATE TABLE wp_posts (
		ID bigint(20) unsigned NOT NULL AUTO_INCREMENT,
		post_author bigint(20) unsigned NOT NULL DEFAULT 0,
		post_date datetime NOT NULL DEFAULT '0000-00-00 00:00:00',
		post_date_gmt datetime NOT NULL DEFAULT '0000-00-00 00:00:00',
		post_content longtext NOT NULL,
		post_title text NOT NULL,
		post_status varchar(20) NOT NULL DEFAULT 'publish',
		post_name varchar(200) NOT NULL DEFAULT '',
		guid varchar(255) NOT NULL DEFAULT '',
		PRIMARY KEY (ID),
		KEY post_name (post_name(191))
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci`,
	`CREATE TABLE wp_postmeta (
		meta_id bigint(20) unsigned NOT NULL AUTO_INCREMENT,
		post_id bigint(20) unsigned NOT NULL DEFAULT 0,
		meta_key varchar(255) DEFAULT NULL,
		meta_value longtext,
		PRIMARY KEY (meta_id),
		KEY post_id (post_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_520_ci`,
	`CREATE TABLE wp_pf_blobs (
		id int NOT NULL PRIMARY KEY,
		data longblob,
		hash binary(20)
	) ENGINE=InnoDB`,
	`CREATE VIEW wp_published AS SELECT ID, post_title FROM wp_posts WHERE post_status = 'publish'`,
	`CREATE TRIGGER wp_postmeta_key BEFORE INSERT ON wp_postmeta FOR EACH ROW
		SET NEW.meta_key = COALESCE(NEW.meta_key, '_pf_default')`,
}

// phpString serializes s the way PHP's serialize() does: the length is in
// bytes, so multibyte characters must survive exactly or unserialize fails.
func phpString(s string) string {
	return fmt.Sprintf(`s:%d:"%s";`, len(s), s)
}

// allBytes is every byte value, twice.
func allBytes() []byte {
	b := make([]byte, 512)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// fillWordPress loads WordPress-like data into db.
func fillWordPress(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%.60s: %v", q, err)
		}
	}
	for _, q := range wpSchema {
		exec(q)
	}
	serialized := "a:3:{" + phpString("emoji") + phpString("héllo 😀 🏳️‍🌈 世界") +
		phpString("quote") + phpString(`it's "quoted" \ back\slash`) +
		phpString("lines") + phpString("one\r\ntwo\n\tthree\x00four") + "}"
	options := [][2]string{
		{"siteurl", "https://example.com"},
		{"blogname", "Ça marche — 😀"},
		{"wp_user_roles", serialized},
		{"widget_text", "a:0:{}"},
		{"empty", ""},
	}
	for _, o := range options {
		exec("INSERT INTO wp_options (option_name, option_value) VALUES (?, ?)", o[0], o[1])
	}
	// A post over mariadb-dump's net_buffer_length and the 64 KiB line
	// buffer that strips DEFINER clauses, so it is dumped on a long line
	// of its own.
	big := strings.Repeat("<!-- wp:paragraph --><p>Lorem ipsum 😀 dolor</p><!-- /wp:paragraph -->\n", 40000)
	exec("INSERT INTO wp_posts (post_author, post_date, post_date_gmt, post_content, post_title, post_status, post_name, guid) VALUES "+
		"(1, '2026-09-28 12:00:00', '2026-09-28 16:00:00', ?, 'Hello world!', 'publish', 'hello-world', 'https://example.com/?p=1'), "+
		"(1, '2026-09-28 12:05:00', '0000-00-00 00:00:00', ?, 'Brouillon 🚧', 'draft', '', 'https://example.com/?p=2'), "+
		"(1, '2026-09-28 12:10:00', '2026-09-28 16:10:00', ?, 'Big', 'publish', 'big', 'https://example.com/?p=3')",
		"<p>Welcome to WordPress. It's “smart” — and 'plain'.</p>", "", big)
	exec("INSERT INTO wp_postmeta (post_id, meta_key, meta_value) VALUES (1, '_edit_lock', '1727539200:1'), (1, NULL, NULL), (2, '_meta', ?)",
		"a:1:{"+phpString("k")+phpString("ü")+"}")
	exec("INSERT INTO wp_pf_blobs VALUES (1, ?, UNHEX(SHA1(?))), (2, '', NULL), (3, NULL, NULL)", allBytes(), allBytes())
	// Leave a gap in the IDs, so AUTO_INCREMENT must be carried over.
	exec("DELETE FROM wp_options WHERE option_name = 'empty'")
}

// snapshot is everything a restore must reproduce: the tables and views,
// their definitions, and every value, byte for byte.
type snapshot map[string][]string

// takeSnapshot reads db's tables and views. user, the site's user, is replaced in view definitions, so snapshots of
// different sites compare.
func takeSnapshot(ctx context.Context, t *testing.T, db *sql.DB, user string) snapshot {
	t.Helper()
	s := snapshot{}
	views := map[string]bool{}
	for _, v := range queryStrings(ctx, t, db, "SELECT table_name FROM information_schema.views WHERE table_schema = DATABASE()") {
		views[v] = true
	}
	for _, table := range queryStrings(ctx, t, db, "SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE()") {
		ident := "`" + table + "`"
		var name, create, cs, coll string
		if views[table] {
			// SHOW CREATE VIEW has two more columns, the view's charsets.
			if err := db.QueryRowContext(ctx, "SHOW CREATE VIEW "+ident).Scan(&name, &create, &cs, &coll); err != nil {
				t.Fatal(err)
			}
		} else if err := db.QueryRowContext(ctx, "SHOW CREATE TABLE "+ident).Scan(&name, &create); err != nil {
			t.Fatal(err)
		}
		s[table] = append(s[table], strings.ReplaceAll(create, user, "SITE_USER"))
		rows, err := db.QueryContext(ctx, "SELECT * FROM "+ident+" ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			vals := make([]sql.RawBytes, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			for _, v := range vals {
				if v == nil {
					b.WriteString("NULL|")
				} else {
					fmt.Fprintf(&b, "%q|", []byte(v))
				}
			}
			s[table] = append(s[table], b.String())
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
	return s
}

func (s snapshot) diff(t *testing.T, what string, want snapshot) {
	t.Helper()
	for table, w := range want {
		g, ok := s[table]
		if !ok {
			t.Errorf("%s: table %s missing", what, table)
			continue
		}
		if len(g) != len(w) {
			t.Errorf("%s: %s has %d definitions and rows, want %d", what, table, len(g), len(w))
			continue
		}
		for i := range w {
			if g[i] != w[i] {
				t.Errorf("%s: %s differs at %d:\n got %.300s\nwant %.300s", what, table, i, g[i], w[i])
			}
		}
	}
	for table := range s {
		if _, ok := want[table]; !ok {
			t.Errorf("%s: unexpected table %s", what, table)
		}
	}
}

// runDumpImportScenario dumps a WordPress-like database and imports it
// into another site's database and back into its own, checking that every
// definition and value survives.
func runDumpImportScenario(ctx context.Context, t *testing.T, addr string, m *mariadb.Manager, st secrets.Store) {
	src, dst := id.New(), id.New()
	creds := map[string]sitedb.Credentials{}
	for _, site := range []string{src, dst} {
		ref, err := m.CreateDatabase(ctx, site)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = m.DropDatabase(context.Background(), site) })
		plain, err := st.Get(ctx, secrets.Ref(ref))
		if err != nil {
			t.Fatal(err)
		}
		if creds[site], err = sitedb.UnmarshalCredentials(plain); err != nil {
			t.Fatal(err)
		}
	}
	connectSite := func(site string) *sql.DB {
		c := creds[site]
		return connect(t, addr, c.Username, c.Password.Reveal(), c.Database)
	}
	srcDB, dstDB := connectSite(src), connectSite(dst)
	fillWordPress(ctx, t, srcDB)
	want := takeSnapshot(ctx, t, srcDB, creds[src].Username)
	if len(want) != 5 || len(want["wp_posts"]) != 4 {
		t.Fatalf("source snapshot has %d tables and %d wp_posts entries, want 5 and 4", len(want), len(want["wp_posts"]))
	}

	// The target has its own data, including a table the dump doesn't
	// have, all of which the import replaces.
	for _, q := range []string{
		"CREATE TABLE wp_options (option_id int PRIMARY KEY, option_value text)",
		"INSERT INTO wp_options VALUES (1, 'target')",
		"CREATE TABLE wp_stale_plugin (id int)",
	} {
		if _, err := dstDB.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	dir := t.TempDir()
	dump := filepath.Join(dir, "db.sql")
	if err := m.Dump(ctx, src, dump); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if fi, err := os.Stat(dump); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("dump file mode = %v, %v; want 0600", fi.Mode(), err)
	}
	text, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing in the dump ties it to the source database or its user.
	for _, bad := range []string{"CREATE DATABASE", "USE `", "DEFINER=", creds[src].Database + "`", creds[src].Password.Reveal()} {
		if strings.Contains(string(text), bad) {
			t.Errorf("dump contains %q", bad)
		}
	}

	if err := m.Import(ctx, dst, dump); err != nil {
		t.Fatalf("Import into another site: %v", err)
	}
	dstDB = connectSite(dst)
	takeSnapshot(ctx, t, dstDB, creds[dst].Username).diff(t, "other site", want)

	// The view and trigger work, and belong to the target's user.
	var definer string
	if err := dstDB.QueryRowContext(ctx, "SELECT definer FROM information_schema.views WHERE table_schema = DATABASE()").Scan(&definer); err != nil ||
		definer != creds[dst].Username+"@%" {
		t.Errorf("view definer = %q, %v; want the target's user", definer, err)
	}
	if got := queryStrings(ctx, t, dstDB, "SELECT post_title FROM wp_published ORDER BY ID"); !slices.Equal(got, []string{"Hello world!", "Big"}) {
		t.Errorf("view = %q", got)
	}
	if _, err := dstDB.ExecContext(ctx, "INSERT INTO wp_postmeta (post_id) VALUES (9)"); err != nil {
		t.Fatal(err)
	}
	var key string
	if err := dstDB.QueryRowContext(ctx, "SELECT meta_key FROM wp_postmeta WHERE post_id = 9").Scan(&key); err != nil || key != "_pf_default" {
		t.Errorf("trigger result = %q, %v", key, err)
	}

	// Back into the source after it changed: a restore.
	if _, err := srcDB.ExecContext(ctx, "UPDATE wp_options SET option_value = 'changed'"); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(ctx, src, dump); err != nil {
		t.Fatalf("Import into the same site: %v", err)
	}
	takeSnapshot(ctx, t, connectSite(src), creds[src].Username).diff(t, "same site", want)

	// Import is retryable: a second run gives the same result.
	if err := m.Import(ctx, src, dump); err != nil {
		t.Fatalf("second Import: %v", err)
	}
	takeSnapshot(ctx, t, connectSite(src), creds[src].Username).diff(t, "second import", want)

	// A broken dump fails with the client's message.
	broken := filepath.Join(dir, "broken.sql")
	if err := os.WriteFile(broken, []byte("CREATE TABLE ok (id int);\nTHIS IS NOT SQL;\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Import(ctx, dst, broken); err == nil || !strings.Contains(err.Error(), "ERROR 1064") {
		t.Errorf("Import of a broken dump: err = %v, want a syntax error", err)
	}
	// A dump can't reach outside the site's database or read local files.
	for _, tt := range []struct{ sql, wantErr string }{
		{"CREATE TABLE `" + creds[src].Database + "`.evil (id int);", "ERROR 1142"},
		{"CREATE DATABASE evil;", "ERROR 1044"},
		{"CREATE DEFINER=`root`@`%` VIEW v AS SELECT 1;", "ERROR 1227"},
		{"\\! touch " + filepath.Join(dir, "pwned"), "Unknown command '\\!'"},
		{"CREATE TABLE f (l text); LOAD DATA LOCAL INFILE '/etc/passwd' INTO TABLE f;", "ERROR 4166"},
	} {
		evil := filepath.Join(dir, "evil.sql")
		if err := os.WriteFile(evil, []byte(tt.sql+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := m.Import(ctx, dst, evil); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("Import of %q: err = %v, want %s", tt.sql, err, tt.wantErr)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a shell command in the dump ran: %v", err)
	}

	// Sites without a database have nothing to dump.
	if err := m.Dump(ctx, id.New(), filepath.Join(dir, "none.sql")); !errors.Is(err, sitedb.ErrDatabaseNotFound) {
		t.Errorf("Dump of an unknown site: err = %v, want ErrDatabaseNotFound", err)
	}
}
