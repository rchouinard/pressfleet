package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestACMESettings(t *testing.T) {
	if got := config.Default().ACME; !reflect.DeepEqual(got, config.ACMEConfig{}) {
		t.Errorf("default acme = %+v", got)
	}
	path := writeFile(t, `acme:
  email: ops@agency.example
  staging: true
  ca_certificates: /etc/pressfleet/acme-ca.pem
  dns_provider: cloudflare
  dns_resolvers: ["1.1.1.1:53", "8.8.8.8:53"]
`)
	cfg, err := config.Load(path, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := config.ACMEConfig{
		Email:          "ops@agency.example",
		Staging:        true,
		CACertificates: "/etc/pressfleet/acme-ca.pem",
		DNSProvider:    "cloudflare",
		DNSResolvers:   []string{"1.1.1.1:53", "8.8.8.8:53"},
	}
	if !reflect.DeepEqual(cfg.ACME, want) {
		t.Errorf("acme from file = %+v", cfg.ACME)
	}
	if got := cfg.ACME.Directory(); got != config.LetsEncryptStagingDirectory {
		t.Errorf("staging directory = %q", got)
	}

	cfg, err = config.Load(path, env(map[string]string{
		"PRESSFLEET_ACME_EMAIL":           "certs@agency.example",
		"PRESSFLEET_ACME_STAGING":         "false",
		"PRESSFLEET_ACME_CA_SERVER":       "https://pebble:14000/dir",
		"PRESSFLEET_ACME_CA_CERTIFICATES": "/srv/pebble.pem",
		"PRESSFLEET_ACME_DNS_PROVIDER":    "exec",
		"PRESSFLEET_ACME_DNS_RESOLVERS":   "challtestsrv:8053, ",
		"PRESSFLEET_ACME_DNS_CREDENTIALS": "pfenc1:0123:c2VhbGVk",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want = config.ACMEConfig{
		Email:          "certs@agency.example",
		CAServer:       "https://pebble:14000/dir",
		CACertificates: "/srv/pebble.pem",
		DNSProvider:    "exec",
		DNSResolvers:   []string{"challtestsrv:8053"},
		DNSCredentials: "pfenc1:0123:c2VhbGVk",
	}
	if !reflect.DeepEqual(cfg.ACME, want) {
		t.Errorf("acme from env = %+v", cfg.ACME)
	}
	if got := cfg.ACME.Directory(); got != "https://pebble:14000/dir" {
		t.Errorf("directory = %q", got)
	}
	cfg.PreviewBaseDomain = "preview.agency.example"
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}

	if _, err := config.Load(path, env(map[string]string{"PRESSFLEET_ACME_STAGING": "sometimes"})); err == nil ||
		!strings.Contains(err.Error(), "PRESSFLEET_ACME_STAGING") {
		t.Errorf("bad PRESSFLEET_ACME_STAGING: err = %v", err)
	}
	if got := config.Default().ACME.Directory(); got != config.LetsEncryptDirectory {
		t.Errorf("default directory = %q", got)
	}
}

func TestValidateACME(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*config.Config)
		want   string // substring of the error; empty for valid
	}{
		{"defaults", func(*config.Config) {}, ""},
		{"email", func(c *config.Config) { c.ACME.Email = "ops@agency.example" }, ""},
		{"no at", func(c *config.Config) { c.ACME.Email = "ops" }, "acme.email"},
		{"no local part", func(c *config.Config) { c.ACME.Email = "@agency.example" }, "acme.email"},
		{"bad domain", func(c *config.Config) { c.ACME.Email = "ops@localhost" }, "acme.email"},
		{"display name", func(c *config.Config) { c.ACME.Email = "Ops <ops@agency.example>" }, "acme.email"},
		{"pebble", func(c *config.Config) { c.ACME.CAServer = "https://pebble:14000/dir" }, ""},
		{"plain http", func(c *config.Config) { c.ACME.CAServer = "http://pebble:14000/dir" }, "acme.ca_server"},
		{"no scheme", func(c *config.Config) { c.ACME.CAServer = "pebble:14000/dir" }, "acme.ca_server"},
		{"credentials", func(c *config.Config) { c.ACME.CAServer = "https://u:p@pebble/dir" }, "acme.ca_server"},
		{"staging", func(c *config.Config) { c.ACME.Staging = true }, ""},
		{"staging and server", func(c *config.Config) {
			c.ACME.Staging = true
			c.ACME.CAServer = "https://pebble:14000/dir"
		}, "acme.staging and acme.ca_server"},
		{"relative ca certificates", func(c *config.Config) { c.ACME.CACertificates = "pebble.pem" }, "acme.ca_certificates"},
		{"dns provider", func(c *config.Config) {
			c.ACME.DNSProvider = "cloudflare"
			c.PreviewBaseDomain = "preview.agency.example"
		}, ""},
		{"dns provider without preview domain", func(c *config.Config) { c.ACME.DNSProvider = "cloudflare" }, "preview_base_domain"},
		{"bad dns provider", func(c *config.Config) {
			c.ACME.DNSProvider = "Cloud Flare"
			c.PreviewBaseDomain = "preview.agency.example"
		}, "acme.dns_provider"},
		{"resolver", func(c *config.Config) { c.ACME.DNSResolvers = []string{"1.1.1.1:53", "[2606:4700::1111]:53"} }, ""},
		{"resolver without port", func(c *config.Config) { c.ACME.DNSResolvers = []string{"1.1.1.1"} }, "acme.dns_resolvers"},
		{"sealed credentials", func(c *config.Config) {
			c.ACME.DNSProvider = "cloudflare"
			c.ACME.DNSCredentials = "pfenc1:0123:c2VhbGVk"
			c.PreviewBaseDomain = "preview.agency.example"
		}, ""},
		{"plaintext credentials", func(c *config.Config) {
			c.ACME.DNSProvider = "cloudflare"
			c.ACME.DNSCredentials = "CF_DNS_API_TOKEN=s3cret"
			c.PreviewBaseDomain = "preview.agency.example"
		}, "acme.dns_credentials must be encrypted"},
		{"credentials without provider", func(c *config.Config) { c.ACME.DNSCredentials = "pfenc1:0123:c2VhbGVk" }, "acme.dns_credentials needs acme.dns_provider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			tt.modify(&cfg)
			err := cfg.Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Validate() = %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Validate() = %v, want an error about %s", err, tt.want)
			}
		})
	}
}
