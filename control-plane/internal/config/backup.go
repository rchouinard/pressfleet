package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
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
	// Schedule is when sites are backed up unless they choose otherwise.
	Schedule BackupScheduleConfig `yaml:"schedule"`
	// Retention is which snapshots each site keeps.
	Retention BackupRetentionConfig `yaml:"retention"`
	// Check is how repositories are checked for damage.
	Check BackupCheckConfig `yaml:"check"`
	// State is how the state database's disaster recovery backups are
	// taken and kept.
	State BackupStateConfig `yaml:"state"`
}

// BackupStateConfig is the schedule and retention of the state database's
// disaster recovery backups (#96): an encrypted copy of the state database
// in the state repository beside the client group repositories, which
// rebuilds the platform on a new host together with the master key.
type BackupStateConfig struct {
	// IntervalHours is the time between backups, 1 to 168. Whatever the
	// platform recorded since the last backup (new sites, settings) is
	// lost if the host is.
	IntervalHours int `yaml:"interval_hours"`
	// Retention is which backups are kept, as backup.retention is for
	// sites. All rules zero keeps every backup.
	Retention BackupRetentionConfig `yaml:"retention"`
}

// DefaultBackupState backs the state database up hourly, keeping the last
// day's hourly backups, then one a day for two weeks and one a week for two
// months.
func DefaultBackupState() BackupStateConfig {
	return BackupStateConfig{
		IntervalHours: 1,
		Retention:     BackupRetentionConfig{KeepHourly: 24, KeepDaily: 14, KeepWeekly: 8},
	}
}

// Interval returns IntervalHours as a duration.
func (c BackupStateConfig) Interval() time.Duration {
	return time.Duration(c.IntervalHours) * time.Hour
}

