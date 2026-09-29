package compose_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// newTestProvisioner returns a provisioner that runs docker with the
// function.
func newTestProvisioner(t *testing.T, docker compose.ExecFunc) *compose.Provisioner {
	t.Helper()
	p, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout: config.Layout{Root: t.TempDir()}, Secrets: memSecrets{}, MariaDBContainer: testMariaDB, Docker: docker,
		Images: imageMap{"8.4": {PHP: "a", Nginx: "b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// A site with pinned images is rendered with the pinned builds instead of
// the tags.
func TestPinnedImagesRendered(t *testing.T) {
	const (
		php   = "site-php@sha256:1111111111111111111111111111111111111111111111111111111111111111"
		nginx = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)
	for _, tt := range []struct {
		name       string
		pins       map[provisioner.Component]string
		php, nginx string
	}{
		{"unpinned", nil, "site-php:php8.4-wp7.1.2", "site-nginx:wp7.1.2"},
		{"php pinned", map[provisioner.Component]string{provisioner.ComponentPHP: php}, php, "site-nginx:wp7.1.2"},
		{"both pinned", map[provisioner.Component]string{provisioner.ComponentPHP: php, provisioner.ComponentNginx: nginx}, php, nginx},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.cfg.PinnedImages = tt.pins
			if _, err := f.prov.CreateSite(context.Background(), f.cfg); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(f.siteDir(t), compose.SiteComposeFile))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"image: \"" + tt.php + "\"", "image: \"" + tt.nginx + "\""} {
				if !strings.Contains(string(got), want) {
					t.Errorf("compose.yaml lacks %s:\n%s", want, got)
				}
			}
		})
	}
	f := newFixture(t)
	f.cfg.PinnedImages = map[provisioner.Component]string{provisioner.ComponentPHP: "site-php:latest"}
	if _, err := f.prov.CreateSite(context.Background(), f.cfg); err == nil {
		t.Error("CreateSite accepted a pin that isn't a digest")
	}
}

func TestPinImage(t *testing.T) {
	const (
		image  = "registry.example:5000/pf/site-php:php8.4-wp7.1.2"
		repo   = "registry.example:5000/pf/site-php"
		id     = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)
	tests := []struct {
		name    string
		digests string
		want    string
	}{
		{"pulled", `["other/site-php@sha256:3333333333333333333333333333333333333333333333333333333333333333","` + repo + `@` + digest + `"]`, repo + "@" + digest},
		{"built locally", `[]`, id},
		{"no digests", `null`, id},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			docker := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
				calls = append(calls, strings.Join(args, " "))
				if slices.Contains(args, "{{.Id}} {{json .RepoDigests}}") {
					return []byte(id + " " + tt.digests + "\n"), nil
				}
				return nil, nil
			}
			p := newTestProvisioner(t, docker)
			got, err := p.PinImage(context.Background(), image)
			if err != nil || got != tt.want {
				t.Fatalf("PinImage = %q, %v; want %q", got, err, tt.want)
			}
			// The build is kept under a tag of its own.
			if tag := "image tag " + id + " " + repo + ":pin-1111111111111111"; !slices.Contains(calls, tag) {
				t.Errorf("calls = %q, want %q", calls, tag)
			}
		})
	}

	p := newTestProvisioner(t, func(context.Context, io.Reader, ...string) ([]byte, error) {
		return []byte("garbage"), nil
	})
	if _, err := p.PinImage(context.Background(), image); err == nil {
		t.Error("PinImage accepted unexpected inspect output")
	}
	for _, bad := range []string{"site-php", "registry.example:5000/site-php", "r/site-php@" + digest, "-x:1", "a b:1"} {
		if _, err := p.PinImage(context.Background(), bad); err == nil {
			t.Errorf("PinImage(%q) succeeded", bad)
		}
		if err := p.PullImage(context.Background(), bad); err == nil {
			t.Errorf("PullImage(%q) succeeded", bad)
		}
	}
}

func TestPullImage(t *testing.T) {
	var calls []string
	p := newTestProvisioner(t, func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	})
	if err := p.PullImage(context.Background(), "r.example/site-nginx:wp7.1.2"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"image pull --quiet -- r.example/site-nginx:wp7.1.2"}; !slices.Equal(calls, want) {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}
