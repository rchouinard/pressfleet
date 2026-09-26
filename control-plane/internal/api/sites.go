package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Sites: provisioning (#27), management (#28), and deletion (#29).

// errNoSites is a wiring bug: serve always passes the site service.
var errNoSites = errors.New("api: no site service configured")

// ListSites lists sites, optionally filtered by client group or status.
func (h *Handlers) ListSites(ctx context.Context, req gen.ListSitesRequestObject) (gen.ListSitesResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.ClientGroup(deref(req.Params.ClientGroupID))); err != nil {
		return nil, err
	}
	return nil, notImplemented(28)
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

// GetSite returns a site. Its status is the recorded lifecycle status; live
// runtime status comes with #28.
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
	return gen.GetSite200JSONResponse(siteResponse(site, aliases)), nil
}

// UpdateSite queues a job that applies a new PHP version or resource limits.
func (h *Handlers) UpdateSite(ctx context.Context, req gen.UpdateSiteRequestObject) (gen.UpdateSiteResponseObject, error) {
	if _, err := h.authorize(ctx, authz.UpdateSite, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	return nil, notImplemented(28)
}

// DeleteSite queues the site deletion job.
func (h *Handlers) DeleteSite(ctx context.Context, req gen.DeleteSiteRequestObject) (gen.DeleteSiteResponseObject, error) {
	if _, err := h.authorize(ctx, authz.DeleteSite, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	return nil, notImplemented(29)
}

// StartSite queues a job that starts a stopped site.
func (h *Handlers) StartSite(ctx context.Context, req gen.StartSiteRequestObject) (gen.StartSiteResponseObject, error) {
	if _, err := h.authorize(ctx, authz.StartSite, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	return nil, notImplemented(28)
}

// StopSite queues a job that stops a running site.
func (h *Handlers) StopSite(ctx context.Context, req gen.StopSiteRequestObject) (gen.StopSiteResponseObject, error) {
	if _, err := h.authorize(ctx, authz.StopSite, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	return nil, notImplemented(28)
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
	} {
		if errors.Is(err, m.from) {
			return fmt.Errorf("%w: %w", m.to, m.from)
		}
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
		CreatedAt:                 s.CreatedAt,
		UpdatedAt:                 s.UpdatedAt,
	}
	if s.PrimaryDomain != "" {
		out.PrimaryDomain = &s.PrimaryDomain
	}
	if s.StatusMessage != "" {
		out.StatusMessage = &s.StatusMessage
	}
	return out
}
