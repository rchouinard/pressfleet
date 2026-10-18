package sitebackup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Scheduled backups (#53).
//
// Every site has a backup schedule (store.SiteBackupSchedule), daily by
// default, that becomes a jobs schedule named ScheduleName(site ID). The
// schedule queues JobType jobs with kind store.BackupScheduled. It exists
// while the site can be backed up: it is created with the site
// (EnsureSchedule in the creation transaction), removed when provisioning
// fails or the site is deleted, and brought in line with every site's
// settings and the platform's Window when the control plane starts
// (ReconcileSchedules).
//
// Backups without a chosen hour start at a time within the platform's
// Window derived from the schedule's name, so each site is backed up at the
// same time every day and sites are spread over the window. Backups with a
// chosen hour are spread over that hour the same way. Missed runs (the
// control plane was down) are coalesced into one backup, and a run isn't
// queued while the previous one still waits for the site's lock key.

// Window is the platform's backup window (config backup.schedule).
type Window struct {
	// Start is when the window opens, as the time since midnight UTC.
	Start time.Duration
	// Length is how long it stays open; zero means DefaultWindow's.
	Length time.Duration
}

// DefaultWindow is the backup window unless the config says otherwise:
// 02:00 to 06:00 UTC.
func DefaultWindow() Window { return Window{Start: 2 * time.Hour, Length: 4 * time.Hour} }

func (w Window) orDefault() Window {
	if w.Length <= 0 {
		return DefaultWindow()
	}
	return w
}

// Limits of store.SiteBackupSchedule.IntervalHours.
const (
	MinIntervalHours = 1
	MaxIntervalHours = 7 * 24
)

// ErrInvalidSchedule means a backup schedule was rejected; the error is a
// *ScheduleError that says which field is wrong.
var ErrInvalidSchedule = errors.New("invalid backup schedule")

// ScheduleError is a rejected field of a backup schedule.
type ScheduleError struct {
	// Field is a JSON pointer into the API's BackupScheduleUpdate.
	Field  string
	Detail string
}

func (e *ScheduleError) Error() string {
	return fmt.Sprintf("%s: %s %s", ErrInvalidSchedule, e.Field, e.Detail)
}

// Unwrap returns ErrInvalidSchedule.
func (e *ScheduleError) Unwrap() error { return ErrInvalidSchedule }

// ValidateSchedule checks a site's backup schedule. Intervals divide a day
// or are whole days, so that backups start at the same times every day
// (or on the same weekday, for weekly ones).
func ValidateSchedule(s store.SiteBackupSchedule) error {
	n := s.IntervalHours
	if n < MinIntervalHours || n > MaxIntervalHours || (24%n != 0 && n%24 != 0) {
		return &ScheduleError{"/interval_hours", "must divide 24 (1, 2, 3, 4, 6, 8, or 12) or be a multiple of 24 up to 168"}
	}
	if s.Hour != nil && (*s.Hour < 0 || *s.Hour > 23) {
		return &ScheduleError{"/hour", "must be a UTC hour from 0 to 23"}
	}
	return nil
}

// ScheduleName returns the name of a site's backup schedule.
func ScheduleName(siteID string) string { return "backup:" + siteID }

// scheduleSpec returns the jobs schedule for a site's backup schedule.
func scheduleSpec(siteID string, s store.SiteBackupSchedule, w Window) jobs.ScheduleSpec {
	w = w.orDefault()
	interval := time.Duration(s.IntervalHours) * time.Hour
	start, spread := w.Start, w.Length
	if s.Hour != nil {
		start, spread = time.Duration(*s.Hour)*time.Hour, time.Hour
	}
	return jobs.ScheduleSpec{
		Name:       ScheduleName(siteID),
		JobType:    JobType,
		TargetType: store.JobTargetSite,
		TargetID:   siteID,
		Payload:    JobPayload{Kind: store.BackupScheduled},
		LockKey:    store.SiteLockKey(siteID),
		Interval:   interval,
		// Slots are counted from the Unix epoch, a midnight UTC, so an
		// offset of start modulo the interval puts one at start every day.
		Offset:          start % interval,
		Stagger:         min(spread, interval),
		MissedRunPolicy: store.MissedCoalesce,
	}
}

// EnsureSchedule brings the site's backup schedule in line with its
// settings and the window: it creates or updates the schedule if the site
// can be backed up and has scheduled backups enabled, and removes it
// otherwise (no site, being deleted, provisioning failed, or disabled). It
// is idempotent, and keeps the next run of a schedule whose timing hasn't
// changed. Call it with the transaction that creates or changes the site.
func EnsureSchedule(ctx context.Context, q store.Querier, siteID string, w Window) error {
	if siteID == "" {
		return errors.New("ensure backup schedule: site ID is required")
	}
	sites := store.NewSites(q)
	site, err := sites.Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return RemoveSchedule(ctx, q, siteID)
	}
	if err != nil {
		return fmt.Errorf("ensure backup schedule for site %s: %w", siteID, err)
	}
	settings, err := sites.BackupSchedule(ctx, siteID)
	if err != nil {
		return fmt.Errorf("ensure backup schedule for site %s: %w", siteID, err)
	}
	// A never-provisioned site (its secrets are gone) has nothing to back
	// up, and a deleted one would only get skipped runs.
	if !settings.Enabled || site.Status == store.SiteDeleting || site.WPSaltsSecretRef == "" {
		return RemoveSchedule(ctx, q, siteID)
	}
	if err := ValidateSchedule(settings); err != nil {
		return fmt.Errorf("ensure backup schedule for site %s: %w", siteID, err)
	}
	if _, err := jobs.UpsertSchedule(ctx, q, scheduleSpec(siteID, settings, w)); err != nil {
		return fmt.Errorf("ensure backup schedule for site %s: %w", siteID, err)
	}
	return nil
}

