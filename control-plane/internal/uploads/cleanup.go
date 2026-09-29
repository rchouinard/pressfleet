package uploads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Names of the cleanup job type and its schedule.
const (
	CleanupJobType      = "upload.cleanup"
	CleanupScheduleName = "upload-cleanup"
)

// CleanupInterval is how often the cleanup job runs: often enough that a
// multi-gigabyte upload doesn't hold its disk space long after its job.
const CleanupInterval = 10 * time.Minute

// The cleanup job targets the platform; job targets need a type and an ID.
const (
	cleanupTargetType = "platform"
	cleanupTargetID   = "uploads"
)

// CleanupResult counts what a cleanup removed.
type CleanupResult struct {
	// Expired uploads were abandoned: no chunk and no job for longer than
	// the expiry.
	Expired int
	// Finished uploads belonged to jobs that ended.
	Finished int
	// Orphaned files had no record.
	Orphaned int
}

// Cleanup removes expired uploads, uploads whose job has ended, and files
// without a record. Uploads that a request is writing to are left for the
// next run. Running it again removes nothing more.
func (s *Service) Cleanup(ctx context.Context) (CleanupResult, error) {
	var res CleanupResult
	uploads := store.NewUploads(s.db)
	at := s.now()
	expired, err := uploads.Expired(ctx, at)
	if err != nil {
		return res, err
	}
	var errs []error
	for _, u := range expired {
		if !s.lock(u.ID) {
			continue
		}
		ok, err := uploads.DeleteExpired(ctx, u.ID, at)
		if err == nil && ok {
			err = s.removeFile(u.ID)
			res.Expired++
		}
		s.unlock(u.ID)
		if err != nil {
			errs = append(errs, err)
		}
	}

	finished, err := uploads.Finished(ctx)
	if err != nil {
		return res, errors.Join(append(errs, err)...)
	}
	for _, u := range finished {
		// A job's end is final, so the upload can't be claimed again.
		ok, err := uploads.Delete(ctx, u.ID)
		if err == nil && ok {
			err = s.removeFile(u.ID)
			res.Finished++
		}
		if err != nil {
			errs = append(errs, err)
		}
	}

	// Records are created before their files and deleted before them, so
	// a file without a record is left over from a removal that failed
	// halfway, or from an upload whose site, group, or user was deleted.
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return res, errors.Join(append(errs, fmt.Errorf("listing uploads: %w", err))...)
	}
	for _, e := range entries {
		name := e.Name()
		if !id.Valid(name) || !e.Type().IsRegular() {
			continue
		}
		ok, err := uploads.Exists(ctx, name)
		if err == nil && !ok {
			err = s.removeFile(name)
			res.Orphaned++
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return res, errors.Join(errs...)
}

// CleanupHandler returns the job handler of the cleanup job, to register
// on the job pool.
func (s *Service) CleanupHandler() jobs.Handler {
	return jobs.Handler{Type: CleanupJobType, Steps: []jobs.Step{{
		Name: "remove_uploads",
		Do: func(ctx context.Context, r *jobs.Run) error {
			res, err := s.Cleanup(ctx)
			if res != (CleanupResult{}) {
				r.Logf("removed %d expired uploads, %d uploads of finished jobs, and %d files without a record",
					res.Expired, res.Finished, res.Orphaned)
			}
			return err
		},
	}}}
}

// EnsureCleanupSchedule creates or updates the cleanup job's schedule. It
// is safe to call on every start: an unchanged schedule keeps its next run.
func EnsureCleanupSchedule(ctx context.Context, q store.Querier) error {
	_, err := jobs.UpsertSchedule(ctx, q, jobs.ScheduleSpec{
		Name:            CleanupScheduleName,
		JobType:         CleanupJobType,
		TargetType:      cleanupTargetType,
		TargetID:        cleanupTargetID,
		LockKey:         CleanupJobType,
		Interval:        CleanupInterval,
		MissedRunPolicy: store.MissedCoalesce,
	})
	if err != nil {
		return fmt.Errorf("upload cleanup schedule: %w", err)
	}
	return nil
}
