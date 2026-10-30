package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Sites: provisioning (#27), management (#28), and deletion (#29).

// ListSites lists sites, newest first, optionally filtered by client group
// or status. The cursor is the ID of the last site on the previous page.
func (h *Handlers) ListSites(ctx context.Context, req gen.ListSitesRequestObject) (gen.ListSitesResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.ClientGroup(deref(req.Params.ClientGroupID))); err != nil {
		return nil, err
	}
	p := req.Params
	f := store.SiteFilter{ClientGroupID: deref(p.ClientGroupID)}
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
		f.Status = store.SiteStatus(*p.Status)
	}
	if p.ParentSiteID != nil {
		f.ParentSiteID = *p.ParentSiteID
	}
	if p.Environment != nil {
		f.Environment = store.SiteEnvironment(*p.Environment)
	}
	page, aliases, err := h.sites.List(ctx, f)
	if err != nil {
		return nil, err
	}
	counts, err := h.updateCounts(ctx)
	if err != nil {
		return nil, err
	}
	out := gen.ListSites200JSONResponse{Items: make([]gen.Site, len(page.Sites))}
	for i, s := range page.Sites {
		out.Items[i] = siteResponse(s, aliases[s.ID])
		out.Items[i].PendingUpdates = pendingUpdateCounts(counts, s.ID)
	}
	if page.NextCursor != "" {
		out.NextCursor = &page.NextCursor
	}
	return out, nil
}

// CreateSite starts the provisioning workflow and returns 202 with the job.
func (h *Handlers) CreateSite(ctx context.Context, req gen.CreateSiteRequestObject) (gen.CreateSiteResponseObject, error) {
	// The new site's target is the group it will belong to.
	p, err := h.authorize(ctx, authz.CreateSite, authz.ClientGroup(deref(req.Body).ClientGroupID))
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := sites.NewSite{
		ClientGroupID:   b.ClientGroupID,
		Name:            b.Name,
		AdminEmail:      string(b.AdminEmail),
		AdminUsername:   deref(b.AdminUsername),
		PHPVersion:      deref(b.PhpVersion),
		CreatedByUserID: p.UserID,
	}
	if r := b.Resources; r != nil {
		in.CPU, in.MemoryReservationMB, in.MemoryLimitMB = r.CPU, r.MemoryReservationMb, r.MemoryLimitMb
	}
	site, job, err := h.sites.Create(ctx, in)
	if err != nil {
		return nil, siteError(err)
	}
	setAuditTarget(ctx, authz.Site(site.ID))
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.CreateSite202JSONResponse{
		Body:    gen.SiteAccepted{Site: siteResponse(site, nil), Job: j},
		Headers: gen.CreateSite202ResponseHeaders{Location: &location},
	}, nil
}

// ImportSite starts the import workflow (#112) and returns 202 with the
// new site's ID and the job. The uploads are checked by the job, before
// the site is created.
func (h *Handlers) ImportSite(ctx context.Context, req gen.ImportSiteRequestObject) (gen.ImportSiteResponseObject, error) {
	// Like creation, the new site's target is the group it will join.
	p, err := h.authorize(ctx, authz.ImportSite, authz.ClientGroup(deref(req.Body).ClientGroupID))
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := sites.Import{
		ClientGroupID:   b.ClientGroupID,
		ArchiveUploadID: b.ArchiveUploadID,
		DumpUploadID:    b.DumpUploadID,
		Name:            deref(b.Name),
		PHPVersion:      deref(b.PhpVersion),
		CreatedByUserID: p.UserID,
	}
	if r := b.Resources; r != nil {
		in.CPU, in.MemoryReservationMB, in.MemoryLimitMB = r.CPU, r.MemoryReservationMb, r.MemoryLimitMb
	}
	siteID, job, err := h.sites.Import(ctx, in)
	if err != nil {
		return nil, siteError(err)
	}
	setAuditTarget(ctx, authz.Site(siteID))
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.ImportSite202JSONResponse{
		Body:    gen.SiteImportAccepted{SiteID: siteID, Job: j},
		Headers: gen.ImportSite202ResponseHeaders{Location: &location},
	}, nil
}

