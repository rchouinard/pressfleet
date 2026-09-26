package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Layout describes the data directory:
//
//	<root>/
//	  state/              control plane database (SQLite)
//	    backups/          pre-migration and pre-restore database backups
//	  platform/           rendered platform services Compose project
//	  sites/<site-id>/    rendered per-site Compose project and wp-config.php
//	  tmp/                scratch space, e.g. database dumps during backups
type Layout struct {
	Root string
}

// StateDir holds the control plane database.
func (l Layout) StateDir() string { return filepath.Join(l.Root, "state") }

// StateBackupsDir holds state database backups taken before migrations and
// restores.
func (l Layout) StateBackupsDir() string { return filepath.Join(l.StateDir(), "backups") }

// PlatformDir holds the rendered platform services Compose project.
func (l Layout) PlatformDir() string { return filepath.Join(l.Root, "platform") }

// SitesDir holds one directory per site.
func (l Layout) SitesDir() string { return filepath.Join(l.Root, "sites") }

// TmpDir is scratch space that may be cleared on startup.
func (l Layout) TmpDir() string { return filepath.Join(l.Root, "tmp") }

// SiteDir returns the directory for one site's rendered files. It rejects
// IDs that would escape the sites directory.
func (l Layout) SiteDir(siteID string) (string, error) {
	if siteID == "." || filepath.Base(siteID) != siteID || !filepath.IsLocal(siteID) {
		return "", fmt.Errorf("invalid site ID %q", siteID)
	}
	return filepath.Join(l.SitesDir(), siteID), nil
}

// Ensure creates the top-level directories, readable only by the owner
// because they hold rendered credentials.
func (l Layout) Ensure() error {
	for _, dir := range []string{l.Root, l.StateDir(), l.PlatformDir(), l.SitesDir(), l.TmpDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating data directory: %w", err)
		}
	}
	return nil
}
