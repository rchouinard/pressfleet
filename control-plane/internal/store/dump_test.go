package store_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// fillState writes rows that exercise every kind of value a dump carries:
// text, integers, doubles, booleans, timestamps, binary data, NULLs, an
// identity column (job_logs), and a table referring to itself (sites).
func fillState(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	g := &store.ClientGroup{Name: "Acme", Notes: "quotes \" and\nnewlines"}
	if err := store.NewClientGroups(db).Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	sites := store.NewSites(db)
	// The staging sites are created first, so a dump in insertion order
	// would put them before their production site.
	child := newTestSite(g.ID, "child", 20001)
	sibling := newTestSite(g.ID, "sibling", 20003)
	parent := newTestSite(g.ID, "parent", 20002)
	parent.EnableCache = false
	parent.Maintenance = true
	for _, s := range []*store.Site{child, sibling, parent} {
		if err := sites.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 10, 20, 12, 34, 56, 789012000, time.UTC)
	for _, stmt := range []struct {
		query string
		args  []any
	}{
		{"UPDATE sites SET parent_site_id = ?, mail_disabled = ?, staging_refreshed_at = ?, staging_snapshot_id = ?, staging_pushed_at = ? WHERE id = ?",
			[]any{parent.ID, true, at, "0123456789abcdef", at.Add(time.Hour), child.ID}},
		{"UPDATE sites SET parent_site_id = ?, mail_disabled = ? WHERE id = ?", []any{parent.ID, false, sibling.ID}},
		{"INSERT INTO secrets (id, kind, key_id, ciphertext, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
			[]any{"sec1", "site-db-password", "k1", []byte{0, 1, 2, 0xff, '"'}, at, at}},
		{"INSERT INTO master_keys (id, check_value, is_current, created_at) VALUES (?, ?, ?, ?)",
			[]any{"k1", []byte("check"), true, at}},
		{"INSERT INTO alert_rules (id, name, scope, kind, expr, threshold, severity, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			[]any{id.New(), "rule", "platform", "metric", "up", 0.25, "warning", false, at, at}},
	} {
		if _, err := db.ExecContext(ctx, stmt.query, stmt.args...); err != nil {
			t.Fatalf("%s: %v", stmt.query, err)
		}
	}
	j := store.Job{Type: "test", TargetType: store.JobTargetSite, TargetID: parent.ID, Payload: json.RawMessage(`{"n":1}`)}
	jobs := store.NewJobs(db)
	if err := jobs.Enqueue(ctx, &j); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := jobs.AppendLog(ctx, j.ID, 0, "info", fmt.Sprintf("line %d", i), at); err != nil {
			t.Fatal(err)
		}
	}
}

