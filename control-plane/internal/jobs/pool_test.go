package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs/jobstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// recorder records step calls in order.
type recorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, s)
}

func (r *recorder) get() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// step returns a step that records "do:<name>" and "undo:<name>" and fails
// Do with the errors in fails, one per call, then succeeds.
func (r *recorder) step(name string, fails ...error) jobs.Step {
	var n atomic.Int32
	return jobs.Step{
		Name: name,
		Do: func(context.Context, *jobs.Run) error {
			r.add("do:" + name)
			if i := int(n.Add(1)) - 1; i < len(fails) {
				return fails[i]
			}
			return nil
		},
		Undo: func(context.Context, *jobs.Run) error {
			r.add("undo:" + name)
			return nil
		},
	}
}

type fixture struct {
	db    *store.DB
	clock *jobstest.Clock
	pool  *jobs.Pool
}

func newFixture(t *testing.T, db *store.DB, opts jobs.Options, handlers ...jobs.Handler) *fixture {
	t.Helper()
	f := &fixture{db: db, clock: jobstest.NewClock(time.Now().Add(time.Second))}
	opts.Clock = f.clock
	if opts.PollInterval == 0 {
		opts.PollInterval = 5 * time.Millisecond
	}
	if opts.Backoff == nil {
		opts.Backoff = func(attempt int) time.Duration { return time.Duration(attempt) * time.Minute }
	}
	opts.Logger = storetest.Logger()
	f.pool = jobs.NewPool(db, opts)
	for _, h := range handlers {
		if err := f.pool.Register(h); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *fixture) enqueue(t *testing.T, typ, lockKey string, payload any) store.Job {
	t.Helper()
	j, err := jobs.Enqueue(context.Background(), f.db, jobs.Spec{
		Type: typ, TargetType: "site", TargetID: id.New(), LockKey: lockKey, Payload: payload, MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func (f *fixture) runNext(t *testing.T) bool {
	t.Helper()
	ran, err := f.pool.RunNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return ran
}

func (f *fixture) detail(t *testing.T, jobID string) store.JobDetail {
	t.Helper()
	d, err := store.NewJobs(f.db).Detail(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func stepStates(d store.JobDetail) string {
	var s []string
	for _, st := range d.Steps {
		s = append(s, fmt.Sprintf("%s=%s/%d", st.Name, st.State, st.Attempts))
	}
	return fmt.Sprint(s)
}

func TestRegisterValidates(t *testing.T) {
	db := storetest.Open(t, store.SQLite)
	pool := jobs.NewPool(db, jobs.Options{})
	noop := func(context.Context, *jobs.Run) error { return nil }
	for _, h := range []jobs.Handler{
		{Steps: []jobs.Step{{Name: "a", Do: noop}}},
		{Type: "t"},
		{Type: "t", Steps: []jobs.Step{{Do: noop}}},
		{Type: "t", Steps: []jobs.Step{{Name: "a"}}},
		{Type: "t", Steps: []jobs.Step{{Name: "a", Do: noop}, {Name: "a", Do: noop}}},
	} {
		if err := pool.Register(h); err == nil {
			t.Errorf("Register(%+v) accepted", h)
		}
	}
	ok := jobs.Handler{Type: "t", Steps: []jobs.Step{{Name: "a", Do: noop}}}
	if err := pool.Register(ok); err != nil {
		t.Fatal(err)
	}
	if err := pool.Register(ok); err == nil {
		t.Error("duplicate Register accepted")
	}
}

// TestStepsShareDataAndResults runs a job whose steps pass data forward,
// set results, store a secret result, and log.
func TestStepsShareDataAndResults(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		secretStore := secrets.NewStore(db, secretstest.Keyring(t, 1))
		type payload struct {
			Name string `json:"name"`
		}
		h := jobs.Handler{Type: "site.create", Steps: []jobs.Step{
			{Name: "allocate", Do: func(_ context.Context, r *jobs.Run) error {
				var p payload
				if err := r.Payload(&p); err != nil {
					return err
				}
				r.Logf("allocating %s with password %v", p.Name, redact.Secret("hunter2"))
				return r.Set("site_id", "site-"+p.Name)
			}},
			{Name: "install", Do: func(_ context.Context, r *jobs.Run) error {
				var siteID string
				if ok, err := r.Get("site_id", &siteID); !ok || err != nil {
					return fmt.Errorf("site_id missing: %w", err)
				}
				if err := r.SetResult("site_id", siteID); err != nil {
					return err
				}
				return r.SetSecretResult("admin_password", "wp-admin-password", redact.Secret("s3cret"))
			}},
		}}
		f := newFixture(t, db, jobs.Options{Secrets: secretStore}, h)
		j := f.enqueue(t, "site.create", "", payload{Name: "acme"})
		if !f.runNext(t) {
			t.Fatal("no job ran")
		}
		d := f.detail(t, j.ID)
		if d.Job.State != store.JobSucceeded || stepStates(d) != "[allocate=succeeded/1 install=succeeded/1]" {
			t.Fatalf("job %s, steps %s, error %q", d.Job.State, stepStates(d), d.Job.Error)
		}
		var result struct {
			SiteID        string            `json:"site_id"`
			AdminPassword jobs.SecretResult `json:"admin_password"`
		}
		if err := json.Unmarshal(d.Job.Result, &result); err != nil {
			t.Fatal(err)
		}
		if result.SiteID != "site-acme" || result.AdminPassword.SecretRef == "" {
			t.Fatalf("result = %s", d.Job.Result)
		}
		plain, err := secretStore.Get(context.Background(), result.AdminPassword.SecretRef)
		if err != nil || plain.Reveal() != "s3cret" {
			t.Errorf("secret result = %v, %v", plain, err)
		}
		if string(d.Job.Data) != `{"site_id":"site-acme"}` {
			t.Errorf("data = %s", d.Job.Data)
		}
		if len(d.Logs) != 1 || d.Logs[0].StepSeq != 0 || d.Logs[0].Message != "allocating acme with password [REDACTED]" {
			t.Errorf("logs = %+v", d.Logs)
		}
		if f.runNext(t) {
			t.Error("ran a finished job again")
		}
	})
}

func TestRetryWithBackoff(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		var rec recorder
		flaky := errors.New("flaky")
		h := jobs.Handler{Type: "t", Steps: []jobs.Step{rec.step("one"), rec.step("two", flaky, flaky), rec.step("three")}}
		f := newFixture(t, db, jobs.Options{}, h)
		j := f.enqueue(t, "t", "", nil)

		f.runNext(t)
		d := f.detail(t, j.ID)
		if d.Job.State != store.JobQueued || d.Job.Error != "two: flaky" || !d.Job.RunAfter.Equal(f.clock.Now().Add(time.Minute).Truncate(time.Microsecond)) {
			t.Fatalf("after first failure: %+v", d.Job)
		}
		if f.runNext(t) {
			t.Fatal("retried before the backoff elapsed")
		}
		f.clock.Advance(time.Minute)
		f.runNext(t)
		if d := f.detail(t, j.ID); d.Job.State != store.JobQueued || !d.Job.RunAfter.Equal(f.clock.Now().Add(2*time.Minute).Truncate(time.Microsecond)) {
			t.Fatalf("second backoff: %+v", d.Job)
		}
		f.clock.Advance(2 * time.Minute)
		f.runNext(t)
		d = f.detail(t, j.ID)
		if d.Job.State != store.JobSucceeded || d.Job.Attempts != 3 {
			t.Fatalf("after retries: state %s attempts %d error %q", d.Job.State, d.Job.Attempts, d.Job.Error)
		}
		// Completed steps are skipped on retry.
		want := "[do:one do:two do:two do:two do:three]"
		if got := fmt.Sprint(rec.get()); got != want {
			t.Errorf("calls = %s, want %s", got, want)
		}
		if got := stepStates(d); got != "[one=succeeded/1 two=succeeded/3 three=succeeded/1]" {
			t.Errorf("steps = %s", got)
		}
	})
}

func TestFailureRollsBack(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name      string
		fails     []error
		runs      int
		wantCalls string
	}{
		{
			name:      "permanent error",
			fails:     []error{jobs.Permanent(boom)},
			runs:      1,
			wantCalls: "[do:one do:two do:three undo:three undo:one]",
		},
		{
			name:      "attempts used up",
			fails:     []error{boom, boom, boom},
			runs:      3,
			wantCalls: "[do:one do:two do:three do:three do:three undo:three undo:one]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				var rec recorder
				// Step two has nothing to undo, and step four never starts.
				noUndo := rec.step("two")
				noUndo.Undo = nil
				h := jobs.Handler{Type: "t", Steps: []jobs.Step{rec.step("one"), noUndo, rec.step("three", tt.fails...), rec.step("four")}}
				f := newFixture(t, db, jobs.Options{}, h)
				j := f.enqueue(t, "t", "site:1", nil)
				for range tt.runs {
					f.runNext(t)
					f.clock.Advance(time.Hour)
				}
				d := f.detail(t, j.ID)
				if d.Job.State != store.JobFailed || d.Job.Error != "three: boom" || d.Job.FinishedAt.IsZero() {
					t.Fatalf("job = %s %q", d.Job.State, d.Job.Error)
				}
				if got := fmt.Sprint(rec.get()); got != tt.wantCalls {
					t.Errorf("calls = %s, want %s", got, tt.wantCalls)
				}
				want := fmt.Sprintf("[one=rolled_back/1 two=succeeded/1 three=rolled_back/%d four=pending/0]", tt.runs)
				if got := stepStates(d); got != want {
					t.Errorf("steps = %s, want %s", got, want)
				}
				// The lock key is free again.
				next := f.enqueue(t, "t", "site:1", nil)
				f.runNext(t)
				if d := f.detail(t, next.ID); d.Job.State != store.JobSucceeded {
					t.Errorf("next job on the key: %s %q", d.Job.State, d.Job.Error)
				}
			})
		})
	}
}

