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

	if err := f.Restore(ctx, "a", s1.ID, "a-staging", backup.FullRestore()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := f.Restore(ctx, "a", other.ID, "a", backup.FullRestore()); !errors.Is(err, backup.ErrSnapshotNotFound) {
		t.Errorf("Restore of another site's snapshot err = %v, want ErrSnapshotNotFound", err)
	}

	if res, err := f.Prune(ctx, "group1"); err != nil || len(res.Forgotten) != 0 {
		t.Fatalf("Prune without a policy = %+v, %v", res, err)
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
	if _, err := f.Prune(context.Background(), "group1"); !errors.Is(err, injected) {
		t.Errorf("Prune err = %v, want injected", err)
	}
	if _, err := f.Prune(context.Background(), "group2"); err != nil {
		t.Errorf("Prune(group2) err = %v", err)
	}
}

func TestFakeRetention(t *testing.T) {
	ctx := context.Background()
	f := backuptest.New()
	f.KeepLast = 1
	f.SetGroup("a", "g1")
	f.SetGroup("b", "g1")
	f.SetGroup("c", "g2")
	a1, _ := f.Backup(ctx, "a", backup.Options{})
	a2, _ := f.Backup(ctx, "a", backup.Options{})
	b1, _ := f.Backup(ctx, "b", backup.Options{})
	c1, _ := f.Backup(ctx, "c", backup.Options{})
	c2, _ := f.Backup(ctx, "c", backup.Options{})

	snaps, err := f.GroupSnapshots(ctx, "g1")
	if err != nil || len(snaps) != 3 {
		t.Fatalf("GroupSnapshots(g1) = %v, %v", snaps, err)
	}
	res, err := f.Prune(ctx, "g1")
	if err != nil || !slices.Equal(res.Forgotten, []backup.SnapshotID{a1.ID}) {
		t.Errorf("Prune(g1) = %+v, %v; want %s forgotten", res, err, a1.ID)
	}
	var ids []backup.SnapshotID
	snaps, _ = f.GroupSnapshots(ctx, "g1")
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	slices.Sort(ids)
	if want := []backup.SnapshotID{a2.ID, b1.ID}; !slices.Equal(ids, want) {
		t.Errorf("after Prune(g1): %v, want %v", ids, want)
	}
	if snaps, _ := f.ListSnapshots(ctx, "c"); len(snaps) != 2 || snaps[0].ID != c1.ID || snaps[1].ID != c2.ID {
		t.Errorf("another group's snapshots changed: %v", snaps)
	}
	if err := f.Check(ctx, "g2"); err != nil {
		t.Errorf("Check = %v", err)
	}
}

func TestFakeForgetSite(t *testing.T) {
	ctx := context.Background()
	f := backuptest.New()
	a1, _ := f.Backup(ctx, "a", backup.Options{})
	a2, _ := f.Backup(ctx, "a", backup.Options{})
	b1, _ := f.Backup(ctx, "b", backup.Options{})

	res, err := f.ForgetSite(ctx, "a")
	if err != nil || !slices.Equal(res.Forgotten, []backup.SnapshotID{a1.ID, a2.ID}) {
		t.Fatalf("ForgetSite = %+v, %v; want [%s %s]", res, err, a1.ID, a2.ID)
	}
	if left, _ := f.ListSnapshots(ctx, "a"); len(left) != 0 {
		t.Errorf("site a still has %d snapshots", len(left))
	}
	if left, _ := f.ListSnapshots(ctx, "b"); len(left) != 1 || left[0].ID != b1.ID {
		t.Errorf("site b's snapshots = %+v, want only %s", left, b1.ID)
	}
	if res, err := f.ForgetSite(ctx, "a"); err != nil || len(res.Forgotten) != 0 {
		t.Errorf("second ForgetSite = %+v, %v", res, err)
	}
}
