package jobs_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// chainHandlers returns a "test.backup" handler that records a snapshot
// in its result, or fails for good if its payload says so, and a
// "test.copy" handler whose first step reads the backup's outcome.
func chainHandlers(rec *recorder) []jobs.Handler {
	backup := jobs.Handler{Type: "test.backup", Steps: []jobs.Step{{
		Name: "snapshot",
		Do: func(_ context.Context, r *jobs.Run) error {
			var p struct {
				Fail bool `json:"fail"`
			}
			if err := r.Payload(&p); err != nil {
				return err
			}
			rec.add("backup:" + r.Job().LockKey)
			if p.Fail {
				return jobs.Permanent(errors.New("restic exploded"))
			}
			return r.SetResult("snapshot_id", "snap-"+r.Job().LockKey)
		},
	}}}
	copyJob := jobs.Handler{Type: "test.copy", Steps: []jobs.Step{
		{
			Name: "resolve_snapshot",
			Do: func(ctx context.Context, r *jobs.Run) error {
				aw, err := r.Awaited(ctx)
				if err != nil {
					return err
				}
				if err := aw.Err(); err != nil {
					return err
				}
				var snap string
				if ok, err := aw.GetResult("snapshot_id", &snap); !ok || err != nil {
					return jobs.Permanent(errors.New("awaited backup has no snapshot"))
				}
				return r.Set("snapshot_id", snap)
			},
			Undo: func(context.Context, *jobs.Run) error {
				rec.add("undo:resolve_snapshot")
				return nil
			},
		},
		{Name: "restore", Do: func(_ context.Context, r *jobs.Run) error {
			var snap string
			if _, err := r.Get("snapshot_id", &snap); err != nil {
				return err
			}
			rec.add("restore:" + r.Job().LockKey + ":" + snap)
			return nil
		}},
	}}
	return []jobs.Handler{backup, copyJob}
}

func enqueueSpec(t *testing.T, db *store.DB, s jobs.Spec) store.Job {
	t.Helper()
	if s.TargetType == "" {
		s.TargetType, s.TargetID = store.JobTargetSite, id.New()
	}
	j, err := jobs.Enqueue(context.Background(), db, s)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (f *fixture) drain(t *testing.T) {
	t.Helper()
	for range 20 {
		if !f.runNext(t) {
			return
		}
	}
	t.Fatal("jobs still runnable after 20 runs")
}

func TestJobChain(t *testing.T) {
	for _, tt := range []struct {
		name       string
		fail       bool
		wantState  store.JobState
		wantCalls  []string
		wantErrSub string
	}{
		{"awaited job succeeds", false, store.JobSucceeded, []string{"backup:site:a", "restore:site:b:snap-site:a"}, ""},
		{"awaited job fails", true, store.JobFailed, []string{"backup:site:a", "undo:resolve_snapshot"}, "test.backup job"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				rec := &recorder{}
				f := newFixture(t, db, jobs.Options{}, chainHandlers(rec)...)
				backup := enqueueSpec(t, db, jobs.Spec{Type: "test.backup", LockKey: "site:a", Payload: map[string]bool{"fail": tt.fail}})
				copyJob := enqueueSpec(t, db, jobs.Spec{Type: "test.copy", LockKey: "site:b", After: backup.ID})
				if copyJob.AfterJobID != backup.ID {
					t.Fatalf("Enqueue = %+v", copyJob)
				}
				f.drain(t)
				if got := f.detail(t, backup.ID).Job.State; !got.Final() {
					t.Fatalf("backup is %s", got)
				}
				d := f.detail(t, copyJob.ID)
				if d.Job.State != tt.wantState || d.Job.Attempts != 1 {
					t.Errorf("copy = %s after %d attempts (error %q), want %s after 1", d.Job.State, d.Job.Attempts, d.Job.Error, tt.wantState)
				}
				if !strings.Contains(d.Job.Error, tt.wantErrSub) {
					t.Errorf("copy error = %q, want it to mention %q", d.Job.Error, tt.wantErrSub)
				}
				if got := rec.get(); !slices.Equal(got, tt.wantCalls) {
					t.Errorf("calls = %v, want %v", got, tt.wantCalls)
				}
			})
		})
	}
}

// Chains crossing two lock keys in opposite directions, like a staging
// push and refresh queued together, both finish.
func TestJobChainsCrossingLockKeys(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		rec := &recorder{}
		f := newFixture(t, db, jobs.Options{}, chainHandlers(rec)...)
		backupA := enqueueSpec(t, db, jobs.Spec{Type: "test.backup", LockKey: "site:a"})
		copyB := enqueueSpec(t, db, jobs.Spec{Type: "test.copy", LockKey: "site:b", After: backupA.ID})
		backupB := enqueueSpec(t, db, jobs.Spec{Type: "test.backup", LockKey: "site:b"})
		copyA := enqueueSpec(t, db, jobs.Spec{Type: "test.copy", LockKey: "site:a", After: backupB.ID})
		f.drain(t)
		for _, j := range []store.Job{backupA, copyB, backupB, copyA} {
			if d := f.detail(t, j.ID); d.Job.State != store.JobSucceeded {
				t.Errorf("%s on %s = %s (%q)", j.Type, j.LockKey, d.Job.State, d.Job.Error)
			}
		}
		// RunNext runs one job at a time, so only the set of calls is
		// stable; package store tests that both backups can run together.
		want := []string{"backup:site:a", "backup:site:b", "restore:site:a:snap-site:b", "restore:site:b:snap-site:a"}
		got := rec.get()
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("calls = %v, want %v", got, want)
		}
	})
}

func TestAwaitedErrors(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		var got error
		h := jobs.Handler{Type: "test.copy", Steps: []jobs.Step{{Name: "a", Do: func(ctx context.Context, r *jobs.Run) error {
			_, got = r.Awaited(ctx)
			return nil
		}}}}
		f := newFixture(t, db, jobs.Options{}, h)
		enqueueSpec(t, db, jobs.Spec{Type: "test.copy"})
		f.drain(t)
		if !errors.Is(got, jobs.ErrNotWaiting) {
			t.Errorf("Awaited without Spec.After: %v, want ErrNotWaiting", got)
		}
		_, err := jobs.Enqueue(context.Background(), db, jobs.Spec{
			Type: "test.copy", TargetType: store.JobTargetSite, TargetID: id.New(), After: id.New(),
		})
		if !errors.Is(err, store.ErrAfterJobNotFound) {
			t.Errorf("Enqueue after a missing job: %v, want store.ErrAfterJobNotFound", err)
		}
	})
}
