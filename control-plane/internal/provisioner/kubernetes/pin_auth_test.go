package kubernetes

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// pullSecretProvisioner is the driver with a "ghcr" pull secret in the
// platform namespace holding the given .dockerconfigjson.
func pullSecretProvisioner(t *testing.T, dockerConfig string) *Provisioner {
	t.Helper()
	cfg := testConfig(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ghcr", Namespace: cfg.Kubernetes.PlatformNamespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(dockerConfig)},
	}
	c, _ := fakeClient(t, secret)
	return newWithClient(cfg, c)
}

func dockerConfigFor(host, user, password string) string {
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + password))
	return fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, host, auth)
}

const privateImage = "/pressfleet/private:1"

// privateRegistry wants Basic credentials user:secret for its token (bearer
// true) or for the manifest itself, and never serves anonymously.
type privateRegistry struct {
	bearer bool
	manif  string
	tokens atomic.Int32
}

func (f *privateRegistry) serve(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pw, hasBasic := r.BasicAuth()
		goodBasic := hasBasic && user == "robot" && pw == "s3cret"
		switch r.URL.Path {
		case "/token":
			f.tokens.Add(1)
			if !goodBasic {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = fmt.Fprint(w, `{"access_token":"priv"}`)
		case "/v2/pressfleet/private/manifests/1":
			ok := goodBasic
			if f.bearer {
				ok = r.Header.Get("Authorization") == "Bearer priv"
			}
			if !ok {
				if f.bearer {
					w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="registry.test"`, srv.URL))
				} else {
					w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
				}
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Docker-Content-Digest", digestOf(f.manif))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPinImagePrivateRegistry(t *testing.T) {
	for _, bearer := range []bool{true, false} {
		t.Run(fmt.Sprintf("bearer %v", bearer), func(t *testing.T) {
			reg := &privateRegistry{bearer: bearer, manif: `{"private":true}`}
			srv := reg.serve(t)
			host := strings.TrimPrefix(srv.URL, "http://")
			image := host + privateImage

			p := pullSecretProvisioner(t, dockerConfigFor("https://"+host+"/v1/", "robot", "s3cret"))
			pinned, err := p.PinImage(t.Context(), image)
			if err != nil {
				t.Fatal(err)
			}
			if want := host + "/pressfleet/private@" + digestOf(reg.manif); pinned != want {
				t.Errorf("PinImage = %s, want %s", pinned, want)
			}

			// Wrong credentials: a clear error that doesn't echo them.
			bad := pullSecretProvisioner(t, dockerConfigFor(host, "robot", "wrong-password"))
			_, err = bad.PinImage(t.Context(), image)
			if !errors.Is(err, ErrRegistry) || !strings.Contains(err.Error(), "refused the credentials") {
				t.Errorf("PinImage with wrong credentials = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "wrong-password") {
				t.Errorf("the error shows the password: %v", err)
			}

			// No entry for the registry.
			none := pullSecretProvisioner(t, dockerConfigFor("other.example.com", "robot", "s3cret"))
			if _, err := none.PinImage(t.Context(), image); !errors.Is(err, ErrRegistry) {
				t.Errorf("PinImage without credentials = %v, want ErrRegistry", err)
			}

			// No pull secrets at all.
			if _, err := testProvisioner(t).PinImage(t.Context(), image); !errors.Is(err, ErrRegistry) {
				t.Errorf("PinImage without pull secrets = %v, want ErrRegistry", err)
			}
		})
	}
}

func TestPinImageAnonymousStillWorks(t *testing.T) {
	reg := &fakeRegistry{header: true}
	m := `{"public":true}`
	reg.manifest.Store(&m)
	srv := reg.serve(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	image := host + "/pressfleet/site-php:8.4"

	// No pull secrets, and a pull secret for another registry only.
	for name, p := range map[string]*Provisioner{
		"none":  testProvisioner(t),
		"other": pullSecretProvisioner(t, dockerConfigFor("other.example.com", "robot", "s3cret")),
	} {
		if _, err := p.PinImage(t.Context(), image); err != nil {
			t.Errorf("%s: PinImage = %v", name, err)
		}
	}
}

func TestPinImageCredentialsStayOnTheirHost(t *testing.T) {
	// A token service on another host than the registry's, over plain HTTP,
	// gets no credentials; neither does a host the redirect names.
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		_, _ = fmt.Fprint(w, `{"token":"t"}`)
	}))
	defer other.Close()
	// 127.0.0.1 and localhost are different hosts to the client.
	tokenURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1) + "/token"

	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm=%q,service="x"`, tokenURL))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer reg.Close()
	host := strings.TrimPrefix(reg.URL, "http://")
	p := pullSecretProvisioner(t, dockerConfigFor(host, "robot", "s3cret"))
	_, err := p.PinImage(t.Context(), host+privateImage)
	if !errors.Is(err, ErrRegistry) {
		t.Errorf("PinImage = %v, want ErrRegistry", err)
	}
	if leaked.Load() {
		t.Error("credentials were sent to a token service on another host over HTTP")
	}

	// A redirect to another host is refused when credentials ride on it.
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	defer target.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, strings.Replace(target.URL, "127.0.0.1", "localhost", 1)+r.URL.Path, http.StatusFound)
	}))
	defer redirecting.Close()
	host = strings.TrimPrefix(redirecting.URL, "http://")
	p = pullSecretProvisioner(t, dockerConfigFor(host, "robot", "s3cret"))
	_, err = p.PinImage(t.Context(), host+privateImage)
	if !errors.Is(err, ErrRegistry) || !strings.Contains(err.Error(), "refusing to send credentials") {
		t.Errorf("PinImage through a redirect = %v", err)
	}
	if leaked.Load() {
		t.Error("credentials followed a redirect to another host")
	}
}

func TestCredentialFor(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("u:p:with:colons"))
	for _, tc := range []struct {
		name, config, host, user, password string
	}{
		{"auth", `{"auths":{"ghcr.io":{"auth":"` + auth + `"}}}`, "ghcr.io", "u", "p:with:colons"},
		{"username and password", `{"auths":{"https://ghcr.io":{"username":"a","password":"b"}}}`, "ghcr.io", "a", "b"},
		{"docker hub", `{"auths":{"https://index.docker.io/v1/":{"username":"a","password":"b"}}}`, "registry-1.docker.io", "a", "b"},
		{"port", `{"auths":{"reg.test:5000":{"username":"a","password":"b"}}}`, "reg.test:5000", "a", "b"},
		{"other host", `{"auths":{"ghcr.io":{"username":"a","password":"b"}}}`, "evilghcr.io", "", ""},
		{"not json", `nope`, "ghcr.io", "", ""},
		{"bad auth", `{"auths":{"ghcr.io":{"auth":"!!"}}}`, "ghcr.io", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := credentialFor([]byte(tc.config), tc.host)
			if tc.user == "" {
				if c != nil {
					t.Errorf("credentialFor = %+v, want none", c)
				}
				return
			}
			if c == nil || c.user != tc.user || c.password.Reveal() != tc.password {
				t.Errorf("credentialFor = %+v, want %s/%s", c, tc.user, tc.password)
			}
		})
	}
}