func TestRollbackFailures(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		undoErr := errors.New("docker unavailable")
		var undoCalls atomic.Int32
		h := jobs.Handler{Type: "t", Steps: []jobs.Step{
			{Name: "create", Do: func(context.Context, *jobs.Run) error { return nil },
				Undo: func(context.Context, *jobs.Run) error {
					if undoCalls.Add(1) == 1 {
						return undoErr // retried
					}
					return jobs.Permanent(undoErr) // gives up
				}},
			{Name: "fail", Do: func(context.Context, *jobs.Run) error { return jobs.Permanent(errors.New("bad input")) }},
		}}
		f := newFixture(t, db, jobs.Options{}, h)
		j := f.enqueue(t, "t", "", nil)
		f.runNext(t)
		d := f.detail(t, j.ID)
		if d.Job.State != store.JobRollingBack || d.Job.LeaseOwner != "" || stepStates(d) != "[create=rollback_failed/1 fail=failed/1]" {
			t.Fatalf("after a failed undo: %s %s", d.Job.State, stepStates(d))
		}
		if f.runNext(t) {
			t.Fatal("rollback retried before the backoff")
		}
		f.clock.Advance(time.Hour)
		f.runNext(t)
		d = f.detail(t, j.ID)
		if d.Job.State != store.JobRollbackFailed || d.Job.Error != "fail: bad input; rollback of create: docker unavailable" {
			t.Fatalf("final: %s %q", d.Job.State, d.Job.Error)
		}
		if undoCalls.Load() != 2 {
			t.Errorf("undo ran %d times", undoCalls.Load())
		}
	})
}

