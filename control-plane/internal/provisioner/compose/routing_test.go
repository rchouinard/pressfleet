package compose_test

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// renderedLabels renders the site and returns each service's labels.
func renderedLabels(t *testing.T, in compose.SiteRender) map[string]map[string]string {
	t.Helper()
	dir, _ := renderSite(t, in)
	raw, err := os.ReadFile(filepath.Join(dir, compose.SiteComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	var f siteFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]string{}
	for name, svc := range f.Services {
		out[name] = svc.Labels
	}
	return out
}

// traefikLabels returns the labels Traefik reads.
func traefikLabels(labels map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if strings.HasPrefix(k, "traefik.") || k == "pressfleet.platform" {
			out[k] = v
		}
	}
	return out
}

// TestRoutingLabels checks the preview router on nginx (#36): HTTPS only,
// the preview certificate resolver, the noindex middleware, and the
// naming scheme of package ingress.
func TestRoutingLabels(t *testing.T) {
	labels := renderedLabels(t, testSiteRender())
	p := "pf-" + testSiteID
	want := map[string]string{
		"pressfleet.platform":    "pressfleet",
		"traefik.enable":         "true",
		"traefik.docker.network": p,
		"traefik.http.services." + p + ".loadbalancer.server.port":                              "8080",
		"traefik.http.routers." + p + "-preview.rule":                                           "Host(`acme.preview.example.com`)",
		"traefik.http.routers." + p + "-preview.entrypoints":                                    "websecure",
		"traefik.http.routers." + p + "-preview.tls.certresolver":                               "acme-preview",
		"traefik.http.routers." + p + "-preview.service":                                        p,
		"traefik.http.routers." + p + "-preview.middlewares":                                    p + "-noindex",
		"traefik.http.middlewares." + p + "-noindex.headers.customresponseheaders.X-Robots-Tag": "noindex",
	}
	got := traefikLabels(labels["nginx"])
	if !maps.Equal(got, want) {
		keys := slices.Sorted(maps.Keys(got))
		t.Errorf("nginx Traefik labels:\n%v\nwant:\n%v", keys, want)
		for _, k := range keys {
			if got[k] != want[k] {
				t.Errorf("%s = %q, want %q", k, got[k], want[k])
			}
		}
	}
	if php := traefikLabels(labels["php"]); len(php) != 0 {
		t.Errorf("php is routed: %v", php)
	}
}

func TestRoutingLabelsWithoutPlatform(t *testing.T) {
	in := testSiteRender()
	in.PlatformProject = ""
	for name, l := range renderedLabels(t, in) {
		if got := traefikLabels(l); len(got) != 0 {
			t.Errorf("%s has Traefik labels without a platform: %v", name, got)
		}
	}
}
