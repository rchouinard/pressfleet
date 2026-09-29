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

func TestDatabaseExports(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		job := store.Job{Type: "site.database_export", TargetType: "site", TargetID: id.New()}
		if err := store.NewJobs(db).Enqueue(ctx, &job); err != nil {
			t.Fatal(err)
		}
		exports := store.NewDatabaseExports(db)
		siteID := job.TargetID

		// Create fills in the running state, keeping a given ID.
		before := time.Now().Add(-time.Second)
		e := &store.DatabaseExport{ID: id.New(), SiteID: siteID, JobID: job.ID, Status: store.DatabaseExportReady, SizeBytes: 9}
		wantID := e.ID
		if err := exports.Create(ctx, e); err != nil {
			t.Fatal(err)
		}
		if e.ID != wantID || e.Status != store.DatabaseExportRunning || e.SizeBytes != 0 || e.CreatedAt.Before(before) ||
			!e.ReadyAt.IsZero() || !e.ExpiresAt.IsZero() || !e.ExpiredAt.IsZero() {
			t.Errorf("created = %+v", e)
		}
		got, err := exports.Get(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got != *e {
			t.Errorf("Get = %+v, want %+v", got, *e)
		}
		if err := exports.Create(ctx, &store.DatabaseExport{ID: e.ID, SiteID: siteID}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("Create with a taken ID: %v, want ErrConflict", err)
		}
		if err := exports.Create(ctx, &store.DatabaseExport{}); err == nil {
			t.Error("Create accepted an export without a site")
		}
		if _, err := exports.Get(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get of a missing export: %v, want ErrNotFound", err)
		}

		// Nothing to expire yet, and a running export can't expire.
		now := time.Now()
		if list, err := exports.ListExpired(ctx, now.Add(time.Hour), 0); err != nil || len(list) != 0 {
			t.Errorf("ListExpired with nothing ready = %+v, %v", list, err)
		}
		if changed, err := exports.Expire(ctx, e.ID); err != nil || changed {
			t.Errorf("Expire of a running export = %v, %v", changed, err)
		}

		// Ready once; a second call changes nothing.
		expires := now.Add(time.Hour)
		if changed, err := exports.Ready(ctx, e.ID, 1234, expires); err != nil || !changed {
			t.Fatalf("Ready = %v, %v", changed, err)
		}
		if changed, err := exports.Ready(ctx, e.ID, 1, expires); err != nil || changed {
			t.Errorf("second Ready = %v, %v", changed, err)
		}
		if changed, err := exports.Fail(ctx, e.ID, "late"); err != nil || changed {
			t.Errorf("Fail of a ready export = %v, %v", changed, err)
		}
		got, err = exports.Get(ctx, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != store.DatabaseExportReady || got.SizeBytes != 1234 || got.ReadyAt.IsZero() ||
			!got.ExpiresAt.Equal(expires.UTC().Truncate(time.Microsecond)) || got.Message != "" {
			t.Errorf("ready = %+v", got)
		}

		// Another export that fails.
		failed := &store.DatabaseExport{SiteID: siteID}
		if err := exports.Create(ctx, failed); err != nil {
			t.Fatal(err)
		}
		if !id.Valid(failed.ID) || failed.ID == e.ID {
			t.Errorf("generated ID %q", failed.ID)
		}
		if changed, err := exports.Fail(ctx, failed.ID, "dump failed"); err != nil || !changed {
			t.Fatalf("Fail = %v, %v", changed, err)
		}
		if got, err := exports.Get(ctx, failed.ID); err != nil || got.Status != store.DatabaseExportFailed || got.Message != "dump failed" {
			t.Errorf("failed = %+v, %v", got, err)
		}

		// Only ready exports past their expiry are listed.
		if list, err := exports.ListExpired(ctx, now, 0); err != nil || len(list) != 0 {
			t.Errorf("ListExpired before the expiry = %+v, %v", list, err)
		}
		list, err := exports.ListExpired(ctx, expires, 10)
		if err != nil || len(list) != 1 || list[0].ID != e.ID {
			t.Fatalf("ListExpired at the expiry = %+v, %v", list, err)
		}
		if changed, err := exports.Expire(ctx, e.ID); err != nil || !changed {
			t.Fatalf("Expire = %v, %v", changed, err)
		}
		if changed, err := exports.Expire(ctx, e.ID); err != nil || changed {
			t.Errorf("second Expire = %v, %v", changed, err)
		}
		if got, err := exports.Get(ctx, e.ID); err != nil || got.Status != store.DatabaseExportExpired || got.ExpiredAt.IsZero() {
			t.Errorf("expired = %+v, %v", got, err)
		}
		if list, err := exports.ListExpired(ctx, expires.Add(time.Hour), 0); err != nil || len(list) != 0 {
			t.Errorf("ListExpired after expiring = %+v, %v", list, err)
		}

		// Deleting the job keeps the record.
		if _, err := db.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", job.ID); err != nil {
			t.Fatal(err)
		}
		if got, err := exports.Get(ctx, e.ID); err != nil || got.JobID != "" {
			t.Errorf("after deleting the job = %+v, %v", got, err)
		}
	})
}