func TestPanicFailsPermanently(t *testing.T) {
	db := storetest.Open(t, store.SQLite)
	h := jobs.Handler{Type: "t", Steps: []jobs.Step{{Name: "p", Do: func(context.Context, *jobs.Run) error { panic("oops") }}}}
	f := newFixture(t, db, jobs.Options{}, h)
	j := f.enqueue(t, "t", "", nil)
	f.runNext(t)
	if d := f.detail(t, j.ID); d.Job.State != store.JobFailed || d.Job.Error != "p: panic: oops" {
		t.Errorf("job = %s %q", d.Job.State, d.Job.Error)
	}
}

// TestResumeAfterWorkerDeath simulates a worker that died during the second
// step: its lease expires and another worker resumes, skipping the first.
func TestResumeAfterWorkerDeath(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		var rec recorder
		h := jobs.Handler{Type: "t", Steps: []jobs.Step{rec.step("one"), rec.step("two"), rec.step("three")}}
		f := newFixture(t, db, jobs.Options{LeaseDuration: time.Minute}, h)
		j := f.enqueue(t, "t", "site:1", nil)

		// The dead worker's progress.
		sj := store.NewJobs(db)
		now := f.clock.Now()
		dead, err := sj.Lease(ctx, []string{"t"}, "dead-worker", now, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		must(t, sj.InitSteps(ctx, j.ID, dead.LeaseOwner, []string{"one", "two", "three"}))
		must(t, sj.StartStep(ctx, j.ID, dead.LeaseOwner, 0, now))
		must(t, sj.FinishStep(ctx, j.ID, dead.LeaseOwner, 0, store.StepSucceeded, "", now))
		must(t, sj.StartStep(ctx, j.ID, dead.LeaseOwner, 1, now))

		if f.runNext(t) {
			t.Fatal("took over a job whose lease hasn't expired")
		}
		f.clock.Advance(2 * time.Minute)
		if !f.runNext(t) {
			t.Fatal("expired job not taken over")
		}
		d := f.detail(t, j.ID)
		if d.Job.State != store.JobSucceeded || d.Job.Attempts != 2 {
			t.Fatalf("job = %s attempts %d", d.Job.State, d.Job.Attempts)
		}
		if got := fmt.Sprint(rec.get()); got != "[do:two do:three]" {
			t.Errorf("calls = %s, want the interrupted step and the rest", got)
		}
		if got := stepStates(d); got != "[one=succeeded/1 two=succeeded/2 three=succeeded/1]" {
			t.Errorf("steps = %s", got)
		}
	})
}

