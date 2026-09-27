package platform_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
)

// dnsConfig is testConfig with the preview wildcard certificate obtained
// with DNS-01 from a Pebble-like CA.
func dnsConfig() platform.Config {
	cfg := testConfig()
	cfg.ACME = platform.ACMEConfig{
		Email:          "ops@agency.example",
		CAServer:       "https://pebble:14000/dir",
		CACertificates: "/etc/pressfleet/pebble.minica.pem",
		DNSProvider:    "cloudflare",
		DNSCredentials: sealedCredentials,
		DNSResolvers:   []string{"1.1.1.1:53", "8.8.8.8:53"},
	}
	return cfg
}

// sealedCredentials has the shape of DNS provider credentials sealed with
// the master key.
const sealedCredentials = "pfenc1:0123456789abcdef:c2VhbGVk"

func TestRenderGoldenDNS(t *testing.T) {
	dir := render(t, dnsConfig())
	for _, tc := range []struct{ file, golden string }{
		{platform.ComposeFile, "dns/compose.yaml.golden"},
		{platform.TraefikConfigFile, "dns/traefik.yaml.golden"},
		{platform.TraefikDynamicConfigFile, "dns/dynamic.yaml.golden"},
		{platform.TraefikEntrypointFile, "dns/entrypoint.sh.golden"},
	} {
		got, err := os.ReadFile(filepath.Join(dir, tc.file))
		if err != nil {
			t.Fatal(err)
		}
		golden := filepath.Join("testdata", tc.golden)
		if *update {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, got, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from %s (run go test -update to accept):\n%s", tc.file, golden, got)
		}
	}
}

// TestPreviewCertResolver checks both ways the preview resolver gets
// certificates.
func TestPreviewCertResolver(t *testing.T) {
	t.Run("DNS-01 wildcard", func(t *testing.T) {
		dir := render(t, dnsConfig())
		tr := readTraefikConfig(t, dir)
		preview := tr.CertificatesResolvers[ingress.CertResolverPreview].ACME
		if preview.DNSChallenge == nil || preview.DNSChallenge.Provider != "cloudflare" ||
			!slices.Equal(preview.DNSChallenge.Resolvers, []string{"1.1.1.1:53", "8.8.8.8:53"}) {
			t.Errorf("preview resolver challenge = %+v", preview.DNSChallenge)
		}
		if preview.HTTPChallenge.EntryPoint != "" {
			t.Errorf("preview resolver also has an HTTP challenge: %+v", preview.HTTPChallenge)
		}
		// Both resolvers use the same account settings and storage.
		http := tr.CertificatesResolvers[ingress.CertResolverHTTP].ACME
		if preview.CAServer != "https://pebble:14000/dir" || preview.Email != "ops@agency.example" ||
			http.CAServer != preview.CAServer || http.Storage != preview.Storage || http.HTTPChallenge.EntryPoint != ingress.EntryPointHTTP {
			t.Errorf("resolvers: http %+v, preview %+v", http, preview)
		}
		if tr.Providers.File == nil || tr.Providers.File.Filename != "/etc/traefik/dynamic.yaml" {
			t.Errorf("file provider = %+v", tr.Providers.File)
		}

		// The file provider's router requests the wildcard certificate for
		// the preview base domain.
		raw, err := os.ReadFile(filepath.Join(dir, platform.TraefikDynamicConfigFile))
		if err != nil {
			t.Fatal(err)
		}
		var dyn struct {
			HTTP struct {
				Routers map[string]struct {
					Rule        string
					EntryPoints []string `yaml:"entryPoints"`
					Service     string
					TLS         struct {
						CertResolver string `yaml:"certResolver"`
						Domains      []struct {
							Main string
							Sans []string
						}
					}
				}
			}
		}
		if err := yaml.Unmarshal(raw, &dyn); err != nil {
			t.Fatal(err)
		}
		r := dyn.HTTP.Routers[ingress.PreviewWildcardRouter]
		if r.Rule != "Host(`preview.agency.example`)" || !slices.Equal(r.EntryPoints, []string{ingress.EntryPointHTTPS}) ||
			r.TLS.CertResolver != ingress.CertResolverPreview || len(r.TLS.Domains) != 1 ||
			r.TLS.Domains[0].Main != "preview.agency.example" ||
			!slices.Equal(r.TLS.Domains[0].Sans, []string{"*.preview.agency.example"}) {
			t.Errorf("wildcard router = %+v", r)
		}

		// The entrypoint exports <VARIABLE>_FILE for the files the control
		// plane writes, after waiting for its marker.
		sh, err := os.ReadFile(filepath.Join(dir, platform.TraefikEntrypointFile))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"dir=" + ingress.DNSCredentialsDir + "\n",
			`"$dir/` + ingress.DNSCredentialsReady + `"`,
			`export "${f##*/}_FILE=$f"`,
			"exec traefik --configFile=/etc/traefik/traefik.yaml\n",
		} {
			if !strings.Contains(string(sh), want) {
				t.Errorf("entrypoint lacks %q:\n%s", want, sh)
			}
		}
	})

	t.Run("HTTP-01 per preview domain", func(t *testing.T) {
		dir := render(t, testConfig())
		tr := readTraefikConfig(t, dir)
		preview := tr.CertificatesResolvers[ingress.CertResolverPreview].ACME
		if preview.DNSChallenge != nil || preview.HTTPChallenge.EntryPoint != ingress.EntryPointHTTP {
			t.Errorf("preview resolver = %+v", preview)
		}
		if tr.Providers.File != nil {
			t.Errorf("file provider without a DNS provider: %+v", tr.Providers.File)
		}
		for _, f := range []string{platform.TraefikDynamicConfigFile, platform.TraefikEntrypointFile} {
			if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s rendered without a DNS provider: %v", f, err)
			}
		}
	})
}

