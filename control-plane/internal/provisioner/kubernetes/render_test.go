package kubernetes

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

const testSiteID = "01hzy3k5q7m9c2v4b6n8r0t1wx"

func testSite() *provisioner.SiteConfig {
	return &provisioner.SiteConfig{
		ID:               testSiteID,
		ClientGroupID:    "01hzy3k0000000000000000000",
		PreviewDomain:    "blog.preview.example.com",
		PHPVersion:       "8.4",
		WPCoreVersion:    "6.9.1",
		DBSecretRef:      "secret/db/site1",
		Resources:        provisioner.DefaultResourceLimits(),
		UID:              provisioner.MinSiteUID,
		WPSaltsSecretRef: "secret/salts/site1",
	}
}

// testProvisioner is the driver on fake clientsets, with its data directory
// in a temporary directory.
func testProvisioner(t *testing.T) *Provisioner {
	t.Helper()
	c, _ := fakeClient(t)
	k := config.DefaultKubernetes()
	return newWithClient(Config{Layout: config.Layout{Root: t.TempDir()}, Kubernetes: k}, c)
}

// golden compares got with testdata/<name>, or rewrites it with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from the golden file (go test -update to accept):\n%s", name, got)
	}
}

func TestRenderSite(t *testing.T) {
	p := testProvisioner(t)
	dir, err := p.RenderSite(testSite())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(p.layout.Root, "sites", testSiteID, SiteManifestDir); dir != want {
		t.Errorf("rendered into %s, want %s", dir, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", e.Name(), fi.Mode().Perm())
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		golden(t, e.Name(), content)
	}
	if want := []string{"namespace.yaml"}; !slices.Equal(names, want) {
		t.Errorf("rendered %q, want %q", names, want)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("manifest directory: %v, %v; want mode 0700", fi, err)
	}

	// What it renders applies.
	if err := p.ApplyDir(t.Context(), dir); err != nil {
		t.Errorf("applying the rendered manifests: %v", err)
	}
}

func TestRenderSiteIsIdempotent(t *testing.T) {
	p := testProvisioner(t)
	dir, err := p.RenderSite(testSite())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "namespace.yaml")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	// A manifest whose template is gone, and a file that isn't one.
	stale, notes := filepath.Join(dir, "removed.yaml"), filepath.Join(dir, "notes.txt")
	for _, f := range []string{stale, notes} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.RenderSite(testSite()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(file); err != nil || !fi.ModTime().Equal(old) {
		t.Errorf("the unchanged manifest was rewritten: %v, %v", fi, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale manifest is still there: %v", err)
	}
	if _, err := os.Stat(notes); err != nil {
		t.Errorf("a file that isn't a manifest was removed: %v", err)
	}
}

func TestRenderSiteRejects(t *testing.T) {
	p := testProvisioner(t)
	long := testSite()
	long.ID = strings.Repeat("a", 61) // pf- + 61 is past the 63 of a namespace
	invalid := testSite()
	invalid.PHPVersion = ""
	for name, tt := range map[string]struct {
		cfg  *provisioner.SiteConfig
		want error
	}{
		"site ID too long for a namespace": {long, ErrInvalidSite},
		"invalid config":                   {invalid, nil},
	} {
		_, err := p.RenderSite(tt.cfg)
		if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
			t.Errorf("%s: err = %v, want %v", name, err, tt.want)
		}
	}
	if _, err := os.Stat(p.layout.SitesDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a rejected site left files: %v", err)
	}
}

func TestTemplateFingerprint(t *testing.T) {
	p := testProvisioner(t)
	fp := p.TemplateFingerprint()
	if !strings.HasPrefix(fp, "r1-") || len(fp) != len("r1-")+16 {
		t.Errorf("TemplateFingerprint = %q, want r1-<16 hex digits>", fp)
	}
	if again := testProvisioner(t).TemplateFingerprint(); again != fp {
		t.Errorf("fingerprint changed: %q, then %q", fp, again)
	}
}