// TestCrashLoopGivesUp: a job whose workers keep dying uses up its attempts
// and rolls back.
func TestCrashLoopGivesUp(t *testing.T) {
	ctx := context.Background()
	db := storetest.Open(t, store.SQLite)
	var rec recorder
	f := newFixture(t, db, jobs.Options{}, jobs.Handler{Type: "t", Steps: []jobs.Step{rec.step("one")}})
	j := f.enqueue(t, "t", "", nil) // MaxAttempts 3
	for range 3 {
		now := f.clock.Now()
		if _, err := store.NewJobs(db).Lease(ctx, []string{"t"}, "dies-"+id.New(), now, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(2 * time.Minute)
	}
	f.runNext(t)
	d := f.detail(t, j.ID)
	if d.Job.State != store.JobFailed || d.Job.Error != "gave up after 3 attempts that never finished" {
		t.Errorf("job = %s %q", d.Job.State, d.Job.Error)
	}
	if len(rec.get()) != 0 {
		t.Errorf("calls = %v", rec.get())
	}
}

// TestLeaseLost: when another worker takes a job over, the old worker's step
// is canceled and it writes nothing more.
func TestLeaseLost(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		started := make(chan struct{})
		canceled := make(chan struct{})
		h := jobs.Handler{Type: "t", Steps: []jobs.Step{{Name: "slow", Do: func(ctx context.Context, _ *jobs.Run) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			return ctx.Err()
		}}}}
		f := newFixture(t, db, jobs.Options{LeaseDuration: 30 * time.Millisecond}, h)
		j := f.enqueue(t, "t", "", nil)
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.runNext(t)
		}()
		<-started
		f.clock.Advance(time.Minute)
		now := f.clock.Now()
		taken, err := store.NewJobs(db).Lease(ctx, []string{"t"}, "new-owner", now, now.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-canceled:
		case <-time.After(5 * time.Second):
			t.Fatal("step not canceled after the lease was lost")
		}
		<-done
		d := f.detail(t, j.ID)
		if d.Job.LeaseOwner != taken.LeaseOwner || d.Job.State != store.JobRunning || d.Steps[0].State != store.StepRunning {
			t.Errorf("old worker changed the job: %+v %s", d.Job, stepStates(d))
		}
	})
}

