package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

// backupAnswers are the backup repository settings init collects (§4.1).
type backupAnswers struct {
	BackupBackend       string
	BackupS3Endpoint    string
	BackupS3Bucket      string
	BackupS3Prefix      string
	BackupS3Region      string
	BackupS3AccessKeyID string
	// BackupS3SecretFile holds the secret access key. Only its content is
	// kept, sealed with the master key.
	BackupS3SecretFile string
	// BackupS3Secret is the secret access key sealed with the master key:
	// the existing one when reinstalling, or the one read from
	// BackupS3SecretFile once the master key is loaded (sealBackupSecret).
	BackupS3Secret string
	// backupNewSecret is the plaintext read from BackupS3SecretFile until
	// it is sealed.
	backupNewSecret redact.Secret
}

// backupSecretPending stands in for a new secret access key while the
// answers are validated, like smtpPasswordPending.
const backupSecretPending = "pfenc1:pending:cGVuZGluZw" //nolint:gosec // G101: a placeholder with no key behind it, not a credential

// notS3 skips the S3 questions for the local backend.
func notS3(a *initAnswers) bool { return a.BackupBackend != config.BackupBackendS3 }

// requiredForS3 makes an S3 answer required with the S3 backend.
func requiredForS3(a *initAnswers) bool { return !notS3(a) }

// backupInitQuestions are asked after the SMTP questions.
var backupInitQuestions = []initQuestion{
	{
		flag:   "backup-backend",
		usage:  "where client group backup repositories live: local (in the data directory) or s3 (default local; also $PRESSFLEET_BACKUP_BACKEND)",
		prompt: "Backup repositories: local (in the data directory) or s3 (an S3-compatible bucket such as Amazon S3 or MinIO)",
		field:  func(a *initAnswers) *string { return &a.BackupBackend },
		check: func(v string, _ *initAnswers) error {
			if v != config.BackupBackendLocal && v != config.BackupBackendS3 {
				return fmt.Errorf("%q is not %s or %s", v, config.BackupBackendLocal, config.BackupBackendS3)
			}
			return nil
		},
	},
	{
		flag:         "backup-s3-endpoint",
		usage:        "S3 service URL for backups, e.g. https://s3.eu-west-1.amazonaws.com (also $PRESSFLEET_BACKUP_S3_ENDPOINT)",
		prompt:       "S3 endpoint URL, e.g. https://s3.eu-west-1.amazonaws.com or http://minio.internal:9000",
		optional:     true,
		requiredWhen: requiredForS3,
		skip:         notS3,
		field:        func(a *initAnswers) *string { return &a.BackupS3Endpoint },
		check:        func(v string, _ *initAnswers) error { return config.CheckS3Endpoint(v) },
	},
	{
		flag:         "backup-s3-bucket",
		usage:        "S3 bucket for backups (also $PRESSFLEET_BACKUP_S3_BUCKET)",
		prompt:       "S3 bucket",
		optional:     true,
		requiredWhen: requiredForS3,
		skip:         notS3,
		field:        func(a *initAnswers) *string { return &a.BackupS3Bucket },
		check: func(v string, _ *initAnswers) error {
			return backupS3Check(func(s *config.BackupS3Config) { s.Bucket = v }, "backup.s3.bucket")
		},
	},
	{
		flag:     "backup-s3-prefix",
		usage:    "key prefix inside the S3 bucket, e.g. pressfleet (optional; also $PRESSFLEET_BACKUP_S3_PREFIX)",
		prompt:   "Key prefix inside the bucket, e.g. pressfleet (optional)",
		optional: true,
		skip:     notS3,
		field:    func(a *initAnswers) *string { return &a.BackupS3Prefix },
		check:    func(v string, _ *initAnswers) error { return config.CheckS3Prefix(v) },
	},
	{
		flag:     "backup-s3-region",
		usage:    "S3 bucket region, e.g. eu-west-1 (optional; default us-east-1; also $PRESSFLEET_BACKUP_S3_REGION)",
		prompt:   "Bucket region, e.g. eu-west-1 (optional; default us-east-1)",
		optional: true,
		skip:     notS3,
		field:    func(a *initAnswers) *string { return &a.BackupS3Region },
		check: func(v string, _ *initAnswers) error {
			return backupS3Check(func(s *config.BackupS3Config) { s.Region = v }, "backup.s3.region")
		},
	},
	{
		flag:         "backup-s3-access-key-id",
		usage:        "S3 access key ID for backups (also $PRESSFLEET_BACKUP_S3_ACCESS_KEY_ID)",
		prompt:       "S3 access key ID",
		optional:     true,
		requiredWhen: requiredForS3,
		skip:         notS3,
		field:        func(a *initAnswers) *string { return &a.BackupS3AccessKeyID },
		check: func(v string, _ *initAnswers) error {
			return backupS3Check(func(s *config.BackupS3Config) { s.AccessKeyID = v }, "backup.s3.access_key_id")
		},
	},
	{
		flag:     "backup-s3-secret-access-key-file",
		usage:    "file holding the S3 secret access key for backups; init stores it encrypted with the master key",
		prompt:   "File holding the S3 secret access key (read once; stored encrypted with the master key)",
		optional: true,
		requiredWhen: func(a *initAnswers) bool {
			// Reinstalling keeps the sealed key from the existing config.
			return requiredForS3(a) && a.BackupS3Secret == ""
		},
		skip:  notS3,
		field: func(a *initAnswers) *string { return &a.BackupS3SecretFile },
		check: func(v string, _ *initAnswers) error {
			_, err := readBackupSecretFile(v)
			return err
		},
	},
}

