package store

import (
	"context"
	"fmt"
)

// JobStats is a snapshot of the job queue, for metrics.
type JobStats struct {
	// Queued counts jobs waiting to run (including retries waiting for
	// their RunAfter); Running counts jobs being worked on, rolling back
	// ones included.
	Queued, Running int
	// Succeeded and Failed count the jobs that reached a final state. A job
	// whose rollback failed counts as failed. Jobs are never deleted, so
	// these only grow.
	Succeeded, Failed int
}

// Stats counts jobs by state.
func (r Jobs) Stats(ctx context.Context) (JobStats, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT state, COUNT(*) FROM jobs GROUP BY state")
	if err != nil {
		return JobStats{}, fmt.Errorf("count jobs: %w", err)
	}
	defer rows.Close()
	var s JobStats
	for rows.Next() {
		var (
			state JobState
			n     int
		)
		if err := rows.Scan(&state, &n); err != nil {
			return JobStats{}, fmt.Errorf("count jobs: %w", err)
		}
		switch state {
		case JobQueued:
			s.Queued += n
		case JobRunning, JobRollingBack:
			s.Running += n
		case JobSucceeded:
			s.Succeeded += n
		case JobFailed, JobRollbackFailed:
			s.Failed += n
		}
	}
	if err := rows.Err(); err != nil {
		return JobStats{}, fmt.Errorf("count jobs: %w", err)
	}
	return s, nil
}