// RemoveSchedule deletes the site's backup schedule. It is idempotent: a
// site without one is not an error. Jobs the schedule already queued stay
// queued and skip themselves if the site can't be backed up.
func RemoveSchedule(ctx context.Context, q store.Querier, siteID string) error {
	err := jobs.DeleteSchedule(ctx, q, ScheduleName(siteID))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("remove backup schedule for site %s: %w", siteID, err)
	}
	return nil
}

// ReconcileSchedules runs EnsureSchedule for every site, so that sites
// created before scheduled backups existed get a schedule and a changed
// Window moves every schedule, and removes the backup schedules of sites
// that no longer exist. The control plane runs it at startup. Each site is
// its own transaction; the first error stops it.
func ReconcileSchedules(ctx context.Context, db *store.DB, w Window) error {
	ids, err := store.NewSites(db).ListIDs(ctx)
	if err != nil {
		return fmt.Errorf("reconcile backup schedules: %w", err)
	}
	exists := make(map[string]bool, len(ids))
	for _, siteID := range ids {
		exists[siteID] = true
		err := db.InTx(ctx, func(q store.Querier) error { return EnsureSchedule(ctx, q, siteID, w) })
		if err != nil {
			return fmt.Errorf("reconcile backup schedules: %w", err)
		}
	}
	all, err := store.NewSchedules(db).List(ctx)
	if err != nil {
		return fmt.Errorf("reconcile backup schedules: %w", err)
	}
	for _, sch := range all {
		if sch.JobType == JobType && sch.TargetType == store.JobTargetSite && !exists[sch.TargetID] {
			if err := EnsureSchedule(ctx, db, sch.TargetID, w); err != nil {
				return fmt.Errorf("reconcile backup schedules: %w", err)
			}
		}
	}
	return nil
}

// ScheduleState is a site's backup schedule and when it next runs.
type ScheduleState struct {
	store.SiteBackupSchedule
	// NextRunAt is when the next backup is due; zero if the site has no
	// schedule (disabled, or the site can't be backed up).
	NextRunAt time.Time
}

// Schedule returns the site's backup schedule, or ErrSiteNotFound.
func (s *Service) Schedule(ctx context.Context, siteID string) (ScheduleState, error) {
	return scheduleState(ctx, s.db, siteID)
}

// SetSchedule saves the site's backup schedule and updates its jobs
// schedule to match. It returns ErrSiteNotFound if the site doesn't exist
// and a *ScheduleError if the schedule is invalid.
func (s *Service) SetSchedule(ctx context.Context, siteID string, settings store.SiteBackupSchedule) (ScheduleState, error) {
	if err := ValidateSchedule(settings); err != nil {
		return ScheduleState{}, err
	}
	var out ScheduleState
	err := s.db.InTx(ctx, func(q store.Querier) error {
		ok, err := store.NewSites(q).SetBackupSchedule(ctx, siteID, settings)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("site %s: %w", siteID, ErrSiteNotFound)
		}
		if err := EnsureSchedule(ctx, q, siteID, s.window); err != nil {
			return err
		}
		out, err = scheduleState(ctx, q, siteID)
		return err
	})
	if err != nil {
		return ScheduleState{}, err
	}
	s.log.InfoContext(ctx, "site backup schedule changed", "site_id", siteID, "enabled", settings.Enabled,
		"interval_hours", settings.IntervalHours, "next_run_at", out.NextRunAt)
	return out, nil
}

func scheduleState(ctx context.Context, q store.Querier, siteID string) (ScheduleState, error) {
	settings, err := store.NewSites(q).BackupSchedule(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return ScheduleState{}, fmt.Errorf("site %s: %w", siteID, ErrSiteNotFound)
	}
	if err != nil {
		return ScheduleState{}, err
	}
	out := ScheduleState{SiteBackupSchedule: settings}
	sch, err := store.NewSchedules(q).Get(ctx, ScheduleName(siteID))
	switch {
	case err == nil:
		out.NextRunAt = sch.NextRunAt
	case !errors.Is(err, store.ErrNotFound):
		return ScheduleState{}, err
	}
	return out, nil
}