// dumpTables dumps db and returns each table's rows, as sorted JSON lines,
// with booleans written as numbers, which is how SQLite returns them.
func dumpTables(t *testing.T, db *store.DB) (store.DumpInfo, map[string][]string) {
	t.Helper()
	var buf bytes.Buffer
	info, err := db.Dump(context.Background(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	tables := map[string][]string{}
	var table string
	sc := bufio.NewScanner(&buf)
	sc.Buffer(nil, 1<<20)
	first := true
	for sc.Scan() {
		line := sc.Text()
		switch {
		case first:
			first = false
		case strings.HasPrefix(line, `{"table":`):
			var rec struct{ Table string }
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatal(err)
			}
			table = rec.Table
			tables[table] = nil
		case strings.HasPrefix(line, "["):
			line = strings.NewReplacer("true", "1", "false", "0").Replace(line)
			tables[table] = append(tables[table], line)
		}
	}
	for _, rows := range tables {
		slices.Sort(rows)
	}
	return info, tables
}

func TestDumpLoad(t *testing.T) {
	ctx := context.Background()
	for _, from := range storetest.Dialects {
		for _, to := range storetest.Dialects {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				src := storetest.Open(t, from)
				fillState(t, src)
				var buf bytes.Buffer
				info, err := src.Dump(ctx, &buf)
				if err != nil {
					t.Fatal(err)
				}
				latest, _ := store.LatestVersion(from)
				if info.Dialect != from || info.SchemaVersion != latest || info.Rows < 8 || info.Tables < 30 {
					t.Errorf("info = %+v", info)
				}
				_, want := dumpTables(t, src)

				// The target is a fresh install with data of its own,
				// which the load replaces.
				cfg := storetest.Config(t, to)
				dst := storetest.OpenUnmigrated(t, cfg)
				if _, err := dst.Migrate(ctx, t.TempDir()); err != nil {
					t.Fatal(err)
				}
				if err := store.NewClientGroups(dst).Create(ctx, &store.ClientGroup{Name: "replaced"}); err != nil {
					t.Fatal(err)
				}
				if err := dst.Close(); err != nil {
					t.Fatal(err)
				}
				checked := false
				res, err := store.Load(ctx, cfg, &buf, store.LoadOptions{
					BackupDir: t.TempDir(),
					Check: func(ctx context.Context, q store.Querier) error {
						checked = true
						_, err := store.NewSites(q).GetBySlug(ctx, "parent")
						return err
					},
				}, storetest.Logger())
				if err != nil {
					t.Fatal(err)
				}
				if !checked || res.Dump.Rows != info.Rows || res.Dump.Tables != info.Tables || res.SchemaVersion != latest {
					t.Errorf("checked = %v, result = %+v", checked, res)
				}
				if (to == store.SQLite) != (res.PreRestorePath != "") {
					t.Errorf("PreRestorePath = %q", res.PreRestorePath)
				}

				loaded := storetest.OpenUnmigrated(t, cfg)
				_, got := dumpTables(t, loaded)
				for table, rows := range want {
					if !slices.Equal(rows, got[table]) {
						t.Errorf("table %s:\n got %q\nwant %q", table, got[table], rows)
					}
				}
				if len(got) != len(want) {
					t.Errorf("%d tables loaded, want %d", len(got), len(want))
				}
				groups := groupNames(t, loaded)
				if !slices.Equal(groups, []string{"Acme"}) {
					t.Errorf("client groups = %q", groups)
				}
				site, err := store.NewSites(loaded).GetBySlug(ctx, "parent")
				if err != nil || site.EnableCache || !site.Maintenance {
					t.Errorf("parent site = %+v, %v", site, err)
				}
				// The staging sites keep their production site and fields.
				for _, tc := range []struct {
					slug      string
					mail      bool
					snapshot  string
					refreshed bool
				}{{"child", true, "0123456789abcdef", true}, {"sibling", false, "", false}} {
					stg, err := store.NewSites(loaded).GetBySlug(ctx, tc.slug)
					if err != nil {
						t.Fatal(err)
					}
					if !stg.IsStaging() || stg.ParentSiteID != site.ID || stg.MailDisabled != tc.mail || stg.StagingSnapshotID != tc.snapshot ||
						stg.StagingRefreshedAt.IsZero() == tc.refreshed || stg.StagingPushedAt.IsZero() == tc.refreshed {
						t.Errorf("staging site %s = %+v", tc.slug, stg)
					}
				}
				// New rows get identities past the loaded ones.
				j := store.Job{Type: "after", TargetType: store.JobTargetSite, TargetID: site.ID}
				jobs := store.NewJobs(loaded)
				if err := jobs.Enqueue(ctx, &j); err != nil {
					t.Fatal(err)
				}
				if err := jobs.AppendLog(ctx, j.ID, 0, "info", "after the load", time.Now()); err != nil {
					t.Fatalf("appending a job log after the load: %v", err)
				}
			})
		}
	}
}

