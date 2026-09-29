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
//	  backups/
//	    repositories/<client-group-id>/
//	                      the group's Restic repository (local backend only)
//	  uploads/<upload-id> files sent through the upload API (never backed up)
//	  tmp/               scratch space, e.g. database dumps during backups
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

// UploadsDir holds the files sent through the resumable upload API, one
// per upload (package uploads). Nothing backs it up.
func (l Layout) UploadsDir() string { return filepath.Join(l.Root, "uploads") }

// TmpDir is scratch space that may be cleared on startup.
func (l Layout) TmpDir() string { return filepath.Join(l.Root, "tmp") }

// SiteDir returns the directory for one site's rendered files. It rejects
// IDs that would escape the sites directory.
func (l Layout) SiteDir(siteID string) (string, error) {
	if !localName(siteID) {
		return "", fmt.Errorf("invalid site ID %q", siteID)
	}
	return filepath.Join(l.SitesDir(), siteID), nil
}

// BackupRepositoriesDir holds the client groups' Restic repositories when
// backups are kept locally (backup.backend local).
func (l Layout) BackupRepositoriesDir() string {
	return filepath.Join(l.Root, "backups", "repositories")
}

// BackupRepositoryDir returns the directory of one client group's local
// Restic repository. It rejects IDs that would escape the repositories
// directory.
func (l Layout) BackupRepositoryDir(clientGroupID string) (string, error) {
	if !localName(clientGroupID) {
		return "", fmt.Errorf("invalid client group ID %q", clientGroupID)
	}
	return filepath.Join(l.BackupRepositoriesDir(), clientGroupID), nil
}

// localName reports whether name is a single path element that stays in
// its directory.
func localName(name string) bool {
	return name != "." && filepath.Base(name) == name && filepath.IsLocal(name)
}

// Ensure creates the top-level directories, readable only by the owner
// because they hold rendered credentials.
func (l Layout) Ensure() error {
	for _, dir := range []string{l.Root, l.StateDir(), l.PlatformDir(), l.SitesDir(), l.UploadsDir(), l.TmpDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating data directory: %w", err)
		}
	}
	return nil
}
