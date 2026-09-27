package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

// dnsAnswers are the certificate settings init collects beyond the ACME
// account (§4.1): the Let's Encrypt environment, and the DNS provider for
// the preview wildcard certificate.
type dnsAnswers struct {
	ACMEStaging bool
	// ACMECACertificates is a host path.
	ACMECACertificates string
	DNSProvider        string
	// DNSResolvers is comma-separated.
	DNSResolvers string
	// DNSCredentialsFile holds the provider's credentials as VARIABLE=value
	// lines. Only its content is kept, sealed with the master key.
	DNSCredentialsFile string
	// DNSCredentials are the credentials sealed with the master key: the
	// existing ones when reinstalling, or the ones read from
	// DNSCredentialsFile once the master key is loaded
	// (sealDNSCredentials).
	DNSCredentials string
	// dnsNewCredentials are the credentials read from DNSCredentialsFile
	// until they are sealed.
	dnsNewCredentials ingress.DNSCredentials
	// dnsCredentialsReplaced says new credentials were sealed, which a
	// running Traefik only reads when it is recreated.
	dnsCredentialsReplaced bool
}

// dnsCredentialsPending stands in for new DNS provider credentials while
// the answers are validated, like smtpPasswordPending.
const dnsCredentialsPending = "pfenc1:pending:cGVuZGluZw" //nolint:gosec // G101: a placeholder with no key behind it, not a credential

// dnsInitQuestions are asked after the SMTP questions.
var dnsInitQuestions = []initQuestion{
	{
		flag: "dns-provider",
		usage: "DNS provider for one wildcard certificate covering every preview domain, as a lego provider code " +
			"like cloudflare (optional; also $PRESSFLEET_ACME_DNS_PROVIDER)",
		prompt: "DNS provider for a wildcard preview certificate, a lego code like cloudflare or route53 " +
			"(optional; without one, each preview domain gets its own certificate)",
		optional: true,
		field:    func(a *initAnswers) *string { return &a.DNSProvider },
		check: func(v string, _ *initAnswers) error {
			return config.ACMEConfig{DNSProvider: v}.Validate()
		},
	},
	{
		flag: "dns-credentials-file",
		usage: "file of VARIABLE=value lines with the DNS provider's credentials, as named in the lego documentation; " +
			"init stores them encrypted with the master key",
		prompt: "File with the DNS provider's credentials, VARIABLE=value lines as named in the lego docs " +
			"(read once; stored encrypted with the master key)",
		optional: true,
		skip:     func(a *initAnswers) bool { return a.DNSProvider == "" },
		field:    func(a *initAnswers) *string { return &a.DNSCredentialsFile },
		check: func(v string, _ *initAnswers) error {
			_, err := readDNSCredentialsFile(v)
			return err
		},
	},
	{
		flag: "acme-dns-resolvers",
		usage: "comma-separated DNS servers (host:port) that check DNS-01 records have propagated " +
			"(default: the host's resolvers; also $PRESSFLEET_ACME_DNS_RESOLVERS)",
		optional: true,
		flagOnly: true,
		field:    func(a *initAnswers) *string { return &a.DNSResolvers },
		check: func(v string, _ *initAnswers) error {
			return config.ACMEConfig{DNSResolvers: splitComma(v)}.Validate()
		},
	},
	{
		flag:     "acme-ca-certificates",
		usage:    "PEM file of extra root certificates to trust for the ACME server, e.g. Pebble's (also $PRESSFLEET_ACME_CA_CERTIFICATES)",
		optional: true,
		flagOnly: true,
		field:    func(a *initAnswers) *string { return &a.ACMECACertificates },
		check: func(v string, _ *initAnswers) error {
			if err := checkCleanAbs(v); err != nil {
				return err
			}
			if info, err := os.Stat(v); err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("%s is not a readable file", v)
			}
			return nil
		},
	},
}

// dnsDefaults prefills the answers from the environment or the existing
// config file.
func dnsDefaults(c config.ACMEConfig) dnsAnswers {
	return dnsAnswers{
		ACMEStaging:        c.Staging,
		ACMECACertificates: c.CACertificates,
		DNSProvider:        c.DNSProvider,
		DNSResolvers:       strings.Join(c.DNSResolvers, ","),
		DNSCredentials:     c.DNSCredentials,
	}
}

// acmeConfig returns the ACME answers as config.ACMEConfig.
func (a initAnswers) acmeConfig() config.ACMEConfig {
	c := config.ACMEConfig{
		Email:          a.ACMEEmail,
		CAServer:       a.ACMECAServer,
		Staging:        a.ACMEStaging,
		CACertificates: a.ACMECACertificates,
		DNSProvider:    a.DNSProvider,
		DNSResolvers:   splitComma(a.DNSResolvers),
		DNSCredentials: a.DNSCredentials,
	}
	if c.DNSProvider == "" {
		// Credentials kept from an earlier install are useless without a
		// provider.
		c.DNSCredentials = ""
	}
	if a.dnsNewCredentials != nil {
		c.DNSCredentials = dnsCredentialsPending
	}
	return c
}

