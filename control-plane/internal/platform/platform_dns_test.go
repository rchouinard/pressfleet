package platform_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
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
		if tr.Providers.File == nil || tr.Providers.File.Directory != "/etc/traefik/dynamic" || !tr.Providers.File.Watch {
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
			`export "${name}_FILE=$f"`,
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
		// The file provider is there for the maintenance page (#44), but
		// has no wildcard router.
		if tr.Providers.File == nil || tr.Providers.File.Directory != "/etc/traefik/dynamic" {
			t.Errorf("file provider = %+v", tr.Providers.File)
		}
		raw, err := os.ReadFile(filepath.Join(dir, platform.TraefikDynamicConfigFile))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(ingress.PreviewWildcardRouter)) || !bytes.Contains(raw, []byte(ingress.MaintenanceService+":")) {
			t.Errorf("%s without a DNS provider:\n%s", platform.TraefikDynamicConfigFile, raw)
		}
		if _, err := os.Stat(filepath.Join(dir, platform.TraefikEntrypointFile)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s rendered without a DNS provider: %v", platform.TraefikEntrypointFile, err)
		}
	})
}

// TestRenderedComposeDNS checks how the credentials reach Traefik: through
// a memory-only volume the control plane writes and Traefik only reads.
// TestEntrypointSkipsInvalidNames runs the rendered entrypoint against a
// credentials directory holding file names that aren't shell identifiers. The
// failed export would abort the script under set -eu and crash-loop Traefik.
func TestEntrypointSkipsInvalidNames(t *testing.T) {
	sh, err := os.ReadFile(filepath.Join(render(t, dnsConfig()), platform.TraefikEntrypointFile))
	if err != nil {
		t.Fatal(err)
	}
	creds, bin := t.TempDir(), t.TempDir()
	script := strings.Replace(string(sh), "dir="+ingress.DNSCredentialsDir+"\n", "dir="+creds+"\n", 1)
	if script == string(sh) {
		t.Fatalf("entrypoint does not set dir=%s:\n%s", ingress.DNSCredentialsDir, sh)
	}
	// .ready and .keep are dot files, which the glob skips; the others reach
	// the name check.
	for name, content := range map[string]string{
		ingress.DNSCredentialsReady: "",
		".keep":                     "",
		"cf-api-token":              "a",
		"1ST":                       "b",
		"CF_DNS_API_TOKEN":          "c",
	} {
		if err := os.WriteFile(filepath.Join(creds, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The stand-in for Traefik reports the *_FILE variables it was given.
	fake := "#!/bin/sh\nenv | grep '_FILE=' | sort\n"
	if err := os.WriteFile(filepath.Join(bin, "traefik"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "entrypoint.sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", path)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("entrypoint failed: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "CF_DNS_API_TOKEN_FILE="+filepath.Join(creds, "CF_DNS_API_TOKEN")) {
		t.Errorf("valid name not exported:\n%s", stdout.String())
	}
	for _, bad := range []string{"cf-api-token", "1ST"} {
		if strings.Contains(stdout.String(), bad+"_FILE") {
			t.Errorf("%q exported:\n%s", bad, stdout.String())
		}
		if !strings.Contains(stderr.String(), "invalid variable name: "+bad) {
			t.Errorf("skip of %q not logged:\n%s", bad, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "with 1 DNS provider credential(s)") {
		t.Errorf("credential count wrong:\n%s", stderr.String())
	}
}

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
	if _, err := os.Stat(filepath.Join(dir, platform.TraefikEntrypointFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s left behind after re-rendering without a DNS provider: %v", platform.TraefikEntrypointFile, err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, platform.TraefikDynamicConfigFile)); err != nil ||
		bytes.Contains(raw, []byte(ingress.PreviewWildcardRouter)) {
		t.Errorf("%s after re-rendering without a DNS provider: %v\n%s", platform.TraefikDynamicConfigFile, err, raw)
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