// backupS3Check validates one S3 setting by validating an otherwise valid
// S3 configuration, reporting only errors about key.
func backupS3Check(set func(*config.BackupS3Config), key string) error {
	b := config.BackupConfig{Backend: config.BackupBackendS3, S3: config.BackupS3Config{
		Endpoint: "https://s3.example.com", Bucket: "bucket", AccessKeyID: "key", SecretAccessKey: backupSecretPending,
	}}
	set(&b.S3)
	if err := b.Validate(); err != nil && strings.Contains(err.Error(), key) {
		return err
	}
	return nil
}

// backupDefaults prefills the answers from the environment or the existing
// config file.
func backupDefaults(c config.BackupConfig) backupAnswers {
	return backupAnswers{
		BackupBackend:       c.Backend,
		BackupS3Endpoint:    c.S3.Endpoint,
		BackupS3Bucket:      c.S3.Bucket,
		BackupS3Prefix:      c.S3.Prefix,
		BackupS3Region:      c.S3.Region,
		BackupS3AccessKeyID: c.S3.AccessKeyID,
		BackupS3Secret:      c.S3.SecretAccessKey,
	}
}

// backupConfig returns the answers as config.BackupConfig.
func (a backupAnswers) backupConfig() config.BackupConfig {
	if a.BackupBackend != config.BackupBackendS3 {
		return config.BackupConfig{Backend: config.BackupBackendLocal}
	}
	c := config.BackupConfig{Backend: config.BackupBackendS3, S3: config.BackupS3Config{
		Endpoint:        a.BackupS3Endpoint,
		Bucket:          a.BackupS3Bucket,
		Prefix:          a.BackupS3Prefix,
		Region:          a.BackupS3Region,
		AccessKeyID:     a.BackupS3AccessKeyID,
		SecretAccessKey: a.BackupS3Secret,
	}}
	if a.backupNewSecret != "" {
		c.S3.SecretAccessKey = backupSecretPending
	}
	return c
}

// readBackupSecret reads the secret access key file named by the answers,
// if any.
func (a *backupAnswers) readBackupSecret() error {
	if a.BackupS3SecretFile == "" || a.BackupBackend != config.BackupBackendS3 {
		return nil
	}
	secret, err := readBackupSecretFile(a.BackupS3SecretFile)
	if err != nil {
		return fmt.Errorf("--backup-s3-secret-access-key-file: %w", err)
	}
	a.backupNewSecret = secret
	return nil
}

// sealBackupSecret encrypts a new secret access key with the master key, or
// checks that the one kept from the existing config still decrypts.
func (a *backupAnswers) sealBackupSecret(key secrets.MasterKey) error {
	if a.BackupBackend != config.BackupBackendS3 || (a.backupNewSecret == "" && a.BackupS3Secret == "") {
		return nil
	}
	keys, err := secrets.NewKeyring(key)
	if err != nil {
		return err
	}
	if a.backupNewSecret != "" {
		a.BackupS3Secret = keys.SealSetting(config.BackupS3SecretAccessKeySetting, a.backupNewSecret)
		a.backupNewSecret = ""
		return nil
	}
	if _, err := keys.OpenSetting(config.BackupS3SecretAccessKeySetting, a.BackupS3Secret); err != nil {
		return fmt.Errorf("the existing S3 secret access key can't be decrypted with this master key (%w); "+
			"give it again with --backup-s3-secret-access-key-file", err)
	}
	return nil
}

// readBackupSecretFile returns the first line of a secret access key file.
func readBackupSecretFile(path string) (redact.Secret, error) {
	if err := checkCleanAbs(path); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator-chosen key file
	if err != nil {
		return "", fmt.Errorf("reading the S3 secret access key: %w", err)
	}
	secret, _, _ := strings.Cut(string(data), "\n")
	secret = strings.TrimSuffix(secret, "\r")
	if secret == "" {
		return "", errors.New("the S3 secret access key file is empty")
	}
	return redact.Secret(secret), nil
}

// fileBackup is the backup section of the config file init writes.
type fileBackup struct {
	Backend string        `yaml:"backend"`
	S3      *fileBackupS3 `yaml:"s3,omitempty"`
}

type fileBackupS3 struct {
	Endpoint    string `yaml:"endpoint"`
	Bucket      string `yaml:"bucket"`
	Prefix      string `yaml:"prefix,omitempty"`
	Region      string `yaml:"region,omitempty"`
	AccessKeyID string `yaml:"access_key_id"`
	// SecretAccessKey is sealed with the master key; the config file never
	// holds it in plaintext.
	SecretAccessKey string `yaml:"secret_access_key"`
}

// backupFileConfig returns the config file's backup section, or nil for
// the default local backend.
func (a backupAnswers) backupFileConfig() *fileBackup {
	c := a.backupConfig()
	if c.Backend != config.BackupBackendS3 {
		return nil
	}
	return &fileBackup{Backend: c.Backend, S3: &fileBackupS3{
		Endpoint: c.S3.Endpoint, Bucket: c.S3.Bucket, Prefix: c.S3.Prefix, Region: c.S3.Region,
		AccessKeyID: c.S3.AccessKeyID, SecretAccessKey: c.S3.SecretAccessKey,
	}}
}

// printBackupSummary tells the operator where backups go.
func (i *installer) printBackupSummary(a initAnswers) {
	c := a.backupConfig()
	if c.Backend != config.BackupBackendS3 {
		i.printf("  Backups:          %s\n", config.Layout{Root: a.DataDir}.BackupRepositoriesDir())
		return
	}
	where := strings.TrimSuffix(c.S3.Endpoint, "/") + "/" + c.S3.Bucket
	if c.S3.Prefix != "" {
		where += "/" + c.S3.Prefix
	}
	i.printf("  Backups:          %s\n", where)
}
