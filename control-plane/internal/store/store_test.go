package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestMigrationsMatchAcrossDialects(t *testing.T) {
	sqlite, err := store.Migrations(store.SQLite)
	if err != nil {
		t.Fatal(err)
	}
	pg, err := store.Migrations(store.Postgres)
	if err != nil {
		t.Fatal(err)
	}
	if len(sqlite) == 0 || len(sqlite) != len(pg) {
		t.Fatalf("sqlite has %d migrations, postgres has %d", len(sqlite), len(pg))
	}
	for i := range sqlite {
		if sqlite[i].Version != pg[i].Version || sqlite[i].Name != pg[i].Name {
			t.Errorf("migration %d: sqlite %04d_%s, postgres %04d_%s",
				i, sqlite[i].Version, sqlite[i].Name, pg[i].Version, pg[i].Name)
		}
	}
}

func TestMigrate(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			db := storetest.OpenUnmigrated(t, storetest.Config(t, d))
			latest, err := store.LatestVersion(d)
			if err != nil {
				t.Fatal(err)
			}
			if v, err := db.SchemaVersion(ctx); err != nil || v != 0 {
				t.Fatalf("SchemaVersion before migrating = %d, %v", v, err)
			}
			backups := filepath.Join(t.TempDir(), "backups")
			res, err := db.Migrate(ctx, backups)
			if err != nil {
				t.Fatal(err)
			}
			if res.From != 0 || res.To != latest || res.BackupPath != "" {
				t.Errorf("first Migrate = %+v, want 0 -> %d without backup", res, latest)
			}
			if v, err := db.SchemaVersion(ctx); err != nil || v != latest {
				t.Errorf("SchemaVersion = %d, %v; want %d", v, err, latest)
			}
			res, err = db.Migrate(ctx, backups)
			if err != nil || res.From != latest || res.To != latest || res.BackupPath != "" {
				t.Errorf("second Migrate = %+v, %v; want no-op", res, err)
			}
			if _, err := os.Stat(backups); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("backup directory created without a backup: %v", err)
			}
		})
	}
}

func TestMigrateBacksUpExistingData(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			db := storetest.OpenUnmigrated(t, storetest.Config(t, d))
			// A database holding data but no PressFleet schema yet.
			if _, err := db.ExecContext(ctx, "CREATE TABLE legacy (x INTEGER)"); err != nil {
				t.Fatal(err)
			}
			res, err := db.Migrate(ctx, filepath.Join(t.TempDir(), "backups"))
			if err != nil {
				t.Fatal(err)
			}
			switch d {
			case store.SQLite:
				if _, err := os.Stat(res.BackupPath); err != nil {
					t.Errorf("no backup before migrating: %+v, %v", res, err)
				}
			case store.Postgres:
				if res.BackupPath != "" {
					t.Errorf("BackupPath = %q, want none for PostgreSQL", res.BackupPath)
				}
			}
		})
	}
}

func TestSchemaTooNew(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		latest, err := store.LatestVersion(db.Dialect())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
			latest+1, "from_the_future", time.Now()); err != nil {
			t.Fatal(err)
		}
		_, err = db.Migrate(ctx, t.TempDir())
		if !errors.Is(err, store.ErrSchemaTooNew) {
			t.Fatalf("Migrate err = %v, want ErrSchemaTooNew", err)
		}
		for _, want := range []string{"newer than this pressfleet binary supports", "version " + strconv.Itoa(latest+1), "up to " + strconv.Itoa(latest)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	})
}

func TestTimestampsAreUTC(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		in := time.Date(2026, 9, 26, 8, 30, 0, 123456789, time.FixedZone("EDT", -4*3600))
		var out time.Time
		err := db.QueryRowContext(ctx,
			"INSERT INTO secrets (id, kind, ciphertext, created_at, updated_at) VALUES (?, ?, ?, ?, ?) RETURNING created_at",
			id.New(), "test", []byte{1, 2, 3}, in, in).Scan(&out)
		if err != nil {
			t.Fatal(err)
		}
		want := time.Date(2026, 9, 26, 12, 30, 0, 123456000, time.UTC)
		if out != want {
			t.Errorf("round-tripped %v as %v (location %v), want %v", in, out, out.Location(), want)
		}
		// Ordering comparisons must work on stored values.
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM secrets WHERE created_at > ? AND created_at < ?",
			want.Add(-time.Microsecond), want.Add(time.Microsecond)).Scan(&n); err != nil || n != 1 {
			t.Errorf("range query matched %d rows, err %v", n, err)
		}
	})
}

