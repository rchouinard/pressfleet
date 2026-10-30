package backup_test

import (
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
)

func TestRestoreOptionsValidate(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts backup.RestoreOptions
		ok   bool
	}{
		{"full", backup.FullRestore(), true},
		{"all files only", backup.RestoreOptions{Files: backup.RestoreFilesAll}, true},
		{"code only", backup.RestoreOptions{Files: backup.RestoreFilesCode}, true},
		{"code and database", backup.RestoreOptions{Files: backup.RestoreFilesCode, Database: true}, true},
		{"database only", backup.RestoreOptions{Files: backup.RestoreFilesNone, Database: true}, true},
		{"nothing", backup.RestoreOptions{Files: backup.RestoreFilesNone}, false},
		{"zero value", backup.RestoreOptions{}, false},
		{"unknown files scope", backup.RestoreOptions{Files: "most", Database: true}, false},
	} {
		err := tt.opts.Validate()
		if tt.ok != (err == nil) || (err != nil && !errors.Is(err, backup.ErrInvalidRestoreOptions)) {
			t.Errorf("%s: Validate = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
	if got := backup.FullRestore(); got.Files != backup.RestoreFilesAll || !got.Database {
		t.Errorf("FullRestore = %+v", got)
	}
}
