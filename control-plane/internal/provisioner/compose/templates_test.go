package compose_test

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
)

func TestTemplateFingerprint(t *testing.T) {
	f := newFixture(t)
	fp := f.prov.TemplateFingerprint()
	if !regexp.MustCompile(`^r[0-9]+-[0-9a-f]{16}$`).MatchString(fp) {
		t.Errorf("fingerprint = %q", fp)
	}
	// The same for every provisioner of the release.
	if other := newProvisionerWith(t, f, f.sec).TemplateFingerprint(); other != fp {
		t.Errorf("fingerprints differ: %q, %q", fp, other)
	}
}

// readRendered returns the site's rendered files by name; a missing file
// is absent.
func readRendered(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range []string{compose.SiteWPConfigFile, compose.SiteComposeFile, compose.SiteMaintenanceFile, compose.SiteDomainsFile} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out[name] = string(b)
	}
	return out
}

func TestSnapshotAndRestoreRender(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.prov.RestoreRender(ctx, testSiteID); !errors.Is(err, provisioner.ErrNoRenderSnapshot) {
		t.Errorf("restore before any snapshot: %v, want ErrNoRenderSnapshot", err)
	}
	if err := f.prov.SnapshotRender(ctx, testSiteID); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("snapshot of a site never rendered: %v, want ErrSiteNotFound", err)
	}
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	dir := f.siteDir(t)
	before := readRendered(t, dir)
	wpMode := mode(t, filepath.Join(dir, compose.SiteWPConfigFile))
	if err := f.prov.SnapshotRender(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	// Snapshotting again replaces the copy with the same files.
	if err := f.prov.SnapshotRender(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}

	// A render that changes every file: a new database password
	// (wp-config.php), another image (compose.yaml), and a primary domain
	// (the routes).
	sec := memSecrets{}
	f.cfg.DBSecretRef, f.cfg.WPSaltsSecretRef = siteSecrets(t, sec, testSiteID,
		sitedb.Credentials{Database: "site_db", Username: "site_user", Password: "new-password"})
	f.prov = newProvisionerWith(t, f, sec, func(c *compose.ProvisionerConfig) { c.TraefikDynamicDir = f.routesDir })
	f.cfg.PHPVersion, f.cfg.PrimaryDomain = "8.3", "www.acme.example"
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	after := readRendered(t, dir)
	for _, name := range []string{compose.SiteWPConfigFile, compose.SiteComposeFile, compose.SiteMaintenanceFile, compose.SiteDomainsFile} {
		if after[name] == before[name] {
			t.Fatalf("the update didn't change %s", name)
		}
	}
	installed := filepath.Join(f.routesDir, compose.DomainRoutesFile(testSiteID))
	if _, err := os.Stat(installed); err != nil {
		t.Fatalf("domain routes not installed: %v", err)
	}
	f.docker.calls = nil

	st, err := f.prov.RestoreRender(ctx, testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != provisioner.SiteRunning {
		t.Errorf("phase = %s, want running", st.Phase)
	}
	if got := readRendered(t, dir); !maps.Equal(got, before) {
		t.Errorf("restored files differ from the snapshot:\n got %v\nwant %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(before)))
	}
	if got := mode(t, filepath.Join(dir, compose.SiteWPConfigFile)); got != wpMode {
		t.Errorf("wp-config.php mode = %v, want %v", got, wpMode)
	}
	if _, err := os.Stat(installed); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("domain routes still installed: %v", err)
	}
	// wp-config.php changed back, which Compose can't see: php first.
	wantCommands(t, f.docker, recreate, upWait)

	// Restoring again changes nothing.
	if _, err := f.prov.RestoreRender(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	wantCommands(t, f.docker, upWait)
}

func TestRestoreRenderKeepsStoppedSiteStopped(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	if err := f.prov.SnapshotRender(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	if err := f.prov.StopSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	f.cfg.PHPVersion = "8.3"
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	f.docker.calls = nil
	st, err := f.prov.RestoreRender(ctx, testSiteID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != provisioner.SiteStopped {
		t.Errorf("phase = %s, want stopped", st.Phase)
	}
	wantCommands(t, f.docker, "compose up --no-start --remove-orphans")

	// A site without containers has nothing to restore onto.
	if err := f.prov.DeleteSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.prov.RestoreRender(ctx, testSiteID); !errors.Is(err, provisioner.ErrNoRenderSnapshot) && !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("restore of a deleted site: %v", err)
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}