// readDNSCredentials reads the credentials file named by the answers, if
// any.
func (a *dnsAnswers) readDNSCredentials() error {
	if a.DNSCredentialsFile == "" || a.DNSProvider == "" {
		return nil
	}
	creds, err := readDNSCredentialsFile(a.DNSCredentialsFile)
	if err != nil {
		return fmt.Errorf("--dns-credentials-file: %w", err)
	}
	a.dnsNewCredentials = creds
	return nil
}

// sealDNSCredentials encrypts new DNS provider credentials with the master
// key, or checks that the ones kept from the existing config still decrypt.
func (a *dnsAnswers) sealDNSCredentials(key secrets.MasterKey) error {
	if a.DNSProvider == "" || (a.dnsNewCredentials == nil && a.DNSCredentials == "") {
		return nil
	}
	keys, err := secrets.NewKeyring(key)
	if err != nil {
		return err
	}
	if a.dnsNewCredentials != nil {
		a.DNSCredentials = keys.SealSetting(config.ACMEDNSCredentialsSetting, a.dnsNewCredentials.Marshal())
		a.dnsNewCredentials = nil
		a.dnsCredentialsReplaced = true
		return nil
	}
	if _, err := openDNSCredentials(config.ACMEConfig{DNSCredentials: a.DNSCredentials}, keys); err != nil {
		return fmt.Errorf("the existing DNS provider credentials can't be decrypted with this master key (%w); give them again with --dns-credentials-file", err)
	}
	return nil
}

// readDNSCredentialsFile reads and checks a DNS provider credentials file.
func readDNSCredentialsFile(path string) (ingress.DNSCredentials, error) {
	if err := checkCleanAbs(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path) //nolint:gosec // G304: the operator-chosen credentials file
	if err != nil {
		return nil, fmt.Errorf("reading the DNS provider credentials: %w", err)
	}
	defer func() { _ = f.Close() }()
	return ingress.ParseDNSCredentials(f)
}

// splitComma splits a comma-separated list, dropping empty items.
func splitComma(v string) []string {
	var out []string
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// fileACMEConfig is the acme section of the config file init writes.
type fileACMEConfig struct {
	Email          string   `yaml:"email,omitempty"`
	CAServer       string   `yaml:"ca_server,omitempty"`
	Staging        bool     `yaml:"staging,omitempty"`
	CACertificates string   `yaml:"ca_certificates,omitempty"`
	DNSProvider    string   `yaml:"dns_provider,omitempty"`
	DNSResolvers   []string `yaml:"dns_resolvers,omitempty"`
	// DNSCredentials are sealed with the master key; the config file never
	// holds them in plaintext.
	DNSCredentials string `yaml:"dns_credentials,omitempty"`
}

// acmeFileConfig returns the acme section for the config file, or nil if
// every ACME setting keeps its default.
func (a initAnswers) acmeFileConfig() *fileACMEConfig {
	c := a.acmeConfig()
	f := fileACMEConfig{
		Email: c.Email, CAServer: c.CAServer, Staging: c.Staging, CACertificates: c.CACertificates,
		DNSProvider: c.DNSProvider, DNSResolvers: c.DNSResolvers, DNSCredentials: c.DNSCredentials,
	}
	if f.Email == "" && f.CAServer == "" && !f.Staging && f.CACertificates == "" && f.DNSProvider == "" && len(f.DNSResolvers) == 0 {
		return nil
	}
	return &f
}

// recreateTraefik recreates Traefik when reinstalling with new DNS provider
// credentials. Starting the platform recreated the control plane, which
// wrote them for Traefik, but Traefik reads them once and keeps them.
func (i *installer) recreateTraefik(ctx context.Context, a initAnswers) error {
	if !i.f.force || !a.dnsCredentialsReplaced {
		return nil
	}
	project := i.a.platformProject(i.projectName(), config.Layout{Root: a.DataDir}.PlatformDir())
	err := project.Up(ctx, compose.UpOptions{
		Services: []string{ingress.TraefikService}, ForceRecreate: true, Wait: true, WaitTimeout: platformStartTimeout,
	})
	if err != nil {
		return fmt.Errorf("recreating Traefik with the new DNS provider credentials: %w", err)
	}
	i.printf("Recreated Traefik with the new DNS provider credentials.\n")
	return nil
}

// printDNSSummary tells the operator how preview certificates are issued.
func (i *installer) printDNSSummary(a initAnswers) {
	switch {
	case a.DNSProvider == "":
		i.printf("  Preview TLS:      one certificate per preview domain (HTTP-01)\n")
	case a.DNSCredentials == "":
		i.printf("  Preview TLS:      wildcard *.%s via %s, but no credentials are stored: rerun init --force "+
			"with --dns-credentials-file (see docs/configuration.md)\n", a.PreviewBaseDomain, a.DNSProvider)
	default:
		i.printf("  Preview TLS:      wildcard *.%s via %s (DNS-01)\n", a.PreviewBaseDomain, a.DNSProvider)
	}
}
