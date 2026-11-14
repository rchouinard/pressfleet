package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// writeTimeout bounds each bookkeeping write. Bookkeeping writes don't use
// the step's context, so that recording a step's outcome still works when
// shutdown has canceled the step.
const writeTimeout = 10 * time.Second

// execution is one attempt at a leased job.
type execution struct {
	p     *Pool
	h     Handler
	job   store.Job
	owner string
	jobs  store.Jobs
	// ctx is passed to steps. It is canceled when the lease is lost or the
	// shutdown grace period ends.
	ctx context.Context
	// stopping is closed when the pool stops leasing; no new step starts
	// after that.
	stopping <-chan struct{}
	run      *Run
	logger   *slog.Logger
}

// execute runs one attempt at job. stopping may be nil.
func (p *Pool) execute(parent context.Context, stopping <-chan struct{}, job store.Job) {
	logger := p.log.With("job_id", job.ID, "job_type", job.Type, "attempt", job.Attempts)
	h, ok := p.handler(job.Type)
	if !ok {
		// Lease only returns registered types; this is a bug.
		logger.Error("leased a job without a handler")
		return
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	e := &execution{
		p: p, h: h, job: job, owner: job.LeaseOwner, jobs: store.NewJobs(p.db),
		ctx: ctx, stopping: stopping, logger: logger,
	}

	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		e.heartbeat(ctx, cancel)
	}()
	defer func() {
		cancel(nil)
		<-heartbeatDone
	}()

	run, err := newRun(e)
	if err != nil {
		logger.Error("job data is unreadable", "error", err)
		e.finish(store.JobRollbackFailed, err.Error())
		return
	}
	e.run = run
	logger.Info("job started", "state", job.State)
	e.process()
}

func (e *execution) process() {
	if e.job.Attempts > e.job.MaxAttempts && e.job.State == store.JobRunning {
		// A worker can die after committing the last step but before
		// finishing the job. The work is done, so rolling it back would
		// undo a completed restore or update.
		steps, ok := e.steps()
		if !ok {
			return
		}
		if e.allSucceeded(steps) {
			if e.finish(store.JobSucceeded, "") {
				e.logger.Info("job succeeded; a previous worker finished every step before it died")
			}
			return
		}
	}
	if e.job.Attempts > e.job.MaxAttempts {
		// Only reachable when workers keep dying mid-attempt (e.g. a step
		// that crashes the process): step errors are counted before retrying.
		msg := fmt.Sprintf("gave up after %d attempts that never finished", e.job.MaxAttempts)
		if e.job.State == store.JobRollingBack {
			e.finish(store.JobRollbackFailed, joinErrors(e.job.Error, "rollback: "+msg))
			return
		}
		if !e.beginRollback(msg) {
			return
		}
	}
	if e.job.State == store.JobRollingBack {
		e.rollback()
		return
	}
	e.forward()
}

// allSucceeded reports whether steps are exactly the handler's steps, all
// succeeded. Steps that don't match the handler don't count, so the rollback
// path sees them.
func (e *execution) allSucceeded(steps []store.JobStep) bool {
	if len(steps) == 0 || len(steps) != len(e.h.Steps) {
		return false
	}
	for i, st := range steps {
		if st.State != store.StepSucceeded || st.Name != e.h.Steps[i].Name {
			return false
		}
	}
	return true
}

// forward runs the steps that haven't succeeded, in order.
func (e *execution) forward() {
	names := make([]string, len(e.h.Steps))
	for i, s := range e.h.Steps {
		names[i] = s.Name
	}
	if !e.write(func(ctx context.Context) error { return e.jobs.InitSteps(ctx, e.job.ID, e.owner, names) }) {
		return
	}
	steps, ok := e.steps()
	if !ok {
		return
	}
	for i, st := range e.h.Steps {
		if i >= len(steps) || steps[i].Name != st.Name {
			// The handler's steps changed since the job started, e.g. across
			// an upgrade. Running the new steps against old progress isn't
			// safe; roll back what was recorded.
			e.fail(Permanent(fmt.Errorf("the %s handler's steps no longer match the job's recorded steps", e.h.Type)), i, st.Name)
			return
		}
		if steps[i].State == store.StepSucceeded {
			continue
		}
		if e.isStopping() {
			e.release()
			return
		}
		if !e.write(func(ctx context.Context) error { return e.jobs.StartStep(ctx, e.job.ID, e.owner, i, e.p.now()) }) {
			return
		}
		e.run.step = i
		err := e.call(st.Do)
		if e.leaseLost() {
			return
		}
		if err == nil {
			if !e.write(func(ctx context.Context) error {
				return e.jobs.FinishStep(ctx, e.job.ID, e.owner, i, store.StepSucceeded, "", e.p.now())
			}) {
				return
			}
			continue
		}
		if e.interrupted() {
			e.logger.Info("job step interrupted by shutdown", "step", st.Name, "error", err)
			e.release()
			return
		}
		e.fail(err, i, st.Name)
		return
	}
	if e.finish(store.JobSucceeded, "") {
		e.logger.Info("job succeeded")
	}
}

