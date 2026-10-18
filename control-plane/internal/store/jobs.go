package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// ErrLeaseLost means the caller no longer holds the job's lease: it expired
// and another worker took the job over, or the job finished. The caller must
// stop working on the job without writing anything else.
var ErrLeaseLost = errors.New("job lease lost")

// JobState is where a job is in its lifecycle. Package jobs documents the
// transitions.
type JobState string

// Job states. Succeeded, Failed, and RollbackFailed are final.
const (
	// JobQueued jobs wait for a worker, either for the first time or to
	// retry after an error (RunAfter says when).
	JobQueued JobState = "queued"
	// JobRunning jobs are leased by a worker running their steps, or were
	// leased by a worker that stopped and wait for the lease to expire.
	JobRunning JobState = "running"
	// JobRollingBack jobs failed permanently and are running the Undo of
	// the steps they started, newest first.
	JobRollingBack JobState = "rolling_back"
	// JobSucceeded jobs completed every step.
	JobSucceeded JobState = "succeeded"
	// JobFailed jobs failed permanently and rolled back what they created.
	JobFailed JobState = "failed"
	// JobRollbackFailed jobs failed permanently, and rolling back failed
	// too: resources may be left behind and need an operator.
	JobRollbackFailed JobState = "rollback_failed"
)

// Valid reports whether s is a known state.
func (s JobState) Valid() bool {
	switch s {
	case JobQueued, JobRunning, JobRollingBack, JobSucceeded, JobFailed, JobRollbackFailed:
		return true
	}
	return false
}

// Final reports whether the job is done and will never run again.
func (s JobState) Final() bool {
	return s == JobSucceeded || s == JobFailed || s == JobRollbackFailed
}

// StepState is the progress of one job step.
type StepState string

// Step states.
const (
	StepPending        StepState = "pending"
	StepRunning        StepState = "running"
	StepSucceeded      StepState = "succeeded"
	StepFailed         StepState = "failed" // the last attempt failed
	StepRolledBack     StepState = "rolled_back"
	StepRollbackFailed StepState = "rollback_failed"
)

// DefaultMaxAttempts is used when a job doesn't set MaxAttempts.
const DefaultMaxAttempts = 5

// Job target types: what Job.TargetType says Job.TargetID names.
const (
	JobTargetSite        = "site"
	JobTargetClientGroup = "client_group"
)

// SiteLockKey is the lock key of every job that changes a site, whichever
// package runs it, so jobs on one site run one at a time (design doc §5).
// Define it nowhere else: two spellings would let them run together.
func SiteLockKey(siteID string) string { return "site:" + siteID }

// Job is a unit of background work (design doc §5).
type Job struct {
	ID         string
	Type       string
	TargetType string
	TargetID   string
	State      JobState
	// Payload is the job's input: JSON, never containing secrets.
	Payload json.RawMessage
	// Data is state the job's steps pass forward: a JSON object.
	Data json.RawMessage
	// Result holds non-secret outputs for API clients: a JSON object.
	Result json.RawMessage
	// Error is the most recent error: why the job failed, or why it is
	// waiting to retry.
	Error string
	// LockKey serializes jobs: jobs with the same non-empty key run one at
	// a time, in the order they were enqueued.
	LockKey string
	// Attempts counts leases in the current phase (running, then rolling
	// back); MaxAttempts bounds each phase.
	Attempts        int
	MaxAttempts     int
	CreatedByUserID string // empty for jobs started by the platform
	ScheduleID      string // empty unless a schedule enqueued the job
	ScheduledFor    time.Time
	// RunAfter is the earliest time the job may be leased.
	RunAfter       time.Time
	LeaseOwner     string
	LeaseExpiresAt time.Time
	// Seq orders jobs for FIFO per lock key.
	Seq        int64
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
	UpdatedAt  time.Time
}

