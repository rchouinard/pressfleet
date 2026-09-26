package audit_test

import (
	"context"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/audit"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestRetentionCleanup(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		now := time.Now().UTC()
		events := store.NewAuditEvents(db)
		for _, age := range []time.Duration{100 * 24 * time.Hour, 31 * 24 * time.Hour, 29 * 24 * time.Hour, time.Hour} {
			if err := events.Insert(ctx, &store.AuditEvent{OccurredAt: now.Add(-age), Action: "site.update", Result: store.AuditSuccess}); err != nil {
				t.Fatal(err)
			}
		}

		// The schedule is created once and survives being ensured again.
		for range 2 {
			if err := audit.EnsureCleanupSchedule(ctx, db); err != nil {
				t.Fatal(err)
			}
		}
		sch, err := store.NewSchedules(db).Get(ctx, audit.CleanupScheduleName)
		if err != nil {
			t.Fatal(err)
		}
		if sch.JobType != audit.CleanupJobType || sch.Interval != 24*time.Hour || sch.MissedRunPolicy != store.MissedCoalesce {
			t.Errorf("schedule = %+v", sch)
		}

		pool := jobs.NewPool(db, jobs.Options{Logger: storetest.Logger()})
		if err := pool.Register(audit.CleanupHandler(db, 30*24*time.Hour, func() time.Time { return now })); err != nil {
			t.Fatal(err)
		}
		// The second run finds nothing to delete and still succeeds.
		for range 2 {
			job, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: sch.JobType, TargetType: sch.TargetType, TargetID: sch.TargetID})
			if err != nil {
				t.Fatal(err)
			}
			if ran, err := pool.RunNext(ctx); !ran || err != nil {
				t.Fatalf("RunNext = %v, %v", ran, err)
			}
			if got, err := store.NewJobs(db).Get(ctx, job.ID); err != nil || got.State != store.JobSucceeded {
				t.Fatalf("cleanup job = %+v, %v", got, err)
			}
		}
		page, err := events.List(ctx, store.AuditFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) != 2 {
			t.Fatalf("%d events left, want the 2 younger than 30 days", len(page.Events))
		}
		for _, e := range page.Events {
			if now.Sub(e.OccurredAt) > 30*24*time.Hour {
				t.Errorf("expired event survived: %+v", e)
			}
		}
	})
}