// TestRenderedComposeDNS checks how the credentials reach Traefik: through
// a memory-only volume the control plane writes and Traefik only reads.
func TestRenderedComposeDNS(t *testing.T) {
	type mount struct {
		Type, Source, Target string
		ReadOnly             bool `yaml:"read_only"`
	}
	type service struct {
		Entrypoint  []string
		Command     []string
		Environment map[string]string
		Volumes     []mount
	}
	var file struct {
		Services map[string]service
		Volumes  map[string]struct {
			DriverOpts map[string]string `yaml:"driver_opts"`
		}
	}
	raw, err := os.ReadFile(filepath.Join(render(t, dnsConfig()), platform.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if opts := file.Volumes["acme-dns-credentials"].DriverOpts; opts["type"] != "tmpfs" || opts["device"] != "tmpfs" {
		t.Errorf("credentials volume driver_opts = %v, want tmpfs", opts)
	}
	credentialsMount := func(svc service) *mount {
		for _, v := range svc.Volumes {
			if v.Source == "acme-dns-credentials" {
				return &v
			}
		}
		return nil
	}
	cp, tr := file.Services["control-plane"], file.Services["traefik"]
	// Only the control plane gets the sealed credentials, and decrypts them.
	if cp.Environment["PRESSFLEET_ACME_DNS_CREDENTIALS"] != sealedCredentials ||
		cp.Environment["PRESSFLEET_ACME_DNS_PROVIDER"] != "cloudflare" {
		t.Errorf("control plane environment = %v", cp.Environment)
	}
	for name, svc := range file.Services {
		if _, ok := svc.Environment["PRESSFLEET_ACME_DNS_CREDENTIALS"]; ok && name != "control-plane" {
			t.Errorf("%s gets the DNS provider credentials", name)
		}
	}
	if m := credentialsMount(cp); m == nil || m.Target != ingress.DNSCredentialsDir || m.ReadOnly {
		t.Errorf("control plane credentials mount = %+v, want read-write at %s", m, ingress.DNSCredentialsDir)
	}
	if m := credentialsMount(tr); m == nil || m.Target != ingress.DNSCredentialsDir || !m.ReadOnly {
		t.Errorf("traefik credentials mount = %+v, want read-only at %s", m, ingress.DNSCredentialsDir)
	}
	for _, name := range []string{"mariadb", "socket-proxy"} {
		if m := credentialsMount(file.Services[name]); m != nil {
			t.Errorf("%s mounts the DNS credentials", name)
		}
	}
	if !slices.Equal(tr.Entrypoint, []string{"/bin/sh", "/etc/traefik/entrypoint.sh"}) || len(tr.Command) != 0 {
		t.Errorf("traefik entrypoint %v, command %v", tr.Entrypoint, tr.Command)
	}
	// Pebble's CA is trusted for the ACME server, on top of the system roots.
	if tr.Environment["LEGO_CA_CERTIFICATES"] != "/etc/traefik/acme-ca.pem" || tr.Environment["LEGO_CA_SYSTEM_CERT_POOL"] != "true" {
		t.Errorf("traefik environment = %v", tr.Environment)
	}
	found := false
	for _, v := range tr.Volumes {
		if v.Source == "/etc/pressfleet/pebble.minica.pem" && v.Target == "/etc/traefik/acme-ca.pem" && v.ReadOnly {
			found = true
		}
	}
	if !found {
		t.Errorf("ACME CA certificates not mounted: %+v", tr.Volumes)
	}
	// Credentials never pass through the rendered project.
	if bytes.Contains(raw, []byte("CF_DNS_API_TOKEN")) {
		t.Error("compose.yaml names a credential")
	}
}

func TestRenderSwitchesOffDNS(t *testing.T) {
	dir := render(t, dnsConfig())
	if err := platform.Render(dir, testConfig()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{platform.TraefikDynamicConfigFile, platform.TraefikEntrypointFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s left behind after re-rendering without a DNS provider: %v", f, err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, platform.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("acme-dns-credentials")) || bytes.Contains(raw, []byte("LEGO_CA_CERTIFICATES")) ||
		bytes.Contains(raw, []byte("PRESSFLEET_ACME_DNS")) {
		t.Errorf("DNS settings rendered without a DNS provider:\n%s", raw)
	}
}

func TestRenderInvalidDNSConfig(t *testing.T) {
	tests := map[string]func(*platform.Config){
		"no preview base domain": func(c *platform.Config) { c.PreviewBaseDomain = "" },
		"bad provider":           func(c *platform.Config) { c.ACME.DNSProvider = "Cloud Flare" },
		"bad resolver":           func(c *platform.Config) { c.ACME.DNSResolvers = []string{"1.1.1.1"} },
		"relative CA file":       func(c *platform.Config) { c.ACME.CACertificates = "pebble.pem" },
		"unclean CA file":        func(c *platform.Config) { c.ACME.CACertificates = "/etc/../pebble.pem" },
		"plaintext credentials":  func(c *platform.Config) { c.ACME.DNSCredentials = "CF_DNS_API_TOKEN=x" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := dnsConfig()
			mutate(&cfg)
			if err := platform.Render(filepath.Join(t.TempDir(), "platform"), cfg); !errors.Is(err, platform.ErrInvalidConfig) {
				t.Fatalf("err = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
