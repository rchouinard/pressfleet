package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// AwaitedJob is the outcome of the job a job waited for (Spec.After).
type AwaitedJob struct {
	ID   string
	Type string
	// State is final: succeeded, failed, or rollback_failed.
	State store.JobState
	// Error is why the awaited job failed; empty if it succeeded.
	Error  string
	result map[string]json.RawMessage
}

// Succeeded reports whether the awaited job completed every step.
func (a AwaitedJob) Succeeded() bool { return a.State == store.JobSucceeded }

// Err returns nil if the awaited job succeeded, and otherwise a permanent
// error wrapping ErrAwaitedJobFailed. A dependent job's first step returns
// it, so the job rolls back without retrying: retrying can't change the
// outcome of a final job.
func (a AwaitedJob) Err() error {
	if a.Succeeded() {
		return nil
	}
	msg := fmt.Sprintf("%s job %s %s", a.Type, a.ID, a.State)
	if a.Error != "" {
		msg += ": " + a.Error
	}
	return Permanent(fmt.Errorf("%s: %w", msg, ErrAwaitedJobFailed))
}

// GetResult decodes the value stored under key in the awaited job's result
// into v, reporting whether it exists.
func (a AwaitedJob) GetResult(key string, v any) (bool, error) {
	return get(a.result, key, v)
}

// Awaited returns the outcome of the job this job waited for. The pool
// only leases the job once that job is final, so the outcome can't change.
// It returns ErrNotWaiting if the job wasn't enqueued with Spec.After, and
// a permanent store.ErrAfterJobNotFound if the awaited job has been
// deleted since.
func (r *Run) Awaited(ctx context.Context) (AwaitedJob, error) {
	afterID := r.e.job.AfterJobID
	if afterID == "" {
		return AwaitedJob{}, fmt.Errorf("job %s: %w", r.e.job.ID, ErrNotWaiting)
	}
	j, err := store.NewJobs(r.e.p.db).Get(ctx, afterID)
	if errors.Is(err, store.ErrNotFound) {
		return AwaitedJob{}, Permanent(fmt.Errorf("job %s waited for job %s: %w", r.e.job.ID, afterID, store.ErrAfterJobNotFound))
	}
	if err != nil {
		return AwaitedJob{}, fmt.Errorf("reading awaited job: %w", err)
	}
	if !j.State.Final() {
		// Lease never hands out a job whose awaited job isn't final, so
		// this is a bug; running on would act on a half-done prerequisite.
		return AwaitedJob{}, Permanent(fmt.Errorf("awaited job %s is %s, not final", j.ID, j.State))
	}
	a := AwaitedJob{ID: j.ID, Type: j.Type, State: j.State, Error: j.Error}
	if err := json.Unmarshal(orEmpty(j.Result), &a.result); err != nil {
		return AwaitedJob{}, Permanent(fmt.Errorf("decoding result of awaited job %s: %w", j.ID, err))
	}
	return a, nil
}
