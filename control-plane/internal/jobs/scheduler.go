package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Scheduler defaults.
const (
	DefaultSchedulerPollInterval = time.Second
	DefaultMisfireGrace          = time.Minute
	DefaultMaxCatchUp            = 100
)

// SchedulerOptions configures a Scheduler. Zero values select the defaults.
type SchedulerOptions struct {
	// PollInterval is how often the scheduler looks for due schedules.
	PollInterval time.Duration
	// MisfireGrace is how late a run may be noticed and still count as on
	// time, for store.MissedSkip. It is capped at the schedule's interval.
	MisfireGrace time.Duration
	// MaxCatchUp caps the runs store.MissedCatchUp enqueues at once; older
	// missed runs are dropped.
	MaxCatchUp int
	// Clock defaults to the system clock.
	Clock Clock
	// Logger defaults to discarding output.
	Logger *slog.Logger
}

// Scheduler enqueues jobs from recurring schedules.
type Scheduler struct {
	db   *store.DB
	opts SchedulerOptions
	log  *slog.Logger
}

// NewScheduler returns a scheduler for the schedules in db.
func NewScheduler(db *store.DB, opts SchedulerOptions) *Scheduler {
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultSchedulerPollInterval
	}
	if opts.MisfireGrace <= 0 {
		opts.MisfireGrace = DefaultMisfireGrace
	}
	if opts.MaxCatchUp <= 0 {
		opts.MaxCatchUp = DefaultMaxCatchUp
	}
	if opts.Clock == nil {
		opts.Clock = systemClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Scheduler{db: db, opts: opts, log: opts.Logger}
}

// Run enqueues due runs until ctx is canceled.
func (s *Scheduler) Run(ctx context.Context) error {
	s.log.Info("scheduler started")
	for {
		if _, err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("scheduling due jobs failed", "error", err)
		}
		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopped")
			return nil
		case <-time.After(s.opts.PollInterval):
		}
	}
}

// dueBatch is how many due schedules RunOnce reads per query.
const dueBatch = 100

// RunOnce claims every due schedule and enqueues its runs, returning how
// many jobs it enqueued. Several schedulers, in one process or several, can
// run it concurrently: each slot is claimed by exactly one of them.
func (s *Scheduler) RunOnce(ctx context.Context) (int, error) {
	now := s.opts.Clock.Now()
	total := 0
	for {
		due, err := store.NewSchedules(s.db).Due(ctx, now, dueBatch)
		if err != nil {
			return total, err
		}
		for _, sch := range due {
			n, err := s.fire(ctx, sch, now)
			if err != nil {
				return total, err
			}
			total += n
		}
		// Claimed schedules move past now, so the next query returns the
		// rest. A schedule another scheduler claimed first moves too.
		if len(due) < dueBatch {
			return total, nil
		}
	}
}

// fire claims a due schedule's slot and enqueues its runs in one
// transaction.
func (s *Scheduler) fire(ctx context.Context, sch store.Schedule, now time.Time) (int, error) {
	slots := s.slots(sch, now)
	enqueued := 0
	err := s.db.InTx(ctx, func(q store.Querier) error {
		enqueued = 0
		claimed, err := store.NewSchedules(q).Claim(ctx, sch.ID, sch.NextRunAt, sch.NextRunAfter(now), now)
		if err != nil || !claimed {
			return err
		}
		jobs := store.NewJobs(q)
		if len(slots) == 0 {
			s.log.Info("skipped missed scheduled run", "schedule", sch.Name, "due_at", sch.NextRunAt)
			return nil
		}
		if sch.MissedRunPolicy != store.MissedCatchUp {
			// Don't pile up runs behind one that hasn't started, e.g. WP-Cron
			// for a site whose earlier job waits on its lock key.
			pending, err := jobs.HasQueuedForSchedule(ctx, sch.ID)
			if err != nil {
				return err
			}
			if pending {
				s.log.Info("skipped scheduled run; the previous run hasn't started", "schedule", sch.Name)
				return nil
			}
		}
		for _, slot := range slots {
			j := store.Job{
				Type:         sch.JobType,
				TargetType:   sch.TargetType,
				TargetID:     sch.TargetID,
				Payload:      sch.Payload,
				LockKey:      sch.LockKey,
				MaxAttempts:  sch.MaxAttempts,
				ScheduleID:   sch.ID,
				ScheduledFor: slot,
			}
			if err := jobs.Enqueue(ctx, &j); err != nil {
				return err
			}
			enqueued++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if enqueued > 0 {
		s.log.Debug("enqueued scheduled jobs", "schedule", sch.Name, "count", enqueued)
	}
	return enqueued, nil
}

// slots returns the slots to enqueue for a due schedule under its
// missed-run policy.
func (s *Scheduler) slots(sch store.Schedule, now time.Time) []time.Time {
	if sch.Interval <= 0 || sch.NextRunAt.After(now) {
		return nil
	}
	missed := int(now.Sub(sch.NextRunAt) / sch.Interval)
	last := sch.NextRunAt.Add(time.Duration(missed) * sch.Interval)
	switch sch.MissedRunPolicy {
	case store.MissedSkip:
		if now.Sub(last) > min(s.opts.MisfireGrace, sch.Interval) {
			return nil
		}
		return []time.Time{last}
	case store.MissedCatchUp:
		n := min(missed+1, s.opts.MaxCatchUp)
		out := make([]time.Time, n)
		for i := range n {
			out[i] = last.Add(-time.Duration(n-1-i) * sch.Interval)
		}
		return out
	default: // coalesce
		return []time.Time{last}
	}
}
