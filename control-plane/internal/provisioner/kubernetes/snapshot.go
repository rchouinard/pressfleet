package kubernetes

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Render snapshots (§4.7, §5.5): the templates rollout keeps a site's
// previous manifests while it moves the site to a release's templates, and
// puts them back if the site doesn't come up healthy.

// SnapshotDir is the directory in a site's manifest directory where
// SnapshotRender keeps the copy of its manifests.
const SnapshotDir = ".previous"

// manifestDir returns the directory a site's manifests are rendered into.
func (p *Provisioner) manifestDir(siteID string) (string, error) {
	if err := p.validSiteNamespace(siteID); err != nil {
		return "", err
	}
	dir, err := p.layout.SiteDir(siteID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	return filepath.Join(dir, SiteManifestDir), nil
}

// SnapshotRender implements provisioner.RenderSnapshotter: it copies the
// site's manifests into SnapshotDir, replacing an earlier copy.
func (p *Provisioner) SnapshotRender(_ context.Context, siteID string) error {
	dir, err := p.manifestDir(siteID)
	if err != nil {
		return fmt.Errorf("snapshot site render: %w", err)
	}
	files, err := readManifests(dir)
	if err != nil {
		return fmt.Errorf("snapshot site render of %s: %w", siteID, err)
	}
	if len(files) == 0 {
		return fmt.Errorf("snapshot site render of %s: %w", siteID, provisioner.ErrSiteNotFound)
	}
	snap := filepath.Join(dir, SnapshotDir)
	if err := os.MkdirAll(snap, 0o700); err != nil {
		return fmt.Errorf("snapshot site render of %s: %w", siteID, err)
	}
	if err := syncManifests(snap, files); err != nil {
		return fmt.Errorf("snapshot site render of %s: %w", siteID, err)
	}
	return nil
}

// RestoreRender implements provisioner.RenderSnapshotter: it puts the
// snapshot's manifests back and applies them as UpdateSite applies a
// render, deleting the objects only the newer manifests had. A stopped site
// stays stopped; a running one is waited for. The snapshot is kept.
func (p *Provisioner) RestoreRender(ctx context.Context, siteID string) (*provisioner.SiteStatus, error) {
	dir, err := p.manifestDir(siteID)
	if err != nil {
		return nil, fmt.Errorf("restore site render: %w", err)
	}
	files, err := readManifests(filepath.Join(dir, SnapshotDir))
	if err != nil {
		return nil, fmt.Errorf("restore site render of %s: %w", siteID, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("restore site render of %s: %w", siteID, provisioner.ErrNoRenderSnapshot)
	}
	d, err := p.deployment(ctx, siteID)
	if err != nil {
		return nil, fmt.Errorf("restore site render: %w", err)
	}
	if err := syncManifests(dir, files); err != nil {
		return nil, fmt.Errorf("restore site render of %s: %w", siteID, err)
	}
	if err := p.applySite(ctx, siteID, dir); err != nil {
		return nil, fmt.Errorf("restore site render of %s: %w", siteID, err)
	}
	if replicas(d) > 0 {
		if err := p.waitForSite(ctx, siteID); err != nil {
			return nil, fmt.Errorf("restore site render of %s: %w", siteID, err)
		}
	}
	p.log.InfoContext(ctx, "site render restored", "site_id", siteID)
	return p.GetSite(ctx, siteID)
}
