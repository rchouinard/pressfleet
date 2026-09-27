package compose_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// maintenanceFile is what the test reads back from a maintenance routes
// file.
type maintenanceFile struct {
	HTTP struct {
		Routers map[string]struct {
			Rule        string
			EntryPoints []string `yaml:"entryPoints"`
			Priority    int
			Service     string
			Middlewares []string
			TLS         struct {
				CertResolver string `yaml:"certResolver"`
			}
		}
	}
}

// TestMaintenance covers SetMaintenance and SiteConfig.Maintenance (#44):
// the maintenance routes mirror the site's routers, go into Traefik's file
// provider directory without touching the containers, survive updates
// only when the config says so, and go away with the site.
func TestMaintenance(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.cfg.PreviewAuth = &provisioner.BasicAuth{Username: "preview", PasswordHash: "$2a$04$abcdefghijklmnopqrstuuq5MvA9Wh8m6cHfVZ1gXWLU4.o9rSPTa"}
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	routes := filepath.Join(f.routesDir, compose.MaintenanceRoutesFile(testSiteID))
	inMaintenance := func() bool {
		t.Helper()
		_, err := os.Stat(routes)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil
	}
	if inMaintenance() {
		t.Fatal("a new site is in maintenance mode")
	}

	f.docker.calls = nil
	if err := f.prov.SetMaintenance(ctx, testSiteID, true); err != nil {
		t.Fatal(err)
	}
	if cmds := f.docker.commands(); len(cmds) != 0 {
		t.Errorf("SetMaintenance ran docker: %q", cmds)
	}
	raw, err := os.ReadFile(routes)
	if err != nil {
		t.Fatal(err)
	}
	var file maintenanceFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("%v:\n%s", err, raw)
	}
	p := "pf-" + testSiteID
	r, ok := file.HTTP.Routers[p+"-preview-maintenance"]
	if len(file.HTTP.Routers) != 1 || !ok {
		t.Fatalf("routers = %+v", file.HTTP.Routers)
	}
	// The preview router's twin: same rule and TLS, above it, to the
	// maintenance page, without the site's own middlewares.
	if r.Rule != "Host(`acme.preview.example.com`)" || len(r.EntryPoints) != 1 || r.EntryPoints[0] != "websecure" ||
		r.Priority != 100000 || r.Service != "pf-maintenance@file" || r.TLS.CertResolver != "acme-preview" || len(r.Middlewares) != 0 {
		t.Errorf("maintenance router = %+v", r)
	}
	if fi, _ := os.Stat(routes); fi.Mode().Perm() != 0o644 {
		t.Errorf("routes file mode = %v, want 0644", fi.Mode().Perm())
	}
	// Idempotent both ways.
	if err := f.prov.SetMaintenance(ctx, testSiteID, true); err != nil || !inMaintenance() {
		t.Errorf("enabling again: %v", err)
	}

	// An update keeps maintenance mode only if the config says so.
	f.cfg.Maintenance = true
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil || !inMaintenance() {
		t.Errorf("update in maintenance mode: %v, in maintenance = %v", err, inMaintenance())
	}
	f.cfg.Maintenance = false
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil || inMaintenance() {
		t.Errorf("update out of maintenance mode: %v, in maintenance = %v", err, inMaintenance())
	}
	f.cfg.Maintenance = true
	if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil || !inMaintenance() {
		t.Errorf("update into maintenance mode: %v, in maintenance = %v", err, inMaintenance())
	}

	if err := f.prov.SetMaintenance(ctx, testSiteID, false); err != nil || inMaintenance() {
		t.Errorf("disabling: %v", err)
	}
	if err := f.prov.SetMaintenance(ctx, testSiteID, false); err != nil {
		t.Errorf("disabling again: %v", err)
	}

	// Deleting the site removes its maintenance routes.
	if err := f.prov.SetMaintenance(ctx, testSiteID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.prov.DeleteSite(ctx, testSiteID); err != nil {
		t.Fatal(err)
	}
	if inMaintenance() {
		t.Error("maintenance routes outlived the site")
	}

	// A site that was never rendered can't go into maintenance mode, but
	// can leave it.
	if err := f.prov.SetMaintenance(ctx, "01k5z3m8q9r7t6v5w4x3y2z1zz", true); !errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Errorf("unknown site: %v", err)
	}
	if err := f.prov.SetMaintenance(ctx, "01k5z3m8q9r7t6v5w4x3y2z1zz", false); err != nil {
		t.Errorf("unknown site, disabling: %v", err)
	}
	if err := f.prov.SetMaintenance(ctx, "../x", true); !errors.Is(err, compose.ErrInvalidSite) {
		t.Errorf("invalid site ID: %v", err)
	}
}

func TestMaintenanceUnavailable(t *testing.T) {
	ctx := context.Background()
	layout := config.Layout{Root: t.TempDir()}
	for name, dir := range map[string]string{
		"not configured": "",
		"not rendered":   filepath.Join(layout.Root, "platform", "traefik", "dynamic"),
	} {
		t.Run(name, func(t *testing.T) {
			prov, err := compose.NewProvisioner(compose.ProvisionerConfig{
				Layout: layout, Secrets: memSecrets{}, Images: imageMap{}, MariaDBContainer: testMariaDB,
				TraefikDynamicDir: dir, Docker: newFakeDocker(t).exec,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := prov.SetMaintenance(ctx, testSiteID, true); !errors.Is(err, compose.ErrMaintenanceUnavailable) {
				t.Errorf("enabling: %v", err)
			}
			if err := prov.SetMaintenance(ctx, testSiteID, false); err != nil {
				t.Errorf("disabling: %v", err)
			}
		})
	}
	if _, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout: layout, Secrets: memSecrets{}, Images: imageMap{}, MariaDBContainer: testMariaDB, TraefikDynamicDir: "relative",
	}); err == nil {
		t.Error("a relative file provider directory was accepted")
	}
}
