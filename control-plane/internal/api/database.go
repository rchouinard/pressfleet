package api

import (
	"context"
	"errors"
	"fmt"
	"mime"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/dbexport"
)

// Database exports (#114, design doc §5.8).

// errNoDatabaseExports is a wiring bug: serve always passes the service.
var errNoDatabaseExports = errors.New("api: no database export service configured")

// ExportSiteDatabase queues a job that exports the site's database and
// returns 202 with the job, whose result links to the download.
func (h *Handlers) ExportSiteDatabase(ctx context.Context, req gen.ExportSiteDatabaseRequestObject) (gen.ExportSiteDatabaseResponseObject, error) {
	p, err := h.authorize(ctx, authz.ExportDatabase, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	if h.dbExports == nil {
		return nil, errNoDatabaseExports
	}
	job, err := h.dbExports.Export(ctx, req.SiteID, p.UserID)
	if err != nil {
		return nil, databaseExportError(err)
	}
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.ExportSiteDatabase202JSONResponse{
		Body:    gen.JobAccepted{Job: j},
		Headers: gen.ExportSiteDatabase202ResponseHeaders{Location: &location},
	}, nil
}

// DownloadSiteDatabaseExport streams a ready export's compressed dump. The
// download is a read, but it hands out the site's data, so it is recorded
// in the audit log like a change.
func (h *Handlers) DownloadSiteDatabaseExport(ctx context.Context, req gen.DownloadSiteDatabaseExportRequestObject) (gen.DownloadSiteDatabaseExportResponseObject, error) {
	auditRead(ctx)
	if _, err := h.authorize(ctx, authz.DownloadDatabaseExport, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	if h.dbExports == nil {
		return nil, errNoDatabaseExports
	}
	d, err := h.dbExports.Open(ctx, req.SiteID, req.ExportID)
	if err != nil {
		return nil, databaseExportError(err)
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": d.Name})
	return gen.DownloadSiteDatabaseExport200ApplicationGzipResponse{
		Body:          d.File, // closed once sent
		ContentLength: d.Size,
		Headers:       gen.DownloadSiteDatabaseExport200ResponseHeaders{ContentDisposition: &disposition},
	}, nil
}

// databaseExportError maps database export errors to API errors.
func databaseExportError(err error) error {
	switch {
	case errors.Is(err, dbexport.ErrSiteNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, dbexport.ErrSiteNotFound)
	case errors.Is(err, dbexport.ErrExportNotFound):
		// The wrapping says why, e.g. that it has expired.
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, dbexport.ErrNotExportable):
		// The wrapping says what the site is doing.
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return err
}
