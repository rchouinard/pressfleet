package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Site backups (#52).

// errNoBackups is a wiring bug: serve always passes the backup service.
var errNoBackups = errors.New("api: no backup service configured")

// ListSiteBackups lists the site's backup records, newest first. The cursor
// is the ID of the last backup on the previous page.
func (h *Handlers) ListSiteBackups(ctx context.Context, req gen.ListSiteBackupsRequestObject) (gen.ListSiteBackupsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewBackups, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	if h.backups == nil {
		return nil, errNoBackups
	}
	p := req.Params
	f := store.BackupRecordFilter{SiteID: req.SiteID}
	if p.Limit != nil {
		f.Limit = *p.Limit
	}
	if p.Cursor != nil {
		if !id.Valid(*p.Cursor) {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "The cursor is invalid; pass the next_cursor of a previous page.")
		}
		f.Before = *p.Cursor
	}
	if p.Status != nil {
		f.Status = store.BackupStatus(*p.Status)
	}
	page, err := h.backups.List(ctx, f)
	if err != nil {
		return nil, backupError(err)
	}
	out := gen.ListSiteBackups200JSONResponse{Items: make([]gen.Backup, len(page.Records))}
	for i, r := range page.Records {
		out.Items[i] = backupResponse(r)
	}
	if page.NextCursor != "" {
		out.NextCursor = &page.NextCursor
	}
	return out, nil
}

// CreateSiteBackup queues a manual backup of the site and returns 202 with
// the job.
func (h *Handlers) CreateSiteBackup(ctx context.Context, req gen.CreateSiteBackupRequestObject) (gen.CreateSiteBackupResponseObject, error) {
	p, err := h.authorize(ctx, authz.CreateBackup, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	if h.backups == nil {
		return nil, errNoBackups
	}
	job, err := h.backups.Backup(ctx, req.SiteID, p.UserID)
	if err != nil {
		return nil, backupError(err)
	}
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.CreateSiteBackup202JSONResponse{
		Body:    gen.JobAccepted{Job: j},
		Headers: gen.CreateSiteBackup202ResponseHeaders{Location: &location},
	}, nil
}

// GetSiteBackupSchedule returns the site's backup schedule (#53).
func (h *Handlers) GetSiteBackupSchedule(ctx context.Context, req gen.GetSiteBackupScheduleRequestObject) (gen.GetSiteBackupScheduleResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewBackups, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	if h.backups == nil {
		return nil, errNoBackups
	}
	s, err := h.backups.Schedule(ctx, req.SiteID)
	if err != nil {
		return nil, backupError(err)
	}
	return gen.GetSiteBackupSchedule200JSONResponse(backupScheduleResponse(s)), nil
}

// SetSiteBackupSchedule replaces the site's backup schedule. It is a site
// setting, so it is authorized (and audited) as a site update.
func (h *Handlers) SetSiteBackupSchedule(ctx context.Context, req gen.SetSiteBackupScheduleRequestObject) (gen.SetSiteBackupScheduleResponseObject, error) {
	if _, err := h.authorize(ctx, authz.UpdateSite, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	if h.backups == nil {
		return nil, errNoBackups
	}
	b := deref(req.Body)
	in := store.DefaultSiteBackupSchedule()
	in.Enabled, in.Hour = b.Enabled, b.Hour
	if b.IntervalHours != nil {
		in.IntervalHours = *b.IntervalHours
	}
	s, err := h.backups.SetSchedule(ctx, req.SiteID, in)
	if err != nil {
		return nil, backupError(err)
	}
	return gen.SetSiteBackupSchedule200JSONResponse(backupScheduleResponse(s)), nil
}

func backupScheduleResponse(s sitebackup.ScheduleState) gen.BackupSchedule {
	out := gen.BackupSchedule{Enabled: s.Enabled, IntervalHours: s.IntervalHours, Hour: s.Hour}
	if !s.NextRunAt.IsZero() {
		next := s.NextRunAt
		out.NextRunAt = &next
	}
	return out
}

// RestoreSite checks the snapshot and queues the job that restores it into
// the site (#55), returning 202 with the site and the job. The caller's
// right to restore is checked on the target site alone: the source must be
// in the same client group, which the authorization check resolves both
// sites to.
func (h *Handlers) RestoreSite(ctx context.Context, req gen.RestoreSiteRequestObject) (gen.RestoreSiteResponseObject, error) {
	p, err := h.authorize(ctx, authz.RestoreBackup, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	if h.sites == nil {
		return nil, errNoSites
	}
	b := deref(req.Body)
	site, job, err := h.sites.Restore(ctx, req.SiteID, sites.Restore{
		SnapshotID:      b.SnapshotID,
		SourceSiteID:    deref(b.SourceSiteID),
		CreatedByUserID: p.UserID,
	})
	if err != nil {
		return nil, siteError(err)
	}
	_, aliases, err := h.sites.Get(ctx, site.ID)
	if err != nil {
		return nil, siteError(err)
	}
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.RestoreSite202JSONResponse{
		Body:    gen.SiteAccepted{Site: siteResponse(site, aliases), Job: j},
		Headers: gen.RestoreSite202ResponseHeaders{Location: &location},
	}, nil
}

// backupError maps backup errors to API errors.
func backupError(err error) error {
	var se *sitebackup.ScheduleError
	if errors.As(err, &se) {
		return &ValidationError{Fields: []httpserver.FieldError{{Field: se.Field, Detail: se.Detail}}}
	}
	switch {
	case errors.Is(err, sitebackup.ErrSiteNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, sitebackup.ErrSiteNotFound)
	case errors.Is(err, sitebackup.ErrNotBackupable):
		// The wrapping says what the site is doing.
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return err
}

func backupResponse(r store.BackupRecord) gen.Backup {
	out := gen.Backup{
		ID:            r.ID,
		SiteID:        r.SiteID,
		SiteName:      r.SiteName,
		SiteSlug:      r.SiteSlug,
		ClientGroupID: r.ClientGroupID,
		Kind:          gen.BackupKind(r.Kind),
		Status:        gen.BackupStatus(r.Status),
		WpCoreVersion: r.WPCoreVersion,
		Message:       r.Message,
		CreatedAt:     r.CreatedAt,
		StartedAt:     r.StartedAt,
	}
	if r.Status == store.BackupSucceeded {
		snap, size, added, files := r.SnapshotID, r.SizeBytes, r.AddedBytes, r.FileCount
		out.SnapshotID, out.SizeBytes, out.AddedBytes, out.FileCount = &snap, &size, &added, &files
	}
	if r.JobID != "" {
		job := r.JobID
		out.JobID = &job
	}
	if !r.FinishedAt.IsZero() {
		finished := r.FinishedAt
		out.FinishedAt = &finished
	}
	if !r.ExpiredAt.IsZero() {
		expired := r.ExpiredAt
		out.ExpiredAt = &expired
	}
	return out
}
