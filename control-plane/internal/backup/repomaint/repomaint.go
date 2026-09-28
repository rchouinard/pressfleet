// Package repomaint keeps client group backup repositories healthy (#54,
// design doc §5.3 step 4): it applies the retention policy and checks each
// repository's integrity, as jobs on per-repository schedules. The work
// itself is backup.Engine's Prune and Check (package backup/restic).
//
// # Jobs
//
// PruneJobType applies the retention policy to one group's repository.
// Its steps are:
//
//  1. prune: backup.Engine.Prune, which removes the snapshots the policy
//     doesn't keep (each site's counted on their own) and the data only
//     they used. Running it again converges on the same state.
//  2. expire_records: marks the backup records (store.BackupRecord) whose
//     snapshots are gone from the repository as expired. It compares the
//     records with a listing of the repository instead of with what the
//     prune step removed, so it is idempotent and also catches snapshots
//     removed by hand. Only backups that finished before the listing began
//     are considered, so a backup finishing meanwhile isn't mistaken for a
//     removed one.
//
// CheckJobType runs backup.Engine.Check, which reads back part of the
// repository's data (config backup.check.read_data_percent). A damaged
// repository fails the job for good (backup.ErrRepositoryDamaged); other
// errors are retried.
//
// A job for a group whose repository was deleted removes the group's
// schedules and succeeds without doing anything.
//
// # Serialization
//
// Prune and check take restic's exclusive repository lock. Their jobs share
// the lock key repo:<client group ID> (LockKey), so they never run at once
// on one repository; jobs for different repositories run in parallel.
// Backups and restores hold their site's lock key instead, and meet prune
// and check only at restic's lock: they take a non-exclusive one and wait
// (--retry-lock) for an exclusive one to be released, and prune and check
// wait for theirs, so they never overlap on a repository. Both remove
// stale locks first (restic unlock), left by restic processes that died.
//
// # Schedules
//
// Each repository has a prune schedule (PruneScheduleName), daily, and a
// check schedule (CheckScheduleName), every backup.check.interval_days.
// Both start after the platform's backup window closes, spread over
// MaxSpread so repositories don't all start at once; checks are also
// spread over the days of their interval. Sites that chose their own
// backup hour may still be backed up meanwhile; they wait at restic's
// lock. The schedules are created in the transaction that records a new
// repository ([OnRepositoryCreated], repository.Options.Created), removed
// in the one that deletes it ([OnRepositoryDeleted]), and brought in line with the configuration when the control plane
// starts ([ReconcileSchedules]).
package repomaint

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Job types.
const (
	PruneJobType = "backup.prune"
	CheckJobType = "backup.check"
)

// Step names. Keep them stable: a queued job resumes by step name.
const (
	stepPrune  = "prune"
	stepExpire = "expire_records"
	stepCheck  = "check"
)

// targetType is the jobs' target type; the target is the client group.
const targetType = "client_group"

// dataSkipped is the job data key that says the group has no repository,
// so the later steps do nothing.
const dataSkipped = "skipped"

// LockKey is the lock key of a repository's maintenance jobs.
func LockKey(clientGroupID string) string { return "repo:" + clientGroupID }

// PruneScheduleName names a repository's prune schedule.
func PruneScheduleName(clientGroupID string) string { return "prune:" + clientGroupID }

// CheckScheduleName names a repository's check schedule.
func CheckScheduleName(clientGroupID string) string { return "check:" + clientGroupID }

// DefaultCheckInterval is the time between checks unless Timing says
// otherwise.
const DefaultCheckInterval = 7 * 24 * time.Hour

// MaxSpread is how long after the backup window closes a repository's
// maintenance may start.
const MaxSpread = 2 * time.Hour

// Timing is when maintenance runs.
type Timing struct {
	// Window is the platform's backup window; maintenance starts after it
	// closes. Zero means sitebackup.DefaultWindow.
	Window sitebackup.Window
	// CheckInterval is the time between checks of a repository, in whole
	// days; zero means DefaultCheckInterval.
	CheckInterval time.Duration
}

func (t Timing) orDefault() Timing {
	if t.Window.Length <= 0 {
		t.Window = sitebackup.DefaultWindow()
	}
	if t.CheckInterval <= 0 {
		t.CheckInterval = DefaultCheckInterval
	}
	return t
}

// Options configure a Service.
type Options struct {
	// DB is the state database. Required.
	DB *store.DB
	// Engine prunes and checks repositories. Required.
	Engine backup.Engine
	// Timing is when maintenance runs.
	Timing Timing
	// Logger defaults to discarding output.
	Logger *slog.Logger
}