// fail records a step's error, then retries the job or rolls it back.
func (e *execution) fail(err error, seq int, name string) {
	msg := fmt.Sprintf("%s: %v", name, err)
	if !e.write(func(ctx context.Context) error {
		return e.jobs.FinishStep(ctx, e.job.ID, e.owner, seq, store.StepFailed, err.Error(), e.p.now())
	}) {
		return
	}
	e.writeLog(seq, slog.LevelError, fmt.Sprintf("Step failed (attempt %d of %d): %v", e.job.Attempts, e.job.MaxAttempts, err))
	if !IsPermanent(err) && e.job.Attempts < e.job.MaxAttempts {
		delay := e.p.opts.Backoff(e.job.Attempts)
		e.writeLog(seq, slog.LevelInfo, fmt.Sprintf("Retrying in %s.", delay))
		now := e.p.now()
		if e.write(func(ctx context.Context) error {
			return e.jobs.Retry(ctx, e.job.ID, e.owner, now.Add(delay), msg, now)
		}) {
			e.logger.Warn("job step failed; will retry", "step", name, "error", err, "retry_in", delay)
		}
		return
	}
	e.logger.Warn("job failed; rolling back", "step", name, "error", err, "permanent", IsPermanent(err))
	if e.beginRollback(msg) {
		e.rollback()
	}
}

func (e *execution) beginRollback(msg string) bool {
	if !e.write(func(ctx context.Context) error { return e.jobs.BeginRollback(ctx, e.job.ID, e.owner, msg, e.p.now()) }) {
		return false
	}
	e.job.State, e.job.Error, e.job.Attempts = store.JobRollingBack, msg, 1
	return true
}

// rollback runs Undo for every step that started, newest first.
func (e *execution) rollback() {
	steps, ok := e.steps()
	if !ok {
		return
	}
	for i := len(steps) - 1; i >= 0; i-- {
		row := steps[i]
		if row.State == store.StepPending || row.State == store.StepRolledBack {
			continue
		}
		st, ok := e.h.stepNamed(row.Name)
		if !ok {
			e.writeLog(row.Seq, slog.LevelError, "Can't roll back this step: this release doesn't know it.")
			e.finish(store.JobRollbackFailed, joinErrors(e.job.Error, fmt.Sprintf("rollback: unknown step %q", row.Name)))
			return
		}
		if st.Undo == nil {
			continue
		}
		if e.isStopping() {
			e.release()
			return
		}
		e.run.step = row.Seq
		err := e.call(st.Undo)
		if e.leaseLost() {
			return
		}
		if err == nil {
			if !e.write(func(ctx context.Context) error {
				return e.jobs.FinishStep(ctx, e.job.ID, e.owner, row.Seq, store.StepRolledBack, "", e.p.now())
			}) {
				return
			}
			e.writeLog(row.Seq, slog.LevelInfo, "Rolled back.")
			continue
		}
		if e.interrupted() {
			e.logger.Info("job rollback interrupted by shutdown", "step", row.Name, "error", err)
			e.release()
			return
		}
		if !e.write(func(ctx context.Context) error {
			return e.jobs.FinishStep(ctx, e.job.ID, e.owner, row.Seq, store.StepRollbackFailed, err.Error(), e.p.now())
		}) {
			return
		}
		e.writeLog(row.Seq, slog.LevelError, fmt.Sprintf("Rollback failed (attempt %d of %d): %v", e.job.Attempts, e.job.MaxAttempts, err))
		if !IsPermanent(err) && e.job.Attempts < e.job.MaxAttempts {
			delay := e.p.opts.Backoff(e.job.Attempts)
			now := e.p.now()
			if e.write(func(ctx context.Context) error {
				return e.jobs.RetryRollback(ctx, e.job.ID, e.owner, now.Add(delay), now)
			}) {
				e.logger.Warn("job rollback step failed; will retry", "step", row.Name, "error", err, "retry_in", delay)
			}
			return
		}
		e.logger.Error("job rollback failed; resources may be left behind", "step", row.Name, "error", err)
		e.finish(store.JobRollbackFailed, joinErrors(e.job.Error, fmt.Sprintf("rollback of %s: %v", row.Name, err)))
		return
	}
	if e.finish(store.JobFailed, e.job.Error) {
		e.logger.Info("job failed and rolled back", "error", e.job.Error)
	}
}

