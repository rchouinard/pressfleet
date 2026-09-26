package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
)

// Sites: provisioning (#27), management (#28), and deletion (#29).

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
	if _, err := h.authorize(ctx, authz.CreateSite, authz.ClientGroup(deref(req.Body).ClientGroupID)); err != nil {
		return nil, err
	}
	return nil, notImplemented(27)
}

// GetSite returns a site with its live runtime status.
func (h *Handlers) GetSite(ctx context.Context, req gen.GetSiteRequestObject) (gen.GetSiteResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	return nil, notImplemented(28)
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
	return nil, notImplemented(27)
}
