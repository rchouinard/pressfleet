package cli

import (
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

func TestSiteSMTP(t *testing.T) {
	keys := secretstest.Keyring(t, 1)
	sealed := keys.SealSetting(config.SMTPPasswordSetting, "relay-pw")
	tests := []struct {
		name string
		cfg  config.SMTPConfig
		want *wpconfig.SMTP
	}{
		{"disabled", config.Default().SMTP, nil},
		{
			"starttls with auth",
			config.SMTPConfig{Host: "smtp.example.com", Username: "relay", Password: sealed, Encryption: "starttls",
				FromEmail: "sites@agency.example", FromName: "Agency"},
			&wpconfig.SMTP{Host: "smtp.example.com", Port: 587, Username: "relay", Password: "relay-pw", Encryption: "tls",
				FromEmail: "sites@agency.example", FromName: "Agency"},
		},
		{
			"implicit tls",
			config.SMTPConfig{Host: "smtp.example.com", Encryption: "tls"},
			&wpconfig.SMTP{Host: "smtp.example.com", Port: 465, Encryption: "ssl"},
		},
		{
			"no tls, custom port",
			config.SMTPConfig{Host: "mailpit", Port: 1025, Encryption: "none"},
			&wpconfig.SMTP{Host: "mailpit", Port: 1025, Encryption: ""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := siteSMTP(tc.cfg, keys)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("siteSMTP = %+v, want nil", got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Errorf("siteSMTP = %+v, want %+v", got, tc.want)
			}
		})
	}

	// A password sealed with another master key stops startup.
	cfg := config.SMTPConfig{Host: "smtp.example.com", Username: "relay", Password: sealed, Encryption: "starttls"}
	if _, err := siteSMTP(cfg, secretstest.Keyring(t, 2)); !errors.Is(err, secrets.ErrUnknownKey) {
		t.Errorf("siteSMTP with another key: err = %v, want ErrUnknownKey", err)
	}
}
