package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
)

// Database exports (#114, design doc §5.8), one kind of export (see
// exports.go).

// ExportSiteDatabase queues a job that exports the site's database and
// returns 202 with the job, whose result links to the download.
func (h *Handlers) ExportSiteDatabase(ctx context.Context, req gen.ExportSiteDatabaseRequestObject) (gen.ExportSiteDatabaseResponseObject, error) {
	p, err := h.authorize(ctx, authz.ExportDatabase, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	job, err := h.exports.ExportDatabase(ctx, req.SiteID, p.UserID)
	if err != nil {
		return nil, exportError(err)
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
	d, err := h.exports.OpenDatabase(ctx, req.SiteID, req.ExportID)
	if err != nil {
		return nil, exportError(err)
	}
	return fileResponse{r: requestFromContext(ctx), d: d}, nil
}
