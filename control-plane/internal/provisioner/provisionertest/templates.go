package provisionertest

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// Templates is a [Fake] that also implements
// [provisioner.TemplateFingerprinter] and [provisioner.RenderSnapshotter].
// It is separate from Fake because those interfaces are not part of
// provisioner.Driver, and a service only runs the templates rollout with a
// driver that has them.
//
// Every CreateSite and UpdateSite renders the site with the current
// fingerprint (Rendered says which one it was rendered with), so tests can
// simulate a release that renders sites differently (SetFingerprint) and
// tell its renders apart, e.g. to make them fail a health check. A
// snapshot keeps the site's config and fingerprint; restoring puts both
// back. It is safe for concurrent use.
type Templates struct {
	*Fake

	fingerprint string
	rendered    map[string]string
	snapshots   map[string]templateSnapshot
}

type templateSnapshot struct {
	config      provisioner.SiteConfig
	fingerprint string
}

var (
	_ provisioner.Driver                = (*Templates)(nil)
	_ provisioner.TemplateFingerprinter = (*Templates)(nil)
	_ provisioner.RenderSnapshotter     = (*Templates)(nil)
)

// NewTemplates returns a Templates over f that renders with fingerprint.
func NewTemplates(f *Fake, fingerprint string) *Templates {
	return &Templates{Fake: f, fingerprint: fingerprint, rendered: map[string]string{}, snapshots: map[string]templateSnapshot{}}
}

// SetFingerprint changes the fingerprint, as an upgrade to a release with
// other templates would.
func (t *Templates) SetFingerprint(fingerprint string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fingerprint = fingerprint
}

// Rendered returns the fingerprint the site was last rendered (or
// restored) with; empty if never.
func (t *Templates) Rendered(siteID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rendered[siteID]
}

// TemplateFingerprint implements provisioner.TemplateFingerprinter.
func (t *Templates) TemplateFingerprint() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fingerprint
}

// CreateSite implements provisioner.Provisioner.
func (t *Templates) CreateSite(ctx context.Context, cfg *provisioner.SiteConfig) (*provisioner.SiteStatus, error) {
	st, err := t.Fake.CreateSite(ctx, cfg)
	if err == nil {
		t.render(cfg.ID)
	}
	return st, err
}

// UpdateSite implements provisioner.Provisioner.
func (t *Templates) UpdateSite(ctx context.Context, cfg *provisioner.SiteConfig) (*provisioner.SiteStatus, error) {
	st, err := t.Fake.UpdateSite(ctx, cfg)
	if err == nil {
		t.render(cfg.ID)
	}
	return st, err
}

func (t *Templates) render(siteID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rendered[siteID] = t.fingerprint
}

// DeleteSite implements provisioner.Provisioner; the snapshot goes too.
func (t *Templates) DeleteSite(ctx context.Context, siteID string) error {
	if err := t.Fake.DeleteSite(ctx, siteID); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.rendered, siteID)
	delete(t.snapshots, siteID)
	return nil
}

// SnapshotRender implements provisioner.RenderSnapshotter.
func (t *Templates) SnapshotRender(_ context.Context, siteID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.begin("SnapshotRender", siteID, nil); err != nil {
		return err
	}
	s, err := t.lookup(siteID)
	if err != nil {
		return err
	}
	t.snapshots[siteID] = templateSnapshot{config: s.Config, fingerprint: t.rendered[siteID]}
	return nil
}

// RestoreRender implements provisioner.RenderSnapshotter. Like the Compose
// driver, a stopped site stays stopped, and anything else ends up running.
func (t *Templates) RestoreRender(_ context.Context, siteID string) (*provisioner.SiteStatus, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.begin("RestoreRender", siteID, nil); err != nil {
		return nil, err
	}
	snap, ok := t.snapshots[siteID]
	if !ok {
		return nil, provisioner.ErrNoRenderSnapshot
	}
	s, err := t.lookup(siteID)
	if err != nil {
		return nil, err
	}
	s.Config = snap.config
	s.Maintenance = snap.config.Maintenance
	if s.Phase != provisioner.SiteStopped {
		s.Phase = provisioner.SiteRunning
	}
	t.rendered[siteID] = snap.fingerprint
	return status(s), nil
}
