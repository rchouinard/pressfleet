package api

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/export"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Site exports (#133, #134, design doc §5.7).

// errNoExports is a wiring bug: serve always passes the service.
var errNoExports = errors.New("api: no export service configured")

// ExportSite queues a job that exports the live site and returns 202 with
// the job, whose result links to the downloads.
func (h *Handlers) ExportSite(ctx context.Context, req gen.ExportSiteRequestObject) (gen.ExportSiteResponseObject, error) {
	p, err := h.authorize(ctx, authz.ExportSite, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	if h.exports == nil {
		return nil, errNoExports
	}
	job, err := h.exports.ExportSite(ctx, req.SiteID, p.UserID)
	if err != nil {
		return nil, exportError(err)
	}
	body, location, err := jobAccepted(job)
	if err != nil {
		return nil, err
	}
	return gen.ExportSite202JSONResponse{Body: body, Headers: gen.ExportSite202ResponseHeaders{Location: &location}}, nil
}

// ExportSiteBackup queues a job that exports one of the site's backups.
func (h *Handlers) ExportSiteBackup(ctx context.Context, req gen.ExportSiteBackupRequestObject) (gen.ExportSiteBackupResponseObject, error) {
	p, err := h.authorize(ctx, authz.ExportSite, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	job, err := h.exportBackup(ctx, req.BackupID, p.UserID, func(rec store.BackupRecord) bool { return rec.SiteID == req.SiteID })
	if err != nil {
		return nil, err
	}
	body, location, err := jobAccepted(job)
	if err != nil {
		return nil, err
	}
	return gen.ExportSiteBackup202JSONResponse{Body: body, Headers: gen.ExportSiteBackup202ResponseHeaders{Location: &location}}, nil
}

// ExportClientGroupBackup queues a job that exports a backup of one of the
// group's sites, which may have been deleted.
func (h *Handlers) ExportClientGroupBackup(ctx context.Context, req gen.ExportClientGroupBackupRequestObject) (gen.ExportClientGroupBackupResponseObject, error) {
	p, err := h.authorize(ctx, authz.ExportSite, authz.ClientGroup(req.ClientGroupID))
	if err != nil {
		return nil, err
	}
	job, err := h.exportBackup(ctx, req.BackupID, p.UserID, func(rec store.BackupRecord) bool { return rec.ClientGroupID == req.ClientGroupID })
	if err != nil {
		return nil, err
	}
	body, location, err := jobAccepted(job)
	if err != nil {
		return nil, err
	}
	return gen.ExportClientGroupBackup202JSONResponse{Body: body, Headers: gen.ExportClientGroupBackup202ResponseHeaders{Location: &location}}, nil
}

// exportBackup queues the export of a backup that belongs to the path's
// site or group, as belongs says; any other is not found.
func (h *Handlers) exportBackup(ctx context.Context, backupID, userID string, belongs func(store.BackupRecord) bool) (store.Job, error) {
	if h.exports == nil {
		return store.Job{}, errNoExports
	}
	rec, err := store.NewBackupRecords(h.db).Get(ctx, backupID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !belongs(rec)) {
		return store.Job{}, fmt.Errorf("%w: %w: %s", ErrNotFound, export.ErrBackupNotFound, backupID)
	}
	if err != nil {
		return store.Job{}, err
	}
	job, err := h.exports.ExportBackup(ctx, rec.ID, userID)
	if err != nil {
		return store.Job{}, exportError(err)
	}
	return job, nil
}

// jobAccepted is the body and Location of a 202 for a queued job.
func jobAccepted(job store.Job) (gen.JobAccepted, string, error) {
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return gen.JobAccepted{}, "", err
	}
	return gen.JobAccepted{Job: j}, jobLocation(job), nil
}

// ListClientGroupBackups lists the backup records of the group's sites,
// newest first; with deleted, those of its deleted sites.
func (h *Handlers) ListClientGroupBackups(ctx context.Context, req gen.ListClientGroupBackupsRequestObject) (gen.ListClientGroupBackupsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewBackups, authz.ClientGroup(req.ClientGroupID)); err != nil {
		return nil, err
	}
	if h.backups == nil {
		return nil, errNoBackups
	}
	p := req.Params
	f := store.BackupRecordFilter{ClientGroupID: req.ClientGroupID, DeletedSites: p.Deleted != nil && *p.Deleted}
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
	page, err := h.backups.ListGroup(ctx, f)
	if errors.Is(err, sitebackup.ErrClientGroupNotFound) {
		return nil, fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	if err != nil {
		return nil, err
	}
	out := gen.ListClientGroupBackups200JSONResponse{Items: make([]gen.Backup, len(page.Records))}
	for i, r := range page.Records {
		out.Items[i] = backupResponse(r)
	}
	if page.NextCursor != "" {
		out.NextCursor = &page.NextCursor
	}
	return out, nil
}

// ListExports lists the exports of a site or a client group's sites,
// newest first.
func (h *Handlers) ListExports(ctx context.Context, req gen.ListExportsRequestObject) (gen.ListExportsResponseObject, error) {
	p := req.Params
	var f store.ExportFilter
	target := authz.Platform()
	switch {
	case p.SiteID != nil:
		f.SiteID, target = *p.SiteID, authz.Site(*p.SiteID)
		if p.ClientGroupID != nil {
			f.ClientGroupID = *p.ClientGroupID
		}
	case p.ClientGroupID != nil:
		f.ClientGroupID, target = *p.ClientGroupID, authz.ClientGroup(*p.ClientGroupID)
	}
	if _, err := h.authorize(ctx, authz.ViewSites, target); err != nil {
		return nil, err
	}
	if h.exports == nil {
		return nil, errNoExports
	}
	if target == authz.Platform() {
		return nil, httpserver.NewProblem(http.StatusBadRequest, "Pass site_id or client_group_id.")
	}
	if p.Kind != nil {
		for _, k := range *p.Kind {
			f.Kinds = append(f.Kinds, store.ExportKind(k))
		}
	}
	if p.Status != nil {
		for _, s := range *p.Status {
			f.Statuses = append(f.Statuses, store.ExportStatus(s))
		}
	}
	if p.Limit != nil {
		f.Limit = *p.Limit
	}
	if p.Cursor != nil {
		if !id.Valid(*p.Cursor) {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "The cursor is invalid; pass the next_cursor of a previous page.")
		}
		f.Before = *p.Cursor
	}
	page, err := h.exports.List(ctx, f)
	if err != nil {
		return nil, err
	}
	out := gen.ListExports200JSONResponse{Items: make([]gen.Export, len(page.Exports))}
	for i, e := range page.Exports {
		out.Items[i] = exportResponse(e)
	}
	if page.NextCursor != "" {
		out.NextCursor = &page.NextCursor
	}
	return out, nil
}

