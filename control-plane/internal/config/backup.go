package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Values of backup.backend.
const (
	// BackupBackendLocal keeps repositories in the data directory
	// (Layout.BackupRepositoryDir).
	BackupBackendLocal = "local"
	// BackupBackendS3 keeps repositories in an S3-compatible bucket, such as
	// Amazon S3 or MinIO.
	BackupBackendS3 = "s3"
)

// BackupS3SecretAccessKeySetting names backup.s3.secret_access_key when it
// is sealed with the master key (secrets.Keyring.SealSetting).
const BackupS3SecretAccessKeySetting = "backup.s3.secret_access_key"

// BackupConfig says where client group backup repositories live. It is
// chosen at install time (§4.1); changing it later only affects
// repositories created afterwards, since each repository records its own
// location.
type BackupConfig struct {
	// Backend is BackupBackendLocal (the default) or BackupBackendS3.
	Backend string `yaml:"backend"`
	// S3 is the bucket used by BackupBackendS3.
	S3 BackupS3Config `yaml:"s3"`
}

// BackupS3Config is an S3-compatible bucket holding one repository per
// client group, at <prefix>/<client group ID>.
type BackupS3Config struct {
	// Endpoint is the service's base URL, such as
	// https://s3.eu-west-1.amazonaws.com or http://minio.internal:9000.
	Endpoint string `yaml:"endpoint"`
	// Bucket must exist or be creatable with the access key.
	Bucket string `yaml:"bucket"`
	// Prefix is an optional key prefix inside the bucket, such as
	// pressfleet/backups, without leading or trailing slashes.
	Prefix string `yaml:"prefix"`
	// Region is the bucket's region; empty means us-east-1, which MinIO
	// accepts by default.
	Region string `yaml:"region"`
	// AccessKeyID and SecretAccessKey authenticate with the service.
	AccessKeyID string `yaml:"access_key_id"`
	// SecretAccessKey is encrypted with the master key under the name
	// BackupS3SecretAccessKeySetting ("pfenc1:..."), as pressfleet init
	// writes it. The control plane decrypts it at startup.
	SecretAccessKey string `yaml:"secret_access_key"`
}

// isZero reports whether no S3 setting is set.
func (s BackupS3Config) isZero() bool { return s == BackupS3Config{} }

var (
	// s3Bucket matches S3 bucket names: 3 to 63 lowercase letters, digits,
	// dots, and hyphens, starting and ending with a letter or digit.
	s3Bucket = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	// s3PrefixSegment matches one /-separated part of backup.s3.prefix.
	s3PrefixSegment = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	// s3Region matches region names, e.g. eu-west-1; MinIO allows custom
	// ones.
	s3Region = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Validate reports every problem with the backup settings, or nil.
func (b BackupConfig) Validate() error {
	var errs []error
	switch b.Backend {
	case BackupBackendLocal:
		if !b.S3.isZero() {
			errs = append(errs, errors.New("backup.s3 settings need backup.backend s3"))
		}
		return errors.Join(errs...)
	case BackupBackendS3:
	default:
		return fmt.Errorf("backup.backend must be %q or %q, got %q", BackupBackendLocal, BackupBackendS3, b.Backend)
	}
	s := b.S3
	if err := CheckS3Endpoint(s.Endpoint); err != nil {
		errs = append(errs, err)
	}
	if !s3Bucket.MatchString(s.Bucket) || strings.Contains(s.Bucket, "..") {
		errs = append(errs, fmt.Errorf("backup.s3.bucket must be an S3 bucket name (3 to 63 lowercase letters, digits, dots, and hyphens), got %q", s.Bucket))
	}
	if err := CheckS3Prefix(s.Prefix); err != nil {
		errs = append(errs, err)
	}
	if s.Region != "" && !s3Region.MatchString(s.Region) {
		errs = append(errs, fmt.Errorf("backup.s3.region must be a region name like eu-west-1, got %q", s.Region))
	}
	if s.AccessKeyID == "" || strings.ContainsFunc(s.AccessKeyID, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		errs = append(errs, errors.New("backup.s3.access_key_id is required and must not contain spaces"))
	}
	switch {
	case s.SecretAccessKey == "":
		errs = append(errs, errors.New("backup.s3.secret_access_key is required"))
	case !strings.HasPrefix(s.SecretAccessKey, sealedSettingPrefix):
		// Not quoted back: it is probably the plaintext key.
		errs = append(errs, errors.New("backup.s3.secret_access_key must be encrypted with the master key (pressfleet init writes it that way), not plaintext"))
	}
	return errors.Join(errs...)
}

// CheckS3Endpoint validates backup.s3.endpoint: an http or https URL with
// no path, query, or credentials.
func CheckS3Endpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("backup.s3.endpoint must be a URL like https://s3.eu-west-1.amazonaws.com or http://minio:9000, got %q", endpoint)
	}
	return nil
}

// CheckS3Prefix validates backup.s3.prefix, which may be empty.
func CheckS3Prefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	for seg := range strings.SplitSeq(prefix, "/") {
		if !s3PrefixSegment.MatchString(seg) || seg == "." || seg == ".." {
			return fmt.Errorf("backup.s3.prefix must be /-separated names of letters, digits, '.', '_', and '-', without leading or trailing slashes, got %q", prefix)
		}
	}
	return nil
}