// JobStep is a job step's persisted progress.
type JobStep struct {
	JobID      string
	Seq        int
	Name       string
	State      StepState
	Error      string
	Attempts   int
	StartedAt  time.Time
	FinishedAt time.Time
}

// JobLog is one log line written by a job step.
type JobLog struct {
	ID       int64
	JobID    string
	StepSeq  int // -1 for lines that don't belong to a step
	LoggedAt time.Time
	Level    string // info, warn, error
	Message  string
}

// Jobs reads and writes jobs, their steps, and their logs. The queue logic
// lives in package jobs; these are its queries.
type Jobs struct {
	q Querier
}

// NewJobs returns a Jobs that runs queries on q.
func NewJobs(q Querier) Jobs {
	return Jobs{q: q}
}

const jobColumns = "id, type, target_type, target_id, state, params, data, result, error, lock_key, attempts, max_attempts, " +
	"created_by_user_id, schedule_id, scheduled_for, run_after, lease_owner, lease_expires_at, seq, " +
	"created_at, started_at, finished_at, updated_at"

func scanJob(s scanner) (Job, error) {
	var (
		j                                       Job
		payload, data, result                   string
		lockKey, createdBy, scheduleID, leaseBy sql.NullString
		scheduledFor, leaseExp, started, done   sql.NullTime
	)
	err := s.Scan(&j.ID, &j.Type, &j.TargetType, &j.TargetID, &j.State, &payload, &data, &result, &j.Error,
		&lockKey, &j.Attempts, &j.MaxAttempts, &createdBy, &scheduleID, &scheduledFor, &j.RunAfter,
		&leaseBy, &leaseExp, &j.Seq, &j.CreatedAt, &started, &done, &j.UpdatedAt)
	j.Payload, j.Data, j.Result = json.RawMessage(payload), json.RawMessage(data), json.RawMessage(result)
	j.LockKey, j.CreatedByUserID, j.ScheduleID, j.LeaseOwner = lockKey.String, createdBy.String, scheduleID.String, leaseBy.String
	j.ScheduledFor, j.LeaseExpiresAt, j.StartedAt, j.FinishedAt = scheduledFor.Time, leaseExp.Time, started.Time, done.Time
	return j, err
}

// nullString stores "" as NULL.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullTime stores the zero time as NULL.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// jsonObject returns raw, or "{}" if it is empty, after checking that it is
// a JSON object.
func jsonObject(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "{}", nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return "", errors.New("must be a JSON object")
	}
	return string(raw), nil
}

// jobIDs keeps jobs enqueued in the same millisecond in order, so List's
// newest-first order (by ID) matches the order they were enqueued.
var jobIDs id.Monotonic

// Enqueue inserts j as a queued job, filling in its ID, state, sequence
// number, and timestamps. RunAfter defaults to now and MaxAttempts to
// DefaultMaxAttempts. Run it inside DB.InTx to enqueue atomically with
// other changes.
func (r Jobs) Enqueue(ctx context.Context, j *Job) error {
	if j.Type == "" || j.TargetType == "" || j.TargetID == "" {
		return errors.New("enqueue job: type, target type, and target ID are required")
	}
	payload, err := jsonObject(j.Payload)
	if err != nil {
		return fmt.Errorf("enqueue job: payload %w", err)
	}
	e := *j
	e.ID = jobIDs.NewAt(time.Now())
	e.State = JobQueued
	e.Payload, e.Data, e.Result = json.RawMessage(payload), json.RawMessage("{}"), json.RawMessage("{}")
	e.Error, e.Attempts, e.LeaseOwner, e.LeaseExpiresAt = "", 0, "", time.Time{}
	e.StartedAt, e.FinishedAt = time.Time{}, time.Time{}
	e.CreatedAt = now()
	e.UpdatedAt = e.CreatedAt
	if e.RunAfter.IsZero() {
		e.RunAfter = e.CreatedAt
	}
	e.RunAfter = utc(e.RunAfter)
	e.ScheduledFor = utc(e.ScheduledFor)
	if e.MaxAttempts <= 0 {
		e.MaxAttempts = DefaultMaxAttempts
	}
	// seq is MAX+1 rather than a sequence or identity column so the query
	// stays portable. Concurrent PostgreSQL transactions can draw the same
	// value; ties are broken by ID, and concurrent enqueues have no
	// meaningful order anyway.
	err = r.q.QueryRowContext(ctx,
		`INSERT INTO jobs (id, type, target_type, target_id, state, params, data, result, lock_key, max_attempts,
			created_by_user_id, schedule_id, scheduled_for, run_after, created_at, updated_at, seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM jobs))
		RETURNING seq`,
		e.ID, e.Type, e.TargetType, e.TargetID, e.State, payload, "{}", "{}", nullString(e.LockKey), e.MaxAttempts,
		nullString(e.CreatedByUserID), nullString(e.ScheduleID), nullTime(e.ScheduledFor), e.RunAfter,
		e.CreatedAt, e.UpdatedAt).Scan(&e.Seq)
	if err != nil {
		return fmt.Errorf("enqueue %s job: %w", e.Type, err)
	}
	*j = e
	return nil
}

