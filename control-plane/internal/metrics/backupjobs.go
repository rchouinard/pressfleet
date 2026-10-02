package metrics

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// BackupJobsCollector reports backup health per site and the job queue,
// both read from the state database (design doc §7.1, §7.2). It scrapes no
// container, so it needs no provisioner.
//
// Per site, from the backup records:
//
//	pressfleet_site_backup_last_run_success                  gauge, 1 if the latest finished backup succeeded, 0 if it failed
//	pressfleet_site_backup_last_success_timestamp_seconds    gauge, Unix time the latest successful backup finished
//	pressfleet_site_backup_size_bytes                        gauge, size of the files in that backup
//	pressfleet_site_backup_duration_seconds                  gauge, how long that backup took
//
// The first is absent for a site with no finished backup, the others for
// one with no successful backup, and all four for a site whose backup
// schedule is disabled (the alert rules rely on that: a site that opted out
// of backups never alerts). Every kind of backup counts, pre-restore and
// pre-update snapshots included, because a failed one is as much a sign of
// a broken repository as a failed scheduled one.
//
// Platform-wide, from the jobs table:
//
//	pressfleet_jobs_queue_depth{state}      gauge, "queued" (waiting) or "running" (including rolling back)
//	pressfleet_jobs_finished_total{result}  counter, "succeeded" or "failed" (rollback failures included)
//
// The counter is the number of jobs in a final state, which is cumulative
// because jobs are never deleted, so it survives restarts, unlike an
// in-memory count.
type BackupJobsCollector struct {
	// DB is the state database.
	DB store.Querier
	// Every is the collection interval; zero means the service default.
	Every time.Duration
}

var _ Collector = BackupJobsCollector{}

// Name implements [Collector].
func (BackupJobsCollector) Name() string { return "backup_jobs" }

// Interval implements [Collector].
func (c BackupJobsCollector) Interval() time.Duration { return c.Every }

// Collect implements [Collector].
func (c BackupJobsCollector) Collect(ctx context.Context, targets []Target) ([]Sample, error) {
	stats, err := store.NewJobs(c.DB).Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("job stats: %w", err)
	}
	out := []Sample{
		{Name: "pressfleet_jobs_queue_depth", Labels: map[string]string{"state": "queued"}, Value: float64(stats.Queued)},
		{Name: "pressfleet_jobs_queue_depth", Labels: map[string]string{"state": "running"}, Value: float64(stats.Running)},
		{Name: "pressfleet_jobs_finished_total", Labels: map[string]string{"result": "succeeded"}, Value: float64(stats.Succeeded)},
		{Name: "pressfleet_jobs_finished_total", Labels: map[string]string{"result": "failed"}, Value: float64(stats.Failed)},
	}

	records := store.NewBackupRecords(c.DB)
	latest, err := records.LatestFinished(ctx)
	if err != nil {
		return out, fmt.Errorf("latest backups: %w", err)
	}
	succeeded, err := records.LatestSucceeded(ctx)
	if err != nil {
		return out, fmt.Errorf("latest successful backups: %w", err)
	}
	sites := store.NewSites(c.DB)
	var errs []error
	for _, t := range targets {
		last, hasLast := latest[t.SiteID]
		ok, hasOK := succeeded[t.SiteID]
		if !hasLast && !hasOK {
			continue
		}
		sched, err := sites.BackupSchedule(ctx, t.SiteID)
		if errors.Is(err, store.ErrNotFound) {
			continue // deleted since the target list was made
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !sched.Enabled {
			continue
		}
		if hasLast {
			v := 0.0
			if last.Status == store.BackupSucceeded {
				v = 1
			}
			out = append(out, t.Sample("pressfleet_site_backup_last_run_success", v))
		}
		if hasOK {
			out = append(out,
				t.Sample("pressfleet_site_backup_last_success_timestamp_seconds", float64(ok.FinishedAt.UnixMilli())/1000),
				t.Sample("pressfleet_site_backup_size_bytes", float64(ok.SizeBytes)),
				t.Sample("pressfleet_site_backup_duration_seconds", max(ok.FinishedAt.Sub(ok.StartedAt), 0).Seconds()),
			)
		}
	}
	return out, errors.Join(errs...)
}
