package dbexport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// maxMessage bounds an export's failure message.
const maxMessage = 2000

// jobExportID returns the ID of the export the job's steps created, and
// whether they have created one yet.
func jobExportID(r *jobs.Run) (string, bool, error) {
	var exportID string
	found, err := r.Get(DataExportID, &exportID)
	return exportID, found, err
}

// record checks the site and creates the export's record, once.
func (s *Service) record(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	if err := exportableSite(ctx, s.db, siteID); errors.Is(err, ErrSiteNotFound) || errors.Is(err, ErrNotExportable) {
		return jobs.Permanent(err)
	} else if err != nil {
		return err
	}
	exportID, found, err := jobExportID(r)
	if err != nil {
		return err
	}
	if !found {
		// Recorded before the row exists, so a retry creates the same one.
		exportID = id.New()
		if err := r.Set(DataExportID, exportID); err != nil {
			return err
		}
	}
	err = store.NewDatabaseExports(s.db).Create(ctx, &store.DatabaseExport{ID: exportID, SiteID: siteID, JobID: r.Job().ID})
	if errors.Is(err, store.ErrConflict) {
		return nil // an earlier attempt created it
	}
	if err != nil {
		return err
	}
	r.Logf("Exporting the site's database (export %s).", exportID)
	return nil
}

// failExport marks the job's export failed and deletes its files, unless
// it is ready: then the caller may already have its link, and it expires
// as usual.
func (s *Service) failExport(ctx context.Context, r *jobs.Run) error {
	exportID, found, err := jobExportID(r)
	if err != nil || !found {
		return err
	}
	exports := store.NewDatabaseExports(s.db)
	e, err := exports.Get(ctx, exportID)
	if errors.Is(err, store.ErrNotFound) {
		return s.removeFiles(exportID)
	}
	if err != nil {
		return err
	}
	if e.Status == store.DatabaseExportReady {
		return nil
	}
	msg := r.Job().Error
	if msg == "" {
		msg = "the export job failed"
	}
	if len(msg) > maxMessage {
		msg = msg[:maxMessage-3] + "..."
	}
	changed, err := exports.Fail(ctx, exportID, msg)
	if err != nil {
		return err
	}
	if changed {
		r.Warnf("Database export %s failed.", exportID)
	}
	return s.removeFiles(exportID)
}

// dump dumps and compresses the database and marks the export ready.
func (s *Service) dump(ctx context.Context, r *jobs.Run) error {
	exportID, found, err := jobExportID(r)
	if err != nil {
		return err
	}
	if !found {
		return jobs.Permanent(errors.New("the job has no database export"))
	}
	exports := store.NewDatabaseExports(s.db)
	e, err := exports.Get(ctx, exportID)
	if err != nil {
		return err
	}
	switch e.Status {
	case store.DatabaseExportReady, store.DatabaseExportExpired:
		return nil
	case store.DatabaseExportFailed:
		return jobs.Permanent(fmt.Errorf("database export %s already failed", exportID))
	}
	start := s.now()
	size, err := s.writeExport(ctx, e.SiteID, exportID)
	if errors.Is(err, sitedb.ErrDatabaseNotFound) {
		return jobs.Permanent(err)
	}
	if err != nil {
		return err
	}
	expires := s.now().Add(s.ttl)
	if _, err := exports.Ready(ctx, exportID, size, expires); err != nil {
		return err
	}
	r.Logf("Dumped and compressed the database: %s, downloadable until %s.",
		formatBytes(size), expires.UTC().Format(time.RFC3339))
	s.log.InfoContext(ctx, "site database exported", "site_id", e.SiteID, "export_id", exportID,
		"size_bytes", size, "duration", s.now().Sub(start))
	return nil
}

// finish puts the export in the job's result.
func (s *Service) finish(ctx context.Context, r *jobs.Run) error {
	exportID, found, err := jobExportID(r)
	if err != nil {
		return err
	}
	if !found {
		return jobs.Permanent(errors.New("the job has no database export"))
	}
	e, err := store.NewDatabaseExports(s.db).Get(ctx, exportID)
	if err != nil {
		return err
	}
	if e.Status != store.DatabaseExportReady && e.Status != store.DatabaseExportExpired {
		return jobs.Permanent(fmt.Errorf("database export %s is %s", exportID, e.Status))
	}
	for k, v := range map[string]any{
		"site_id":      e.SiteID,
		"export_id":    e.ID,
		"download_url": DownloadPath(e.SiteID, e.ID),
		"size_bytes":   e.SizeBytes,
		"expires_at":   e.ExpiresAt.UTC().Format(time.RFC3339),
	} {
		if err := r.SetResult(k, v); err != nil {
			return err
		}
	}
	return nil
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
