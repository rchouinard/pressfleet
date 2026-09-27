package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

// dnsCredentialsDir is where the platform mounts the memory-only volume
// Traefik reads DNS provider credentials from.
func (a *app) dnsCredentialsDir() string {
	if a.opts.DNSCredentialsDir != "" {
		return a.opts.DNSCredentialsDir
	}
	return ingress.DNSCredentialsDir
}

// openDNSCredentials decrypts acme.dns_credentials; nil if unset.
func openDNSCredentials(cfg config.ACMEConfig, keys *secrets.Keyring) (ingress.DNSCredentials, error) {
	if cfg.DNSCredentials == "" {
		return nil, nil
	}
	plain, err := keys.OpenSetting(config.ACMEDNSCredentialsSetting, cfg.DNSCredentials)
	if err != nil {
		return nil, fmt.Errorf("decrypting the DNS provider credentials: %w", err)
	}
	creds, err := ingress.ParseDNSCredentials(strings.NewReader(plain.Reveal()))
	if err != nil {
		return nil, fmt.Errorf("decrypting the DNS provider credentials: %w", err)
	}
	return creds, nil
}

// syncDNSCredentials decrypts the DNS provider credentials and writes them
// for Traefik, which waits for them before it starts (see package ingress).
// The volume they go to is memory-only, so this runs every time the control
// plane starts. Without the volume (no DNS provider in the rendered
// platform, or not in the control plane container) there is nothing to do.
// Like the SMTP password, credentials that don't decrypt stop the control
// plane.
func (a *app) syncDNSCredentials(ctx context.Context, keys *secrets.Keyring) error {
	creds, err := openDNSCredentials(a.cfg.ACME, keys)
	if err != nil {
		return err
	}
	dir := a.dnsCredentialsDir()
	if info, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) || (err == nil && !info.IsDir()) {
		if len(creds) > 0 {
			a.log.WarnContext(ctx, "DNS provider credentials are configured, but the platform has no volume to pass them to Traefik; "+
				"render the platform project with acme.dns_provider", "dir", dir)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("writing DNS provider credentials: %w", err)
	}
	if err := ingress.WriteDNSCredentials(dir, creds); err != nil {
		return err
	}
	if len(creds) == 0 {
		a.log.WarnContext(ctx, "acme.dns_provider is set but acme.dns_credentials isn't; "+
			"Traefik can't get the wildcard preview certificate (rerun pressfleet init --force with --dns-credentials-file)")
		return nil
	}
	a.log.InfoContext(ctx, "DNS provider credentials written for Traefik", "variables", creds.Names())
	return nil
}
