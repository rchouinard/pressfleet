package backuptest_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/backuptest"
)

func TestFakeRecordsRestoreOptions(t *testing.T) {
	ctx := context.Background()
	f := backuptest.New()
	s, _ := f.Backup(ctx, "a", backup.Options{})
	scope := backup.RestoreOptions{Files: backup.RestoreFilesCode}
	if err := f.Restore(ctx, "a", s.ID, "a-staging", scope); err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	if last := calls[len(calls)-1]; last.Op != "Restore" || last.Restore != scope || last.TargetSiteID != "a-staging" {
		t.Errorf("last call = %+v", last)
	}
	if err := f.Restore(ctx, "a", s.ID, "a", backup.RestoreOptions{}); !errors.Is(err, backup.ErrInvalidRestoreOptions) {
		t.Errorf("Restore with an empty scope = %v, want ErrInvalidRestoreOptions", err)
	}
}
