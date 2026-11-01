package restic

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

const summaryLine = `{"message_type":"summary","files_new":2,"data_added":3888,"data_added_packed":2973,` +
	`"total_files_processed":2,"total_bytes_processed":7,"backup_start":"2026-09-28T11:34:11.487320106-04:00",` +
	`"snapshot_id":"3be90c95b9962949a5d839b7fd7adbef112b4a3d667f214a32989f615ce4f20b"}`

func errorLine(item string) string {
	return fmt.Sprintf(`{"message_type":"error","error":{"message":"open %s: permission denied"},"during":"archival","item":%q}`, item, item)
}

func TestBackupResult(t *testing.T) {
	incomplete := `{"message_type":"exit_error","code":3,"message":"Warning: at least one source file could not be read"}`
	var many []string
	for i := range 12 {
		many = append(many, errorLine(fmt.Sprintf("/site/wp-content/f%d", i)))
	}
	tests := []struct {
		name     string
		res      *provisioner.TaskResult
		err      error
		wantErr  []error // wrapped by the error; nil means success
		inErr    string
		warnings []string
	}{
		{
			name: "success",
			res:  &provisioner.TaskResult{Stdout: summaryLine + "\n"},
		},
		{
			name: "unreadable files",
			err: &provisioner.ExecError{ExitCode: 3, Stdout: summaryLine + "\n",
				Stderr: errorLine("/site/wp-content/a") + "\n" + incomplete + "\n"},
			warnings: []string{"open /site/wp-content/a: permission denied"},
		},
		{
			name:     "many unreadable files",
			err:      &provisioner.ExecError{ExitCode: 3, Stdout: summaryLine, Stderr: strings.Join(many, "\n")},
			warnings: nil, // checked below
		},
		{
			name:    "exit 3 without a snapshot",
			err:     &provisioner.ExecError{ExitCode: 3, Stderr: incomplete},
			wantErr: []error{ErrBackupFailed},
			inErr:   "at least one source file",
		},
		{
			name:    "fatal",
			err:     &provisioner.ExecError{ExitCode: 1, Stderr: "Fatal: all source directories/files do not exist\n"},
			wantErr: []error{ErrBackupFailed},
			inErr:   "all source directories",
		},
		{
			name:    "locked",
			err:     &provisioner.ExecError{ExitCode: 11, Stderr: `{"message_type":"exit_error","code":11,"message":"repository is already locked"}`},
			wantErr: []error{ErrBackupFailed, repository.ErrLocked},
			inErr:   "already locked",
		},
		{
			name:    "wrong password",
			err:     &provisioner.ExecError{ExitCode: 12},
			wantErr: []error{ErrBackupFailed, repository.ErrWrongPassword},
		},
		{
			name:    "killed, with nothing from restic",
			err:     &provisioner.ExecError{ExitCode: 137},
			wantErr: []error{ErrBackupFailed},
			inErr:   "(exit status 137): killed, probably for exceeding the task's memory limit: raise backup.task_memory_mb",
		},
		{
			name:    "killed, after restic said something",
			err:     &provisioner.ExecError{ExitCode: 137, Stderr: "Fatal: something went wrong\n"},
			wantErr: []error{ErrBackupFailed},
			inErr:   "Fatal: something went wrong (killed, probably",
		},
		{
			name:    "no summary",
			res:     &provisioner.TaskResult{Stdout: "something else\n"},
			wantErr: []error{ErrBackupFailed},
			inErr:   "no snapshot ID",
		},
		{
			name:    "task error",
			err:     provisioner.ErrSiteNotFound,
			wantErr: []error{provisioner.ErrSiteNotFound},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap, err := backupResult(tt.res, tt.err)
			if tt.wantErr != nil {
				for _, want := range tt.wantErr {
					if !errors.Is(err, want) {
						t.Errorf("err = %v, want %v", err, want)
					}
				}
				if !strings.Contains(fmt.Sprint(err), tt.inErr) {
					t.Errorf("err = %v, want it to mention %q", err, tt.inErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if snap.ID != "3be90c95b9962949a5d839b7fd7adbef112b4a3d667f214a32989f615ce4f20b" || snap.FileCount != 2 ||
				snap.SizeBytes != 7 || snap.AddedBytes != 2973 || snap.Time.Location().String() != "UTC" || snap.Time.Hour() != 15 {
				t.Errorf("snapshot = %+v", snap)
			}
			if tt.name == "many unreadable files" {
				if len(snap.Warnings) != maxWarnings+1 || snap.Warnings[maxWarnings] != "and 2 more" {
					t.Errorf("warnings = %q", snap.Warnings)
				}
				return
			}
			if !slices.Equal(snap.Warnings, tt.warnings) {
				t.Errorf("warnings = %q, want %q", snap.Warnings, tt.warnings)
			}
		})
	}
}

func TestParseSnapshotsRejectsGarbage(t *testing.T) {
	if _, err := parseSnapshots([]byte("not json")); err == nil {
		t.Error("parseSnapshots accepted garbage")
	}
	if _, err := parseSnapshots([]byte(`[{"time":"2026-09-28T11:34:11Z"}]`)); err == nil {
		t.Error("parseSnapshots accepted a snapshot without an ID")
	}
	snaps, err := parseSnapshots([]byte(`[{"id":"b","time":"2026-09-28T12:00:00Z","tags":["site:x"]},{"id":"a","time":"2026-09-28T11:00:00Z"}]`))
	if err != nil || len(snaps) != 2 || snaps[0].ID != "a" || snaps[1].SiteID != "x" || snaps[0].SizeBytes != 0 {
		t.Errorf("parseSnapshots = %+v, %v", snaps, err)
	}
}

// A killed restore prints nothing either, and the job's rollback runs another
// restore in the same limit, so the operator needs to be told what to change.
func TestRestoreResultKilled(t *testing.T) {
	_, err := restoreResult(nil, &provisioner.ExecError{ExitCode: 137})
	if !errors.Is(err, ErrRestoreFailed) || !strings.Contains(fmt.Sprint(err), "(exit status 137): "+repository.KilledHint) {
		t.Errorf("err = %v", err)
	}
	if _, err := restoreResult(nil, &provisioner.ExecError{ExitCode: 1}); strings.Contains(fmt.Sprint(err), "backup.task_memory_mb") {
		t.Errorf("another failure names the memory limit: %v", err)
	}
}
