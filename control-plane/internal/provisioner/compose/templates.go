package compose

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/templates"
)

// Template fingerprints and render snapshots (#95, design doc §4.7, §5.5):
// what the templates rollout needs to move sites to a release's templates
// one at a time, and to undo a re-render that left a site unhealthy.

var (
	_ provisioner.TemplateFingerprinter = (*Provisioner)(nil)
	_ provisioner.RenderSnapshotter     = (*Provisioner)(nil)
)

// renderVersion is the part of the template fingerprint that the embedded
// templates don't cover: the Go code that renders a site's files.
//
// Bump it in any change that alters what RenderSite (or the code it calls:
// wpconfig.Render, the routing labels, the maintenance and domain routes,
// the resource split) writes for an existing site with the same SiteConfig
// and install-wide configuration, so that the change reaches sites through
// the templates rollout, one site at a time with a health check, instead of
// all at once through the startup reconciliation. Changes to
// templates/compose/*.tmpl change the fingerprint by themselves, and
// changes that render nothing differently (refactoring, new SiteConfig
// fields that default to the old output) need no bump.
const renderVersion = 1

// templateFingerprint hashes the embedded site templates with
// renderVersion. The embedded files can't change while the process runs, so
// it is computed once.
var templateFingerprint = sync.OnceValue(func() string {
	fp, err := fingerprintTemplates(templates.Compose(), renderVersion)
	if err != nil {
		// The templates are embedded in the binary: failing to read them
		// is a build defect.
		panic(err)
	}
	return fp
})

// fingerprintTemplates hashes every file of fsys, by path and content, with
// version: r<version>-<16 hex digits>.
func fingerprintTemplates(fsys fs.FS, version int) (string, error) {
	h := sha256.New()
	// WalkDir visits files in lexical order, so the hash is stable.
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		// Length-prefixed, so no two different trees hash the same input.
		_, _ = fmt.Fprintf(h, "%d:%s%d:", len(path), path, len(content))
		_, _ = h.Write(content)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("fingerprinting site templates: %w", err)
	}
	return "r" + strconv.Itoa(version) + "-" + fmt.Sprintf("%x", h.Sum(nil)[:8]), nil
}

// TemplateFingerprint implements provisioner.TemplateFingerprinter: a hash
// of templates/compose and renderVersion.
func (p *Provisioner) TemplateFingerprint() string { return templateFingerprint() }

// SiteSnapshotDir is the directory in a site's directory where
// SnapshotRender keeps the copy of its rendered files.
const SiteSnapshotDir = ".previous"

// renderedFiles are the files RenderSite writes, in the order it writes
// them: wp-config.php before compose.yaml, which refers to it.
var renderedFiles = []string{SiteWPConfigFile, SiteComposeFile, SiteMaintenanceFile, SiteDomainsFile}

// SnapshotRender implements provisioner.RenderSnapshotter: it copies the
// site's rendered files into SiteSnapshotDir, with their modes and, when
// the control plane runs as root, owners. A file the site doesn't have
// (custom domain routes before go-live) is absent from the copy too.
func (p *Provisioner) SnapshotRender(_ context.Context, siteID string) error {
	dir, err := p.renderedDir(siteID)
	if err != nil {
		return fmt.Errorf("snapshot site render: %w", err)
	}
	if _, err := os.Stat(filepath.Join(dir, SiteComposeFile)); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("snapshot site render of %s: %w", siteID, provisioner.ErrSiteNotFound)
	}
	snap := filepath.Join(dir, SiteSnapshotDir)
	if err := os.MkdirAll(snap, 0o700); err != nil {
		return fmt.Errorf("snapshot site render of %s: %w", siteID, err)
	}
	if _, err := copyRendered(dir, snap); err != nil {
		return fmt.Errorf("snapshot site render of %s: %w", siteID, err)
	}
	return nil
}

// RestoreRender implements provisioner.RenderSnapshotter: it copies the
// snapshot's files back over the rendered ones and applies them as
// UpdateSite applies a render, recreating php if wp-config.php changed. The
// site's routes in Traefik's directory follow: its custom domain routes
// are installed or removed as the snapshot has them, and its maintenance
// routes, if the site is in maintenance mode, are refreshed.
func (p *Provisioner) RestoreRender(ctx context.Context, siteID string) (*provisioner.SiteStatus, error) {
	dir, err := p.renderedDir(siteID)
	if err != nil {
		return nil, fmt.Errorf("restore site render: %w", err)
	}
	snap := filepath.Join(dir, SiteSnapshotDir)
	if _, err := os.Stat(filepath.Join(snap, SiteComposeFile)); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("restore site render of %s: %w", siteID, provisioner.ErrNoRenderSnapshot)
	}
	proj := p.project(siteID)
	containers, err := proj.Ps(ctx)
	if err != nil {
		return nil, fmt.Errorf("restore site render of %s: %w", siteID, err)
	}
	if len(containers) == 0 {
		return nil, fmt.Errorf("restore site render of %s: %w", siteID, provisioner.ErrSiteNotFound)
	}
	phase, _ := sitePhase(containers)
	wpConfigChanged, err := copyRendered(snap, dir)
	if err != nil {
		return nil, fmt.Errorf("restore site render of %s: %w", siteID, err)
	}
	if err := p.bringUp(ctx, siteID, proj, dir, wpConfigChanged, phase != provisioner.SiteStopped); err != nil {
		return nil, fmt.Errorf("restore site render: %w", err)
	}
	_, statErr := os.Stat(filepath.Join(dir, SiteDomainsFile))
	if _, err := p.applyDomains(siteID, statErr == nil); err != nil {
		return nil, fmt.Errorf("restore site render of %s: %w", siteID, err)
	}
	if p.routesDir != "" {
		if _, err := os.Stat(filepath.Join(p.routesDir, MaintenanceRoutesFile(siteID))); err == nil {
			if _, err := p.applyMaintenance(siteID, true); err != nil {
				return nil, fmt.Errorf("restore site render of %s: %w", siteID, err)
			}
		}
	}
	p.log.InfoContext(ctx, "site render restored", "site_id", siteID)
	return p.GetSite(ctx, siteID)
}

// renderedDir returns the directory a site is rendered into.
func (p *Provisioner) renderedDir(siteID string) (string, error) {
	if err := validSiteID(siteID); err != nil {
		return "", err
	}
	dir, err := p.layout.SiteDir(siteID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	return dir, nil
}

// copyRendered makes the rendered files in to match those in from: each is
// replaced with from's content, mode, and (as root) owner, or removed if
// from has none. Unchanged files are left alone, so copying again is a
// no-op. It reports whether wp-config.php changed, which the running php
// container doesn't see by itself (RenderResult).
func copyRendered(from, to string) (wpConfigChanged bool, err error) {
	for _, name := range renderedFiles {
		src, dst := filepath.Join(from, name), filepath.Join(to, name)
		fi, err := os.Stat(src)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return false, err
			}
			continue
		}
		if err != nil {
			return false, err
		}
		content, err := os.ReadFile(src) //nolint:gosec // G304: a rendered file in the site's directory
		if err != nil {
			return false, err
		}
		owner := -1
		if os.Geteuid() == 0 {
			owner = fileOwner(fi)
		}
		changed, err := replaceFile(dst, content, fi.Mode().Perm(), owner)
		if err != nil {
			return false, err
		}
		if name == SiteWPConfigFile {
			wpConfigChanged = changed
		}
	}
	return wpConfigChanged, nil
}
