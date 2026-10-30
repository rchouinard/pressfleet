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

func enqueueAfter(t *testing.T, q store.Querier, lockKey, afterID string) store.Job {
	t.Helper()
	j := store.Job{Type: "test.a", TargetType: store.JobTargetSite, TargetID: id.New(), LockKey: lockKey, AfterJobID: afterID}
	if err := store.NewJobs(q).Enqueue(context.Background(), &j); err != nil {
		t.Fatal(err)
	}
	return j
}

// leaseAll leases every runnable job, returning them by ID.
func leaseAll(t *testing.T, db *store.DB, now time.Time) map[string]store.Job {
	t.Helper()
	got := map[string]store.Job{}
	for {
		j, ok := lease(t, db, now)
		if !ok {
			return got
		}
		got[j.ID] = j
	}
}

func finish(t *testing.T, db *store.DB, j store.Job, state store.JobState, now time.Time) {
	t.Helper()
	if err := store.NewJobs(db).Finish(context.Background(), j.ID, j.LeaseOwner, state, "", now); err != nil {
		t.Fatal(err)
	}
}

func TestJobsAfter(t *testing.T) {
	ctx := context.Background()
	for _, final := range []store.JobState{store.JobSucceeded, store.JobFailed, store.JobRollbackFailed} {
		t.Run(string(final), func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				awaited := enqueue(t, db, "test.a", "site:a")
				dependent := enqueueAfter(t, db, "site:b", awaited.ID)
				if dependent.AfterJobID != awaited.ID {
					t.Fatalf("Enqueue = %+v", dependent)
				}
				if got, err := store.NewJobs(db).Get(ctx, dependent.ID); err != nil || got.AfterJobID != awaited.ID {
					t.Fatalf("Get = %+v, %v", got, err)
				}
				now := time.Now().Add(time.Second)
				leased := leaseAll(t, db, now)
				if _, ok := leased[awaited.ID]; !ok || len(leased) != 1 {
					t.Fatalf("leased %v, want only the awaited job", leased)
				}
				// Waiting to retry isn't final.
				if err := store.NewJobs(db).Retry(ctx, awaited.ID, leased[awaited.ID].LeaseOwner, now, "boom", now); err != nil {
					t.Fatal(err)
				}
				retry, ok := lease(t, db, now)
				if !ok || retry.ID != awaited.ID {
					t.Fatalf("leased %+v, %v; want the awaited job's retry", retry, ok)
				}
				if j, ok := lease(t, db, now); ok {
					t.Fatalf("leased %s while the awaited job runs", j.ID)
				}
				finish(t, db, retry, final, now)
				if j, ok := lease(t, db, now); !ok || j.ID != dependent.ID {
					t.Fatalf("after the awaited job was %s leased %+v, %v; want the dependent", final, j, ok)
				}
			})
		})
	}
}

func TestJobsAfterMissing(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		j := store.Job{Type: "test.a", TargetType: store.JobTargetSite, TargetID: id.New(), AfterJobID: id.New()}
		if err := store.NewJobs(db).Enqueue(context.Background(), &j); !errors.Is(err, store.ErrAfterJobNotFound) {
			t.Errorf("Enqueue after a missing job: %v, want ErrAfterJobNotFound", err)
		}
	})
}

// A waiting job doesn't hold up later jobs on its lock key, and takes its
// place in line again once its awaited job is final.
func TestJobsAfterLockKey(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		awaited := enqueue(t, db, "test.a", "site:a")
		dependent := enqueueAfter(t, db, "site:b", awaited.ID)
		later := enqueue(t, db, "test.a", "site:b")
		now := time.Now().Add(time.Second)
		leased := leaseAll(t, db, now)
		_, gotAwaited := leased[awaited.ID]
		_, gotLater := leased[later.ID]
		if len(leased) != 2 || !gotAwaited || !gotLater {
			t.Fatalf("leased %v, want the awaited job and the later job on the dependent's key", leased)
		}
		// The later job has started, so it keeps site:b: the dependent waits
		// for it even though its awaited job is now final.
		finish(t, db, leased[awaited.ID], store.JobSucceeded, now)
		if j, ok := lease(t, db, now); ok {
			t.Fatalf("leased %s while a started job holds its key", j.ID)
		}
		last := enqueue(t, db, "test.a", "site:b")
		finish(t, db, leased[later.ID], store.JobSucceeded, now)
		// No longer waiting, the dependent is ahead of jobs enqueued after it.
		if j, ok := lease(t, db, now); !ok || j.ID != dependent.ID {
			t.Fatalf("leased %+v, %v; want the dependent", j, ok)
		}
		if j, ok := lease(t, db, now); ok {
			t.Fatalf("leased %s while the dependent runs, want %s to wait", j.ID, last.ID)
		}
	})
}

// Two chains crossing the same pair of lock keys in opposite directions,
// such as a staging push and refresh queued together, both finish.
func TestJobsAfterCrossingChains(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		backupA := enqueue(t, db, "test.a", "site:a")
		copyB := enqueueAfter(t, db, "site:b", backupA.ID)
		backupB := enqueue(t, db, "test.a", "site:b")
		copyA := enqueueAfter(t, db, "site:a", backupB.ID)
		pending := map[string]bool{backupA.ID: true, copyB.ID: true, backupB.ID: true, copyA.ID: true}
		now := time.Now().Add(time.Second)
		var rounds [][]string
		for len(pending) > 0 {
			leased := leaseAll(t, db, now)
			if len(leased) == 0 {
				t.Fatalf("deadlock: nothing runnable after rounds %v", rounds)
			}
			var round []string
			for _, j := range leased {
				round = append(round, j.ID)
				delete(pending, j.ID)
				finish(t, db, j, store.JobSucceeded, now)
			}
			slices.Sort(round)
			rounds = append(rounds, round)
		}
		first := []string{backupA.ID, backupB.ID}
		slices.Sort(first)
		if len(rounds) != 2 || !slices.Equal(rounds[0], first) {
			t.Errorf("rounds = %v, want both backups, then both copies", rounds)
		}
	})
}
