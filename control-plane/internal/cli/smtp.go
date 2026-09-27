package cli

import (
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// phpMailerEncryption maps smtp.encryption to PHPMailer's SMTPSecure values,
// which wpconfig.SMTP uses: PHPMailer calls STARTTLS "tls" and implicit TLS
// "ssl".
var phpMailerEncryption = map[string]string{
	config.SMTPEncryptionSTARTTLS: "tls",
	config.SMTPEncryptionTLS:      "ssl",
	config.SMTPEncryptionNone:     "",
}

// siteSMTP returns the relay settings for every site's wp-config.php, or nil
// if outbound email isn't configured. It decrypts the password, so a wrong
// master key or a damaged value stops the control plane at startup rather
// than failing each site's mail later.
func siteSMTP(cfg config.SMTPConfig, keys *secrets.Keyring) (*wpconfig.SMTP, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	encryption, ok := phpMailerEncryption[cfg.Encryption]
	if !ok {
		// Unreachable after config.Validate.
		return nil, fmt.Errorf("smtp.encryption %q is not supported", cfg.Encryption)
	}
	var password redact.Secret
	if cfg.Password != "" {
		var err error
		if password, err = keys.OpenSetting(config.SMTPPasswordSetting, cfg.Password); err != nil {
			return nil, fmt.Errorf("decrypting the SMTP password: %w", err)
		}
	}
	return &wpconfig.SMTP{
		Host:       cfg.Host,
		Port:       cfg.EffectivePort(),
		Username:   cfg.Username,
		Password:   password,
		Encryption: encryption,
		FromEmail:  cfg.FromEmail,
		FromName:   cfg.FromName,
	}, nil
}
