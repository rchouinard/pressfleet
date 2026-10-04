package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/pluginupdates"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Pending plugin and theme updates (#125, design doc §5.6, §9 Q2). The
// platform only reports them; nothing here runs an update.

// errNoUpdates is a wiring bug: the handlers make the service from the
// database.
var errNoUpdates = errors.New("api: no update report service configured")

// GetSiteUpdates returns the site's pending plugin and theme updates.
func (h *Handlers) GetSiteUpdates(ctx context.Context, req gen.GetSiteUpdatesRequestObject) (gen.GetSiteUpdatesResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	if h.updates == nil {
		return nil, errNoUpdates
	}
	rep, err := h.updates.Report(ctx, req.SiteID)
	if err != nil {
		return nil, updatesError(err)
	}
	out := gen.GetSiteUpdates200JSONResponse{Plugins: []gen.PendingUpdate{}, Themes: []gen.PendingUpdate{}}
	if !rep.CheckedAt.IsZero() {
		out.CheckedAt = &rep.CheckedAt
	}
	for _, u := range rep.Updates {
		item := gen.PendingUpdate{Name: u.Name, Title: u.Title, Version: u.Version, UpdateVersion: u.UpdateVersion}
		if u.Kind == store.UpdateKindTheme {
			out.Themes = append(out.Themes, item)
		} else {
			out.Plugins = append(out.Plugins, item)
		}
	}
	return out, nil
}

// CheckSiteUpdates queues a check of the site for updates now. Who may
// change the site may ask for it; the check itself only reads.
func (h *Handlers) CheckSiteUpdates(ctx context.Context, req gen.CheckSiteUpdatesRequestObject) (gen.CheckSiteUpdatesResponseObject, error) {
	p, err := h.authorize(ctx, authz.UpdateSite, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	if h.updates == nil {
		return nil, errNoUpdates
	}
	job, err := h.updates.Check(ctx, req.SiteID, p.UserID)
	if err != nil {
		return nil, updatesError(err)
	}
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.CheckSiteUpdates202JSONResponse{
		Body:    gen.JobAccepted{Job: j},
		Headers: gen.CheckSiteUpdates202ResponseHeaders{Location: &location},
	}, nil
}

// updateCounts returns every checked site's pending update counts, for
// the site list and detail.
func (h *Handlers) updateCounts(ctx context.Context) (map[string]store.UpdateCounts, error) {
	if h.updates == nil {
		return nil, nil
	}
	return h.updates.Counts(ctx)
}

// pendingUpdateCounts returns the site's counts for the API, or nil if it
// hasn't been checked.
func pendingUpdateCounts(counts map[string]store.UpdateCounts, siteID string) *gen.PendingUpdateCounts {
	c, ok := counts[siteID]
	if !ok {
		return nil
	}
	return &gen.PendingUpdateCounts{Plugins: c.Plugins, Themes: c.Themes, CheckedAt: c.CheckedAt}
}

func updatesError(err error) error {
	if errors.Is(err, pluginupdates.ErrSiteNotFound) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}
