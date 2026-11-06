package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// fakeBackups is a state backups directory as one reader sees it.
type fakeBackups struct {
	files   map[string]string // name -> contents
	listErr error
	readErr error
}

func (f fakeBackups) reader() backupsReader {
	return backupsReader{
		list: func(context.Context, string) ([]string, error) {
			if f.listErr != nil {
				return nil, f.listErr
			}
			names := make([]string, 0, len(f.files))
			for name := range f.files {
				names = append(names, name)
			}
			return names, nil
		},
		read: func(_ context.Context, p string) ([]byte, error) {
			if f.readErr != nil {
				return nil, f.readErr
			}
			data, ok := f.files[filepath.Base(p)]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return []byte(data), nil
		},
	}
}

func markerJSON(t *testing.T, m store.MigrationMarker) string {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func markerFile(at time.Time, from int) string {
	return fmt.Sprintf("migration-%s-schema%04d.json", at.UTC().Format(backupTimeLayout), from)
}

func TestFindMigrationFromMarkers(t *testing.T) {
	const dir = "/data/state/backups"
	start := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	at := start.Add(20 * time.Second)
	backupAt := at.Add(-time.Second)
	backup := fmt.Sprintf("pressfleet-%s-schema0035.db", backupAt.Format(backupTimeLayout))
	// The control plane may name the backup by a path of its own.
	backupPath := "/elsewhere/" + backup
	sqliteDone := store.MigrationMarker{
		Status: store.MigrationSucceeded, StartedAt: at, FinishedAt: at.Add(time.Second), Version: "v1.3.0",
		Dialect: store.SQLite, FromSchema: 35, ToSchema: 37, ReachedSchema: 37, Backup: backupPath,
	}
	pgDone := store.MigrationMarker{
		Status: store.MigrationSucceeded, StartedAt: at, FinishedAt: at.Add(time.Second), Version: "v1.3.0",
		Dialect: store.Postgres, FromSchema: 35, ToSchema: 37, ReachedSchema: 37,
	}
	pgFailedFirst := pgDone
	pgFailedFirst.Status, pgFailedFirst.ReachedSchema, pgFailedFirst.Error = store.MigrationFailed, 35, "migration 0036: boom"
	pgRunning := pgDone
	pgRunning.Status, pgRunning.FinishedAt, pgRunning.ReachedSchema = store.MigrationRunning, time.Time{}, 35
	// A run that failed after one migration, and the retry that finished.
	sqliteFailed := sqliteDone
	sqliteFailed.Status, sqliteFailed.ReachedSchema, sqliteFailed.Error = store.MigrationFailed, 36, "migration 0037: boom"
	later := at.Add(time.Minute)
	retryBackup := fmt.Sprintf("pressfleet-%s-schema0036.db", later.Format(backupTimeLayout))
	sqliteRetry := sqliteDone
	sqliteRetry.StartedAt, sqliteRetry.FromSchema, sqliteRetry.Backup = later, 36, "/elsewhere/"+retryBackup
	// A migration from before the upgrade.
	old := sqliteDone
	old.StartedAt = start.Add(-time.Hour)
	oldBackup := fmt.Sprintf("pressfleet-%s-schema0030.db", old.StartedAt.Format(backupTimeLayout))
	old.Backup = oldBackup

	denied := &fs.PathError{Op: "open", Path: dir, Err: fs.ErrPermission}
	sqliteDir := map[string]string{
		backup: "", markerFile(at, 35): markerJSON(t, sqliteDone),
		oldBackup: "", markerFile(old.StartedAt, 30): markerJSON(t, old),
	}

	tests := []struct {
		name         string
		host         fakeBackups
		fallbacks    []fakeBackups
		wantMarkers  int
		wantMigrated bool
		wantBackup   string
		wantErr      bool
	}{
		{
			name:         "SQLite, readable",
			host:         fakeBackups{files: sqliteDir},
			wantMarkers:  1,
			wantMigrated: true,
			wantBackup:   filepath.Join(dir, backup),
		},
		{
			name:         "SQLite, read through Docker",
			host:         fakeBackups{listErr: denied},
			fallbacks:    []fakeBackups{{files: sqliteDir}},
			wantMarkers:  1,
			wantMigrated: true,
			wantBackup:   filepath.Join(dir, backup),
		},
		{
			name:         "SQLite, the first Docker read fails and the second works",
			host:         fakeBackups{listErr: denied},
			fallbacks:    []fakeBackups{{files: sqliteDir, readErr: errors.New("no such image")}, {files: sqliteDir}},
			wantMarkers:  1,
			wantMigrated: true,
			wantBackup:   filepath.Join(dir, backup),
		},
		{
			name: "SQLite, a failed run and its retry: the first backup",
			host: fakeBackups{files: map[string]string{
				backup: "", markerFile(at, 35): markerJSON(t, sqliteFailed),
				retryBackup: "", markerFile(later, 36): markerJSON(t, sqliteRetry),
			}},
			wantMarkers:  2,
			wantMigrated: true,
			wantBackup:   filepath.Join(dir, backup),
		},
		{
			name: "only markers from before the upgrade: by file name",
			host: fakeBackups{files: map[string]string{
				oldBackup: "", markerFile(old.StartedAt, 30): markerJSON(t, old),
			}},
		},
		{
			name:         "PostgreSQL, migrated",
			host:         fakeBackups{files: map[string]string{markerFile(at, 35): markerJSON(t, pgDone)}},
			wantMarkers:  1,
			wantMigrated: true,
		},
		{
			name:         "PostgreSQL, read through Docker",
			host:         fakeBackups{listErr: denied},
			fallbacks:    []fakeBackups{{files: map[string]string{markerFile(at, 35): markerJSON(t, pgDone)}}},
			wantMarkers:  1,
			wantMigrated: true,
		},
		{
			name:        "PostgreSQL, failed before changing anything",
			host:        fakeBackups{files: map[string]string{markerFile(at, 35): markerJSON(t, pgFailedFirst)}},
			wantMarkers: 1,
		},
		{
			name:         "PostgreSQL, never said how it ended",
			host:         fakeBackups{files: map[string]string{markerFile(at, 35): markerJSON(t, pgRunning)}},
			wantMarkers:  1,
			wantMigrated: true,
		},
		{
			name:    "a corrupt marker",
			host:    fakeBackups{files: map[string]string{markerFile(at, 35): "{"}},
			wantErr: true,
		},
		{
			name:      "unreadable, and no Docker read works",
			host:      fakeBackups{listErr: denied},
			fallbacks: []fakeBackups{{files: sqliteDir, readErr: errors.New("crash loop")}},
			wantErr:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fallbacks []backupsReader
			for _, f := range tt.fallbacks {
				fallbacks = append(fallbacks, f.reader())
			}
			got, err := findMigration(t.Context(), dir, start, tt.host.reader(), fallbacks...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, want error: %t", err, tt.wantErr)
			}
			if len(got.Markers) != tt.wantMarkers || got.Migrated() != tt.wantMigrated || got.Backup != tt.wantBackup {
				t.Errorf("migration = %+v (migrated %t), want %d markers, migrated %t, backup %q",
					got, got.Migrated(), tt.wantMarkers, tt.wantMigrated, tt.wantBackup)
			}
		})
	}
}

