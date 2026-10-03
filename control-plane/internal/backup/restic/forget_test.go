package restic_test

import (
	"context"
	"errors"
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

func TestForgetSite(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, storetest.Open(t, store.SQLite), func(o *restic.Options) { o.CheckReadDataPercent = 100 })
	groupID := f.group(t, "Acme")
	site := f.site(t, groupID, "acme")
	other := f.site(t, groupID, "acme-shop")
	var taken []backup.SnapshotID
	for i := range 3 {
		if err := os.WriteFile(filepath.Join(f.siteRoots[site.ID], "wp-content", "uploads", "new.txt"), []byte{byte(i)}, 0o600); err != nil {
			t.Fatal(err)
		}
		snap, err := f.engine.Backup(ctx, site.ID, backup.Options{Kind: "manual"})
		if err != nil {
			t.Fatal(err)
		}
		taken = append(taken, snap.ID)
	}
	otherSnap, err := f.engine.Backup(ctx, other.ID, backup.Options{Kind: "manual"})
	if err != nil {
		t.Fatal(err)
	}

	res, err := f.engine.ForgetSite(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Clone(res.Forgotten)
	slices.Sort(got)
	want := slices.Clone(taken)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("forgotten = %v, want %v", got, want)
	}
	if left, err := f.engine.ListSnapshots(ctx, site.ID); err != nil || len(left) != 0 {
		t.Errorf("site's snapshots after ForgetSite = %v, %v; want none", snapshotIDs(left), err)
	}
	// The other site in the same repository keeps its snapshot, which
	// still restores from the pruned repository.
	all, err := f.engine.GroupSnapshots(ctx, groupID)
	if err != nil || !slices.Equal(snapshotIDs(all), []backup.SnapshotID{otherSnap.ID}) {
		t.Errorf("group's snapshots = %v, %v; want only %s", snapshotIDs(all), err, otherSnap.ID)
	}
	if err := f.engine.Check(ctx, groupID); err != nil {
		t.Errorf("Check after ForgetSite = %v", err)
	}
	if err := f.engine.Restore(ctx, other.ID, otherSnap.ID, other.ID); err != nil {
		t.Errorf("Restore of the other site's snapshot = %v", err)
	}

	// Again: nothing left to forget, and no error.
	if res, err := f.engine.ForgetSite(ctx, site.ID); err != nil || len(res.Forgotten) != 0 {
		t.Errorf("second ForgetSite = %+v, %v", res, err)
	}
}

func TestForgetSiteWithoutSnapshots(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, storetest.Open(t, store.SQLite))
	groupID := f.group(t, "Acme")
	site := f.site(t, groupID, "acme")
	// The group has no repository yet.
	if res, err := f.engine.ForgetSite(ctx, site.ID); err != nil || res.Forgotten == nil || len(res.Forgotten) != 0 {
		t.Errorf("ForgetSite without a repository = %+v, %v; want none forgotten", res, err)
	}
	if _, err := f.engine.ForgetSite(ctx, id.New()); !errors.Is(err, backup.ErrSiteNotFound) {
		t.Errorf("ForgetSite of an unknown site = %v, want ErrSiteNotFound", err)
	}
	if _, err := f.engine.ForgetSite(ctx, "../x"); err == nil {
		t.Error("ForgetSite accepted an invalid ID")
	}
}