// Lease claims the next runnable job whose type is in types for owner until
// expires, and returns it with its state set to running (or still
// rolling_back) and Attempts incremented. It returns ErrNotFound if no job
// is runnable. A job is runnable if RunAfter has passed and either
//
//   - it is queued and no other job with its lock key is ahead of it: one
//     that is running or rolling back, one that has started and is queued
//     to retry, or one that is queued with a lower sequence number; or
//   - it is running or rolling back and its lease has expired or was
//     released (its worker died, was stopped, or is waiting to retry a
//     rollback).
//
// Lease is a single statement. PostgreSQL workers skip rows other workers
// have locked (FOR UPDATE SKIP LOCKED); SQLite runs one writer at a time.
func (r Jobs) Lease(ctx context.Context, types []string, owner string, now, expires time.Time) (Job, error) {
	if len(types) == 0 {
		return Job{}, ErrNotFound
	}
	if owner == "" {
		return Job{}, errors.New("lease job: owner is required")
	}
	lock := ""
	if r.q.Dialect() == Postgres {
		lock = " FOR UPDATE SKIP LOCKED"
	}
	args := []any{owner, expires, now, now}
	for _, t := range types {
		args = append(args, t)
	}
	args = append(args, now, now)
	query := `UPDATE jobs SET
			state = CASE WHEN state = 'queued' THEN 'running' ELSE state END,
			lease_owner = ?, lease_expires_at = ?, attempts = attempts + 1,
			started_at = COALESCE(started_at, ?), updated_at = ?
		WHERE id = (
			SELECT j.id FROM jobs j
			WHERE j.type IN (` + placeholders(len(types)) + `) AND j.run_after <= ?
			AND (
				(j.state = 'queued' AND (j.lock_key IS NULL OR NOT EXISTS (
					SELECT 1 FROM jobs o
					WHERE o.lock_key = j.lock_key AND o.id <> j.id AND (
						o.state IN ('running', 'rolling_back')
						OR (o.state = 'queued' AND o.started_at IS NOT NULL AND j.started_at IS NULL)
						OR (o.state = 'queued' AND (o.started_at IS NULL) = (j.started_at IS NULL)
							AND (o.seq < j.seq OR (o.seq = j.seq AND o.id < j.id)))
					)
				)))
				OR (j.state IN ('running', 'rolling_back') AND (j.lease_expires_at IS NULL OR j.lease_expires_at < ?))
			)
			ORDER BY j.seq, j.id
			LIMIT 1` + lock + `
		)
		RETURNING ` + jobColumns
	j, err := scanJob(r.q.QueryRowContext(ctx, query, args...))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Job{}, ErrNotFound
		}
		return Job{}, fmt.Errorf("lease job: %w", err)
	}
	return j, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// Get returns the job with the given ID, or ErrNotFound.
