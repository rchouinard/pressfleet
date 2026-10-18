package store_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestAlertEnums(t *testing.T) {
	ops := []struct {
		op             store.AlertOperator
		less, eq, more bool // value 1, 2, 3 against threshold 2
	}{
		{store.OpGreater, false, false, true},
		{store.OpGreaterEqual, false, true, true},
		{store.OpLess, true, false, false},
		{store.OpLessEqual, true, true, false},
		{store.OpEqual, false, true, false},
		{store.OpNotEqual, true, false, true},
		{"between", false, false, false},
	}
	for _, tt := range ops {
		if got := [3]bool{tt.op.Compare(1, 2), tt.op.Compare(2, 2), tt.op.Compare(3, 2)}; got != [3]bool{tt.less, tt.eq, tt.more} {
			t.Errorf("%s: Compare(1|2|3, 2) = %v", tt.op, got)
		}
		if tt.op.Valid() != (tt.op != "between") {
			t.Errorf("%s: Valid() = %v", tt.op, tt.op.Valid())
		}
	}
	if !store.AlertScopeSite.Valid() || !store.AlertScopePlatform.Valid() || store.AlertScope("host").Valid() {
		t.Error("AlertScope.Valid")
	}
	if !store.AlertKindThreshold.Valid() || !store.AlertKindExpression.Valid() || store.AlertRuleKind("ratio").Valid() {
		t.Error("AlertRuleKind.Valid")
	}
	if !store.SeverityInfo.Valid() || !store.SeverityWarning.Valid() || !store.SeverityCritical.Valid() || store.AlertSeverity("page").Valid() {
		t.Error("AlertSeverity.Valid")
	}
}

func TestJobStatsAndRollbackStates(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		jobs := store.NewJobs(db)
		// Jobs are runnable from when they are enqueued, after this.
		now := time.Now().Add(time.Minute)
		enqueue := func(scheduleID string) store.Job {
			t.Helper()
			j := store.Job{Type: "test.stats", TargetType: store.JobTargetSite, TargetID: id.New(), ScheduleID: scheduleID}
			if err := jobs.Enqueue(ctx, &j); err != nil {
				t.Fatal(err)
			}
			return j
		}
		lease := func() (store.Job, string) {
			t.Helper()
			owner := id.New()
			j, err := jobs.Lease(ctx, []string{"test.stats"}, owner, now, now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			return j, owner
		}

		if s, err := jobs.Stats(ctx); err != nil || s != (store.JobStats{}) {
			t.Errorf("empty Stats = %+v, %v", s, err)
		}
		scheduleID := id.New()
		for range 5 {
			enqueue("")
		}
		enqueue(scheduleID)
		if ok, err := jobs.HasQueuedForSchedule(ctx, scheduleID); err != nil || !ok {
			t.Errorf("HasQueuedForSchedule before any run = %v, %v", ok, err)
		}

		succeeded, o1 := lease()
		if err := jobs.Finish(ctx, succeeded.ID, o1, store.JobSucceeded, "", now); err != nil {
			t.Fatal(err)
		}
		failed, o2 := lease()
		if err := jobs.Finish(ctx, failed.ID, o2, store.JobFailed, "boom", now); err != nil {
			t.Fatal(err)
		}

		// BeginRollback needs the lease, restarts attempts, and records
		// the error; RetryRollback releases the lease in rolling_back.
		rolling, o3 := lease()
		if err := jobs.BeginRollback(ctx, rolling.ID, "someone else", "x", now); !errors.Is(err, store.ErrLeaseLost) {
			t.Errorf("BeginRollback without the lease: %v, want ErrLeaseLost", err)
		}
		if err := jobs.BeginRollback(ctx, rolling.ID, o3, "step failed", now); err != nil {
			t.Fatal(err)
		}
		later := now.Add(time.Minute)
		if err := jobs.RetryRollback(ctx, rolling.ID, o3, later, now); err != nil {
			t.Fatal(err)
		}
		if err := jobs.RetryRollback(ctx, rolling.ID, o3, later, now); !errors.Is(err, store.ErrLeaseLost) {
			t.Errorf("RetryRollback after releasing: %v, want ErrLeaseLost", err)
		}
		got, err := jobs.Get(ctx, rolling.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != store.JobRollingBack || got.Error != "step failed" || got.Attempts != 1 || got.LeaseOwner != "" ||
			!got.RunAfter.Equal(later.UTC().Truncate(time.Microsecond)) {
			t.Errorf("after RetryRollback: %+v", got)
		}

		lease() // left running
		s, err := jobs.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// 6 enqueued: 1 succeeded, 1 failed, 1 rolling back and 1 running
		// count as running, 2 still queued.
		if want := (store.JobStats{Queued: 2, Running: 2, Succeeded: 1, Failed: 1}); s != want {
			t.Errorf("Stats = %+v, want %+v", s, want)
		}

		// Leasing everything else starts the scheduled job.
		for range 2 {
			lease()
		}
		if ok, err := jobs.HasQueuedForSchedule(ctx, scheduleID); err != nil || ok {
			t.Errorf("HasQueuedForSchedule after it started = %v, %v", ok, err)
		}
	})
}

