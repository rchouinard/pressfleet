package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
)

// Sites: provisioning (#27), management (#28), and deletion (#29).

// ListSites lists sites, optionally filtered by client group or status.
func (h *Handlers) ListSites(context.Context, gen.ListSitesRequestObject) (gen.ListSitesResponseObject, error) {
	return nil, notImplemented(28)
}

// CreateSite starts the provisioning workflow and returns 202 with the job.
func (h *Handlers) CreateSite(context.Context, gen.CreateSiteRequestObject) (gen.CreateSiteResponseObject, error) {
	return nil, notImplemented(27)
}

// GetSite returns a site with its live runtime status.
func (h *Handlers) GetSite(context.Context, gen.GetSiteRequestObject) (gen.GetSiteResponseObject, error) {
	return nil, notImplemented(28)
}

// UpdateSite queues a job that applies a new PHP version or resource limits.
func (h *Handlers) UpdateSite(context.Context, gen.UpdateSiteRequestObject) (gen.UpdateSiteResponseObject, error) {
	return nil, notImplemented(28)
}

// DeleteSite queues the site deletion job.
func (h *Handlers) DeleteSite(context.Context, gen.DeleteSiteRequestObject) (gen.DeleteSiteResponseObject, error) {
	return nil, notImplemented(29)
}

// StartSite queues a job that starts a stopped site.
func (h *Handlers) StartSite(context.Context, gen.StartSiteRequestObject) (gen.StartSiteResponseObject, error) {
	return nil, notImplemented(28)
}

// StopSite queues a job that stops a running site.
func (h *Handlers) StopSite(context.Context, gen.StopSiteRequestObject) (gen.StopSiteResponseObject, error) {
	return nil, notImplemented(28)
}

// RetrieveSiteAdminCredentials returns the initial WordPress admin
// credentials exactly once.
func (h *Handlers) RetrieveSiteAdminCredentials(context.Context, gen.RetrieveSiteAdminCredentialsRequestObject) (gen.RetrieveSiteAdminCredentialsResponseObject, error) {
	return nil, notImplemented(27)
}
