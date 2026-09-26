// Package audit maintains the audit log (design doc §4.6): the retention
// cleanup job and its schedule. Events are written by the API at its
// authorization check point (package api) into store.AuditEvents.
package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Names of the cleanup job type and its schedule.
const (
	CleanupJobType      = "audit.cleanup"
	CleanupScheduleName = "audit-retention"
)

// The cleanup job targets the platform; job targets need a type and an ID.
const (
	cleanupTargetType = "platform"
	cleanupTargetID   = "audit_events"
)

// cleanupInterval is how often expired events are deleted. Retention is
// configured in days, so daily is precise enough.
const cleanupInterval = 24 * time.Hour

// CleanupHandler returns the job handler that deletes audit events older
// than retention. now is the clock (time.Now if nil). The retention is
// read from the configuration when the control plane starts, not stored in
// the schedule, so changing it takes effect on the next run after a
// restart. Deleting is idempotent, so the step can safely run again.
func CleanupHandler(db *store.DB, retention time.Duration, now func() time.Time) jobs.Handler {
	if now == nil {
		now = time.Now
	}
	return jobs.Handler{Type: CleanupJobType, Steps: []jobs.Step{{
		Name: "delete_expired_events",
		Do: func(ctx context.Context, r *jobs.Run) error {
			cutoff := now().Add(-retention)
			n, err := store.NewAuditEvents(db).DeleteBefore(ctx, cutoff)
			if err != nil {
				return err
			}
			r.Logf("deleted %d audit events from before %s", n, cutoff.UTC().Format(time.RFC3339))
			return nil
		},
	}}}
}

// EnsureCleanupSchedule creates or updates the daily cleanup schedule. It
// is safe to call on every start: an unchanged schedule keeps its next run.
func EnsureCleanupSchedule(ctx context.Context, q store.Querier) error {
	_, err := jobs.UpsertSchedule(ctx, q, jobs.ScheduleSpec{
		Name:            CleanupScheduleName,
		JobType:         CleanupJobType,
		TargetType:      cleanupTargetType,
		TargetID:        cleanupTargetID,
		LockKey:         CleanupJobType,
		Interval:        cleanupInterval,
		MissedRunPolicy: store.MissedCoalesce,
	})
	if err != nil {
		return fmt.Errorf("audit cleanup schedule: %w", err)
	}
	return nil
}
