package backuptest_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/backuptest"
)

func TestFakeBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	f := backuptest.New()

	s1, err := f.Backup(ctx, "a", backup.Options{Kind: "manual", BackupID: "b1"})
	if err != nil {
		t.Fatal(err)
	}
	if s1.SiteID != "a" || s1.Kind != "manual" || s1.BackupID != "b1" || s1.SizeBytes == 0 {
		t.Errorf("snapshot = %+v", s1)
	}
	if again, _ := f.Backup(ctx, "a", backup.Options{BackupID: "b1"}); again.ID != s1.ID {
		t.Errorf("Backup with the same BackupID took %s, want %s again", again.ID, s1.ID)
	}
	s2, _ := f.Backup(ctx, "a", backup.Options{})
	other, _ := f.Backup(ctx, "b", backup.Options{})

	got, err := f.ListSnapshots(ctx, "a")
	var ids []backup.SnapshotID
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	if err != nil || !slices.Equal(ids, []backup.SnapshotID{s1.ID, s2.ID}) {
		t.Fatalf("ListSnapshots = %v, %v; want [%s %s]", ids, err, s1.ID, s2.ID)
	}

	if err := f.Restore(ctx, "a", s1.ID, "a-staging"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := f.Restore(ctx, "a", other.ID, "a"); !errors.Is(err, backup.ErrSnapshotNotFound) {
		t.Errorf("Restore of another site's snapshot err = %v, want ErrSnapshotNotFound", err)
	}

	if err := f.Prune(ctx, "group1"); err != nil {
		t.Fatal(err)
	}
	calls := f.Calls()
	if last := calls[len(calls)-1]; last.Op != "Prune" || last.ClientGroupID != "group1" {
		t.Errorf("last call = %+v", last)
	}
}

func TestFakeFaultInjection(t *testing.T) {
	injected := errors.New("injected")
	f := backuptest.New()
	f.Err = func(op, id string) error {
		if op == "Prune" && id == "group1" {
			return injected
		}
		return nil
	}
	if err := f.Prune(context.Background(), "group1"); !errors.Is(err, injected) {
		t.Errorf("Prune err = %v, want injected", err)
	}
	if err := f.Prune(context.Background(), "group2"); err != nil {
		t.Errorf("Prune(group2) err = %v", err)
	}
}
