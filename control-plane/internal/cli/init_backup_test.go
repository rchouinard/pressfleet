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

const backupSecretPlaintext = "s3-s3cret/ACCESS+key"

// backupConfigOf loads the written config file and decrypts its S3 secret
// access key with key.
func backupConfigOf(t *testing.T, e *initEnv, key secrets.MasterKey) (config.BackupConfig, string) {
	t.Helper()
	cfg, err := config.Load(e.configPath, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backup.S3.SecretAccessKey == "" {
		return cfg.Backup, ""
	}
	keys, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := keys.OpenSetting(config.BackupS3SecretAccessKeySetting, cfg.Backup.S3.SecretAccessKey)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Backup, secret.Reveal()
}

func TestInitBackupLocalByDefault(t *testing.T) {
	e := newInitEnv(t)
	r := e.run("", e.nonInteractiveArgs()...)
	key := e.assertInstalled(r)
	if b, _ := backupConfigOf(t, e, key); b != (config.BackupConfig{Backend: config.BackupBackendLocal}) {
		t.Errorf("backup = %+v, want local", b)
	}
	if want := "Backups:          " + filepath.Join(e.dataDir, "backups", "repositories"); !strings.Contains(r.stdout, want) {
		t.Errorf("summary lacks %q:\n%s", want, r.stdout)
	}
	configFile, err := os.ReadFile(e.configPath)
	if err != nil || strings.Contains(string(configFile), "backup:") {
		t.Errorf("config file has a backup section for the default: %v\n%s", err, configFile)
	}
}

func TestInitBackupS3(t *testing.T) {
	e := newInitEnv(t)
	secretFile := filepath.Join(t.TempDir(), "s3-secret")
	writeFile(t, secretFile, backupSecretPlaintext+"\n")
	s3Args := []string{
		"--backup-backend", "s3", "--backup-s3-endpoint", "http://minio.internal:9000", "--backup-s3-bucket", "pf-backups",
		"--backup-s3-prefix", "prod", "--backup-s3-access-key-id", "pressfleet",
	}
	r := e.run("", e.nonInteractiveArgs(append(s3Args, "--backup-s3-secret-access-key-file", secretFile)...)...)
	key := e.assertInstalled(r)

	b, secret := backupConfigOf(t, e, key)
	want := config.BackupConfig{Backend: "s3", S3: config.BackupS3Config{
		Endpoint: "http://minio.internal:9000", Bucket: "pf-backups", Prefix: "prod", AccessKeyID: "pressfleet",
		SecretAccessKey: b.S3.SecretAccessKey,
	}}
	if b != want || secret != backupSecretPlaintext {
		t.Errorf("backup = %+v (secret %q), want %+v", b, secret, want)
	}
	if !strings.Contains(r.stdout, "Backups:          http://minio.internal:9000/pf-backups/prod") {
		t.Errorf("summary lacks the bucket:\n%s", r.stdout)
	}

	// The settings reach the control plane, and the plaintext secret is in
	// none of the files init wrote or its output.
	composeFile, err := os.ReadFile(filepath.Join(e.dataDir, "platform", platform.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`PRESSFLEET_BACKUP_BACKEND: "s3"`,
		`PRESSFLEET_BACKUP_S3_BUCKET: "pf-backups"`,
		`PRESSFLEET_BACKUP_S3_SECRET_ACCESS_KEY: "` + b.S3.SecretAccessKey + `"`,
	} {
		if !strings.Contains(string(composeFile), line) {
			t.Errorf("compose.yaml lacks %s:\n%s", line, composeFile)
		}
	}
	configFile, err := os.ReadFile(e.configPath)
	if err != nil {
		t.Fatal(err)
	}
	for what, s := range map[string]string{
		"compose.yaml": string(composeFile), "config file": string(configFile), "stdout": r.stdout, "stderr": r.stderr,
	} {
		if strings.Contains(s, backupSecretPlaintext) {
			t.Errorf("%s holds the plaintext secret access key", what)
		}
	}

	// Reinstalling keeps the sealed secret without asking for it again.
	r = e.run("", e.nonInteractiveArgs("--force")...)
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if b2, secret2 := backupConfigOf(t, e, key); b2.S3.Bucket != "pf-backups" || secret2 != backupSecretPlaintext {
		t.Errorf("after --force: backup = %+v, secret %q", b2, secret2)
	}

	// S3 without the secret or bucket is refused before anything is written.
	e2 := newInitEnv(t)
	r = e2.run("", e2.nonInteractiveArgs(s3Args...)...)
	if r.code != 1 || !strings.Contains(r.stderr, "--backup-s3-secret-access-key-file: a value is required") || len(e2.platform.ups) != 0 {
		t.Errorf("without a secret: exit %d, ups %d, stderr: %s", r.code, len(e2.platform.ups), r.stderr)
	}
	r = e2.run("", e2.nonInteractiveArgs("--backup-backend", "s3", "--backup-s3-endpoint", "http://minio:9000", "--backup-s3-bucket", "Bad_Bucket")...)
	if r.code != 1 || !strings.Contains(r.stderr, "backup.s3.bucket") {
		t.Errorf("bad bucket: exit %d, stderr: %s", r.code, r.stderr)
	}
}
