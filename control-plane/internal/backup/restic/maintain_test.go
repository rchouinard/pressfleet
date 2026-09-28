package restic_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/restic"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func snapshotIDs(snaps []backup.Snapshot) []backup.SnapshotID {
	ids := make([]backup.SnapshotID, 0, len(snaps))
	for _, s := range snaps {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestPrune(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, storetest.Open(t, store.SQLite), func(o *restic.Options) {
		o.Retention = backup.Retention{KeepLast: 2}
		o.CheckReadDataPercent = 100
	})
	groupID := f.group(t, "Acme")
	site := f.site(t, groupID, "acme")
	other := f.site(t, groupID, "acme-shop")
	var taken []backup.SnapshotID
	for i := range 4 {
		// Each backup changes something, so each one adds data that only
		// it (and later ones) use.
		if err := os.WriteFile(filepath.Join(f.siteRoots[site.ID], "wp-content", "uploads", "new.txt"), []byte{byte(i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		snap, err := f.engine.Backup(ctx, site.ID, backup.Options{Kind: "scheduled"})
		if err != nil {
			t.Fatal(err)
		}
		taken = append(taken, snap.ID)
	}
	otherSnap, err := f.engine.Backup(ctx, other.ID, backup.Options{Kind: "scheduled"})
	if err != nil {
		t.Fatal(err)
	}

	all, err := f.engine.GroupSnapshots(ctx, groupID)
	if err != nil || len(all) != 5 {
		t.Fatalf("GroupSnapshots = %v, %v; want 5", snapshotIDs(all), err)
	}

	// Each site keeps its own last two: the other site's only snapshot
	// stays although it isn't among the group's last two.
	res, err := f.engine.Prune(ctx, groupID)
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Clone(res.Forgotten)
	slices.Sort(got)
	want := slices.Clone(taken[:2])
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("forgotten = %v, want %v", got, want)
	}
	left, err := f.engine.ListSnapshots(ctx, site.ID)
	if err != nil || !slices.Equal(snapshotIDs(left), taken[2:]) {
		t.Errorf("site's snapshots after pruning = %v, %v; want %v", snapshotIDs(left), err, taken[2:])
	}
	all, err = f.engine.GroupSnapshots(ctx, groupID)
	if err != nil || len(all) != 3 || !slices.Contains(snapshotIDs(all), otherSnap.ID) {
		t.Errorf("group's snapshots after pruning = %v, %v", snapshotIDs(all), err)
	}

	// Pruning again changes nothing, and the pruned repository checks
	// clean, reading all of its data, and still restores.
	if res, err := f.engine.Prune(ctx, groupID); err != nil || len(res.Forgotten) != 0 {
		t.Errorf("second Prune = %+v, %v", res, err)
	}
	if err := f.engine.Check(ctx, groupID); err != nil {
		t.Errorf("Check after pruning = %v", err)
	}
	if err := f.engine.Restore(ctx, site.ID, taken[2], site.ID); err != nil {
		t.Errorf("Restore of a kept snapshot = %v", err)
	}
	if err := f.engine.Restore(ctx, site.ID, taken[0], site.ID); !errors.Is(err, backup.ErrSnapshotNotFound) {
		t.Errorf("Restore of a forgotten snapshot = %v, want ErrSnapshotNotFound", err)
	}
}

func TestPruneKeepsEverythingWithoutAPolicy(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, storetest.Open(t, store.SQLite))
	groupID := f.group(t, "Acme")
	site := f.site(t, groupID, "acme")
	for range 2 {
		if _, err := f.engine.Backup(ctx, site.ID, backup.Options{}); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := f.engine.Prune(ctx, groupID); err != nil || len(res.Forgotten) != 0 {
		t.Errorf("Prune = %+v, %v", res, err)
	}
	if snaps, err := f.engine.ListSnapshots(ctx, site.ID); err != nil || len(snaps) != 2 {
		t.Errorf("snapshots = %v, %v; want both", snapshotIDs(snaps), err)
	}
}

func TestMaintenanceWithoutRepository(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, storetest.Open(t, store.SQLite), func(o *restic.Options) { o.Retention = backup.Retention{KeepDaily: 1} })
	groupID := f.group(t, "Acme")
	if _, err := f.engine.Prune(ctx, groupID); !errors.Is(err, backup.ErrNoRepository) {
		t.Errorf("Prune = %v, want ErrNoRepository", err)
	}
	if err := f.engine.Check(ctx, groupID); !errors.Is(err, backup.ErrNoRepository) {
		t.Errorf("Check = %v, want ErrNoRepository", err)
	}
	if snaps, err := f.engine.GroupSnapshots(ctx, groupID); err != nil || snaps == nil || len(snaps) != 0 {
		t.Errorf("GroupSnapshots = %v, %v; want none", snaps, err)
	}
	for _, bad := range []string{"", "../x"} {
		if _, err := f.engine.Prune(ctx, bad); err == nil {
			t.Errorf("Prune(%q) accepted an invalid ID", bad)
		}
		if err := f.engine.Check(ctx, bad); err == nil {
			t.Errorf("Check(%q) accepted an invalid ID", bad)
		}
	}
	if _, err := f.engine.GroupSnapshots(ctx, id.New()+"x"); err == nil {
		t.Error("GroupSnapshots accepted an invalid ID")
	}
}

func TestCheckFindsDamage(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, storetest.Open(t, store.SQLite), func(o *restic.Options) { o.CheckReadDataPercent = 100 })
	groupID := f.group(t, "Acme")
	site := f.site(t, groupID, "acme")
	if _, err := f.engine.Backup(ctx, site.ID, backup.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Check(ctx, groupID); err != nil {
		t.Fatalf("Check of a healthy repository = %v", err)
	}

	// Overwrite part of every pack file.
	dir, err := f.layout.BackupRepositoryDir(groupID)
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(filepath.Join(dir, "data"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		_, err = file.WriteAt([]byte("damaged!"), 0)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Check(ctx, groupID); !errors.Is(err, backup.ErrRepositoryDamaged) {
		t.Errorf("Check of a damaged repository = %v, want ErrRepositoryDamaged", err)
	}
}