// CreateStagingSite creates a staging site of a production site (#202,
// design doc §5.11) and returns 202 with the staging site, its one-time
// preview credentials, and the clone job. It is checked against the
// production site, whose client group the staging site joins.
func (h *Handlers) CreateStagingSite(ctx context.Context, req gen.CreateStagingSiteRequestObject) (gen.CreateStagingSiteResponseObject, error) {
	p, err := h.authorize(ctx, authz.CreateStagingSite, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	b := deref(req.Body)
	in := sites.NewStaging{
		Name:            deref(b.Name),
		SnapshotID:      deref(b.SnapshotID),
		CreatedByUserID: p.UserID,
	}
	if r := b.Resources; r != nil {
		in.CPU, in.MemoryReservationMB, in.MemoryLimitMB = r.CPU, r.MemoryReservationMb, r.MemoryLimitMb
	}
	out, err := h.sites.CreateStaging(ctx, req.SiteID, in)
	if err != nil {
		return nil, siteError(err)
	}
	setAuditTarget(ctx, authz.Site(out.Site.ID))
	details := map[string]string{"parent_site_id": req.SiteID, "job_id": out.Job.ID}
	if out.BackupJob != nil {
		details["backup_job_id"] = out.BackupJob.ID
	} else {
		details["snapshot_id"] = in.SnapshotID
	}
	setAuditDetails(ctx, details)
	j, err := jobResponse(out.Job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(out.Job)
	return gen.CreateStagingSite202JSONResponse{
		Body: gen.SiteAccepted{
			Site: siteResponse(out.Site, nil),
			Job:  j,
			PreviewCredentials: &gen.PreviewCredentials{
				Username: out.Preview.Username, Password: out.Preview.Password.Reveal(),
			},
		},
		Headers: gen.CreateStagingSite202ResponseHeaders{Location: &location},
	}, nil
}

// RefreshStagingSite queues the jobs that copy a production site into its
// staging site (#203, design doc §5.11), returning 202 with the staging site
// and the refresh job. It is checked against the staging site, whose client
// group its production site shares.
func (h *Handlers) RefreshStagingSite(ctx context.Context, req gen.RefreshStagingSiteRequestObject) (gen.RefreshStagingSiteResponseObject, error) {
	p, err := h.authorize(ctx, authz.RefreshStagingSite, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	b := deref(req.Body)
	out, err := h.sites.RefreshStaging(ctx, req.SiteID, sites.StagingRefresh{
		Files:           backup.RestoreFiles(b.Files),
		Database:        b.Database,
		SnapshotID:      deref(b.SnapshotID),
		CreatedByUserID: p.UserID,
	})
	if err != nil {
		return nil, siteError(err)
	}
	details := map[string]string{
		"parent_site_id": out.Site.ParentSiteID,
		"files":          string(b.Files),
		"database":       strconv.FormatBool(b.Database),
		"job_id":         out.Job.ID,
	}
	if out.BackupJob != nil {
		details["backup_job_id"] = out.BackupJob.ID
	} else {
		details["snapshot_id"] = out.SnapshotID
	}
	setAuditDetails(ctx, details)
	_, aliases, err := h.sites.Get(ctx, out.Site.ID)
	if err != nil {
		return nil, siteError(err)
	}
	j, err := jobResponse(out.Job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(out.Job)
	return gen.RefreshStagingSite202JSONResponse{
		Body:    gen.SiteAccepted{Site: siteResponse(out.Site, aliases), Job: j},
		Headers: gen.RefreshStagingSite202ResponseHeaders{Location: &location},
	}, nil
}

// GetSite returns a site with its recorded status and its live runtime
// state. The live state is best effort: if the container runtime can't be
// reached in time, the phase is unknown and the request still succeeds.
func (h *Handlers) GetSite(ctx context.Context, req gen.GetSiteRequestObject) (gen.GetSiteResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	site, aliases, err := h.sites.Get(ctx, req.SiteID)
	if err != nil {
		return nil, siteError(err)
	}
	counts, err := h.updateCounts(ctx)
	if err != nil {
		return nil, err
	}
	rt := h.sites.Runtime(ctx, site.ID)
	out := siteResponse(site, aliases)
	out.PendingUpdates = pendingUpdateCounts(counts, site.ID)
	out.Runtime = &gen.SiteRuntime{Phase: gen.SiteRuntimePhase(rt.Phase)}
	if rt.Message != "" {
		out.Runtime.Message = &rt.Message
	}
	return gen.GetSite200JSONResponse(out), nil
}

// UpdateSite saves a new PHP version or resource limits and queues the job
// that applies them.
func (h *Handlers) UpdateSite(ctx context.Context, req gen.UpdateSiteRequestObject) (gen.UpdateSiteResponseObject, error) {
	p, err := h.authorize(ctx, authz.UpdateSite, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	b := deref(req.Body)
	in := sites.SiteUpdate{PHPVersion: b.PhpVersion, OutboundMail: b.OutboundMail, CreatedByUserID: p.UserID}
	if r := b.Resources; r != nil {
		in.CPU, in.MemoryReservationMB, in.MemoryLimitMB = r.CPU, r.MemoryReservationMb, r.MemoryLimitMb
	}
	if pp := b.PreviewProtection; pp != nil {
		in.PreviewProtection, in.PreviewUsername = pp.Enabled, pp.Username
	}
	site, job, preview, err := h.sites.Update(ctx, req.SiteID, in)
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
	out := gen.UpdateSite202JSONResponse{
		Body:    gen.SiteAccepted{Site: siteResponse(site, aliases), Job: j},
		Headers: gen.UpdateSite202ResponseHeaders{Location: &location},
	}
	if preview != nil {
		// The generated password is returned by this response only. Like
		// every API response, it is sent with Cache-Control: no-store.
		out.Body.PreviewCredentials = &gen.PreviewCredentials{Username: preview.Username, Password: preview.Password.Reveal()}
	}
	return out, nil
}

// SetSiteMaintenance records whether the site is in maintenance mode and
// queues the job that applies it.
func (h *Handlers) SetSiteMaintenance(ctx context.Context, req gen.SetSiteMaintenanceRequestObject) (gen.SetSiteMaintenanceResponseObject, error) {
	p, err := h.authorize(ctx, authz.SetSiteMaintenance, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	site, job, err := h.sites.SetMaintenance(ctx, req.SiteID, deref(req.Body).Enabled, p.UserID)
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
	return gen.SetSiteMaintenance202JSONResponse{
		Body:    gen.SiteAccepted{Site: siteResponse(site, aliases), Job: j},
		Headers: gen.SetSiteMaintenance202ResponseHeaders{Location: &location},
	}, nil
}

// GoLiveSite checks the site's new domains, saves them, and queues the job
// that switches the site to its primary domain (#45).
func (h *Handlers) GoLiveSite(ctx context.Context, req gen.GoLiveSiteRequestObject) (gen.GoLiveSiteResponseObject, error) {
	p, err := h.authorize(ctx, authz.ManageSiteDomains, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	b := deref(req.Body)
	site, aliases, job, err := h.sites.GoLive(ctx, req.SiteID, sites.GoLive{
		PrimaryDomain:   b.PrimaryDomain,
		Aliases:         deref(b.Aliases),
		SkipDNSCheck:    deref(b.SkipDNSCheck),
		CreatedByUserID: p.UserID,
	})
	if err != nil {
		return nil, siteError(err)
	}
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.GoLiveSite202JSONResponse{
		Body:    gen.SiteAccepted{Site: siteResponse(site, aliases), Job: j},
		Headers: gen.GoLiveSite202ResponseHeaders{Location: &location},
	}, nil
}

// SetSiteAliases checks and saves a live site's new aliases and queues the
// job that routes them (#46).
func (h *Handlers) SetSiteAliases(ctx context.Context, req gen.SetSiteAliasesRequestObject) (gen.SetSiteAliasesResponseObject, error) {
	p, err := h.authorize(ctx, authz.ManageSiteDomains, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	b := deref(req.Body)
	site, aliases, job, err := h.sites.SetAliases(ctx, req.SiteID, sites.SetAliases{
		Aliases:         b.Aliases,
		SkipDNSCheck:    deref(b.SkipDNSCheck),
		CreatedByUserID: p.UserID,
	})
	if err != nil {
		return nil, siteError(err)
	}
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.SetSiteAliases202JSONResponse{
		Body:    gen.SiteAccepted{Site: siteResponse(site, aliases), Job: j},
		Headers: gen.SetSiteAliases202ResponseHeaders{Location: &location},
	}, nil
}

// DNSCheckProblemType is the problem type of a request whose domains
// failed the DNS check (package domains). It links to the documentation
// of the check.
const DNSCheckProblemType = "https://github.com/rchouinard/pressfleet/blob/main/docs/configuration.md#dns-check"

// dnsProblem is the 422 of a failed DNS check.
func dnsProblem(e *sites.DNSCheckError) *httpserver.Problem {
	p := httpserver.NewProblem(http.StatusUnprocessableEntity, e.Detail)
	p.Type, p.Title = DNSCheckProblemType, "DNS check failed"
	if p.Detail == "" {
		p.Detail = "Some domains don't resolve to this host yet. Point their DNS records here, " +
			"and send the request again once the change has propagated."
	}
	for _, f := range e.Fields {
		p.Errors = append(p.Errors, httpserver.FieldError{Field: f.Field, Detail: f.Detail})
	}
	return p
}

// RegenerateSitePreviewPassword replaces the site's preview password and
// returns the new one, once, with the job that applies it.
func (h *Handlers) RegenerateSitePreviewPassword(ctx context.Context, req gen.RegenerateSitePreviewPasswordRequestObject) (gen.RegenerateSitePreviewPasswordResponseObject, error) {
	p, err := h.authorize(ctx, authz.UpdateSite, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	creds, job, err := h.sites.RegeneratePreviewPassword(ctx, req.SiteID, p.UserID)
	if err != nil {
		return nil, siteError(err)
	}
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location, noStore := jobLocation(job), "no-store"
	return gen.RegenerateSitePreviewPassword202JSONResponse{
		Body: gen.PreviewPasswordAccepted{
			Credentials: gen.PreviewCredentials{Username: creds.Username, Password: creds.Password.Reveal()},
			Job:         j,
		},
		Headers: gen.RegenerateSitePreviewPassword202ResponseHeaders{Location: &location, CacheControl: &noStore},
	}, nil
}

// DeleteSite queues the site deletion job, or returns the one already
// queued. The action depends on the target: deleting a staging site needs
// authz.DeleteStagingSite (Operator), a production site authz.DeleteSite
// (Admin), and deleting the site's backups too authz.DeleteSiteBackups
// (Admin) for either. The audit event records the action checked, with the
// choice in its details.
func (h *Handlers) DeleteSite(ctx context.Context, req gen.DeleteSiteRequestObject) (gen.DeleteSiteResponseObject, error) {
	deleteBackups := req.Params.DeleteBackups != nil && *req.Params.DeleteBackups
	action := authz.DeleteSite
	switch {
	case deleteBackups:
		action = authz.DeleteSiteBackups
	default:
		// A site that doesn't exist is checked as a production site, so
		// callers who may only delete staging sites can't tell it from
		// one they may not delete.
		site, err := store.NewSites(h.db).Get(ctx, req.SiteID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, siteError(err)
		}
		if err == nil && site.IsStaging() {
			action = authz.DeleteStagingSite
		}
	}
	p, err := h.authorize(ctx, action, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	job, err := h.sites.Delete(ctx, req.SiteID, p.UserID, sites.DeleteOptions{DeleteBackups: deleteBackups})
	if err != nil {
		return nil, siteError(err)
	}
	setAuditDetails(ctx, map[string]string{"delete_backups": strconv.FormatBool(deleteBackups), "job_id": job.ID})
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.DeleteSite202JSONResponse{
		Body:    gen.JobAccepted{Job: j},
		Headers: gen.DeleteSite202ResponseHeaders{Location: &location},
	}, nil
}

// StartSite queues a job that starts a stopped site.
func (h *Handlers) StartSite(ctx context.Context, req gen.StartSiteRequestObject) (gen.StartSiteResponseObject, error) {
	p, err := h.authorize(ctx, authz.StartSite, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	job, err := h.sites.Start(ctx, req.SiteID, p.UserID)
	if err != nil {
		return nil, siteError(err)
	}
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.StartSite202JSONResponse{
		Body:    gen.JobAccepted{Job: j},
		Headers: gen.StartSite202ResponseHeaders{Location: &location},
	}, nil
}

// StopSite queues a job that stops a running site.
func (h *Handlers) StopSite(ctx context.Context, req gen.StopSiteRequestObject) (gen.StopSiteResponseObject, error) {
	p, err := h.authorize(ctx, authz.StopSite, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	job, err := h.sites.Stop(ctx, req.SiteID, p.UserID)
	if err != nil {
		return nil, siteError(err)
	}
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.StopSite202JSONResponse{
		Body:    gen.JobAccepted{Job: j},
		Headers: gen.StopSite202ResponseHeaders{Location: &location},
	}, nil
}

// PurgeSiteCache queues a job that purges the site's object cache and page
// cache (#41).
func (h *Handlers) PurgeSiteCache(ctx context.Context, req gen.PurgeSiteCacheRequestObject) (gen.PurgeSiteCacheResponseObject, error) {
	p, err := h.authorize(ctx, authz.PurgeSiteCache, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	job, err := h.sites.PurgeCache(ctx, req.SiteID, p.UserID)
	if err != nil {
		return nil, siteError(err)
	}
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.PurgeSiteCache202JSONResponse{
		Body:    gen.JobAccepted{Job: j},
		Headers: gen.PurgeSiteCache202ResponseHeaders{Location: &location},
	}, nil
}

// RetrieveSiteAdminCredentials returns the initial WordPress admin
// credentials exactly once.
func (h *Handlers) RetrieveSiteAdminCredentials(ctx context.Context, req gen.RetrieveSiteAdminCredentialsRequestObject) (gen.RetrieveSiteAdminCredentialsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.RetrieveSiteCredentials, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	creds, err := h.sites.TakeAdminCredentials(ctx, req.SiteID)
	if err != nil {
		return nil, siteError(err)
	}
	noStore := "no-store"
	return gen.RetrieveSiteAdminCredentials200JSONResponse{
		Body: gen.SiteAdminCredentials{
			Username: creds.Username,
			Email:    gen.Email(creds.Email),
			Password: creds.Password.Reveal(),
			LoginURL: creds.LoginURL,
		},
		Headers: gen.RetrieveSiteAdminCredentials200ResponseHeaders{CacheControl: &noStore},
	}, nil
}

// siteError maps site workflow errors to API errors.
func siteError(err error) error {
	var ve *sites.ValidationError
	if errors.As(err, &ve) {
		out := &ValidationError{}
		for _, f := range ve.Fields {
			out.Fields = append(out.Fields, httpserver.FieldError{Field: f.Field, Detail: f.Detail})
		}
		return out
	}
	var dnsErr *sites.DNSCheckError
	if errors.As(err, &dnsErr) {
		return dnsProblem(dnsErr)
	}
	for _, m := range []struct{ from, to error }{
		{sites.ErrNotFound, ErrNotFound},
		{sites.ErrNoPreviewDomain, ErrConflict},
		{sites.ErrNotReady, ErrConflict},
		{sites.ErrCredentialsGone, ErrGone},
		{sites.ErrNotProvisioned, ErrConflict},
		{sites.ErrNotRunning, ErrConflict},
		{sites.ErrNotLive, ErrConflict},
		{sites.ErrUploadNotReady, ErrConflict},
		{sites.ErrSourceFailed, ErrConflict},
	} {
		if errors.Is(err, m.from) {
			return fmt.Errorf("%w: %w", m.to, m.from)
		}
	}
	// The wrapping says what the site is doing.
	if errors.Is(err, sites.ErrBusy) || errors.Is(err, sites.ErrHasStagingSites) {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return err
}

func jobLocation(j store.Job) string { return "/api/v1/jobs/" + j.ID }

// optTime is nil for the zero time, for nullable timestamps.
func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return timePtr(t)
}

// optString is nil for the empty string, for nullable strings.
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func siteResponse(s store.Site, aliases []string) gen.Site {
	if aliases == nil {
		aliases = []string{}
	}
	url := sites.URL(s)
	out := gen.Site{
		ID:                        s.ID,
		ClientGroupID:             s.ClientGroupID,
		Name:                      s.Name,
		Slug:                      s.Slug,
		PreviewDomain:             s.PreviewDomain,
		Aliases:                   aliases,
		PhpVersion:                s.PHPVersion,
		WpCoreVersion:             s.WPCoreVersion,
		Resources:                 gen.ResourceLimits{CPU: s.CPULimit, MemoryReservationMb: s.MemoryReservationMB, MemoryLimitMb: s.MemoryLimitMB},
		Status:                    gen.SiteStatus(s.Status),
		URL:                       &url,
		AdminCredentialsAvailable: s.AdminPasswordSecretRef != "",
		PreviewProtection:         gen.PreviewProtection{Enabled: s.PreviewProtection},
		Maintenance:               s.Maintenance,
		CreatedAt:                 s.CreatedAt,
		UpdatedAt:                 s.UpdatedAt,
	}
	if s.PreviewUsername != "" {
		out.PreviewProtection.Username = &s.PreviewUsername
	}
	if s.PrimaryDomain != "" {
		out.PrimaryDomain = &s.PrimaryDomain
	}
	if s.StatusMessage != "" {
		out.StatusMessage = &s.StatusMessage
	}
	out.Environment = gen.Production
	if s.IsStaging() {
		out.Environment = gen.Staging
		out.ParentSiteID = &s.ParentSiteID
		out.Staging = &gen.StagingState{
			ProductionSiteID: s.ParentSiteID,
			LastRefreshedAt:  optTime(s.StagingRefreshedAt),
			LastSnapshotID:   optString(s.StagingSnapshotID),
			LastPushedAt:     optTime(s.StagingPushedAt),
			OutboundMail:     !s.MailDisabled,
		}
	}
	return out
}