func TestInTx(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		boom := errors.New("boom")
		err := db.InTx(ctx, func(q store.Querier) error {
			if err := store.NewClientGroups(q).Create(ctx, &store.ClientGroup{Name: "rolled back"}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("InTx err = %v", err)
		}
		err = db.InTx(ctx, func(q store.Querier) error {
			return store.NewClientGroups(q).Create(ctx, &store.ClientGroup{Name: "committed"})
		})
		if err != nil {
			t.Fatal(err)
		}
		groups, err := store.NewClientGroups(db).List(ctx, store.ClientGroupFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(groups) != 1 || groups[0].Name != "committed" {
			t.Errorf("groups = %+v, want only the committed one", groups)
		}
	})
}

// TestSchemaConstraints checks the constraints later issues rely on.
func TestSchemaConstraints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		g := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, g); err != nil {
			t.Fatal(err)
		}
		uid := 20000
		insertSite := func(slug, groupID string, parent any) (string, error) {
			siteID := id.New()
			now := time.Now()
			uid++
			_, err := db.ExecContext(ctx, `INSERT INTO sites
				(id, client_group_id, parent_site_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, 'provisioning', ?, '8.4', '6.9.1', ?, ?, ?)`,
				siteID, groupID, parent, slug, slug, slug+".preview.example", uid, now, now)
			return siteID, err
		}

		prod, err := insertSite("prod", g.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		staging, err := insertSite("staging", g.ID, prod)
		if err != nil {
			t.Fatalf("inserting a site with parent_site_id: %v", err)
		}

		var parent *string
		var cache bool
		var memRes, memLimit int
		if err := db.QueryRowContext(ctx,
			"SELECT parent_site_id, enable_cache, memory_reservation_mb, memory_limit_mb FROM sites WHERE id = ?",
			staging).Scan(&parent, &cache, &memRes, &memLimit); err != nil {
			t.Fatal(err)
		}
		if parent == nil || *parent != prod || !cache || memRes != 128 || memLimit != 512 {
			t.Errorf("staging site: parent=%v cache=%v mem=%d/%d", parent, cache, memRes, memLimit)
		}

		if _, err := insertSite("prod", g.ID, nil); !errors.Is(err, store.ErrConflict) {
			t.Errorf("duplicate slug: err = %v, want ErrConflict", err)
		}
		if _, err := insertSite("orphan", id.New(), nil); !errors.Is(err, store.ErrReference) {
			t.Errorf("unknown client group: err = %v, want ErrReference", err)
		}
		if _, err := db.ExecContext(ctx, "DELETE FROM sites WHERE id = ?", prod); !errors.Is(err, store.ErrReference) {
			t.Errorf("deleting a parent site: err = %v, want ErrReference", err)
		}

		// Job logs get increasing IDs from the database on both dialects.
		now := time.Now()
		jobID := id.New()
		if _, err := db.ExecContext(ctx, `INSERT INTO jobs (id, type, target_type, target_id, state, run_after, created_at, updated_at)
			VALUES (?, 'site.create', 'site', ?, 'queued', ?, ?, ?)`, jobID, prod, now, now, now); err != nil {
			t.Fatal(err)
		}
		var ids []int64
		for _, msg := range []string{"first", "second"} {
			var logID int64
			if err := db.QueryRowContext(ctx,
				"INSERT INTO job_logs (job_id, logged_at, level, message) VALUES (?, ?, 'info', ?) RETURNING id",
				jobID, now, msg).Scan(&logID); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, logID)
		}
		if ids[1] <= ids[0] {
			t.Errorf("job log IDs not increasing: %v", ids)
		}
		// Deleting a job removes its logs.
		if _, err := db.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", jobID); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM job_logs").Scan(&n); err != nil || n != 0 {
			t.Errorf("job logs after deleting job: %d, %v", n, err)
		}
	})
}

// TestSchemaParity compares tables and columns across dialects, so the two
// migration directories can't drift apart.
func TestSchemaParity(t *testing.T) {
	ctx := context.Background()
	schemas := map[store.Dialect][]string{}
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			db := storetest.Open(t, d)
			query := "SELECT m.name || '.' || p.name FROM sqlite_schema m, pragma_table_info(m.name) p WHERE m.type = 'table'"
			if d == store.Postgres {
				query = "SELECT table_name || '.' || column_name FROM information_schema.columns WHERE table_schema = current_schema()"
			}
			rows, err := db.QueryContext(ctx, query)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rows.Close() }()
			var cols []string
			for rows.Next() {
				var c string
				if err := rows.Scan(&c); err != nil {
					t.Fatal(err)
				}
				cols = append(cols, c)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			sort.Strings(cols)
			schemas[d] = cols
		})
	}
	sqlite, pg := schemas[store.SQLite], schemas[store.Postgres]
	if pg == nil {
		return // PostgreSQL skipped
	}
	if strings.Join(sqlite, "\n") != strings.Join(pg, "\n") {
		t.Errorf("schemas differ:\nsqlite:\n  %s\npostgres:\n  %s", strings.Join(sqlite, "\n  "), strings.Join(pg, "\n  "))
	}
}

func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := store.Open(ctx, store.Config{Driver: "mysql"}, storetest.Logger()); err == nil {
		t.Error("unknown driver accepted")
	}
	_, err := store.Open(ctx, store.Config{Driver: store.Postgres, PostgresDSN: "postgres://u:hunter2@[bad"}, storetest.Logger())
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("bad DSN: err = %v (must fail without echoing the password)", err)
	}
}
