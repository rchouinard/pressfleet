package cli

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Without markers, as a control plane from before them leaves, the backups'
// names tell whether the upgrade migrated.
func TestFindMigrationByBackupName(t *testing.T) {
	const dir = "/data/state/backups"
	start := time.Date(2026, 10, 31, 20, 25, 0, 0, time.UTC)
	old := "pressfleet-20200101T000000.000Z-schema0001.db"
	first := "pressfleet-20261031T202537.624Z-schema0029.db"
	second := "pressfleet-20261031T202901.000Z-schema0035.db"

	// lister reads names, none of them markers.
	lister := func(names []string, err error, calls *int) backupsReader {
		return backupsReader{
			list: func(context.Context, string) ([]string, error) {
				if calls != nil {
					*calls++
				}
				return names, err
			},
			read: func(_ context.Context, p string) ([]byte, error) {
				t.Errorf("reading %s, which isn't a marker", p)
				return nil, fs.ErrNotExist
			},
		}
	}
	denied := &fs.PathError{Op: "open", Path: dir, Err: fs.ErrPermission}

	tests := []struct {
		name      string
		since     time.Time
		host      backupsReader
		fallbacks []backupsReader
		want      string
		wantErr   bool
		// fallbackCalls is how many of the fallbacks run.
		fallbackCalls int
	}{
		{
			name:  "readable with a backup, which is the first after the start",
			since: start,
			host:  lister([]string{old, second, first, "notes.txt", "pressfleet-bad.db"}, nil, nil),
			want:  filepath.Join(dir, first),
		},
		{
			name:  "readable and empty",
			since: start,
			host:  lister(nil, nil, nil),
		},
		{
			name:  "readable with only backups from before the upgrade",
			since: start,
			host:  lister([]string{old}, nil, nil),
		},
		{
			name:  "missing directory",
			since: start,
			host:  lister(nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrNotExist}, nil),
		},
		{
			name:      "unreadable, listed through Docker",
			since:     start,
			host:      lister(nil, denied, nil),
			fallbacks: []backupsReader{lister([]string{old, first}, nil, nil)},
			want:      filepath.Join(dir, first),
		},
		{
			name:      "unreadable, listed through Docker and empty",
			since:     start,
			host:      lister(nil, denied, nil),
			fallbacks: []backupsReader{lister([]string{old}, nil, nil)},
		},
		{
			name:  "unreadable, the first Docker listing fails and the second works",
			since: start,
			host:  lister(nil, denied, nil),
			fallbacks: []backupsReader{
				lister(nil, errors.New("pull access denied"), nil),
				lister([]string{first}, nil, nil),
			},
			want: filepath.Join(dir, first),
		},
		{
			name:  "unreadable and every Docker listing fails",
			since: start,
			host:  lister(nil, denied, nil),
			fallbacks: []backupsReader{
				lister(nil, errors.New("no such image"), nil),
				lister(nil, errors.New("crash loop"), nil),
			},
			wantErr: true,
		},
		{
			name:    "unreadable and no Docker listing",
			since:   start,
			host:    lister(nil, denied, nil),
			wantErr: true,
		},
		{
			// Docker's clock a few seconds behind the host's.
			name:  "backup stamped a few seconds before the start",
			since: start.Add(37*time.Second + 700*time.Millisecond),
			host:  lister([]string{first}, nil, nil),
			want:  filepath.Join(dir, first),
		},
		{
			name:  "backup stamped longer before the start than the skew",
			since: start.Add(37*time.Second + 624*time.Millisecond + backupClockSkew + time.Second),
			host:  lister([]string{first}, nil, nil),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findMigration(t.Context(), dir, tt.since, tt.host, tt.fallbacks...)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, want error: %t", err, tt.wantErr)
			}
			if got.Backup != tt.want || got.Migrated() != (tt.want != "") || len(got.Markers) != 0 {
				t.Errorf("migration = %+v, want backup %q from its name", got, tt.want)
			}
		})
	}
}

// A missing directory is decided on the host: Docker isn't asked.
func TestFindMigrationMissingDirSkipsDocker(t *testing.T) {
	calls := 0
	host := backupsReader{list: func(context.Context, string) ([]string, error) { return nil, fs.ErrNotExist }}
	via := backupsReader{list: func(context.Context, string) ([]string, error) { calls++; return nil, nil }}
	if _, err := findMigration(t.Context(), "/x", time.Now(), host, via); err != nil || calls != 0 {
		t.Errorf("error = %v, Docker listings = %d", err, calls)
	}
}

func TestRecoveryStepForUnknownBackup(t *testing.T) {
	var b strings.Builder
	err := recoveryTemplate.Execute(&b, recoveryData{
		From: "v0.1.0", Project: "pressfleet", PlatformDir: "/d/platform", PreviousDir: "/d/upgrade/previous-platform",
		PreviousImage: "img@sha256:1", BackupUnchecked: true, BackupsDir: "/d/state/backups", Since: "20261031T202500.000Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"couldn't check whether the control plane migrated",
		"--project-directory /d/upgrade/previous-platform run --rm --no-deps --no-TTY --entrypoint ls control-plane -1 /d/state/backups\n",
		"timestamp of 20261031T202500.000Z or later",
		"state restore /d/state/backups/<file name from the listing>",
		"database schema is newer than this pressfleet binary supports",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("steps lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "needs no restore") {
		t.Errorf("steps claim no restore is needed without knowing:\n%s", out)
	}
}

func TestRecoveryStepForNoBackup(t *testing.T) {
	var b strings.Builder
	if err := recoveryTemplate.Execute(&b, recoveryData{From: "v0.1.0"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "The state database needs no restore") {
		t.Errorf("steps for a checked directory without a backup:\n%s", b.String())
	}
}