func (r Jobs) Get(ctx context.Context, jobID string) (Job, error) {
	j, err := scanJob(r.q.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id = ?", jobID))
	if err != nil {
		return Job{}, fmt.Errorf("get job %s: %w", jobID, err)
	}
	return j, nil
}

// JobFilter selects jobs for List. Zero fields don't filter.
type JobFilter struct {
	States     []JobState
	Types      []string
	TargetType string
	TargetID   string
	ScheduleID string
	// Before is a cursor: only jobs with a smaller ID (older ones) are
	// returned. Pass the previous page's NextCursor.
	Before string
	// Limit is the page size: default 50, at most 200.
	Limit int
}

// JobPage is one page of List results, newest first.
type JobPage struct {
	Jobs []Job
	// NextCursor is the Before value for the next page, or empty on the
	// last page.
	NextCursor string
}

// List returns jobs matching f, newest first, one page at a time.
func (r Jobs) List(ctx context.Context, f JobFilter) (JobPage, error) {
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = 50
	case limit > 200:
		limit = 200
	}
	var (
		where []string
		args  []any
	)
	if len(f.States) > 0 {
		where = append(where, "state IN ("+placeholders(len(f.States))+")")
		for _, s := range f.States {
			args = append(args, s)
		}
	}
	if len(f.Types) > 0 {
		where = append(where, "type IN ("+placeholders(len(f.Types))+")")
		for _, t := range f.Types {
			args = append(args, t)
		}
	}
	for _, c := range []struct{ col, val string }{
		{"target_type", f.TargetType}, {"target_id", f.TargetID}, {"schedule_id", f.ScheduleID},
	} {
		if c.val != "" {
			where = append(where, c.col+" = ?")
			args = append(args, c.val)
		}
	}
	if f.Before != "" {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}
	query := "SELECT " + jobColumns + " FROM jobs"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// Fetch one extra row to learn whether there is another page.
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return JobPage{}, fmt.Errorf("list jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := JobPage{Jobs: []Job{}}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return JobPage{}, fmt.Errorf("list jobs: %w", err)
		}
		page.Jobs = append(page.Jobs, j)
	}
	if err := rows.Err(); err != nil {
		return JobPage{}, fmt.Errorf("list jobs: %w", err)
	}
	if len(page.Jobs) > limit {
		page.Jobs = page.Jobs[:limit]
		page.NextCursor = page.Jobs[limit-1].ID
	}
	return page, nil
}

// Steps returns a job's steps in order.
func (r Jobs) Steps(ctx context.Context, jobID string) ([]JobStep, error) {
	return r.stepsWhere(ctx, "job_id = ?", jobID)
}

// StepsForJobs returns the steps of several jobs, keyed by job ID, for
// listings.
func (r Jobs) StepsForJobs(ctx context.Context, jobIDs []string) (map[string][]JobStep, error) {
	out := map[string][]JobStep{}
	if len(jobIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(jobIDs))
	for i, v := range jobIDs {
		args[i] = v
	}
	steps, err := r.stepsWhere(ctx, "job_id IN ("+placeholders(len(jobIDs))+")", args...)
	if err != nil {
		return nil, err
	}
	for _, s := range steps {
		out[s.JobID] = append(out[s.JobID], s)
	}
	return out, nil
}