// GetExport returns an export.
func (h *Handlers) GetExport(ctx context.Context, req gen.GetExportRequestObject) (gen.GetExportResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Export(req.ExportID)); err != nil {
		return nil, err
	}
	if h.exports == nil {
		return nil, errNoExports
	}
	e, err := h.exports.Get(ctx, req.ExportID)
	if err != nil {
		return nil, exportError(err)
	}
	return gen.GetExport200JSONResponse(exportResponse(e)), nil
}

// DownloadExportFile streams one of a ready export's files, honoring Range
// requests so a download can resume. Like a database export's download, it
// is recorded in the audit log.
func (h *Handlers) DownloadExportFile(ctx context.Context, req gen.DownloadExportFileRequestObject) (gen.DownloadExportFileResponseObject, error) {
	auditRead(ctx)
	if _, err := h.authorize(ctx, authz.ExportSite, authz.Export(req.ExportID)); err != nil {
		return nil, err
	}
	if h.exports == nil {
		return nil, errNoExports
	}
	setAuditDetails(ctx, map[string]string{"file": string(req.FileName)})
	d, err := h.exports.Open(ctx, req.ExportID, string(req.FileName))
	if err != nil {
		return nil, exportError(err)
	}
	setAuditDetails(ctx, map[string]string{"site_id": d.Export.SiteID})
	return fileResponse{r: requestFromContext(ctx), d: d}, nil
}