func TestBackupHealth(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		records := store.NewBackupRecords(db)
		siteA, siteB, siteC := id.New(), id.New(), id.New()
		// backup creates a record and finishes it: ok true succeeds, false
		// fails, and a nil ok leaves it running.
		backup := func(siteID string, ok *bool) store.BackupRecord {
			t.Helper()
			rec := &store.BackupRecord{SiteID: siteID, ClientGroupID: group.ID, Kind: store.BackupScheduled}
			if err := records.Create(ctx, rec); err != nil {
				t.Fatal(err)
			}
			switch {
			case ok == nil:
			case *ok:
				if _, err := records.Succeed(ctx, rec.ID, store.BackupResult{SnapshotID: "s"}); err != nil {
					t.Fatal(err)
				}
			default:
				if _, err := records.Fail(ctx, rec.ID, "failed"); err != nil {
					t.Fatal(err)
				}
			}
			// finished_at has microsecond precision; keep records apart.
			time.Sleep(2 * time.Millisecond)
			return *rec
		}
		yes, no := true, false

		a1 := backup(siteA, &yes)
		a2 := backup(siteA, &no)
		backup(siteA, nil) // running: ignored by both
		b1 := backup(siteB, &yes)
		backup(siteC, &no)

		finished, err := records.LatestFinished(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(finished) != 3 || finished[siteA].ID != a2.ID || finished[siteA].Status != store.BackupFailed ||
			finished[siteB].ID != b1.ID || finished[siteC].Status != store.BackupFailed {
			t.Errorf("LatestFinished = %+v", finished)
		}
		succeeded, err := records.LatestSucceeded(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(succeeded) != 2 || succeeded[siteA].ID != a1.ID || succeeded[siteB].ID != b1.ID {
			t.Errorf("LatestSucceeded = %+v", succeeded)
		}
		if _, ok := succeeded[siteC]; ok {
			t.Error("a site whose only backup failed has a latest success")
		}
	})
}

func TestLockActiveAdmins(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		a := createUser(t, db, "a@example.com", store.RoleAdmin)
		b := createUser(t, db, "b@example.com", store.RoleAdmin)
		createUser(t, db, "viewer@example.com", store.RoleViewer)
		disabled := createUser(t, db, "c@example.com", store.RoleAdmin)
		now := time.Now()
		disabled.DisabledAt = &now
		if err := store.NewUsers(db).Update(ctx, &disabled, disabled.UpdatedAt); err != nil {
			t.Fatal(err)
		}
		want := []string{a.ID, b.ID}
		slices.Sort(want)
		err := db.InTx(ctx, func(q store.Querier) error {
			ids, err := store.NewUsers(q).LockActiveAdmins(ctx)
			if err != nil {
				return err
			}
			if !slices.Equal(ids, want) {
				t.Errorf("LockActiveAdmins = %v, want %v", ids, want)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}
