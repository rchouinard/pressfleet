package cli

import (
	"errors"
	"fmt"
	"net/mail"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

// smtpAnswers are the outbound email relay settings init collects (§5.6).
type smtpAnswers struct {
	SMTPHost         string
	SMTPEncryption   string
	SMTPPort         string
	SMTPUsername     string
	SMTPPasswordFile string
	SMTPFromEmail    string
	SMTPFromName     string
	// SMTPPassword is the password sealed with the master key: the existing
	// one when reinstalling, or the one read from SMTPPasswordFile once the
	// master key is loaded (sealSMTPPassword).
	SMTPPassword string
	// smtpNewPassword is the plaintext read from SMTPPasswordFile until it
	// is sealed.
	smtpNewPassword redact.Secret
}

// smtpPasswordPending stands in for a new SMTP password while the answers
// are validated: it can only be sealed once the master key is loaded, and
// that happens after validation so an invalid answer leaves nothing behind.
const smtpPasswordPending = "pfenc1:pending:cGVuZGluZw" //nolint:gosec // G101: a placeholder with no key behind it, not a credential

// noSMTPHost skips the rest of the SMTP questions when no relay is given.
func noSMTPHost(a *initAnswers) bool { return a.SMTPHost == "" }

// smtpInitQuestions are asked after initQuestions.
var smtpInitQuestions = []initQuestion{
	{
		flag:     "smtp-host",
		usage:    "SMTP relay sites send email through, e.g. smtp.mailgun.org (optional; also $PRESSFLEET_SMTP_HOST)",
		prompt:   "SMTP relay host sites send email through, e.g. smtp.mailgun.org (optional; without one sites send no email)",
		optional: true,
		field:    func(a *initAnswers) *string { return &a.SMTPHost },
		check: func(v string, _ *initAnswers) error {
			return config.SMTPConfig{Host: v, Encryption: config.SMTPEncryptionSTARTTLS}.Validate()
		},
	},
	{
		flag:  "smtp-encryption",
		usage: "SMTP relay encryption: starttls, tls, or none (default starttls; also $PRESSFLEET_SMTP_ENCRYPTION)",
		prompt: fmt.Sprintf("SMTP encryption: %s, %s (TLS from the start), or %s",
			config.SMTPEncryptionSTARTTLS, config.SMTPEncryptionTLS, config.SMTPEncryptionNone),
		skip:  noSMTPHost,
		field: func(a *initAnswers) *string { return &a.SMTPEncryption },
		check: func(v string, _ *initAnswers) error {
			switch v {
			case config.SMTPEncryptionSTARTTLS, config.SMTPEncryptionTLS, config.SMTPEncryptionNone:
				return nil
			}
			return fmt.Errorf("%q is not %s, %s, or %s", v, config.SMTPEncryptionSTARTTLS, config.SMTPEncryptionTLS, config.SMTPEncryptionNone)
		},
	},
	{
		flag:     "smtp-port",
		usage:    "SMTP relay port (default 587 for starttls, 465 for tls, 25 for none; also $PRESSFLEET_SMTP_PORT)",
		prompt:   "SMTP port (optional; default 587 for starttls, 465 for tls, 25 for none)",
		optional: true,
		skip:     noSMTPHost,
		field:    func(a *initAnswers) *string { return &a.SMTPPort },
		check: func(v string, _ *initAnswers) error {
			if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("%q is not a port number", v)
			}
			return nil
		},
	},
	{
		flag:     "smtp-username",
		usage:    "SMTP relay user name (optional; also $PRESSFLEET_SMTP_USERNAME)",
		prompt:   "SMTP user name (optional; empty if the relay needs no login)",
		optional: true,
		skip:     noSMTPHost,
		field:    func(a *initAnswers) *string { return &a.SMTPUsername },
	},
	{
		flag:     "smtp-password-file",
		usage:    "file holding the SMTP relay password; init stores it encrypted with the master key",
		prompt:   "File holding the SMTP password (read once; stored encrypted with the master key)",
		optional: true,
		skip:     func(a *initAnswers) bool { return a.SMTPHost == "" || a.SMTPUsername == "" },
		field:    func(a *initAnswers) *string { return &a.SMTPPasswordFile },
		check: func(v string, _ *initAnswers) error {
			_, err := readSMTPPasswordFile(v)
			return err
		},
	},
	{
		flag:     "smtp-from-email",
		usage:    "sender address for site email instead of wordpress@<site domain> (optional; also $PRESSFLEET_SMTP_FROM_EMAIL)",
		prompt:   "Sender address for site email, instead of wordpress@<site domain> (optional)",
		optional: true,
		skip:     noSMTPHost,
		field:    func(a *initAnswers) *string { return &a.SMTPFromEmail },
		check: func(v string, _ *initAnswers) error {
			if addr, err := mail.ParseAddress(v); err != nil || addr.Address != v {
				return fmt.Errorf("%q is not an email address like wordpress@agency.example", v)
			}
			return nil
		},
	},
	{
		flag:     "smtp-from-name",
		usage:    "sender name for site email instead of WordPress (optional; also $PRESSFLEET_SMTP_FROM_NAME)",
		prompt:   "Sender name for site email, instead of \"WordPress\" (optional)",
		optional: true,
		skip:     noSMTPHost,
		field:    func(a *initAnswers) *string { return &a.SMTPFromName },
	},
}

