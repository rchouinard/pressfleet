package kubernetes

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestParseImageRef(t *testing.T) {
	tests := []struct {
		image string
		want  imageRef
	}{
		{"ghcr.io/rchouinard/pressfleet/site-php:php8.4-wp6.9.1", imageRef{
			name: "ghcr.io/rchouinard/pressfleet/site-php", host: "ghcr.io", repository: "rchouinard/pressfleet/site-php", tag: "php8.4-wp6.9.1",
		}},
		{"localhost:5001/site-nginx:wp6.9", imageRef{name: "localhost:5001/site-nginx", host: "localhost:5001", repository: "site-nginx", tag: "wp6.9"}},
		{"nginx", imageRef{name: "nginx", host: "registry-1.docker.io", repository: "library/nginx", tag: "latest"}},
		{"docker.io/bitnami/mariadb:11.4", imageRef{name: "docker.io/bitnami/mariadb", host: "registry-1.docker.io", repository: "bitnami/mariadb", tag: "11.4"}},
		{"traefik/whoami:v1", imageRef{name: "traefik/whoami", host: "registry-1.docker.io", repository: "traefik/whoami", tag: "v1"}},
	}
	for _, tt := range tests {
		got, err := parseImageRef(tt.image)
		if err != nil || got != tt.want {
			t.Errorf("parseImageRef(%q) = %+v, %v; want %+v", tt.image, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "GHCR.io/x:1", "ghcr.io/x:", "ghcr.io/x@sha256:abc"} {
		if _, err := parseImageRef(bad); !errors.Is(err, ErrRegistry) {
			t.Errorf("parseImageRef(%q) = %v, want ErrRegistry", bad, err)
		}
	}
}

// fakeRegistry serves one manifest by tag, behind an anonymous token as
// GHCR does. With header, it names the manifest's digest in the
// Docker-Content-Digest header, as most registries do.
type fakeRegistry struct {
	manifest   atomic.Pointer[string]
	header     bool
	tokenCalls atomic.Int32
}

func (f *fakeRegistry) serve(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			f.tokenCalls.Add(1)
			if r.URL.Query().Get("scope") != "repository:pressfleet/site-php:pull" || r.URL.Query().Get("service") != "registry.test" {
				http.Error(w, "bad scope", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, `{"token":"anon"}`)
		case "/v2/pressfleet/site-php/manifests/8.4":
			if r.Header.Get("Authorization") != "Bearer anon" {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry.test",scope="repository:pressfleet/site-php:pull"`, srv.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
				http.Error(w, "no index accepted", http.StatusNotAcceptable)
				return
			}
			body := *f.manifest.Load()
			if f.header {
				w.Header().Set("Docker-Content-Digest", digestOf(body))
			}
			if r.Method == http.MethodHead {
				return
			}
			_, _ = fmt.Fprint(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestPinImage(t *testing.T) {
	for _, header := range []bool{true, false} {
		t.Run(fmt.Sprintf("digest header %v", header), func(t *testing.T) {
			reg := &fakeRegistry{header: header}
			first := `{"manifests":[1]}`
			reg.manifest.Store(&first)
			srv := reg.serve(t)
			p := testProvisioner(t)
			host := strings.TrimPrefix(srv.URL, "http://")
			image := host + "/pressfleet/site-php:8.4"

			pinned, err := p.PinImage(t.Context(), image)
			if err != nil {
				t.Fatal(err)
			}
			if want := host + "/pressfleet/site-php@" + digestOf(first); pinned != want {
				t.Errorf("PinImage = %s, want %s", pinned, want)
			}
			if !pinnedRE.MatchString(pinned) {
				t.Errorf("%s isn't a pinned reference", pinned)
			}
			if reg.tokenCalls.Load() == 0 {
				t.Error("PinImage didn't get an anonymous token")
			}

			// A new build of the tag: PinImage keeps the build it found until
			// PullImage looks again.
			second := `{"manifests":[2]}`
			reg.manifest.Store(&second)
			if again, err := p.PinImage(t.Context(), image); err != nil || again != pinned {
				t.Errorf("PinImage again = %s, %v; want %s", again, err, pinned)
			}
			if err := p.PullImage(t.Context(), image); err != nil {
				t.Fatal(err)
			}
			if newer, err := p.PinImage(t.Context(), image); err != nil || newer != host+"/pressfleet/site-php@"+digestOf(second) {
				t.Errorf("PinImage after PullImage = %s, %v", newer, err)
			}

			// A pinned reference is pinned already.
			if same, err := p.PinImage(t.Context(), pinned); err != nil || same != pinned {
				t.Errorf("PinImage(%s) = %s, %v", pinned, same, err)
			}
			if err := p.PullImage(t.Context(), pinned); err != nil {
				t.Errorf("PullImage(%s) = %v", pinned, err)
			}
		})
	}
}

func TestPinImageErrors(t *testing.T) {
	reg := &fakeRegistry{header: true}
	m := "{}"
	reg.manifest.Store(&m)
	srv := reg.serve(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	p := testProvisioner(t)
	for _, image := range []string{
		host + "/pressfleet/site-php:9.9",    // unknown tag
		host + "/pressfleet/site-nginx:wp69", // unknown repository
	} {
		if _, err := p.PinImage(t.Context(), image); !errors.Is(err, ErrRegistry) {
			t.Errorf("PinImage(%s) = %v, want ErrRegistry", image, err)
		}
	}

	// A registry that wants real credentials.
	basic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer basic.Close()
	_, err := p.PinImage(t.Context(), strings.TrimPrefix(basic.URL, "http://")+"/private/site:1")
	if !errors.Is(err, ErrRegistry) || !strings.Contains(err.Error(), "anonymous") {
		t.Errorf("PinImage from a registry that wants credentials = %v", err)
	}
}

func TestParseChallenge(t *testing.T) {
	got := parseChallenge(`realm="https://ghcr.io/token",service="ghcr.io",scope="repository:a/b:pull,push"`)
	want := map[string]string{"realm": "https://ghcr.io/token", "service": "ghcr.io", "scope": "repository:a/b:pull,push"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("parseChallenge = %v, want %v", got, want)
	}
}

func TestRegistryURL(t *testing.T) {
	for host, want := range map[string]string{
		"ghcr.io":            "https://ghcr.io/v2/",
		"localhost:5001":     "http://localhost:5001/v2/",
		"127.0.0.1:5000":     "http://127.0.0.1:5000/v2/",
		"registry.local":     "https://registry.local/v2/",
		"10.0.0.5:5000":      "https://10.0.0.5:5000/v2/",
		"[::1]:5000":         "http://[::1]:5000/v2/",
		"registry.test:1":    "https://registry.test:1/v2/",
		"kind-registry:5000": "http://kind-registry:5000/v2/",
		"kind-registry":      "https://kind-registry/v2/",
	} {
		if got := registryURL(host, "/v2/", []string{"kind-registry:5000"}); got != want {
			t.Errorf("registryURL(%s) = %s, want %s", host, got, want)
		}
	}
}