// exportError maps export errors to API errors.
func exportError(err error) error {
	switch {
	case errors.Is(err, export.ErrSiteNotFound), errors.Is(err, export.ErrBackupNotFound),
		errors.Is(err, export.ErrExportNotFound):
		// The wrapping says why, e.g. that an export has expired.
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, export.ErrNotExportable), errors.Is(err, export.ErrBackupNotExportable):
		// The wrapping says what the site or backup is doing.
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return err
}

func exportResponse(e store.Export) gen.Export {
	out := gen.Export{
		ID:            e.ID,
		Kind:          gen.ExportKind(e.Kind),
		SiteID:        e.SiteID,
		SiteSlug:      e.SiteSlug,
		ClientGroupID: e.ClientGroupID,
		Status:        gen.ExportStatus(e.Status),
		Source: gen.ExportSource{
			SiteURL: e.Source.SiteURL, TablePrefix: e.Source.TablePrefix,
			WpCoreVersion: e.Source.WPCoreVersion, PhpVersion: e.Source.PHPVersion,
			TakenAt: optionalTime(e.Source.TakenAt),
		},
		Files:     []gen.ExportFile{},
		Message:   e.Message,
		CreatedAt: e.CreatedAt,
		ReadyAt:   optionalTime(e.ReadyAt),
		ExpiresAt: optionalTime(e.ExpiresAt),
		ExpiredAt: optionalTime(e.ExpiredAt),
	}
	if e.BackupID != "" {
		backupID, snap := e.BackupID, e.SnapshotID
		out.BackupID, out.SnapshotID = &backupID, &snap
	}
	if e.JobID != "" {
		job := e.JobID
		out.JobID = &job
	}
	if e.Status == store.ExportReady {
		size := e.SizeBytes
		out.SizeBytes = &size
		for _, f := range e.Files {
			out.Files = append(out.Files, gen.ExportFile{
				Name: gen.ExportFileName(f.Name), SizeBytes: f.SizeBytes, DownloadURL: export.FilePath(e.ID, f.Name),
			})
		}
	}
	return out
}

// requestKey holds the HTTP request in a strict handler's context.
type requestKey struct{}

// withRequest puts the HTTP request in the strict handlers' context, for
// responses that depend on its headers, such as Range downloads.
func withRequest(f gen.StrictHandlerFunc, _ string) gen.StrictHandlerFunc {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, request any) (any, error) {
		return f(context.WithValue(ctx, requestKey{}, r), w, r, request)
	}
}

// requestFromContext returns the request withRequest stored, or a plain
// GET without headers.
func requestFromContext(ctx context.Context) *http.Request {
	if r, ok := ctx.Value(requestKey{}).(*http.Request); ok {
		return r
	}
	r, _ := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
	return r
}

// fileResponse serves an export's file with http.ServeContent, which
// answers Range and If-Range (206 or 416) and HEAD requests. Export files
// never change once ready, so the export's ID and the file's name make a
// strong ETag.
type fileResponse struct {
	r *http.Request
	d export.Download
}

func (f fileResponse) serve(w http.ResponseWriter) error {
	defer func() { _ = f.d.File.Close() }()
	// A large file takes longer to send than the server's write timeout,
	// which is for responses of ordinary size.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", export.ContentType)
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.d.Name}))
	h.Set("ETag", `"`+f.d.Export.ID+"-"+f.d.Name+`"`)
	http.ServeContent(w, f.r, "", f.d.ModTime, f.d.File)
	return nil
}

// VisitDownloadExportFileResponse implements gen.DownloadExportFileResponseObject.
func (f fileResponse) VisitDownloadExportFileResponse(w http.ResponseWriter) error { return f.serve(w) }

// VisitDownloadSiteDatabaseExportResponse implements
// gen.DownloadSiteDatabaseExportResponseObject.
func (f fileResponse) VisitDownloadSiteDatabaseExportResponse(w http.ResponseWriter) error {
	return f.serve(w)
}
