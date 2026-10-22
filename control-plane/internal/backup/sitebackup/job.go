package sitebackup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// maxMessage bounds a record's message.
const maxMessage = 2000

// Handler returns the JobType job handler, to register on the job pool.
func (s *Service) Handler() jobs.Handler {
	steps := s.steps(payloadKind)
	steps = append(steps, jobs.Step{Name: stepFinish, Do: s.finish})
	return jobs.Handler{Type: JobType, Steps: steps}
}

// Steps returns the steps that back up the job's target site, recording a
// backup of the given kind, for a job type that takes a backup as part of
// its own work (e.g. store.BackupPreRestore before a restore). They are
// named StepRecord and StepSnapshot and use the job data key DataBackupID,
// so the job's other steps must not. After them, JobBackup returns the
// backup.
func (s *Service) Steps(kind store.BackupKind) []jobs.Step {
	return s.steps(func(*jobs.Run) (store.BackupKind, error) {
		if !kind.Valid() {
			return "", jobs.Permanent(fmt.Errorf("invalid backup kind %q", kind))
		}
		return kind, nil
	})
}

// kindFunc says what kind of backup a job takes.
type kindFunc func(r *jobs.Run) (store.BackupKind, error)

// payloadKind reads the kind from a JobType job's payload.
func payloadKind(r *jobs.Run) (store.BackupKind, error) {
	var p JobPayload
	if err := r.Payload(&p); err != nil {
		return "", err
	}
	if p.Kind == "" {
		return store.BackupManual, nil
	}
	if !p.Kind.Valid() {
		return "", jobs.Permanent(fmt.Errorf("invalid backup kind %q", p.Kind))
	}
	return p.Kind, nil
}

func (s *Service) steps(kind kindFunc) []jobs.Step {
	return []jobs.Step{
		{Name: StepRecord, Do: func(ctx context.Context, r *jobs.Run) error { return s.record(ctx, r, kind) }, Undo: s.failRecord},
		{Name: StepSnapshot, Do: s.snapshot},
	}
}

// JobBackup returns the backup record that the backup steps of the job
// created, and whether they have created one yet.
func (s *Service) JobBackup(ctx context.Context, r *jobs.Run) (store.BackupRecord, bool, error) {
	var backupID string
	if found, err := r.Get(DataBackupID, &backupID); err != nil || !found {
		return store.BackupRecord{}, false, err
	}
	rec, err := store.NewBackupRecords(s.db).Get(ctx, backupID)
	if errors.Is(err, store.ErrNotFound) {
		return store.BackupRecord{}, false, nil
	}
	if err != nil {
		return store.BackupRecord{}, false, err
	}
	return rec, true, nil
}

// record checks the site and creates its backup record, once.
func (s *Service) record(ctx context.Context, r *jobs.Run, kindOf kindFunc) error {
	kind, err := kindOf(r)
	if err != nil {
		return err
	}
	siteID := r.Job().TargetID
	site, err := backupableSite(ctx, s.db, siteID)
	if kind == store.BackupScheduled && (errors.Is(err, ErrSiteNotFound) || errors.Is(err, ErrNotBackupable)) {
		// Nobody asked for this backup, so there is no one to tell it
		// failed: the site went away or can't be backed up right now, and
		// the next run tries again if it still has a schedule.
		return s.skip(ctx, r, err)
	}
	if errors.Is(err, ErrSiteNotFound) || errors.Is(err, ErrNotBackupable) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err
	}
	var backupID string
	found, err := r.Get(DataBackupID, &backupID)
	if err != nil {
		return err
	}
	if !found {
		// Recorded before the row exists, so a retry creates the same one.
		backupID = id.New()
		if err := r.Set(DataBackupID, backupID); err != nil {
			return err
		}
	}
	rec := store.BackupRecord{
		ID: backupID, SiteID: siteID, SiteName: site.Name, SiteSlug: site.Slug, ClientGroupID: site.ClientGroupID, Kind: kind,
		WPCoreVersion: site.WPCoreVersion, JobID: r.Job().ID,
	}
	err = store.NewBackupRecords(s.db).Create(ctx, &rec)
	if errors.Is(err, store.ErrConflict) {
		return nil // an earlier attempt created it
	}
	if err != nil {
		return err
	}
	r.Logf("Backing up the site's wp-content and database into its client group's repository (%s backup %s).", kind, backupID)
	return nil
}

// skip ends a scheduled backup without taking one. The schedule of a site
// that no longer exists should have gone with it; it is removed in case it
// didn't, so it stops queuing jobs for nothing.
func (s *Service) skip(ctx context.Context, r *jobs.Run, reason error) error {
	if errors.Is(reason, ErrSiteNotFound) {
		if err := RemoveSchedule(ctx, s.db, r.Job().TargetID); err != nil {
			return err
		}
	}
	if err := r.Set(dataSkipped, true); err != nil {
		return err
	}
	r.Logf("Skipped the scheduled backup: %v.", reason)
	return nil
}

