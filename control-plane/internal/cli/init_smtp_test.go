package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

const smtpPlaintext = "relay-p@ss w0rd"

// smtpConfigOf loads the written config file and decrypts its SMTP password
// with key.
func smtpConfigOf(t *testing.T, e *initEnv, key secrets.MasterKey) (config.SMTPConfig, string) {
	t.Helper()
	cfg, err := config.Load(e.configPath, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.Password == "" {
		return cfg.SMTP, ""
	}
	keys, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	pw, err := keys.OpenSetting(config.SMTPPasswordSetting, cfg.SMTP.Password)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.SMTP, pw.Reveal()
}

func TestInitSMTP(t *testing.T) {
	e := newInitEnv(t)
	pwFile := filepath.Join(t.TempDir(), "smtp-password")
	writeFile(t, pwFile, smtpPlaintext+"\n")
	r := e.run("", e.nonInteractiveArgs(
		"--smtp-host", "smtp.example.com", "--smtp-port", "2525", "--smtp-username", "relay",
		"--smtp-password-file", pwFile, "--smtp-from-email", "sites@agency.example", "--smtp-from-name", "Agency Sites",
	)...)
	key := e.assertInstalled(r)

	smtp, pw := smtpConfigOf(t, e, key)
	want := config.SMTPConfig{
		Host: "smtp.example.com", Port: 2525, Username: "relay", Password: smtp.Password,
		Encryption: "starttls", FromEmail: "sites@agency.example", FromName: "Agency Sites",
	}
	if smtp != want || pw != smtpPlaintext {
		t.Errorf("smtp = %+v (password %q), want %+v", smtp, pw, want)
	}
	if !strings.Contains(r.stdout, "SMTP relay:       smtp.example.com:2525 (starttls)") {
		t.Errorf("summary lacks the relay:\n%s", r.stdout)
	}

	// The relay reaches the control plane, and the plaintext password is in
	// none of the files init wrote or its output.
	composeFile, err := os.ReadFile(filepath.Join(e.dataDir, "platform", platform.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(composeFile), `PRESSFLEET_SMTP_PASSWORD: "`+smtp.Password+`"`) {
		t.Errorf("compose.yaml lacks the sealed SMTP password:\n%s", composeFile)
	}
	configFile, err := os.ReadFile(e.configPath)
	if err != nil {
		t.Fatal(err)
	}
	for what, s := range map[string]string{
		"compose.yaml": string(composeFile), "config file": string(configFile), "stdout": r.stdout, "stderr": r.stderr,
	} {
		if strings.Contains(s, smtpPlaintext) {
			t.Errorf("%s holds the plaintext SMTP password", what)
		}
	}

	// Reinstalling keeps the sealed password without asking for it again.
	r = e.run("", e.nonInteractiveArgs("--force")...)
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if smtp2, pw2 := smtpConfigOf(t, e, key); smtp2.Host != "smtp.example.com" || pw2 != smtpPlaintext {
		t.Errorf("after --force: smtp = %+v, password %q", smtp2, pw2)
	}

	// A password sealed with another master key is caught before the
	// platform is touched.
	other := filepath.Join(t.TempDir(), "other.key")
	e2 := newInitEnv(t)
	e2.env["PRESSFLEET_SMTP_HOST"] = "smtp.example.com"
	e2.env["PRESSFLEET_SMTP_USERNAME"] = "relay"
	e2.env["PRESSFLEET_SMTP_PASSWORD"] = smtp.Password
	r = e2.run("", e2.nonInteractiveArgs("--master-key-file", other)...)
	if r.code != 1 || !strings.Contains(r.stderr, "--smtp-password-file") || len(e2.platform.ups) != 0 {
		t.Errorf("exit %d, ups %d, stderr: %s", r.code, len(e2.platform.ups), r.stderr)
	}
}

func TestInitSMTPInteractive(t *testing.T) {
	e := newInitEnv(t)
	e.env["PRESSFLEET_DATA_DIR"] = e.dataDir
	pwFile := filepath.Join(t.TempDir(), "smtp-password")
	writeFile(t, pwFile, smtpPlaintext)
	answers := strings.Join([]string{
		"fleet.example.com",
		"preview.example.com",
		"", "", "",
		"admin@example.com",
		"",                // no Let's Encrypt contact
		"mailpit",         // SMTP host
		"ssl",             // rejected: not an encryption mode
		"none",            // encryption
		"",                // default port
		"relay",           // user name
		"/does/not/exist", // rejected: unreadable
		pwFile,            // password file
		"not an address",  // rejected
		"wp@agency.example",
		"", // no sender name
		"yes",
	}, "\n") + "\n"
	r := e.run(answers, "--control-plane-image", "pressfleet-control-plane:test")
	key := e.assertInstalled(r)
	for _, want := range []string{
		"SMTP encryption: starttls, tls (TLS from the start), or none [starttls]: ",
		`"ssl" is not starttls, tls, or none`,
		"reading the SMTP password",
		`"not an address" is not an email address`,
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	smtp, pw := smtpConfigOf(t, e, key)
	if smtp.Host != "mailpit" || smtp.Encryption != "none" || smtp.Port != 0 || smtp.Username != "relay" ||
		pw != smtpPlaintext || smtp.FromEmail != "wp@agency.example" || smtp.FromName != "" {
		t.Errorf("smtp = %+v, password %q", smtp, pw)
	}
}

func TestInitSMTPValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"user without password", []string{"--smtp-host", "smtp.example.com", "--smtp-username", "relay"}, "smtp.username and smtp.password"},
		{"settings without host", []string{"--smtp-username", "relay"}, "smtp.host is required"},
		{"bad encryption", []string{"--smtp-host", "smtp.example.com", "--smtp-encryption", "ssl"}, "--smtp-encryption"},
		{"bad port", []string{"--smtp-host", "smtp.example.com", "--smtp-port", "0"}, "--smtp-port"},
		{"bad host", []string{"--smtp-host", "smtp://example.com"}, "--smtp-host"},
		{"missing password file", []string{"--smtp-host", "smtp.example.com", "--smtp-username", "relay", "--smtp-password-file", "/does/not/exist"}, "--smtp-password-file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newInitEnv(t)
			r := e.run("", e.nonInteractiveArgs(tt.args...)...)
			if r.code != 1 || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
			}
			if fileExists(e.keyFile) || len(e.platform.ups) != 0 {
				t.Error("init went ahead")
			}
		})
	}
}
