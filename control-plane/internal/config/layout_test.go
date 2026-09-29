package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestLayoutEnsure(t *testing.T) {
	l := config.Layout{Root: filepath.Join(t.TempDir(), "data")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{l.Root, l.StateDir(), l.PlatformDir(), l.SitesDir(), l.UploadsDir(), l.TmpDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %o, want 700", dir, perm)
		}
	}
	// Idempotent.
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
}

func TestLayoutSiteDir(t *testing.T) {
	l := config.Layout{Root: "/data"}
	got, err := l.SiteDir("site1")
	if err != nil || got != "/data/sites/site1" {
		t.Errorf("SiteDir(site1) = %q, %v", got, err)
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", "/abs"} {
		if _, err := l.SiteDir(bad); err == nil {
			t.Errorf("SiteDir(%q) accepted", bad)
		}
	}
}

func TestLayoutBackupRepositoryDir(t *testing.T) {
	l := config.Layout{Root: "/data"}
	got, err := l.BackupRepositoryDir("group1")
	if err != nil || got != "/data/backups/repositories/group1" {
		t.Errorf("BackupRepositoryDir(group1) = %q, %v", got, err)
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", "/abs"} {
		if _, err := l.BackupRepositoryDir(bad); err == nil {
			t.Errorf("BackupRepositoryDir(%q) accepted", bad)
		}
	}
}