// TestGracefulShutdown: on shutdown the running step finishes, no new step
// starts, and the job is released for the next process without using an
// attempt.
func TestGracefulShutdown(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		var rec recorder
		started, proceed := make(chan struct{}), make(chan struct{})
		first := jobs.Step{Name: "one", Do: func(context.Context, *jobs.Run) error {
			rec.add("do:one")
			close(started)
			<-proceed
			return nil
		}}
		h := jobs.Handler{Type: "t", Steps: []jobs.Step{first, rec.step("two")}}
		f := newFixture(t, db, jobs.Options{ShutdownGrace: time.Minute}, h)
		j := f.enqueue(t, "t", "", nil)

		ctx, cancel := context.WithCancel(context.Background())
		stopped := make(chan error, 1)
		go func() { stopped <- f.pool.Run(ctx) }()
		<-started
		cancel()
		select {
		case <-stopped:
			t.Fatal("Run returned while a step was running")
		case <-time.After(20 * time.Millisecond):
		}
		close(proceed)
		if err := <-stopped; err != nil {
			t.Fatal(err)
		}
		d := f.detail(t, j.ID)
		if d.Job.State != store.JobRunning || d.Job.LeaseOwner != "" || d.Job.Attempts != 0 || stepStates(d) != "[one=succeeded/1 two=pending/0]" {
			t.Fatalf("after shutdown: %+v %s", d.Job, stepStates(d))
		}

		// The next process resumes at once.
		f2 := newFixture(t, db, jobs.Options{}, h)
		f2.clock.Set(f.clock.Now())
		f2.runNext(t)
		if d := f2.detail(t, j.ID); d.Job.State != store.JobSucceeded || d.Job.Attempts != 1 {
			t.Fatalf("after resume: %s attempts %d", d.Job.State, d.Job.Attempts)
		}
		if got := fmt.Sprint(rec.get()); got != "[do:one do:two]" {
			t.Errorf("calls = %s", got)
		}
	})
}

// TestShutdownGraceExpires: a step still running when the grace period ends
// is canceled, and the job is released to run the step again.
func TestShutdownGraceExpires(t *testing.T) {
	db := storetest.Open(t, store.SQLite)
	started := make(chan struct{})
	h := jobs.Handler{Type: "t", Steps: []jobs.Step{{Name: "slow", Do: func(ctx context.Context, _ *jobs.Run) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}}}
	f := newFixture(t, db, jobs.Options{ShutdownGrace: 20 * time.Millisecond}, h)
	j := f.enqueue(t, "t", "", nil)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- f.pool.Run(ctx) }()
	<-started
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the grace period")
	}
	d := f.detail(t, j.ID)
	if d.Job.State != store.JobRunning || d.Job.LeaseOwner != "" || d.Job.Attempts != 0 || d.Steps[0].State != store.StepRunning || d.Job.Error != "" {
		t.Errorf("after shutdown: %+v %s", d.Job, stepStates(d))
	}
}