func TestLoadRefusesBadDumps(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, src *store.DB) {
		fillState(t, src)
		var buf bytes.Buffer
		if _, err := src.Dump(ctx, &buf); err != nil {
			t.Fatal(err)
		}
		full := buf.String()
		lines := strings.SplitAfter(full, "\n")
		latest, _ := store.LatestVersion(src.Dialect())
		header := strings.Replace(lines[0], fmt.Sprintf(`"schema_version":%d`, latest), fmt.Sprintf(`"schema_version":%d`, latest+1), 1)
		for _, tc := range []struct {
			name string
			dump string
			want error
		}{
			{"empty", "", store.ErrInvalidDump},
			{"not a dump", `{"format":"something else"}`, store.ErrInvalidDump},
			{"truncated", strings.Join(lines[:len(lines)/2], ""), store.ErrInvalidDump},
			{"without end record", strings.Join(lines[:len(lines)-2], ""), store.ErrInvalidDump},
			{"wrong row count", strings.Replace(full, `"rows":`, `"rows":1`, 1), store.ErrInvalidDump},
			{"newer schema", header + strings.Join(lines[1:], ""), store.ErrSchemaTooNew},
		} {
			t.Run(tc.name, func(t *testing.T) {
				cfg := storetest.Config(t, src.Dialect())
				dst := storetest.OpenUnmigrated(t, cfg)
				if _, err := dst.Migrate(ctx, t.TempDir()); err != nil {
					t.Fatal(err)
				}
				if err := store.NewClientGroups(dst).Create(ctx, &store.ClientGroup{Name: "kept"}); err != nil {
					t.Fatal(err)
				}
				if err := dst.Close(); err != nil {
					t.Fatal(err)
				}
				_, err := store.Load(ctx, cfg, strings.NewReader(tc.dump), store.LoadOptions{BackupDir: t.TempDir()}, storetest.Logger())
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
				// The database is as it was.
				if got := groupNames(t, storetest.OpenUnmigrated(t, cfg)); !slices.Equal(got, []string{"kept"}) {
					t.Errorf("client groups after a refused load = %q", got)
				}
			})
		}
	})
}

// A failing check rolls the load back.
func TestLoadCheckFails(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, src *store.DB) {
		fillState(t, src)
		var buf bytes.Buffer
		if _, err := src.Dump(ctx, &buf); err != nil {
			t.Fatal(err)
		}
		cfg := storetest.Config(t, src.Dialect())
		dst := storetest.OpenUnmigrated(t, cfg)
		if _, err := dst.Migrate(ctx, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if err := store.NewClientGroups(dst).Create(ctx, &store.ClientGroup{Name: "kept"}); err != nil {
			t.Fatal(err)
		}
		if err := dst.Close(); err != nil {
			t.Fatal(err)
		}
		errWrongKey := errors.New("wrong key")
		_, err := store.Load(ctx, cfg, &buf, store.LoadOptions{
			BackupDir: t.TempDir(),
			Check:     func(context.Context, store.Querier) error { return errWrongKey },
		}, storetest.Logger())
		if !errors.Is(err, errWrongKey) {
			t.Fatalf("err = %v", err)
		}
		if got := groupNames(t, storetest.OpenUnmigrated(t, cfg)); !slices.Equal(got, []string{"kept"}) {
			t.Errorf("client groups = %q", got)
		}
	})
}

// A dump from an older schema loads at its version and is then migrated.
func TestLoadOlderSchema(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			src := storetest.OpenUnmigrated(t, storetest.Config(t, d))
			const old = 20
			if _, err := src.MigrateTo(ctx, t.TempDir(), old); err != nil {
				t.Fatal(err)
			}
			if err := store.NewClientGroups(src).Create(ctx, &store.ClientGroup{Name: "old"}); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			info, err := src.Dump(ctx, &buf)
			if err != nil || info.SchemaVersion != old {
				t.Fatalf("Dump = %+v, %v", info, err)
			}
			cfg := storetest.Config(t, d)
			res, err := store.Load(ctx, cfg, &buf, store.LoadOptions{BackupDir: filepath.Join(t.TempDir(), "backups")}, storetest.Logger())
			if err != nil {
				t.Fatal(err)
			}
			latest, _ := store.LatestVersion(d)
			if res.Dump.SchemaVersion != old || res.SchemaVersion != latest {
				t.Errorf("result = %+v", res)
			}
			if got := groupNames(t, storetest.OpenUnmigrated(t, cfg)); !slices.Equal(got, []string{"old"}) {
				t.Errorf("client groups = %q", got)
			}
		})
	}
}

func TestLoadWhileInUse(t *testing.T) {
	cfg := storetest.Config(t, store.SQLite)
	db := storetest.OpenUnmigrated(t, cfg)
	if _, err := db.Migrate(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := db.Dump(context.Background(), &buf); err != nil {
		t.Fatal(err)
	}
	_, err := store.Load(context.Background(), cfg, &buf, store.LoadOptions{BackupDir: t.TempDir()}, storetest.Logger())
	if !errors.Is(err, store.ErrInUse) {
		t.Fatalf("err = %v, want ErrInUse", err)
	}
}
