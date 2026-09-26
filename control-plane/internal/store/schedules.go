package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// MissedRunPolicy says what the scheduler does about runs that were due
// while no control plane was running (or while it was too busy to notice).
type MissedRunPolicy string

// Missed-run policies.
const (
	// MissedSkip drops missed runs: a run is only enqueued if the scheduler
	// notices it within the grace period, otherwise the schedule waits for
	// its next slot. For checks whose late results are worthless, such as
	// uptime probes.
	MissedSkip MissedRunPolicy = "skip"
	// MissedCoalesce enqueues one run for any number of missed slots. For
	// work where running late beats not running and once is enough: WP-Cron,
	// backups, prune.
	MissedCoalesce MissedRunPolicy = "coalesce"
	// MissedCatchUp enqueues one run per missed slot, up to the scheduler's
	// limit, for work that must happen once per period.
	MissedCatchUp MissedRunPolicy = "catch_up"
)

// Valid reports whether p is a known policy.
func (p MissedRunPolicy) Valid() bool {
	return p == MissedSkip || p == MissedCoalesce || p == MissedCatchUp
}

// Schedule enqueues a job at a fixed interval (design doc §5.3, §5.6).
//
// Runs are due at slots t where t - Unix epoch = Phase() + k * Interval.
// The phase is Offset plus a deterministic stagger derived from the name,
// so schedules with the same interval, such as every site's WP-Cron, are
// spread out instead of firing together.
type Schedule struct {
	ID string
	// Name identifies the schedule, e.g. "wp-cron:<site-id>". Upsert
	// replaces the schedule with the same name.
	Name       string
	JobType    string
	TargetType string
	TargetID   string
	// Payload is copied into every job: a JSON object without secrets.
	Payload     json.RawMessage
	LockKey     string
	MaxAttempts int
	// Interval is the time between runs, in whole seconds.
	Interval time.Duration
	// Offset shifts every slot, e.g. 2h for daily runs after 02:00 UTC.
	Offset time.Duration
	// Stagger is the window after Offset that the name-derived stagger
	// falls in. Zero means the whole interval. A window of one second pins
	// the schedule to Offset exactly.
	Stagger         time.Duration
	MissedRunPolicy MissedRunPolicy
	NextRunAt       time.Time
	LastRunAt       time.Time // zero if never run
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Phase returns the offset of the schedule's slots from the Unix epoch,
// in [0, Interval).
func (s Schedule) Phase() time.Duration {
	interval := int64(s.Interval / time.Second)
	if interval <= 0 {
		return 0
	}
	window := int64(s.Stagger / time.Second)
	if window <= 0 || window > interval {
		window = interval
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(s.Name))
	stagger := int64(h.Sum64() % uint64(window)) //nolint:gosec // G115: the modulus is at most the interval, a small positive int64
	return time.Duration((int64(s.Offset/time.Second)+stagger)%interval) * time.Second
}

// NextRunAfter returns the schedule's first slot strictly after t.
func (s Schedule) NextRunAfter(t time.Time) time.Time {
	interval := int64(s.Interval / time.Second)
	if interval <= 0 {
		return time.Time{}
	}
	phase := int64(s.Phase() / time.Second)
	k := floorDiv(t.Unix()-phase, interval) + 1
	return time.Unix(phase+k*interval, 0).UTC()
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// validate checks s and fills in defaults.
func (s *Schedule) validate() error {
	switch {
	case s.Name == "" || s.JobType == "" || s.TargetType == "" || s.TargetID == "":
		return errors.New("name, job type, target type, and target ID are required")
	case s.Interval < time.Second || s.Interval%time.Second != 0:
		return fmt.Errorf("interval must be a whole number of seconds, at least 1s, got %s", s.Interval)
	case s.Offset < 0 || s.Offset >= s.Interval || s.Offset%time.Second != 0:
		return fmt.Errorf("offset must be whole seconds in [0, interval), got %s", s.Offset)
	case s.Stagger < 0 || s.Stagger > s.Interval || s.Stagger%time.Second != 0:
		return fmt.Errorf("stagger must be whole seconds in [0, interval], got %s", s.Stagger)
	case !s.MissedRunPolicy.Valid():
		return fmt.Errorf("unknown missed-run policy %q", s.MissedRunPolicy)
	}
	if s.Stagger == 0 {
		s.Stagger = s.Interval
	}
	if s.MaxAttempts <= 0 {
		s.MaxAttempts = DefaultMaxAttempts
	}
	payload, err := jsonObject(s.Payload)
	if err != nil {
		return fmt.Errorf("payload %w", err)
	}
	s.Payload = json.RawMessage(payload)
	return nil
}

// Schedules reads and writes recurring schedules.
type Schedules struct {
	q Querier
}

// NewSchedules returns a Schedules that runs queries on q.
func NewSchedules(q Querier) Schedules {
	return Schedules{q: q}
}

const scheduleColumns = "id, name, job_type, target_type, target_id, payload, lock_key, max_attempts, interval_seconds, " +
	"offset_seconds, stagger_seconds, missed_run_policy, next_run_at, last_run_at, created_at, updated_at"

func scanSchedule(sc scanner) (Schedule, error) {
	var (
		s                         Schedule
		payload                   string
		lockKey                   sql.NullString
		interval, offset, stagger int64
		last                      sql.NullTime
	)
	err := sc.Scan(&s.ID, &s.Name, &s.JobType, &s.TargetType, &s.TargetID, &payload, &lockKey, &s.MaxAttempts,
		&interval, &offset, &stagger, &s.MissedRunPolicy, &s.NextRunAt, &last, &s.CreatedAt, &s.UpdatedAt)
	s.Payload, s.LockKey, s.LastRunAt = json.RawMessage(payload), lockKey.String, last.Time
	s.Interval, s.Offset, s.Stagger = time.Duration(interval)*time.Second, time.Duration(offset)*time.Second, time.Duration(stagger)*time.Second
	return s, err
}

// Upsert creates the schedule named s.Name or replaces its definition,
// filling in s's ID, defaults, and timestamps. The next run is the first
// slot after now, except that an existing schedule whose timing (interval,
// offset, stagger) is unchanged keeps its next run. Run it inside DB.InTx
// to change schedules atomically with other changes.
func (r Schedules) Upsert(ctx context.Context, s *Schedule, now time.Time) error {
	u := *s
	if err := u.validate(); err != nil {
		return fmt.Errorf("upsert schedule %q: %w", s.Name, err)
	}
	u.NextRunAt = u.NextRunAfter(now)
	existing, err := r.Get(ctx, u.Name)
	switch {
	case err == nil:
		u.ID, u.CreatedAt, u.LastRunAt = existing.ID, existing.CreatedAt, existing.LastRunAt
		if existing.Interval == u.Interval && existing.Offset == u.Offset && existing.Stagger == u.Stagger {
			u.NextRunAt = existing.NextRunAt
		}
	case errors.Is(err, ErrNotFound):
		u.ID, u.CreatedAt, u.LastRunAt = id.New(), utc(now), time.Time{}
	default:
		return fmt.Errorf("upsert schedule %q: %w", s.Name, err)
	}
	u.UpdatedAt = utc(now)
	_, err = r.q.ExecContext(ctx,
		`INSERT INTO schedules (`+scheduleColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET job_type = excluded.job_type, target_type = excluded.target_type,
			target_id = excluded.target_id, payload = excluded.payload, lock_key = excluded.lock_key,
			max_attempts = excluded.max_attempts, interval_seconds = excluded.interval_seconds,
			offset_seconds = excluded.offset_seconds, stagger_seconds = excluded.stagger_seconds,
			missed_run_policy = excluded.missed_run_policy, next_run_at = excluded.next_run_at,
			updated_at = excluded.updated_at`,
		u.ID, u.Name, u.JobType, u.TargetType, u.TargetID, string(u.Payload), nullString(u.LockKey), u.MaxAttempts,
		int64(u.Interval/time.Second), int64(u.Offset/time.Second), int64(u.Stagger/time.Second), u.MissedRunPolicy,
		u.NextRunAt, nullTime(u.LastRunAt), u.CreatedAt, u.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert schedule %q: %w", s.Name, err)
	}
	*s = u
	return nil
}

// Get returns the schedule with the given name, or ErrNotFound.
func (r Schedules) Get(ctx context.Context, name string) (Schedule, error) {
	s, err := scanSchedule(r.q.QueryRowContext(ctx, "SELECT "+scheduleColumns+" FROM schedules WHERE name = ?", name))
	if err != nil {
		return Schedule{}, fmt.Errorf("get schedule %q: %w", name, err)
	}
	return s, nil
}

// List returns every schedule ordered by name.
func (r Schedules) List(ctx context.Context) ([]Schedule, error) {
	return r.list(ctx, "SELECT "+scheduleColumns+" FROM schedules ORDER BY name")
}

// Due returns up to limit schedules whose next run is at or before now,
// most overdue first.
func (r Schedules) Due(ctx context.Context, now time.Time, limit int) ([]Schedule, error) {
	return r.list(ctx, "SELECT "+scheduleColumns+" FROM schedules WHERE next_run_at <= ? ORDER BY next_run_at, id LIMIT ?", now, limit)
}

func (r Schedules) list(ctx context.Context, query string, args ...any) ([]Schedule, error) {
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Schedule{}
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, fmt.Errorf("list schedules: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	return out, nil
}

// Claim advances a due schedule from its next run, dueAt, to next. It
// reports false if the schedule's next run is no longer dueAt: another
// scheduler claimed it first, or it was redefined. The conditional UPDATE
// is what makes each slot fire once across control plane processes; run it
// in the same transaction as the enqueue.
func (r Schedules) Claim(ctx context.Context, scheduleID string, dueAt, next, now time.Time) (bool, error) {
	res, err := r.q.ExecContext(ctx,
		"UPDATE schedules SET next_run_at = ?, last_run_at = ?, updated_at = ? WHERE id = ? AND next_run_at = ?",
		next, now, now, scheduleID, dueAt)
	if err != nil {
		return false, fmt.Errorf("claim schedule %s: %w", scheduleID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim schedule %s: %w", scheduleID, err)
	}
	return n == 1, nil
}

// Delete removes the schedule with the given name. It returns ErrNotFound
// if there is none.
func (r Schedules) Delete(ctx context.Context, name string) error {
	res, err := r.q.ExecContext(ctx, "DELETE FROM schedules WHERE name = ?", name)
	if err != nil {
		return fmt.Errorf("delete schedule %q: %w", name, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("delete schedule %q: %w", name, err)
	} else if n == 0 {
		return fmt.Errorf("delete schedule %q: %w", name, ErrNotFound)
	}
	return nil
}

// DeleteForTarget removes every schedule for a target, e.g. when a site is
// deleted, and returns how many there were.
func (r Schedules) DeleteForTarget(ctx context.Context, targetType, targetID string) (int, error) {
	res, err := r.q.ExecContext(ctx, "DELETE FROM schedules WHERE target_type = ? AND target_id = ?", targetType, targetID)
	if err != nil {
		return 0, fmt.Errorf("delete schedules for %s %s: %w", targetType, targetID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete schedules for %s %s: %w", targetType, targetID, err)
	}
	return int(n), nil
}
