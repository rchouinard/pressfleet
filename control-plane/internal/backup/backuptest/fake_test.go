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

	s1, err := f.Backup(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := f.Backup(ctx, "a")
	other, _ := f.Backup(ctx, "b")

	got, err := f.ListSnapshots(ctx, "a")
	if err != nil || !slices.Equal(got, []backup.SnapshotID{s1, s2}) {
		t.Fatalf("ListSnapshots = %v, %v; want [%s %s]", got, err, s1, s2)
	}

	if err := f.Restore(ctx, "a", s1, "a-staging"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := f.Restore(ctx, "a", other, "a"); !errors.Is(err, backup.ErrSnapshotNotFound) {
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