// TestPoolConcurrency runs jobs with different lock keys concurrently and
// jobs with the same key one at a time, in order.
func TestPoolConcurrency(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		const keys = 3
		var (
			mu        sync.Mutex
			running   = map[string]int{}
			maxAll    int
			total     int
			order     = map[string][]int{}
			allActive = make(chan struct{})
			once      sync.Once
		)
		type payload struct {
			Key string `json:"key"`
			N   int    `json:"n"`
		}
		h := jobs.Handler{Type: "t", Steps: []jobs.Step{{Name: "work", Do: func(ctx context.Context, r *jobs.Run) error {
			var p payload
			if err := r.Payload(&p); err != nil {
				return err
			}
			mu.Lock()
			running[p.Key]++
			if running[p.Key] > 1 {
				mu.Unlock()
				return jobs.Permanent(fmt.Errorf("two jobs with key %s at once", p.Key))
			}
			total++
			maxAll = max(maxAll, total)
			order[p.Key] = append(order[p.Key], p.N)
			if total == keys {
				once.Do(func() { close(allActive) })
			}
			mu.Unlock()
			// The first wave waits until one job per key is running at
			// once, proving different keys run concurrently.
			select {
			case <-allActive:
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
			}
			mu.Lock()
			running[p.Key]--
			total--
			mu.Unlock()
			return nil
		}}}}
		f := newFixture(t, db, jobs.Options{Concurrency: 8}, h)
		var ids []string
		for n := range 3 {
			for k := range keys {
				key := fmt.Sprintf("site:%d", k)
				ids = append(ids, f.enqueue(t, "t", key, payload{Key: key, N: n}).ID)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		stopped := make(chan error, 1)
		go func() { stopped <- f.pool.Run(ctx) }()
		deadline := time.Now().Add(10 * time.Second)
		for {
			page, err := store.NewJobs(db).List(context.Background(), store.JobFilter{States: []store.JobState{store.JobSucceeded, store.JobFailed}})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Jobs) == len(ids) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("only %d of %d jobs finished", len(page.Jobs), len(ids))
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		if err := <-stopped; err != nil {
			t.Fatal(err)
		}
		for _, jobID := range ids {
			if d := f.detail(t, jobID); d.Job.State != store.JobSucceeded {
				t.Errorf("job %s: %s %q", jobID, d.Job.State, d.Job.Error)
			}
		}
		if maxAll != keys {
			t.Errorf("at most %d jobs ran at once, want %d", maxAll, keys)
		}
		for k, ns := range order {
			if fmt.Sprint(ns) != "[0 1 2]" {
				t.Errorf("key %s ran jobs in order %v", k, ns)
			}
		}
	})
}

func TestEnqueueRollsBackWithTransaction(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		boom := errors.New("site insert failed")
		var jobID string
		err := db.InTx(ctx, func(q store.Querier) error {
			j, err := jobs.Enqueue(ctx, q, jobs.Spec{Type: "site.create", TargetType: "site", TargetID: id.New(), LockKey: "site:x"})
			if err != nil {
				return err
			}
			jobID = j.ID
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatal(err)
		}
		if _, err := store.NewJobs(db).Get(ctx, jobID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("job survived the rolled-back transaction: %v", err)
		}
		if _, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: "t", TargetType: "site", TargetID: "x", Payload: []int{1}}); err == nil {
			t.Error("Enqueue accepted a non-object payload")
		}
	})
}

func TestNoSecretStore(t *testing.T) {
	db := storetest.Open(t, store.SQLite)
	var got error
	h := jobs.Handler{Type: "t", Steps: []jobs.Step{{Name: "s", Do: func(_ context.Context, r *jobs.Run) error {
		got = r.SetSecretResult("pw", "wp-admin-password", redact.Secret("x"))
		return nil
	}}}}
	f := newFixture(t, db, jobs.Options{}, h)
	f.enqueue(t, "t", "", nil)
	f.runNext(t)
	if !errors.Is(got, jobs.ErrNoSecretStore) {
		t.Errorf("SetSecretResult without a store: %v", got)
	}
}

func TestDefaultBackoff(t *testing.T) {
	for _, tt := range []struct {
		attempt int
		want    time.Duration
	}{{1, 15 * time.Second}, {2, 30 * time.Second}, {3, time.Minute}, {7, 15 * time.Minute}, {50, 15 * time.Minute}} {
		if got := jobs.DefaultBackoff(tt.attempt); got != tt.want {
			t.Errorf("DefaultBackoff(%d) = %s, want %s", tt.attempt, got, tt.want)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