func joinErrors(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// call runs a step function, turning a panic into a permanent error.
func (e *execution) call(fn StepFunc) (err error) {
	defer func() {
		if r := recover(); r != nil {
			e.logger.Error("job step panicked", "panic", r, "stack", string(debug.Stack()))
			err = Permanent(fmt.Errorf("panic: %v", r))
		}
	}()
	return fn(e.ctx, e.run)
}

func (e *execution) steps() ([]store.JobStep, bool) {
	var steps []store.JobStep
	ok := e.write(func(ctx context.Context) error {
		var err error
		steps, err = e.jobs.Steps(ctx, e.job.ID)
		return err
	})
	return steps, ok
}

func (e *execution) isStopping() bool {
	if e.stopping == nil {
		return false
	}
	select {
	case <-e.stopping:
		return true
	default:
		return false
	}
}

func (e *execution) interrupted() bool {
	return errors.Is(context.Cause(e.ctx), errShutdown)
}

func (e *execution) leaseLost() bool {
	if errors.Is(context.Cause(e.ctx), errLeaseLost) {
		e.logger.Warn("job lease lost; another worker has taken over")
		return true
	}
	return false
}

// release hands the job back without using up an attempt.
func (e *execution) release() {
	if e.write(func(ctx context.Context) error { return e.jobs.Release(ctx, e.job.ID, e.owner, e.p.now()) }) {
		e.logger.Info("job released for another worker to resume")
	}
}

func (e *execution) finish(state store.JobState, msg string) bool {
	ok := e.write(func(ctx context.Context) error {
		return e.jobs.Finish(ctx, e.job.ID, e.owner, state, msg, e.p.now())
	})
	if ok && state == store.JobRollbackFailed && e.h.OnRollbackFailed != nil {
		e.afterRollbackFailed(msg)
	}
	return ok
}

// afterRollbackFailed runs the handler's OnRollbackFailed hook. Its failure
// is only logged: the job is finished either way.
func (e *execution) afterRollbackFailed(msg string) {
	ctx, cancel := e.writeContext()
	defer cancel()
	job := e.job
	job.State, job.Error = store.JobRollbackFailed, msg
	defer func() {
		if r := recover(); r != nil {
			e.logger.Error("job rollback_failed hook panicked", "panic", r, "stack", string(debug.Stack()))
		}
	}()
	if err := e.h.OnRollbackFailed(ctx, job); err != nil {
		e.logger.Error("job rollback_failed hook failed", "error", err)
	}
}

func (e *execution) writeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(e.ctx), writeTimeout)
}

// write runs a bookkeeping write and reports whether it worked. On failure
// the attempt is abandoned: with a lost lease the job belongs to another
// worker, and after a database error the lease expires and the job is
// retried from its recorded progress.
func (e *execution) write(fn func(ctx context.Context) error) bool {
	ctx, cancel := e.writeContext()
	defer cancel()
	err := fn(ctx)
	switch {
	case err == nil:
		e.p.opts.Changes.Notify(e.job.ID)
		return true
	case errors.Is(err, store.ErrLeaseLost):
		e.logger.Warn("job lease lost; another worker has taken over")
	default:
		e.logger.Error("recording job progress failed; the job will be retried when its lease expires", "error", err)
	}
	return false
}

// writeLog stores a log line and mirrors it to the control plane log.
func (e *execution) writeLog(step int, level slog.Level, msg string) {
	e.logger.Log(e.ctx, level, "job log", "step", step, "message", msg)
	ctx, cancel := e.writeContext()
	defer cancel()
	if err := e.jobs.AppendLog(ctx, e.job.ID, step, levelName(level), msg, e.p.now()); err != nil {
		e.logger.Warn("storing a job log line failed", "error", err)
		return
	}
	e.p.opts.Changes.Notify(e.job.ID)
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	default:
		return "info"
	}
}

// heartbeat extends the lease until ctx is done. If the lease is gone, it
// cancels the job's steps.
func (e *execution) heartbeat(ctx context.Context, cancel context.CancelCauseFunc) {
	t := time.NewTicker(e.p.opts.LeaseDuration / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
		now := e.p.now()
		err := e.jobs.Heartbeat(wctx, e.job.ID, e.owner, now.Add(e.p.opts.LeaseDuration), now)
		wcancel()
		switch {
		case err == nil:
		case errors.Is(err, store.ErrLeaseLost):
			cancel(errLeaseLost)
			return
		case ctx.Err() != nil:
			return
		default:
			e.logger.Warn("job heartbeat failed", "error", err)
		}
	}
}