// Service runs repository maintenance jobs and keeps their schedules.
type Service struct {
	db     *store.DB
	engine backup.Engine
	timing Timing
	log    *slog.Logger
	now    func() time.Time
}

// New returns a Service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.Engine == nil {
		return nil, errors.New("repomaint: DB and Engine are required")
	}
	if opts.Timing.CheckInterval%(24*time.Hour) != 0 {
		return nil, fmt.Errorf("repomaint: check interval %s is not a whole number of days", opts.Timing.CheckInterval)
	}
	s := &Service{db: opts.DB, engine: opts.Engine, timing: opts.Timing.orDefault(), log: opts.Logger, now: time.Now}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// Handlers returns the job handlers, to register on the job pool.
func (s *Service) Handlers() []jobs.Handler {
	return []jobs.Handler{
		{Type: PruneJobType, Steps: []jobs.Step{
			{Name: stepPrune, Do: s.prune},
			{Name: stepExpire, Do: s.expire},
		}},
		{Type: CheckJobType, Steps: []jobs.Step{
			{Name: stepCheck, Do: s.check},
		}},
	}
}

// Enqueue queues a prune or check (jobType) of the group's repository now,
// e.g. from the CLI. q may be a transaction.
func Enqueue(ctx context.Context, q store.Querier, jobType, clientGroupID, createdByUserID string) (store.Job, error) {
	if jobType != PruneJobType && jobType != CheckJobType {
		return store.Job{}, fmt.Errorf("queue repository maintenance: unknown job type %q", jobType)
	}
	job, err := jobs.Enqueue(ctx, q, jobs.Spec{
		Type: jobType, TargetType: targetType, TargetID: clientGroupID,
		LockKey: LockKey(clientGroupID), CreatedByUserID: createdByUserID,
	})
	if err != nil {
		return store.Job{}, fmt.Errorf("queue %s of client group %s: %w", jobType, clientGroupID, err)
	}
	return job, nil
}

func (s *Service) prune(ctx context.Context, r *jobs.Run) error {
	groupID := r.Job().TargetID
	res, err := s.engine.Prune(ctx, groupID)
	if errors.Is(err, backup.ErrNoRepository) {
		return s.skip(ctx, r)
	}
	if err != nil {
		return err
	}
	r.Logf("Applied the retention policy: removed %d snapshots and the data only they used.", len(res.Forgotten))
	return r.SetResult("forgotten_snapshots", len(res.Forgotten))
}

func (s *Service) expire(ctx context.Context, r *jobs.Run) error {
	if skip, err := skipped(r); err != nil || skip {
		return err
	}
	groupID := r.Job().TargetID
	// Taken before listing: a backup that finished earlier has its
	// snapshot in the listing unless the snapshot was removed.
	listedAt := s.now()
	snaps, err := s.engine.GroupSnapshots(ctx, groupID)
	if err != nil {
		return err
	}
	present := make(map[backup.SnapshotID]bool, len(snaps))
	for _, snap := range snaps {
		present[snap.ID] = true
	}
	records := store.NewBackupRecords(s.db)
	candidates, err := records.Restorable(ctx, groupID, listedAt)
	if err != nil {
		return err
	}
	var gone []string
	for _, rec := range candidates {
		if !present[backup.SnapshotID(rec.SnapshotID)] {
			gone = append(gone, rec.ID)
		}
	}
	n, err := records.Expire(ctx, gone)
	if err != nil {
		return err
	}
	if n > 0 {
		r.Logf("Marked %d backups whose snapshots were removed as expired.", n)
	}
	s.log.InfoContext(ctx, "expired backups recorded", "client_group_id", groupID, "snapshots", len(snaps), "expired_backups", n)
	return r.SetResult("expired_backups", n)
}

func (s *Service) check(ctx context.Context, r *jobs.Run) error {
	groupID := r.Job().TargetID
	err := s.engine.Check(ctx, groupID)
	switch {
	case errors.Is(err, backup.ErrNoRepository):
		return s.skip(ctx, r)
	case errors.Is(err, backup.ErrRepositoryDamaged):
		// Checking again won't repair it; an operator must.
		s.log.ErrorContext(ctx, "backup repository check found damage", "client_group_id", groupID, "error", err)
		return jobs.Permanent(err)
	case err != nil:
		return err
	}
	r.Logf("The repository checked out clean.")
	return nil
}

// skip ends a job for a group without a repository, removing the group's
// schedules, which should have gone with the repository.
func (s *Service) skip(ctx context.Context, r *jobs.Run) error {
	groupID := r.Job().TargetID
	if err := s.db.InTx(ctx, func(q store.Querier) error { return RemoveSchedules(ctx, q, groupID) }); err != nil {
		return err
	}
	if err := r.Set(dataSkipped, true); err != nil {
		return err
	}
	r.Logf("Skipped: the client group has no backup repository.")
	return nil
}

