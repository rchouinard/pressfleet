package store_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// Migration 0036 marks the repository of a group with a succeeded backup
// as opened by its password, and leaves every other repository to be
// opened once, including one whose backups have only failed, as they do
// after a state restore that predates the repository (#277).
func TestMigrateBackupRepositoryPasswordVerified(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			db := storetest.OpenUnmigrated(t, storetest.Config(t, d))
			if _, err := db.MigrateTo(ctx, t.TempDir(), 35); err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			groups := map[string]string{}
			for _, tc := range []struct {
				name     string
				statuses []string
			}{
				{"succeeded", []string{"failed", "succeeded"}},
				{"failed", []string{"failed", "running"}},
				{"none", nil},
			} {
				group := createGroup(ctx, t, db)
				groups[tc.name] = group
				secret := id.New()
				if _, err := db.ExecContext(ctx, "INSERT INTO secrets (id, kind, ciphertext, created_at, updated_at) VALUES (?, 'restic_repo_key', ?, ?, ?)",
					secret, []byte("x"), at, at); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, "INSERT INTO backup_repositories (client_group_id, backend, location, secret_ref, created_at) VALUES (?, 'local', ?, ?, ?)",
					group, "/data/backups/repositories/"+group, secret, at); err != nil {
					t.Fatal(err)
				}
				for _, status := range tc.statuses {
					if _, err := db.ExecContext(ctx, "INSERT INTO backup_records (id, site_id, client_group_id, kind, status, wp_core_version, created_at, started_at) "+
						"VALUES (?, ?, ?, 'manual', ?, '7.1.2', ?, ?)", id.New(), id.New(), group, status, at, at); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := db.Migrate(ctx, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			for name, group := range groups {
				var verified sql.NullTime
				if err := db.QueryRowContext(ctx, "SELECT password_verified_at FROM backup_repositories WHERE client_group_id = ?", group).Scan(&verified); err != nil {
					t.Fatal(err)
				}
				if want := name == "succeeded"; verified.Valid != want || (want && !verified.Time.Equal(at)) {
					t.Errorf("%s: password_verified_at = %+v, want set %v (to %v)", name, verified, want, at)
				}
			}
		})
	}
}
