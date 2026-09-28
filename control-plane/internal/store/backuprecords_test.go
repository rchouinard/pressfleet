package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestBackupRecords(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		job := store.Job{Type: "site.backup", TargetType: "site", TargetID: id.New()}
		if err := store.NewJobs(db).Enqueue(ctx, &job); err != nil {
			t.Fatal(err)
		}
		records := store.NewBackupRecords(db)
		siteID := job.TargetID

		// Create fills in the running state, keeping a given ID.
		before := time.Now().Add(-time.Second)
		rec := &store.BackupRecord{
			ID: id.New(), SiteID: siteID, ClientGroupID: group.ID, Kind: store.BackupManual,
			WPCoreVersion: "6.9.1", JobID: job.ID,
			// Ignored: a new record is running and has no result.
			Status: store.BackupSucceeded, SnapshotID: "abc", SizeBytes: 5, Message: "x",
		}
		wantID := rec.ID
		if err := records.Create(ctx, rec); err != nil {
			t.Fatal(err)
		}
		if rec.ID != wantID || rec.Status != store.BackupRunning || rec.SnapshotID != "" || rec.SizeBytes != 0 || rec.Message != "" ||
			rec.CreatedAt.Before(before) || rec.StartedAt != rec.CreatedAt || !rec.FinishedAt.IsZero() {
			t.Errorf("created = %+v", rec)
		}
		got, err := records.Get(ctx, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got != *rec {
			t.Errorf("Get = %+v, want %+v", got, *rec)
		}
		if err := records.Create(ctx, &store.BackupRecord{ID: rec.ID, SiteID: siteID, ClientGroupID: group.ID, Kind: store.BackupManual}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("Create with a taken ID: %v, want ErrConflict", err)
		}
		if err := records.Create(ctx, &store.BackupRecord{SiteID: siteID, ClientGroupID: id.New(), Kind: store.BackupManual}); !errors.Is(err, store.ErrReference) {
			t.Errorf("Create in a missing group: %v, want ErrReference", err)
		}
		if err := records.Create(ctx, &store.BackupRecord{SiteID: siteID, ClientGroupID: group.ID, Kind: "weekly"}); err == nil {
			t.Error("Create accepted an unknown kind")
		}
		if _, err := records.Get(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get of a missing record: %v, want ErrNotFound", err)
		}

		// Start and Succeed change a running record only.
		if ok, err := records.Start(ctx, rec.ID); err != nil || !ok {
			t.Fatalf("Start = %v, %v", ok, err)
		}
		res := store.BackupResult{SnapshotID: "f3b098f1", SizeBytes: 1 << 40, AddedBytes: 2048, FileCount: 12, Warning: "1 file couldn't be read"}
		if ok, err := records.Succeed(ctx, rec.ID, res); err != nil || !ok {
			t.Fatalf("Succeed = %v, %v", ok, err)
		}
		if ok, err := records.Succeed(ctx, rec.ID, res); err != nil || ok {
			t.Errorf("Succeed again = %v, %v; want no change", ok, err)
		}
		if ok, err := records.Fail(ctx, rec.ID, "late"); err != nil || ok {
			t.Errorf("Fail after success = %v, %v; want no change", ok, err)
		}
		if ok, err := records.Start(ctx, rec.ID); err != nil || ok {
			t.Errorf("Start after success = %v, %v; want no change", ok, err)
		}
		if _, err := records.Succeed(ctx, rec.ID, store.BackupResult{}); err == nil {
			t.Error("Succeed accepted no snapshot ID")
		}
		got, err = records.Get(ctx, rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != store.BackupSucceeded || got.SnapshotID != res.SnapshotID || got.SizeBytes != res.SizeBytes ||
			got.AddedBytes != res.AddedBytes || got.FileCount != res.FileCount || got.Message != res.Warning ||
			got.FinishedAt.Before(got.StartedAt) || got.StartedAt.Before(got.CreatedAt) {
			t.Errorf("succeeded = %+v", got)
		}

		// A failed record keeps its message. IDs are ULIDs, ordered by the
		// millisecond.
		time.Sleep(2 * time.Millisecond)
		failed := &store.BackupRecord{SiteID: siteID, ClientGroupID: group.ID, Kind: store.BackupScheduled, WPCoreVersion: "6.9.1"}
		if err := records.Create(ctx, failed); err != nil {
			t.Fatal(err)
		}
		if !id.Valid(failed.ID) || failed.JobID != "" {
			t.Errorf("created without an ID = %+v", failed)
		}
		if ok, err := records.Fail(ctx, failed.ID, "restic backup: exit 1"); err != nil || !ok {
			t.Fatalf("Fail = %v, %v", ok, err)
		}
		got, err = records.Get(ctx, failed.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != store.BackupFailed || got.Message != "restic backup: exit 1" || got.FinishedAt.IsZero() || got.SnapshotID != "" {
			t.Errorf("failed = %+v", got)
		}

		// List: newest first, paged, per site, by status.
		other := &store.BackupRecord{SiteID: id.New(), ClientGroupID: group.ID, Kind: store.BackupPreRestore}
		if err := records.Create(ctx, other); err != nil {
			t.Fatal(err)
		}
		page, err := records.List(ctx, store.BackupRecordFilter{SiteID: siteID, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) != 1 || page.Records[0].ID != failed.ID || page.NextCursor != failed.ID {
			t.Errorf("first page = %+v", page)
		}
		page, err = records.List(ctx, store.BackupRecordFilter{SiteID: siteID, Before: page.NextCursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) != 1 || page.Records[0].ID != rec.ID || page.NextCursor != "" {
			t.Errorf("second page = %+v", page)
		}
		page, err = records.List(ctx, store.BackupRecordFilter{SiteID: siteID, Status: store.BackupSucceeded})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) != 1 || page.Records[0].ID != rec.ID {
			t.Errorf("succeeded only = %+v", page)
		}
		if page, err := records.List(ctx, store.BackupRecordFilter{SiteID: id.New()}); err != nil || page.Records == nil || len(page.Records) != 0 {
			t.Errorf("List of a site without backups = %+v, %v", page, err)
		}
		if _, err := records.List(ctx, store.BackupRecordFilter{}); err == nil {
			t.Error("List without a site succeeded")
		}

		if g, err := records.SiteClientGroup(ctx, siteID); err != nil || g != group.ID {
			t.Errorf("SiteClientGroup = %q, %v", g, err)
		}
		if _, err := records.SiteClientGroup(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("SiteClientGroup of a site without backups: %v, want ErrNotFound", err)
		}

		// Records outlive their job.
		if _, err := db.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", job.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := records.Get(ctx, rec.ID); err != nil || got.JobID != "" {
			t.Errorf("after the job was deleted: %+v, %v", got, err)
		}

		// Deleting a group's records, and the group itself, removes them.
		if n, err := records.DeleteForClientGroup(ctx, group.ID); err != nil || n != 3 {
			t.Errorf("DeleteForClientGroup = %d, %v", n, err)
		}
		again := &store.BackupRecord{SiteID: siteID, ClientGroupID: group.ID, Kind: store.BackupManual}
		if err := records.Create(ctx, again); err != nil {
			t.Fatal(err)
		}
		if err := store.NewClientGroups(db).Delete(ctx, group.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := records.Get(ctx, again.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("record of a deleted group: %v, want ErrNotFound", err)
		}
	})
}

func TestBackupKindValid(t *testing.T) {
	for _, k := range []store.BackupKind{store.BackupManual, store.BackupScheduled, store.BackupPreRestore} {
		if !k.Valid() {
			t.Errorf("%q is not valid", k)
		}
	}
	if store.BackupKind("hourly").Valid() || store.BackupKind("").Valid() {
		t.Error("unknown kinds are valid")
	}
}

func TestBackupRecordsExpire(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		groups := store.NewClientGroups(db)
		group, otherGroup := &store.ClientGroup{Name: "Acme"}, &store.ClientGroup{Name: "Globex"}
		for _, g := range []*store.ClientGroup{group, otherGroup} {
			if err := groups.Create(ctx, g); err != nil {
				t.Fatal(err)
			}
		}
		records := store.NewBackupRecords(db)
		create := func(groupID string, succeed bool) store.BackupRecord {
			t.Helper()
			r := &store.BackupRecord{SiteID: id.New(), ClientGroupID: groupID, Kind: store.BackupScheduled, WPCoreVersion: "6.9.1"}
			if err := records.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			if succeed {
				if _, err := records.Succeed(ctx, r.ID, store.BackupResult{SnapshotID: "snap-" + r.ID}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := records.Get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		old, kept := create(group.ID, true), create(group.ID, true)
		running := create(group.ID, false)
		elsewhere := create(otherGroup.ID, true)
		time.Sleep(10 * time.Millisecond)
		listedAt := time.Now()
		time.Sleep(10 * time.Millisecond)
		late := create(group.ID, true)

		// Only the group's succeeded backups that finished before the
		// listing count.
		got, err := records.Restorable(ctx, group.ID, listedAt)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].ID != old.ID || got[1].ID != kept.ID {
			t.Errorf("Restorable = %+v, want %s and %s", got, old.ID, kept.ID)
		}

		n, err := records.Expire(ctx, []string{old.ID, running.ID, elsewhere.ID})
		if err != nil || n != 2 {
			t.Errorf("Expire = %d, %v; want the two succeeded backups", n, err)
		}
		if n, err := records.Expire(ctx, []string{old.ID}); err != nil || n != 0 {
			t.Errorf("Expire again = %d, %v; want no change", n, err)
		}
		if n, err := records.Expire(ctx, nil); err != nil || n != 0 {
			t.Errorf("Expire of nothing = %d, %v", n, err)
		}
		for _, r := range []store.BackupRecord{old, kept, running, elsewhere, late} {
			got, err := records.Get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantExpired := r.ID == old.ID || r.ID == elsewhere.ID
			if got.ExpiredAt.IsZero() == wantExpired {
				t.Errorf("record %s expired at %v, want expired %v", r.ID, got.ExpiredAt, wantExpired)
			}
		}
		got, err = records.Restorable(ctx, group.ID, time.Now().Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].ID != kept.ID || got[1].ID != late.ID {
			t.Errorf("Restorable after expiring = %+v", got)
		}

		// Many IDs go in batches.
		ids := make([]string, 1200)
		for i := range ids {
			ids[i] = id.New()
		}
		ids[1100] = kept.ID
		if n, err := records.Expire(ctx, ids); err != nil || n != 1 {
			t.Errorf("Expire of 1200 IDs = %d, %v", n, err)
		}
	})
}
