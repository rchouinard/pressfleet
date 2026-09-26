package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

var testTypes = []string{"test.a", "test.b"}

func enqueue(t *testing.T, q store.Querier, typ, lockKey string) store.Job {
	t.Helper()
	j := store.Job{Type: typ, TargetType: "site", TargetID: id.New(), LockKey: lockKey, Payload: json.RawMessage(`{"n":1}`)}
	if err := store.NewJobs(q).Enqueue(context.Background(), &j); err != nil {
		t.Fatal(err)
	}
	return j
}

// lease leases at now with a one-minute lease, returning the job ID or "".
func lease(t *testing.T, db *store.DB, now time.Time) (store.Job, bool) {
	t.Helper()
	j, err := store.NewJobs(db).Lease(context.Background(), testTypes, "owner-"+id.New(), now, now.Add(time.Minute))
	if errors.Is(err, store.ErrNotFound) {
		return store.Job{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return j, true
}

func TestJobsEnqueueLeaseFinish(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		jobs := store.NewJobs(db)
		j := enqueue(t, db, "test.a", "site:1")
		if !id.Valid(j.ID) || j.State != store.JobQueued || j.Seq < 1 || j.MaxAttempts != store.DefaultMaxAttempts || j.RunAfter.IsZero() {
			t.Fatalf("Enqueue = %+v", j)
		}
		if err := jobs.Enqueue(ctx, &store.Job{Type: "x", TargetType: "site", TargetID: "1", Payload: json.RawMessage(`[1]`)}); err == nil {
			t.Error("Enqueue accepted a non-object payload")
		}
		if _, ok := lease(t, db, j.RunAfter.Add(-time.Second)); ok {
			t.Fatal("leased a job before its RunAfter")
		}
		if _, err := jobs.Lease(ctx, []string{"other"}, "o", time.Now().Add(time.Hour), time.Now().Add(2*time.Hour)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("Lease of an unregistered type: %v", err)
		}
		now := time.Now().Add(time.Second)
		got, ok := lease(t, db, now)
		if !ok || got.ID != j.ID || got.State != store.JobRunning || got.Attempts != 1 || got.LeaseOwner == "" || got.StartedAt.IsZero() ||
			string(got.Payload) != `{"n":1}` || got.LockKey != "site:1" {
			t.Fatalf("Lease = %+v, %v", got, ok)
		}
		if err := jobs.Finish(ctx, j.ID, "someone-else", store.JobSucceeded, "", now); !errors.Is(err, store.ErrLeaseLost) {
			t.Errorf("Finish by another owner: %v", err)
		}
		if err := jobs.Finish(ctx, j.ID, got.LeaseOwner, store.JobQueued, "", now); err == nil {
			t.Error("Finish accepted a non-final state")
		}
		if err := jobs.SetResult(ctx, j.ID, got.LeaseOwner, json.RawMessage(`{"url":"x"}`), now); err != nil {
			t.Fatal(err)
		}
		if err := jobs.Finish(ctx, j.ID, got.LeaseOwner, store.JobSucceeded, "", now); err != nil {
			t.Fatal(err)
		}
		done, err := jobs.Get(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if done.State != store.JobSucceeded || done.LeaseOwner != "" || done.FinishedAt.IsZero() || string(done.Result) != `{"url":"x"}` {
			t.Errorf("finished job = %+v", done)
		}
		if _, ok := lease(t, db, now.Add(time.Hour)); ok {
			t.Error("leased a finished job")
		}
		if _, err := jobs.Get(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get missing: %v", err)
		}
	})
}

func TestJobsLockKeys(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		jobs := store.NewJobs(db)
		a1 := enqueue(t, db, "test.a", "site:a")
		a2 := enqueue(t, db, "test.b", "site:a")
		b1 := enqueue(t, db, "test.a", "site:b")
		free := enqueue(t, db, "test.a", "")
		a3 := enqueue(t, db, "test.a", "site:a")

		now := time.Now().Add(time.Second)
		var order []string
		leased := map[string]store.Job{}
		for {
			j, ok := lease(t, db, now)
			if !ok {
				break
			}
			order = append(order, j.ID)
			leased[j.ID] = j
		}
		want := []string{a1.ID, b1.ID, free.ID}
		if fmt.Sprint(order) != fmt.Sprint(want) {
			t.Fatalf("leased %v, want %v (one per lock key, plus the unkeyed job)", order, want)
		}

		for _, j := range []store.Job{b1, free} {
			if err := jobs.Finish(ctx, j.ID, leased[j.ID].LeaseOwner, store.JobSucceeded, "", now); err != nil {
				t.Fatal(err)
			}
		}

		// a1 fails and waits to retry: it keeps site:a, so a2 still waits.
		if err := jobs.Retry(ctx, a1.ID, leased[a1.ID].LeaseOwner, now.Add(time.Minute), "boom", now); err != nil {
			t.Fatal(err)
		}
		if j, ok := lease(t, db, now); ok {
			t.Fatalf("leased %s while a1 waits to retry", j.ID)
		}
		later := now.Add(2 * time.Minute)
		j, ok := lease(t, db, later)
		if !ok || j.ID != a1.ID || j.Attempts != 2 || j.Error != "boom" {
			t.Fatalf("retry lease = %+v, %v; want a1 attempt 2", j, ok)
		}
		if err := jobs.Finish(ctx, a1.ID, j.LeaseOwner, store.JobSucceeded, "", later); err != nil {
			t.Fatal(err)
		}
		// FIFO: a2 before a3.
		if j, ok := lease(t, db, later); !ok || j.ID != a2.ID {
			t.Fatalf("after a1 finished leased %+v, want a2", j)
		}
		if j, ok := lease(t, db, later); ok {
			t.Fatalf("leased %s while a2 runs", j.ID)
		}
		_ = a3
	})
}

func TestJobsLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		jobs := store.NewJobs(db)
		j := enqueue(t, db, "test.a", "site:x")
		next := enqueue(t, db, "test.a", "site:x")
		now := time.Now().Add(time.Second)
		first, _ := lease(t, db, now)
		if err := jobs.Heartbeat(ctx, j.ID, first.LeaseOwner, now.Add(90*time.Second), now.Add(30*time.Second)); err != nil {
			t.Fatal(err)
		}
		if got, ok := lease(t, db, now.Add(80*time.Second)); ok {
			t.Fatalf("leased %s before the heartbeat-extended lease expired", got.ID)
		}
		second, ok := lease(t, db, now.Add(2*time.Minute))
		if !ok || second.ID != j.ID || second.Attempts != 2 || second.LeaseOwner == first.LeaseOwner {
			t.Fatalf("after expiry leased %+v, %v; want the same job, attempt 2", second, ok)
		}
		if err := jobs.Heartbeat(ctx, j.ID, first.LeaseOwner, now.Add(time.Hour), now); !errors.Is(err, store.ErrLeaseLost) {
			t.Errorf("old owner's heartbeat: %v, want ErrLeaseLost", err)
		}
		if err := jobs.SetData(ctx, j.ID, first.LeaseOwner, json.RawMessage(`{}`), now); !errors.Is(err, store.ErrLeaseLost) {
			t.Errorf("old owner's write: %v, want ErrLeaseLost", err)
		}

		// Release hands the job back at once without using an attempt, and
		// it keeps its lock key.
		if err := jobs.Release(ctx, j.ID, second.LeaseOwner, now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		third, ok := lease(t, db, now.Add(2*time.Minute))
		if !ok || third.ID != j.ID || third.Attempts != 2 || third.State != store.JobRunning {
			t.Fatalf("after release leased %+v, %v", third, ok)
		}
		_ = next
	})
}

func TestJobsSteps(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		jobs := store.NewJobs(db)
		j := enqueue(t, db, "test.a", "")
		now := time.Now().Add(time.Second)
		l, _ := lease(t, db, now)
		if err := jobs.InitSteps(ctx, j.ID, "nobody", []string{"one"}); !errors.Is(err, store.ErrLeaseLost) {
			t.Errorf("InitSteps without lease: %v", err)
		}
		if err := jobs.InitSteps(ctx, j.ID, l.LeaseOwner, []string{"one", "two"}); err != nil {
			t.Fatal(err)
		}
		if err := jobs.InitSteps(ctx, j.ID, l.LeaseOwner, []string{"one", "two"}); err != nil {
			t.Fatalf("InitSteps again: %v", err)
		}
		for range 2 {
			if err := jobs.StartStep(ctx, j.ID, l.LeaseOwner, 0, now); err != nil {
				t.Fatal(err)
			}
		}
		if err := jobs.FinishStep(ctx, j.ID, l.LeaseOwner, 0, store.StepSucceeded, "", now); err != nil {
			t.Fatal(err)
		}
		if err := jobs.StartStep(ctx, j.ID, "nobody", 1, now); !errors.Is(err, store.ErrLeaseLost) {
			t.Errorf("StartStep without lease: %v", err)
		}
		if err := jobs.AppendLog(ctx, j.ID, 0, "info", "hello", now); err != nil {
			t.Fatal(err)
		}
		if err := jobs.AppendLog(ctx, j.ID, -1, "warn", "job-level", now); err != nil {
			t.Fatal(err)
		}
		d, err := jobs.Detail(ctx, j.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Steps) != 2 || d.Steps[0].State != store.StepSucceeded || d.Steps[0].Attempts != 2 || d.Steps[0].StartedAt.IsZero() ||
			d.Steps[1].State != store.StepPending || d.Steps[1].Name != "two" {
			t.Errorf("steps = %+v", d.Steps)
		}
		if len(d.Logs) != 2 || d.Logs[0].StepSeq != 0 || d.Logs[0].Message != "hello" || d.Logs[1].StepSeq != -1 || d.Logs[1].Level != "warn" {
			t.Errorf("logs = %+v", d.Logs)
		}
		tail, err := jobs.Logs(ctx, j.ID, d.Logs[0].ID, 10)
		if err != nil || len(tail) != 1 || tail[0].Message != "job-level" {
			t.Errorf("Logs after first = %+v, %v", tail, err)
		}
		byJob, err := jobs.StepsForJobs(ctx, []string{j.ID, id.New()})
		if err != nil || len(byJob) != 1 || len(byJob[j.ID]) != 2 {
			t.Errorf("StepsForJobs = %+v, %v", byJob, err)
		}
	})
}

