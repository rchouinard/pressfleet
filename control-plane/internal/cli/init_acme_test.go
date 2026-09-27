package cli_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

const dnsCredentials = "# Cloudflare\nCF_DNS_API_TOKEN=s3cret-token\n"

// dnsConfigOf loads the written config file and decrypts its DNS provider
// credentials with key.
func dnsConfigOf(t *testing.T, e *initEnv, key secrets.MasterKey) (config.ACMEConfig, string) {
	t.Helper()
	cfg, err := config.Load(e.configPath, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ACME.DNSCredentials == "" {
		return cfg.ACME, ""
	}
	keys, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := keys.OpenSetting(config.ACMEDNSCredentialsSetting, cfg.ACME.DNSCredentials)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.ACME, plain.Reveal()
}

func TestInitDNSProvider(t *testing.T) {
	e := newInitEnv(t)
	credsFile := filepath.Join(t.TempDir(), "dns.env")
	writeFile(t, credsFile, dnsCredentials)
	caFile := filepath.Join(t.TempDir(), "pebble.pem")
	writeFile(t, caFile, "-----BEGIN CERTIFICATE-----\n")
	e.env["PRESSFLEET_ACME_DNS_RESOLVERS"] = "1.1.1.1:53"
	r := e.run("", e.nonInteractiveArgs(
		"--dns-provider", "cloudflare", "--dns-credentials-file", credsFile,
		"--acme-staging", "--acme-ca-certificates", caFile,
	)...)
	// One start, one exec (the admin): nothing to restart, since the
	// control plane hands the credentials to Traefik when it starts.
	key := e.assertInstalled(r)

	acme, plain := dnsConfigOf(t, e, key)
	want := config.ACMEConfig{
		Staging: true, CACertificates: caFile, DNSProvider: "cloudflare",
		DNSResolvers: []string{"1.1.1.1:53"}, DNSCredentials: acme.DNSCredentials,
	}
	if !reflect.DeepEqual(acme, want) || plain != "CF_DNS_API_TOKEN=s3cret-token\n" {
		t.Errorf("acme = %+v (credentials %q)", acme, plain)
	}
	if !strings.Contains(r.stdout, "Preview TLS:      wildcard *.preview.example.com via cloudflare (DNS-01)") {
		t.Errorf("summary lacks the preview certificate:\n%s", r.stdout)
	}

	// Traefik gets the staging directory and the DNS challenge; the control
	// plane gets the sealed credentials.
	dir := filepath.Join(e.dataDir, "platform")
	traefik, err := os.ReadFile(filepath.Join(dir, platform.TraefikConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`caServer: "` + config.LetsEncryptStagingDirectory + `"`, `provider: "cloudflare"`, `- "1.1.1.1:53"`} {
		if !strings.Contains(string(traefik), want) {
			t.Errorf("traefik.yaml lacks %s:\n%s", want, traefik)
		}
	}
	composeFile, err := os.ReadFile(filepath.Join(dir, platform.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(composeFile), `PRESSFLEET_ACME_DNS_CREDENTIALS: "`+acme.DNSCredentials+`"`) {
		t.Errorf("compose.yaml lacks the sealed credentials:\n%s", composeFile)
	}
	configFile, err := os.ReadFile(e.configPath)
	if err != nil {
		t.Fatal(err)
	}
	for what, s := range map[string]string{
		"compose.yaml": string(composeFile), "config file": string(configFile), "stdout": r.stdout, "stderr": r.stderr,
	} {
		if strings.Contains(s, "s3cret") || strings.Contains(s, credsFile) {
			t.Errorf("%s holds the plaintext credentials or their file", what)
		}
	}

	// Reinstalling keeps the sealed credentials without asking again.
	r = e.run("", e.nonInteractiveArgs("--force")...)
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if acme2, plain2 := dnsConfigOf(t, e, key); acme2.DNSProvider != "cloudflare" || plain2 != plain {
		t.Errorf("after --force: acme = %+v, credentials %q", acme2, plain2)
	}
	if n := len(e.platform.ups); n != 2 {
		t.Errorf("up calls = %d, want 2 (no Traefik recreate without new credentials)", n)
	}

	// Reinstalling with new credentials recreates Traefik, which only reads
	// them when it starts.
	writeFile(t, credsFile, "CF_DNS_API_TOKEN=rotated\n")
	r = e.run("", e.nonInteractiveArgs("--force", "--dns-credentials-file", credsFile)...)
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if _, plain3 := dnsConfigOf(t, e, key); plain3 != "CF_DNS_API_TOKEN=rotated\n" {
		t.Errorf("rotated credentials = %q", plain3)
	}
	ups := e.platform.ups
	if last := ups[len(ups)-1]; len(ups) != 4 || !slices.Equal(last.Services, []string{"traefik"}) || !last.ForceRecreate {
		t.Errorf("up calls = %+v, want Traefik recreated last", ups)
	}

	// Credentials sealed with another master key are caught before the
	// platform is touched.
	e2 := newInitEnv(t)
	e2.env["PRESSFLEET_ACME_DNS_PROVIDER"] = "cloudflare"
	e2.env["PRESSFLEET_ACME_DNS_CREDENTIALS"] = acme.DNSCredentials
	r = e2.run("", e2.nonInteractiveArgs()...)
	if r.code != 1 || !strings.Contains(r.stderr, "--dns-credentials-file") || len(e2.platform.ups) != 0 {
		t.Errorf("exit %d, ups %d, stderr: %s", r.code, len(e2.platform.ups), r.stderr)
	}
}

func TestInitDNSProviderWithoutCredentials(t *testing.T) {
	e := newInitEnv(t)
	r := e.run("", e.nonInteractiveArgs("--dns-provider", "cloudflare")...)
	e.assertInstalled(r)
	if !strings.Contains(r.stdout, "no credentials are stored: rerun init --force with --dns-credentials-file") {
		t.Errorf("no hint how to store the credentials:\n%s", r.stdout)
	}
}

func TestInitDNSInvalid(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.env")
	writeFile(t, bad, "token s3cret\n")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"bad provider", []string{"--dns-provider", "Cloud Flare"}, "--dns-provider"},
		{"bad credentials", []string{"--dns-provider", "cloudflare", "--dns-credentials-file", bad}, "--dns-credentials-file"},
		{"missing credentials", []string{"--dns-provider", "cloudflare", "--dns-credentials-file", "/nonexistent/dns.env"}, "--dns-credentials-file"},
		{"staging and CA server", []string{"--acme-staging", "--acme-ca-server", "https://pebble:14000/dir"}, "acme.staging and acme.ca_server"},
		{"missing CA certificates", []string{"--acme-ca-certificates", "/nonexistent/ca.pem"}, "--acme-ca-certificates"},
		{"bad resolver", []string{"--acme-dns-resolvers", "1.1.1.1"}, "--acme-dns-resolvers"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newInitEnv(t)
			r := e.run("", e.nonInteractiveArgs(tt.args...)...)
			if r.code == 0 || !strings.Contains(r.stderr, tt.want) {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			if strings.Contains(r.stderr, "s3cret") {
				t.Errorf("error shows a credential value: %s", r.stderr)
			}
			if len(e.platform.ups) != 0 {
				t.Error("platform started")
			}
		})
	}
}

func TestInitDNSInteractive(t *testing.T) {
	e := newInitEnv(t)
	e.env["PRESSFLEET_DATA_DIR"] = e.dataDir
	credsFile := filepath.Join(t.TempDir(), "dns.env")
	writeFile(t, credsFile, dnsCredentials)
	answers := strings.Join([]string{
		"fleet.example.com", "preview.example.com", "", "", "",
		"admin@example.com",
		"", // no Let's Encrypt contact
		"", // no SMTP relay
		"cloudflare",
		"relative/dns.env", // rejected: not absolute
		credsFile,
		"yes",
	}, "\n") + "\n"
	r := e.run(answers, "--control-plane-image", "pressfleet-control-plane:test")
	key := e.assertInstalled(r)
	for _, want := range []string{
		"DNS provider for a wildcard preview certificate",
		"File with the DNS provider's credentials",
		`"relative/dns.env" must be a clean absolute path`,
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if _, plain := dnsConfigOf(t, e, key); plain != "CF_DNS_API_TOKEN=s3cret-token\n" {
		t.Errorf("credentials = %q", plain)
	}
}
