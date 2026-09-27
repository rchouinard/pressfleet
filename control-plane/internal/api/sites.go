package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Sites: provisioning (#27), management (#28), and deletion (#29).

// errNoSites is a wiring bug: serve always passes the site service.
var errNoSites = errors.New("api: no site service configured")

// ListSites lists sites, newest first, optionally filtered by client group
// or status. The cursor is the ID of the last site on the previous page.
func (h *Handlers) ListSites(ctx context.Context, req gen.ListSitesRequestObject) (gen.ListSitesResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.ClientGroup(deref(req.Params.ClientGroupID))); err != nil {
		return nil, err
	}
	if h.sites == nil {
		return nil, errNoSites
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
	page, aliases, err := h.sites.List(ctx, f)
	if err != nil {
		return nil, err
	}
	out := gen.ListSites200JSONResponse{Items: make([]gen.Site, len(page.Sites))}
	for i, s := range page.Sites {
		out.Items[i] = siteResponse(s, aliases[s.ID])
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
	if h.sites == nil {
		return nil, errNoSites
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

// GetSite returns a site with its recorded status and its live runtime
// state. The live state is best effort: if the container runtime can't be
// reached in time, the phase is unknown and the request still succeeds.
func (h *Handlers) GetSite(ctx context.Context, req gen.GetSiteRequestObject) (gen.GetSiteResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	if h.sites == nil {
		return nil, errNoSites
	}
	site, aliases, err := h.sites.Get(ctx, req.SiteID)
	if err != nil {
		return nil, siteError(err)
	}
	rt := h.sites.Runtime(ctx, site.ID)
	out := siteResponse(site, aliases)
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
	if h.sites == nil {
		return nil, errNoSites
	}
	b := deref(req.Body)
	in := sites.SiteUpdate{PHPVersion: b.PhpVersion, CreatedByUserID: p.UserID}
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
	if h.sites == nil {
		return nil, errNoSites
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

// RegenerateSitePreviewPassword replaces the site's preview password and
// returns the new one, once, with the job that applies it.
func (h *Handlers) RegenerateSitePreviewPassword(ctx context.Context, req gen.RegenerateSitePreviewPasswordRequestObject) (gen.RegenerateSitePreviewPasswordResponseObject, error) {
	p, err := h.authorize(ctx, authz.UpdateSite, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	if h.sites == nil {
		return nil, errNoSites
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
// queued.
func (h *Handlers) DeleteSite(ctx context.Context, req gen.DeleteSiteRequestObject) (gen.DeleteSiteResponseObject, error) {
	p, err := h.authorize(ctx, authz.DeleteSite, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	if h.sites == nil {
		return nil, errNoSites
	}
	job, err := h.sites.Delete(ctx, req.SiteID, p.UserID)
	if err != nil {
		return nil, siteError(err)
	}
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
	if h.sites == nil {
		return nil, errNoSites
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
	if h.sites == nil {
		return nil, errNoSites
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
	if h.sites == nil {
		return nil, errNoSites
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
	if h.sites == nil {
		return nil, errNoSites
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
	for _, m := range []struct{ from, to error }{
		{sites.ErrNotFound, ErrNotFound},
		{sites.ErrNoPreviewDomain, ErrConflict},
		{sites.ErrNotReady, ErrConflict},
		{sites.ErrCredentialsGone, ErrGone},
		{sites.ErrNotProvisioned, ErrConflict},
	} {
		if errors.Is(err, m.from) {
			return fmt.Errorf("%w: %w", m.to, m.from)
		}
	}
	// The wrapping says what the site is doing.
	if errors.Is(err, sites.ErrBusy) {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return err
}

func jobLocation(j store.Job) string { return "/api/v1/jobs/" + j.ID }

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
		WpCorePinned:              s.WPCorePinned,
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
	return out
}
