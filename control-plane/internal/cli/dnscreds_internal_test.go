package cli

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
)

func TestSyncDNSCredentials(t *testing.T) {
	keys := secretstest.Keyring(t, 1)
	sealed := keys.SealSetting(config.ACMEDNSCredentialsSetting, "CF_DNS_API_TOKEN=s3cret\n")
	app := func(dir, credentials string) *app {
		return &app{
			cfg:  config.Config{ACME: config.ACMEConfig{DNSProvider: "cloudflare", DNSCredentials: credentials}},
			log:  slog.New(slog.DiscardHandler),
			opts: Options{DNSCredentialsDir: dir},
		}
	}
	ctx := context.Background()

	t.Run("writes the credentials and the marker", func(t *testing.T) {
		dir := t.TempDir()
		if err := app(dir, sealed).syncDNSCredentials(ctx, keys); err != nil {
			t.Fatal(err)
		}
		if got, err := os.ReadFile(filepath.Join(dir, "CF_DNS_API_TOKEN")); err != nil || string(got) != "s3cret" {
			t.Errorf("credential = %q, %v", got, err)
		}
		if _, err := os.Stat(filepath.Join(dir, ingress.DNSCredentialsReady)); err != nil {
			t.Errorf("marker: %v", err)
		}
	})

	t.Run("no credentials still releases Traefik", func(t *testing.T) {
		dir := t.TempDir()
		if err := app(dir, "").syncDNSCredentials(ctx, keys); err != nil {
			t.Fatal(err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 || entries[0].Name() != ingress.DNSCredentialsReady {
			t.Errorf("entries = %v, want only the marker", entries)
		}
	})

	t.Run("no volume", func(t *testing.T) {
		if err := app(filepath.Join(t.TempDir(), "missing"), sealed).syncDNSCredentials(ctx, keys); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})

	t.Run("another master key stops startup", func(t *testing.T) {
		dir := t.TempDir()
		err := app(dir, sealed).syncDNSCredentials(ctx, secretstest.Keyring(t, 2))
		if !errors.Is(err, secrets.ErrUnknownKey) {
			t.Errorf("err = %v, want ErrUnknownKey", err)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("wrote %v", entries)
		}
	})

	t.Run("sealed for another setting", func(t *testing.T) {
		other := keys.SealSetting(config.SMTPPasswordSetting, "CF_DNS_API_TOKEN=s3cret\n")
		if err := app(t.TempDir(), other).syncDNSCredentials(ctx, keys); !errors.Is(err, secrets.ErrDecrypt) {
			t.Errorf("err = %v, want ErrDecrypt", err)
		}
	})
}
