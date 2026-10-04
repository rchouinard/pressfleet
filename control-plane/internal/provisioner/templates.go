package provisioner

import (
	"context"
	"errors"
)

// ErrNoRenderSnapshot is returned by [RenderSnapshotter.RestoreRender] when
// the site has no snapshot to restore.
var ErrNoRenderSnapshot = errors.New("provisioner: no snapshot of the site's rendered definition")

// TemplateFingerprinter identifies how a driver renders sites (§4.7, §5.5).
// A platform upgrade can change the templates and code that turn a
// SiteConfig into a site's runtime definition (in Compose, the site's
// project files). Re-rendering every site at once, as the startup
// reconciliation would, could break all of them together, so the control
// plane records the fingerprint each site was rendered with and moves the
// sites whose fingerprint differs to the current one with the templates
// rollout, one site at a time with a health check. The rollout also needs
// [RenderSnapshotter], to undo a failed re-render.
//
// It is its own interface, outside [Driver], because only that rollout
// needs it.
type TemplateFingerprinter interface {
	// TemplateFingerprint identifies the driver's site templates and
	// rendering logic. It is the same for every site and doesn't change
	// while the control plane runs; a release that renders sites
	// differently changes it.
	TemplateFingerprint() string
}

// RenderSnapshotter keeps a copy of a site's rendered runtime definition
// and puts it back, which undoes a re-render with new templates: the
// templates the site was rendered with before are not in the new release,
// so the previous definition can't be rendered again, only restored.
type RenderSnapshotter interface {
	// SnapshotRender copies the site's current rendered definition aside,
	// replacing an earlier copy. Returns ErrSiteNotFound if the site was
	// never rendered.
	SnapshotRender(ctx context.Context, siteID string) error
	// RestoreRender puts the copy SnapshotRender made back and converges
	// the site's containers on it, as UpdateSite does: a stopped site
	// stays stopped, otherwise the containers are started and waited for.
	// It keeps the copy, so restoring again is safe. Returns
	// ErrNoRenderSnapshot if there is no copy and ErrSiteNotFound if the
	// site has no containers.
	RestoreRender(ctx context.Context, siteID string) (*SiteStatus, error)
}