// allInitQuestions returns every question init asks, in order.
func allInitQuestions() []initQuestion {
	return slices.Concat(initQuestions, smtpInitQuestions, backupInitQuestions, dnsInitQuestions)
}

// smtpDefaults prefills the SMTP answers from the environment or the
// existing config file.
func smtpDefaults(c config.SMTPConfig) smtpAnswers {
	a := smtpAnswers{
		SMTPHost:       c.Host,
		SMTPEncryption: c.Encryption,
		SMTPUsername:   c.Username,
		SMTPPassword:   c.Password,
		SMTPFromEmail:  c.FromEmail,
		SMTPFromName:   c.FromName,
	}
	if c.Port != 0 {
		a.SMTPPort = strconv.Itoa(c.Port)
	}
	return a
}

// smtpConfig returns the answers as config.SMTPConfig.
func (a smtpAnswers) smtpConfig() config.SMTPConfig {
	c := config.SMTPConfig{
		Host:       a.SMTPHost,
		Username:   a.SMTPUsername,
		Password:   a.SMTPPassword,
		Encryption: a.SMTPEncryption,
		FromEmail:  a.SMTPFromEmail,
		FromName:   a.SMTPFromName,
	}
	if c.Encryption == "" {
		c.Encryption = config.SMTPEncryptionSTARTTLS
	}
	c.Port, _ = strconv.Atoi(a.SMTPPort) // validated by its question
	if a.smtpNewPassword != "" {
		c.Password = smtpPasswordPending
	}
	return c
}

// readSMTPPassword reads the password file named by the answers, if any.
func (a *smtpAnswers) readSMTPPassword() error {
	if a.SMTPPasswordFile == "" {
		return nil
	}
	pw, err := readSMTPPasswordFile(a.SMTPPasswordFile)
	if err != nil {
		return fmt.Errorf("--smtp-password-file: %w", err)
	}
	a.smtpNewPassword = pw
	return nil
}

// sealSMTPPassword encrypts a new SMTP password with the master key, or
// checks that the one kept from the existing config still decrypts.
func (a *smtpAnswers) sealSMTPPassword(key secrets.MasterKey) error {
	if a.smtpNewPassword == "" && a.SMTPPassword == "" {
		return nil
	}
	keys, err := secrets.NewKeyring(key)
	if err != nil {
		return err
	}
	if a.smtpNewPassword != "" {
		a.SMTPPassword = keys.SealSetting(config.SMTPPasswordSetting, a.smtpNewPassword)
		a.smtpNewPassword = ""
		return nil
	}
	if _, err := keys.OpenSetting(config.SMTPPasswordSetting, a.SMTPPassword); err != nil {
		return fmt.Errorf("the existing SMTP password can't be decrypted with this master key (%w); give it again with --smtp-password-file", err)
	}
	return nil
}

// readSMTPPasswordFile returns the first line of a password file.
func readSMTPPasswordFile(path string) (redact.Secret, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator-chosen password file
	if err != nil {
		return "", fmt.Errorf("reading the SMTP password: %w", err)
	}
	pw, _, _ := strings.Cut(string(data), "\n")
	pw = strings.TrimSuffix(pw, "\r")
	if pw == "" {
		return "", errors.New("the SMTP password file is empty")
	}
	return redact.Secret(pw), nil
}

// fileSMTP is the smtp section of the config file init writes.
type fileSMTP struct {
	Host       string `yaml:"host"`
	Port       int    `yaml:"port,omitempty"`
	Encryption string `yaml:"encryption"`
	Username   string `yaml:"username,omitempty"`
	// Password is sealed with the master key; the config file never holds
	// it in plaintext.
	Password  string `yaml:"password,omitempty"`
	FromEmail string `yaml:"from_email,omitempty"`
	FromName  string `yaml:"from_name,omitempty"`
}

// smtpFileConfig returns the config file's smtp section, or nil without a
// relay.
func (a smtpAnswers) smtpFileConfig() *fileSMTP {
	c := a.smtpConfig()
	if !c.Enabled() {
		return nil
	}
	return &fileSMTP{
		Host: c.Host, Port: c.Port, Encryption: c.Encryption, Username: c.Username,
		Password: c.Password, FromEmail: c.FromEmail, FromName: c.FromName,
	}
}