// Validate reports every problem with the state backup settings, or nil.
func (c BackupStateConfig) Validate() error {
	var errs []error
	if c.IntervalHours < 1 || c.IntervalHours > 168 {
		errs = append(errs, fmt.Errorf("backup.state.interval_hours must be from 1 to 168, got %d", c.IntervalHours))
	}
	if err := c.Retention.validate("backup.state.retention"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// BackupRetentionConfig is the retention policy (#54): which of each site's
// snapshots are kept when a client group repository is pruned, as restic
// forget's --keep-* options. A snapshot is kept if any rule keeps it, and
// each site's snapshots are counted separately. With every rule zero,
// nothing is ever removed.
type BackupRetentionConfig struct {
	// KeepLast keeps the newest n snapshots.
	KeepLast int `yaml:"keep_last"`
	// KeepHourly, KeepDaily, KeepWeekly, KeepMonthly, and KeepYearly keep
	// the newest snapshot of each of the last n hours, days, weeks, months,
	// and years that have one.
	KeepHourly  int `yaml:"keep_hourly"`
	KeepDaily   int `yaml:"keep_daily"`
	KeepWeekly  int `yaml:"keep_weekly"`
	KeepMonthly int `yaml:"keep_monthly"`
	KeepYearly  int `yaml:"keep_yearly"`
}

// DefaultBackupRetention keeps the last 3 snapshots, then one a day for a
// week, one a week for a month, and one a month for half a year.
func DefaultBackupRetention() BackupRetentionConfig {
	return BackupRetentionConfig{KeepLast: 3, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}
}

// maxKeep bounds each retention rule; more is surely a typo.
const maxKeep = 10000

// retentionRule is one rule of a retention policy, by config key.
type retentionRule struct {
	key string
	n   int
}

func (r BackupRetentionConfig) rules() []retentionRule {
	return []retentionRule{
		{"keep_last", r.KeepLast}, {"keep_hourly", r.KeepHourly}, {"keep_daily", r.KeepDaily},
		{"keep_weekly", r.KeepWeekly}, {"keep_monthly", r.KeepMonthly}, {"keep_yearly", r.KeepYearly},
	}
}

// KeepsEverything reports whether every rule is zero, which disables
// retention.
func (r BackupRetentionConfig) KeepsEverything() bool { return r == BackupRetentionConfig{} }

// Validate reports every problem with the retention policy, or nil.
func (r BackupRetentionConfig) Validate() error {
	return r.validate("backup.retention")
}

// validate is Validate for the policy at the config key prefix.
func (r BackupRetentionConfig) validate(prefix string) error {
	var errs []error
	for _, rule := range r.rules() {
		if rule.n < 0 || rule.n > maxKeep {
			errs = append(errs, fmt.Errorf("%s.%s must be from 0 to %d, got %d", prefix, rule.key, maxKeep, rule.n))
		}
	}
	return errors.Join(errs...)
}

// BackupCheckConfig is how often client group repositories are checked for
// damage with restic check (#54), and how much of their data each check
// reads back.
type BackupCheckConfig struct {
	// IntervalDays is the time between checks of a repository, 1 to 365.
	IntervalDays int `yaml:"interval_days"`
	// ReadDataPercent is the share of the repository's data that each
	// check downloads and verifies, 0 to 100, chosen at random each time.
	// Zero checks only the repository's structure.
	ReadDataPercent int `yaml:"read_data_percent"`
}

// DefaultBackupCheck checks every repository weekly, reading 10% of its
// data.
func DefaultBackupCheck() BackupCheckConfig {
	return BackupCheckConfig{IntervalDays: 7, ReadDataPercent: 10}
}

// Interval returns IntervalDays as a duration.
func (c BackupCheckConfig) Interval() time.Duration {
	return time.Duration(c.IntervalDays) * 24 * time.Hour
}

// Validate reports every problem with the check settings, or nil.
func (c BackupCheckConfig) Validate() error {
	var errs []error
	if c.IntervalDays < 1 || c.IntervalDays > 365 {
		errs = append(errs, fmt.Errorf("backup.check.interval_days must be from 1 to 365, got %d", c.IntervalDays))
	}
	if c.ReadDataPercent < 0 || c.ReadDataPercent > 100 {
		errs = append(errs, fmt.Errorf("backup.check.read_data_percent must be from 0 to 100, got %d", c.ReadDataPercent))
	}
	return errors.Join(errs...)
}

// BackupScheduleConfig is the window that scheduled backups start in (#53).
// Each site's daily backup starts at a time within the window derived from
// the site's ID, so backups are spread out instead of all starting at once,
// and a site is backed up at the same time every day. A site can choose
// another interval or hour (store.SiteBackupSchedule).
type BackupScheduleConfig struct {
	// WindowStart is the UTC time of day the window opens, "HH:MM".
	WindowStart string `yaml:"window_start"`
	// WindowHours is the window's length in hours, 1 to 24.
	WindowHours int `yaml:"window_hours"`
}

// DefaultBackupSchedule is the default backup window: 02:00 to 06:00 UTC.
func DefaultBackupSchedule() BackupScheduleConfig {
	return BackupScheduleConfig{WindowStart: "02:00", WindowHours: 4}
}

// Start returns WindowStart as the time since midnight UTC. It is only
// meaningful once Validate has accepted the config.
func (s BackupScheduleConfig) Start() time.Duration {
	t, err := time.Parse("15:04", s.WindowStart)
	if err != nil {
		return 0
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute
}

// Window returns WindowHours as a duration.
func (s BackupScheduleConfig) Window() time.Duration {
	return time.Duration(s.WindowHours) * time.Hour
}

// Validate reports every problem with the backup window, or nil.
func (s BackupScheduleConfig) Validate() error {
	var errs []error
	if _, err := time.Parse("15:04", s.WindowStart); err != nil || len(s.WindowStart) != len("15:04") {
		errs = append(errs, fmt.Errorf("backup.schedule.window_start must be a UTC time of day like 02:00, got %q", s.WindowStart))
	}
	if s.WindowHours < 1 || s.WindowHours > 24 {
		errs = append(errs, fmt.Errorf("backup.schedule.window_hours must be from 1 to 24, got %d", s.WindowHours))
	}
	return errors.Join(errs...)
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

// Validate reports every problem with where repositories live (Backend
// and S3), or nil. The platform Compose project, which passes them to the
// control plane, checks them too. Config.Validate also checks Schedule.
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