func TestJobsList(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		jobs := store.NewJobs(db)
		site := id.New()
		var all []store.Job
		for i := range 5 {
			j := store.Job{Type: "test.a", TargetType: "site", TargetID: site}
			if i%2 == 1 {
				j.Type, j.TargetType, j.TargetID = "test.b", "client_group", id.New()
			}
			if err := jobs.Enqueue(ctx, &j); err != nil {
				t.Fatal(err)
			}
			all = append(all, j)
			time.Sleep(2 * time.Millisecond) // distinct ID timestamps, for a stable newest-first order
		}
		var got []string
		cursor := ""
		pages := 0
		for {
			page, err := jobs.List(ctx, store.JobFilter{Limit: 2, Before: cursor})
			if err != nil {
				t.Fatal(err)
			}
			pages++
			for _, j := range page.Jobs {
				got = append(got, j.ID)
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		want := []string{all[4].ID, all[3].ID, all[2].ID, all[1].ID, all[0].ID}
		if fmt.Sprint(got) != fmt.Sprint(want) || pages != 3 {
			t.Errorf("paged list = %v in %d pages, want %v in 3", got, pages, want)
		}
		for _, tt := range []struct {
			name string
			f    store.JobFilter
			want int
		}{
			{"target", store.JobFilter{TargetType: "site", TargetID: site}, 3},
			{"type", store.JobFilter{Types: []string{"test.b"}}, 2},
			{"state", store.JobFilter{States: []store.JobState{store.JobQueued}}, 5},
			{"no match", store.JobFilter{States: []store.JobState{store.JobFailed}}, 0},
		} {
			page, err := jobs.List(ctx, tt.f)
			if err != nil || len(page.Jobs) != tt.want || page.NextCursor != "" {
				t.Errorf("%s: %d jobs, cursor %q, err %v; want %d", tt.name, len(page.Jobs), page.NextCursor, err, tt.want)
			}
		}
	})
}

func TestJobsEnqueueInTransaction(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		boom := errors.New("boom")
		var jobID string
		err := db.InTx(ctx, func(q store.Querier) error {
			jobID = enqueue(t, q, "test.a", "").ID
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatal(err)
		}
		if _, err := store.NewJobs(db).Get(ctx, jobID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("job from a rolled-back transaction: %v", err)
		}
	})
}

// TestJobsConcurrentLease leases from many goroutines at once: every job is
// leased exactly once, and jobs sharing a lock key never run together.
func TestJobsConcurrentLease(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		const n = 30
		for i := range n {
			enqueue(t, db, "test.a", fmt.Sprintf("key:%d", i%5))
		}
		now := time.Now().Add(time.Second)
		var (
			mu      sync.Mutex
			seen    = map[string]int{}
			running = map[string]bool{}
			wg      sync.WaitGroup
			errs    = make(chan error, 100)
		)
		for w := range 8 {
			wg.Go(func() {
				jobs := store.NewJobs(db)
				for {
					mu.Lock()
					done := len(seen) == n
					mu.Unlock()
					if done {
						return
					}
					owner := fmt.Sprintf("w%d-%s", w, id.New())
					j, err := jobs.Lease(ctx, testTypes, owner, now, now.Add(time.Hour))
					if errors.Is(err, store.ErrNotFound) {
						time.Sleep(time.Millisecond)
						continue
					}
					if err != nil {
						errs <- err
						return
					}
					mu.Lock()
					seen[j.ID]++
					if running[j.LockKey] {
						errs <- fmt.Errorf("two jobs with lock key %s leased at once", j.LockKey)
					}
					running[j.LockKey] = true
					mu.Unlock()
					time.Sleep(time.Millisecond)
					mu.Lock()
					running[j.LockKey] = false
					mu.Unlock()
					if err := jobs.Finish(ctx, j.ID, owner, store.JobSucceeded, "", now); err != nil {
						errs <- err
						return
					}
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		for jobID, c := range seen {
			if c != 1 {
				t.Errorf("job %s leased %d times", jobID, c)
			}
		}
		if len(seen) != n {
			t.Errorf("leased %d jobs, want %d", len(seen), n)
		}
	})
}