// skipped reports whether the job's scheduled backup was skipped.
func skipped(r *jobs.Run) (bool, error) {
	var skip bool
	_, err := r.Get(dataSkipped, &skip)
	return skip, err
}

// failRecord marks the job's backup failed, unless it finished.
func (s *Service) failRecord(ctx context.Context, r *jobs.Run) error {
	var backupID string
	found, err := r.Get(DataBackupID, &backupID)
	if err != nil || !found {
		return err
	}
	msg := r.Job().Error
	if msg == "" {
		msg = "the backup job failed"
	}
	changed, err := store.NewBackupRecords(s.db).Fail(ctx, backupID, truncate(msg))
	if err != nil {
		return err
	}
	if changed {
		r.Warnf("Backup %s failed.", backupID)
	}
	return nil
}

// snapshot takes the snapshot and finishes the record.
func (s *Service) snapshot(ctx context.Context, r *jobs.Run) error {
	if skip, err := skipped(r); err != nil || skip {
		return err
	}
	var backupID string
	found, err := r.Get(DataBackupID, &backupID)
	if err != nil {
		return err
	}
	if !found {
		return jobs.Permanent(errors.New("the job has no backup record"))
	}
	records := store.NewBackupRecords(s.db)
	rec, err := records.Get(ctx, backupID)
	if err != nil {
		return err
	}
	switch rec.Status {
	case store.BackupSucceeded:
		return nil
	case store.BackupFailed:
		return jobs.Permanent(fmt.Errorf("backup %s already failed", backupID))
	}
	if _, err := records.Start(ctx, backupID); err != nil {
		return err
	}
	snap, err := s.engine.Backup(ctx, rec.SiteID, backup.Options{
		Kind: string(rec.Kind), BackupID: backupID,
		// The first attempt has no earlier one to find; a later one may
		// follow a snapshot whose result was lost.
		Retry: r.Job().Attempts > 1,
	})
	if err != nil {
		if permanent(err) {
			return jobs.Permanent(err)
		}
		return err
	}
	res := store.BackupResult{
		SnapshotID: string(snap.ID), SizeBytes: snap.SizeBytes, AddedBytes: snap.AddedBytes, FileCount: snap.FileCount,
	}
	if len(snap.Warnings) > 0 {
		res.Warning = truncate(fmt.Sprintf("%d files couldn't be read and are missing from the snapshot: %s",
			len(snap.Warnings), strings.Join(snap.Warnings, "; ")))
		r.Warnf("%s", res.Warning)
	}
	if _, err := records.Succeed(ctx, backupID, res); err != nil {
		return err
	}
	r.Logf("Took snapshot %s: %d files, %s, of which %s were new to the repository.",
		shortID(snap.ID), snap.FileCount, formatBytes(snap.SizeBytes), formatBytes(snap.AddedBytes))
	s.log.InfoContext(ctx, "site backup recorded", "site_id", rec.SiteID, "backup_id", backupID, "snapshot_id", snap.ID)
	return nil
}

// permanent reports whether retrying a failed backup can't help: the site
// or its database is gone, or the engine can't do it.
func permanent(err error) bool {
	for _, target := range []error{backup.ErrSiteNotFound, provisioner.ErrSiteNotFound, sitedb.ErrDatabaseNotFound, errors.ErrUnsupported} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// finish puts the backup in the job's result.
func (s *Service) finish(ctx context.Context, r *jobs.Run) error {
	skip, err := skipped(r)
	if err != nil {
		return err
	}
	if skip {
		return r.SetResult("site_id", r.Job().TargetID)
	}
	rec, found, err := s.JobBackup(ctx, r)
	if err != nil {
		return err
	}
	if !found {
		return jobs.Permanent(errors.New("the job has no backup record"))
	}
	for k, v := range map[string]string{"site_id": rec.SiteID, "backup_id": rec.ID, "snapshot_id": rec.SnapshotID} {
		if err := r.SetResult(k, v); err != nil {
			return err
		}
	}
	return nil
}

func truncate(msg string) string {
	if len(msg) > maxMessage {
		return msg[:maxMessage-3] + "..."
	}
	return msg
}

// shortID is the short form restic shows snapshot IDs in.
func shortID(snap backup.SnapshotID) string {
	if len(snap) > 8 {
		return string(snap[:8])
	}
	return string(snap)
}

// formatBytes formats a size in binary units, e.g. "1.5 MiB".
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