func (r Jobs) stepsWhere(ctx context.Context, where string, args ...any) ([]JobStep, error) {
	rows, err := r.q.QueryContext(ctx,
		"SELECT job_id, seq, name, state, error, attempts, started_at, finished_at FROM job_steps WHERE "+where+" ORDER BY job_id, seq",
		args...)
	if err != nil {
		return nil, fmt.Errorf("get job steps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	steps := []JobStep{}
	for rows.Next() {
		var (
			s             JobStep
			started, done sql.NullTime
		)
		if err := rows.Scan(&s.JobID, &s.Seq, &s.Name, &s.State, &s.Error, &s.Attempts, &started, &done); err != nil {
			return nil, fmt.Errorf("get job steps: %w", err)
		}
		s.StartedAt, s.FinishedAt = started.Time, done.Time
		steps = append(steps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get job steps: %w", err)
	}
	return steps, nil
}

// Logs returns up to limit of a job's log lines with IDs greater than
// afterID, oldest first. Pass the last ID seen to tail a running job.
func (r Jobs) Logs(ctx context.Context, jobID string, afterID int64, limit int) ([]JobLog, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := r.q.QueryContext(ctx,
		"SELECT id, job_id, step_seq, logged_at, level, message FROM job_logs WHERE job_id = ? AND id > ? ORDER BY id LIMIT ?",
		jobID, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("get job logs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	logs := []JobLog{}
	for rows.Next() {
		var (
			l   JobLog
			seq sql.NullInt64
		)
		if err := rows.Scan(&l.ID, &l.JobID, &seq, &l.LoggedAt, &l.Level, &l.Message); err != nil {
			return nil, fmt.Errorf("get job logs: %w", err)
		}
		l.StepSeq = -1
		if seq.Valid {
			l.StepSeq = int(seq.Int64)
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get job logs: %w", err)
	}
	return logs, nil
}

// TargetJobLog is a log line of a job, with what identifies the job and
// step it belongs to.
type TargetJobLog struct {
	JobLog
	JobType string
	// StepName is empty for lines that don't belong to a step.
	StepName string
}

// LogsForTarget returns up to limit log lines of the jobs for one target
// (for example a site), newest first, with IDs below beforeID. A zero
// beforeID starts at the newest line. Line IDs grow with time, so passing
// the last ID of a page continues with the older lines.
func (r Jobs) LogsForTarget(ctx context.Context, targetType, targetID string, beforeID int64, limit int) ([]TargetJobLog, error) {
	if limit <= 0 {
		limit = 200
	}
	query := `SELECT l.id, l.job_id, l.step_seq, l.logged_at, l.level, l.message, j.type, COALESCE(s.name, '')
FROM job_logs l
JOIN jobs j ON j.id = l.job_id
LEFT JOIN job_steps s ON s.job_id = l.job_id AND s.seq = l.step_seq
WHERE j.target_type = ? AND j.target_id = ?`
	args := []any{targetType, targetID}
	if beforeID > 0 {
		query += " AND l.id < ?"
		args = append(args, beforeID)
	}
	query += " ORDER BY l.id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get target job logs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	logs := []TargetJobLog{}
	for rows.Next() {
		var (
			l   TargetJobLog
			seq sql.NullInt64
		)
		if err := rows.Scan(&l.ID, &l.JobID, &seq, &l.LoggedAt, &l.Level, &l.Message, &l.JobType, &l.StepName); err != nil {
			return nil, fmt.Errorf("get target job logs: %w", err)
		}
		l.StepSeq = -1
		if seq.Valid {
			l.StepSeq = int(seq.Int64)
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get target job logs: %w", err)
	}
	return logs, nil
}

// JobDetail is a job with its steps and logs.
type JobDetail struct {
	Job   Job
	Steps []JobStep
	// Logs holds the first MaxDetailLogs lines; use Logs for the rest.
	Logs []JobLog
}

// MaxDetailLogs caps the log lines Detail returns.
const MaxDetailLogs = 1000

// Detail returns a job with its steps and logs, or ErrNotFound.
func (r Jobs) Detail(ctx context.Context, jobID string) (JobDetail, error) {
	var d JobDetail
	var err error
	if d.Job, err = r.Get(ctx, jobID); err != nil {
		return JobDetail{}, err
	}
	if d.Steps, err = r.Steps(ctx, jobID); err != nil {
		return JobDetail{}, err
	}
	if d.Logs, err = r.Logs(ctx, jobID, 0, MaxDetailLogs); err != nil {
		return JobDetail{}, err
	}
	return d, nil
}

// AppendLog adds a log line to a job. stepSeq is -1 for a line that doesn't
// belong to a step. The message must not contain secrets.
func (r Jobs) AppendLog(ctx context.Context, jobID string, stepSeq int, level, message string, at time.Time) error {
	var seq any
	if stepSeq >= 0 {
		seq = stepSeq
	}
	if _, err := r.q.ExecContext(ctx,
		"INSERT INTO job_logs (job_id, step_seq, logged_at, level, message) VALUES (?, ?, ?, ?, ?)",
		jobID, seq, at, level, message); err != nil {
		return fmt.Errorf("append job log: %w", err)
	}
	return nil
}

// HasQueuedForSchedule reports whether a job enqueued by the schedule is
// still waiting for its first run.
func (r Jobs) HasQueuedForSchedule(ctx context.Context, scheduleID string) (bool, error) {
	var n int
	if err := r.q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM jobs WHERE schedule_id = ? AND state = 'queued' AND started_at IS NULL",
		scheduleID).Scan(&n); err != nil {
		return false, fmt.Errorf("check queued jobs for schedule %s: %w", scheduleID, err)
	}
	return n > 0, nil
}

// The methods below change a leased job. Each takes the lease owner and
// returns ErrLeaseLost, changing nothing, if the caller no longer holds the
// lease.

// leased runs an UPDATE on the job that only applies while owner holds the
// lease.
func (r Jobs) leased(ctx context.Context, what, jobID, owner, set string, args ...any) error {
	args = append(args, jobID, owner)
	res, err := r.q.ExecContext(ctx, "UPDATE jobs SET "+set+" WHERE id = ? AND lease_owner = ?", args...)
	if err != nil {
		return fmt.Errorf("%s job %s: %w", what, jobID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s job %s: %w", what, jobID, err)
	}
	if n == 0 {
		return fmt.Errorf("%s job %s: %w", what, jobID, ErrLeaseLost)
	}
	return nil
}

// Heartbeat extends the lease to expires.
func (r Jobs) Heartbeat(ctx context.Context, jobID, owner string, expires, now time.Time) error {
	return r.leased(ctx, "heartbeat", jobID, owner, "lease_expires_at = ?, updated_at = ?", expires, now)
}

// SetData replaces the job's step data, a JSON object.
func (r Jobs) SetData(ctx context.Context, jobID, owner string, data json.RawMessage, now time.Time) error {
	return r.leased(ctx, "set data of", jobID, owner, "data = ?, updated_at = ?", string(data), now)
}

// SetResult replaces the job's result, a JSON object.
func (r Jobs) SetResult(ctx context.Context, jobID, owner string, result json.RawMessage, now time.Time) error {
	return r.leased(ctx, "set result of", jobID, owner, "result = ?, updated_at = ?", string(result), now)
}

// Retry releases the lease and queues the job to run again at runAfter,
// recording the error that caused the retry.
func (r Jobs) Retry(ctx context.Context, jobID, owner string, runAfter time.Time, errMsg string, now time.Time) error {
	return r.leased(ctx, "retry", jobID, owner,
		"state = 'queued', lease_owner = NULL, lease_expires_at = NULL, run_after = ?, error = ?, updated_at = ?",
		runAfter, errMsg, now)
}

// RetryRollback releases the lease of a rolling-back job so its rollback
// resumes at runAfter.
func (r Jobs) RetryRollback(ctx context.Context, jobID, owner string, runAfter, now time.Time) error {
	return r.leased(ctx, "retry rollback of", jobID, owner,
		"lease_owner = NULL, lease_expires_at = NULL, run_after = ?, updated_at = ?", runAfter, now)
}

// Release gives up the lease without using up an attempt, so the job can be
// leased again at once. Workers call it when shutting down. The job keeps
// its state, so it keeps its lock key.
func (r Jobs) Release(ctx context.Context, jobID, owner string, now time.Time) error {
	return r.leased(ctx, "release", jobID, owner,
		"lease_owner = NULL, lease_expires_at = NULL, attempts = attempts - 1, run_after = ?, updated_at = ?", now, now)
}

// BeginRollback moves a running job to rolling_back, recording the error
// that failed it. Attempts restarts at 1 for the lease the caller holds, so
// the rollback gets its own MaxAttempts.
func (r Jobs) BeginRollback(ctx context.Context, jobID, owner, errMsg string, now time.Time) error {
	return r.leased(ctx, "begin rollback of", jobID, owner,
		"state = 'rolling_back', attempts = 1, error = ?, updated_at = ?", errMsg, now)
}

// Finish moves the job to a final state and releases the lease.
func (r Jobs) Finish(ctx context.Context, jobID, owner string, state JobState, errMsg string, now time.Time) error {
	if !state.Final() {
		return fmt.Errorf("finish job %s: %q is not a final state", jobID, state)
	}
	return r.leased(ctx, "finish", jobID, owner,
		"state = ?, error = ?, lease_owner = NULL, lease_expires_at = NULL, finished_at = ?, updated_at = ?",
		state, errMsg, now, now)
}

// InitSteps records a job's steps as pending, so clients can see the whole
// plan. Steps that already exist are left alone.
func (r Jobs) InitSteps(ctx context.Context, jobID, owner string, names []string) error {
	if err := r.checkLease(ctx, jobID, owner); err != nil {
		return fmt.Errorf("init steps of job %s: %w", jobID, err)
	}
	for i, name := range names {
		if _, err := r.q.ExecContext(ctx,
			"INSERT INTO job_steps (id, job_id, seq, name, state) VALUES (?, ?, ?, ?, 'pending') ON CONFLICT (job_id, seq) DO NOTHING",
			id.New(), jobID, i, name); err != nil {
			return fmt.Errorf("init steps of job %s: %w", jobID, err)
		}
	}
	return nil
}

func (r Jobs) checkLease(ctx context.Context, jobID, owner string) error {
	var n int
	if err := r.q.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE id = ? AND lease_owner = ?", jobID, owner).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrLeaseLost
	}
	return nil
}

// StartStep marks a step running and counts an attempt. The step must have
// been created by InitSteps.
func (r Jobs) StartStep(ctx context.Context, jobID, owner string, seq int, now time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE job_steps SET state = 'running', error = '', attempts = attempts + 1,
			started_at = COALESCE(started_at, ?), finished_at = NULL
		WHERE job_id = ? AND seq = ? AND EXISTS (SELECT 1 FROM jobs WHERE id = ? AND lease_owner = ?)`,
		now, jobID, seq, jobID, owner)
	return stepResult(res, err, "start", jobID, seq)
}

// FinishStep records a step's outcome: succeeded, failed, rolled_back, or
// rollback_failed.
func (r Jobs) FinishStep(ctx context.Context, jobID, owner string, seq int, state StepState, errMsg string, now time.Time) error {
	res, err := r.q.ExecContext(ctx,
		`UPDATE job_steps SET state = ?, error = ?, finished_at = ?
		WHERE job_id = ? AND seq = ? AND EXISTS (SELECT 1 FROM jobs WHERE id = ? AND lease_owner = ?)`,
		state, errMsg, now, jobID, seq, jobID, owner)
	return stepResult(res, err, "finish", jobID, seq)
}

func stepResult(res sql.Result, err error, what, jobID string, seq int) error {
	if err != nil {
		return fmt.Errorf("%s step %d of job %s: %w", what, seq, jobID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s step %d of job %s: %w", what, seq, jobID, err)
	}
	if n == 0 {
		return fmt.Errorf("%s step %d of job %s: %w", what, seq, jobID, ErrLeaseLost)
	}
	return nil
}