func skipped(r *jobs.Run) (bool, error) {
	var skip bool
	_, err := r.Get(dataSkipped, &skip)
	return skip, err
}

// OnRepositoryCreated returns the repository.Options.Created hook that
// schedules a new repository's maintenance with t, in the transaction that
// records the repository.
func OnRepositoryCreated(t Timing) func(ctx context.Context, q store.Querier, clientGroupID string) error {
	return func(ctx context.Context, q store.Querier, clientGroupID string) error {
		return EnsureSchedules(ctx, q, clientGroupID, t)
	}
}

// OnRepositoryDeleted is the repository.Options.Deleted hook: it removes
// the repository's maintenance schedules in the transaction that deletes
// it.
func OnRepositoryDeleted(ctx context.Context, q store.Querier, clientGroupID string) error {
	return RemoveSchedules(ctx, q, clientGroupID)
}

// EnsureSchedules creates or updates the group's prune and check
// schedules. It is idempotent, and keeps the next run of a schedule whose
// timing hasn't changed.
func EnsureSchedules(ctx context.Context, q store.Querier, clientGroupID string, t Timing) error {
	for _, spec := range scheduleSpecs(clientGroupID, t.orDefault()) {
		if _, err := jobs.UpsertSchedule(ctx, q, spec); err != nil {
			return fmt.Errorf("schedule maintenance of the repository of client group %s: %w", clientGroupID, err)
		}
	}
	return nil
}

// RemoveSchedules deletes the group's prune and check schedules. A group
// without them is not an error.
func RemoveSchedules(ctx context.Context, q store.Querier, clientGroupID string) error {
	for _, name := range []string{PruneScheduleName(clientGroupID), CheckScheduleName(clientGroupID)} {
		if err := jobs.DeleteSchedule(ctx, q, name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("remove maintenance schedules of client group %s: %w", clientGroupID, err)
		}
	}
	return nil
}

// scheduleSpecs returns the group's prune and check schedules.
func scheduleSpecs(groupID string, t Timing) []jobs.ScheduleSpec {
	const day = 24 * time.Hour
	windowEnd := (t.Window.Start + t.Window.Length) % day
	// Stay out of the next window, if the day leaves room.
	spread := MaxSpread
	if gap := day - t.Window.Length; gap > 0 && gap < spread {
		spread = gap
	}
	// Each repository is checked on a day of the interval derived from its
	// ID, at the same time as its prune.
	h := fnv.New32a()
	_, _ = h.Write([]byte(groupID))
	checkDay := time.Duration(h.Sum32()%uint32(t.CheckInterval/day)) * day //nolint:gosec // G115: the interval is at most a year of days
	return []jobs.ScheduleSpec{
		{
			Name: PruneScheduleName(groupID), JobType: PruneJobType, TargetType: targetType, TargetID: groupID,
			LockKey: LockKey(groupID), Interval: day, Offset: windowEnd, Stagger: spread,
			MissedRunPolicy: store.MissedCoalesce,
		},
		{
			Name: CheckScheduleName(groupID), JobType: CheckJobType, TargetType: targetType, TargetID: groupID,
			LockKey: LockKey(groupID), Interval: t.CheckInterval, Offset: checkDay + windowEnd, Stagger: spread,
			MissedRunPolicy: store.MissedCoalesce,
		},
	}
}

// ReconcileSchedules brings every repository's maintenance schedules in
// line with t, so repositories created before maintenance existed get them
// and a changed window or check interval moves them, and removes the
// schedules of groups that no longer have a repository. The control plane
// runs it at startup.
func ReconcileSchedules(ctx context.Context, db *store.DB, t Timing) error {
	ids, err := repository.ClientGroupIDs(ctx, db)
	if err != nil {
		return fmt.Errorf("reconcile repository maintenance schedules: %w", err)
	}
	has := make(map[string]bool, len(ids))
	for _, groupID := range ids {
		has[groupID] = true
		if err := EnsureSchedules(ctx, db, groupID, t); err != nil {
			return fmt.Errorf("reconcile repository maintenance schedules: %w", err)
		}
	}
	all, err := store.NewSchedules(db).List(ctx)
	if err != nil {
		return fmt.Errorf("reconcile repository maintenance schedules: %w", err)
	}
	for _, sch := range all {
		if (sch.JobType == PruneJobType || sch.JobType == CheckJobType) && !has[sch.TargetID] {
			if err := RemoveSchedules(ctx, db, sch.TargetID); err != nil {
				return fmt.Errorf("reconcile repository maintenance schedules: %w", err)
			}
		}
	}
	return nil
}