// The CLI finds what the store's Migrate recorded, on both dialects, from
// the host and through Docker.
func TestFindMigrationFromStore(t *testing.T) {
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			ctx := t.Context()
			cfg := storetest.Config(t, d)
			cfg.Version = "v9.9.9"
			db := storetest.OpenUnmigrated(t, cfg)
			// Data to protect, so SQLite takes a backup.
			if _, err := db.ExecContext(ctx, "CREATE TABLE legacy (x INTEGER)"); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(t.TempDir(), "backups")
			start := time.Now()
			res, err := db.Migrate(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			host := backupsReader{list: hostListDir, read: hostReadFile}
			denied := backupsReader{list: func(context.Context, string) ([]string, error) {
				return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrPermission}
			}}
			for name, readers := range map[string][]backupsReader{
				"host":   {host},
				"docker": {denied, {list: hostListDir, read: func(_ context.Context, p string) ([]byte, error) { return os.ReadFile(p) }}},
			} {
				t.Run(name, func(t *testing.T) {
					got, err := findMigration(ctx, dir, start, readers[0], readers[1:]...)
					if err != nil {
						t.Fatal(err)
					}
					if len(got.Markers) != 1 || !got.Migrated() {
						t.Fatalf("migration = %+v, want one marker that migrated", got)
					}
					m := got.Markers[0]
					if m.Dialect != d || m.Version != "v9.9.9" || m.Status != store.MigrationSucceeded ||
						m.FromSchema != res.From || m.ReachedSchema != res.To {
						t.Errorf("marker = %+v, want %s v9.9.9 succeeded %d -> %d", m, d, res.From, res.To)
					}
					if got.Backup != res.BackupPath {
						t.Errorf("backup = %q, want %q", got.Backup, res.BackupPath)
					}
					if d == store.SQLite && got.Backup == "" {
						t.Error("no SQLite backup found")
					}
				})
			}
			// Nothing migrated after this.
			got, err := findMigration(ctx, dir, time.Now().Add(time.Minute), host)
			if err != nil || got.Migrated() || len(got.Markers) != 0 {
				t.Errorf("migration after the last one = %+v, %v", got, err)
			}
		})
	}
}
