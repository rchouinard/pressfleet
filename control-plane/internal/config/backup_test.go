package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestBackupScheduleSettings(t *testing.T) {
	def := config.Default().Backup.Schedule
	if def.Start() != 2*time.Hour || def.Window() != 4*time.Hour {
		t.Errorf("default window = %s + %s, want 02:00 for 4h", def.Start(), def.Window())
	}
	path := writeFile(t, "backup:\n  schedule:\n    window_start: \"23:30\"\n")
	cfg, err := config.Load(path, env(map[string]string{"PRESSFLEET_BACKUP_SCHEDULE_WINDOW_HOURS": "2"}))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.Backup.Schedule
	if s != (config.BackupScheduleConfig{WindowStart: "23:30", WindowHours: 2}) || s.Start() != 23*time.Hour+30*time.Minute {
		t.Errorf("Schedule = %+v (start %s)", s, s.Start())
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	cfg, err = config.Load(path, env(map[string]string{"PRESSFLEET_BACKUP_SCHEDULE_WINDOW_START": "04:15"}))
	if err != nil || cfg.Backup.Schedule.WindowStart != "04:15" {
		t.Errorf("window start from env = %+v, %v", cfg.Backup.Schedule, err)
	}
	if _, err := config.Load(path, env(map[string]string{"PRESSFLEET_BACKUP_SCHEDULE_WINDOW_HOURS": "four"})); err == nil {
		t.Error("Load accepted a window that isn't a number")
	}

	for _, tt := range []struct {
		start string
		hours int
		want  string // empty: valid
	}{
		{"00:00", 24, ""},
		{"23:59", 1, ""},
		{"2:00", 4, "window_start"},
		{"24:00", 4, "window_start"},
		{"02:00 UTC", 4, "window_start"},
		{"", 4, "window_start"},
		{"02:00", 0, "window_hours"},
		{"02:00", 25, "window_hours"},
	} {
		cfg := config.Default()
		cfg.Backup.Schedule = config.BackupScheduleConfig{WindowStart: tt.start, WindowHours: tt.hours}
		err := cfg.Validate()
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%q, %d: Validate() = %v, want nil", tt.start, tt.hours, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), "backup.schedule."+tt.want)):
			t.Errorf("%q, %d: Validate() = %v, want error mentioning %q", tt.start, tt.hours, err, tt.want)
		}
	}
}

func TestBackupSettings(t *testing.T) {
	if got := config.Default().Backup; got.Backend != config.BackupBackendLocal {
		t.Errorf("default backup = %+v, want the local backend", got)
	}
	path := writeFile(t, "backup:\n  backend: s3\n  s3:\n    endpoint: https://s3.example.com\n    bucket: backups\n    secret_access_key: pfenc1:abc:ZGVm\n")
	cfg, err := config.Load(path, env(map[string]string{
		"PRESSFLEET_BACKUP_S3_PREFIX":        "pressfleet/prod",
		"PRESSFLEET_BACKUP_S3_REGION":        "eu-west-1",
		"PRESSFLEET_BACKUP_S3_ACCESS_KEY_ID": "AKIAEXAMPLE",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := config.BackupConfig{Backend: "s3", Schedule: config.DefaultBackupSchedule(), Retention: config.DefaultBackupRetention(), Check: config.DefaultBackupCheck(), S3: config.BackupS3Config{
		Endpoint: "https://s3.example.com", Bucket: "backups", Prefix: "pressfleet/prod", Region: "eu-west-1",
		AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "pfenc1:abc:ZGVm",
	}}
	if cfg.Backup != want {
		t.Errorf("Backup = %+v, want %+v", cfg.Backup, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	cfg, err = config.Load(path, env(map[string]string{
		"PRESSFLEET_BACKUP_BACKEND":              "local",
		"PRESSFLEET_BACKUP_S3_ENDPOINT":          "",
		"PRESSFLEET_BACKUP_S3_BUCKET":            "",
		"PRESSFLEET_BACKUP_S3_SECRET_ACCESS_KEY": "",
	}))
	if err != nil || cfg.Backup != (config.BackupConfig{Backend: "local", Schedule: config.DefaultBackupSchedule(), Retention: config.DefaultBackupRetention(), Check: config.DefaultBackupCheck()}) {
		t.Errorf("Backup from env = %+v, %v; want local", cfg.Backup, err)
	}
}

func TestValidateBackup(t *testing.T) {
	s3 := func(b *config.BackupConfig) {
		b.Backend = config.BackupBackendS3
		b.S3 = config.BackupS3Config{
			Endpoint: "http://minio:9000", Bucket: "pf-backups", AccessKeyID: "minio", SecretAccessKey: "pfenc1:abc:ZGVm",
		}
	}
	tests := []struct {
		name   string
		modify func(*config.BackupConfig)
		want   string // empty: valid
	}{
		{"local", func(*config.BackupConfig) {}, ""},
		{"s3", s3, ""},
		{"s3 with prefix and region", func(b *config.BackupConfig) {
			s3(b)
			b.S3.Endpoint, b.S3.Prefix, b.S3.Region = "https://s3.eu-west-1.amazonaws.com/", "a/b.c/d_e", "eu-west-1"
		}, ""},
		{"unknown backend", func(b *config.BackupConfig) { b.Backend = "sftp" }, "backup.backend"},
		{"empty backend", func(b *config.BackupConfig) { b.Backend = "" }, "backup.backend"},
		{"s3 settings with local", func(b *config.BackupConfig) { b.S3.Bucket = "x" }, "need backup.backend s3"},
		{"no endpoint", func(b *config.BackupConfig) { s3(b); b.S3.Endpoint = "" }, "backup.s3.endpoint"},
		{"endpoint without scheme", func(b *config.BackupConfig) { s3(b); b.S3.Endpoint = "minio:9000" }, "backup.s3.endpoint"},
		{"endpoint with path", func(b *config.BackupConfig) { s3(b); b.S3.Endpoint = "https://s3.example.com/bucket" }, "backup.s3.endpoint"},
		{"endpoint with credentials", func(b *config.BackupConfig) { s3(b); b.S3.Endpoint = "https://k:s@s3.example.com" }, "backup.s3.endpoint"},
		{"no bucket", func(b *config.BackupConfig) { s3(b); b.S3.Bucket = "" }, "backup.s3.bucket"},
		{"bad bucket", func(b *config.BackupConfig) { s3(b); b.S3.Bucket = "Backups" }, "backup.s3.bucket"},
		{"prefix with slash", func(b *config.BackupConfig) { s3(b); b.S3.Prefix = "/backups" }, "backup.s3.prefix"},
		{"prefix with dot dot", func(b *config.BackupConfig) { s3(b); b.S3.Prefix = "a/../b" }, "backup.s3.prefix"},
		{"bad region", func(b *config.BackupConfig) { s3(b); b.S3.Region = "eu west" }, "backup.s3.region"},
		{"no access key", func(b *config.BackupConfig) { s3(b); b.S3.AccessKeyID = "" }, "backup.s3.access_key_id"},
		{"no secret", func(b *config.BackupConfig) { s3(b); b.S3.SecretAccessKey = "" }, "backup.s3.secret_access_key is required"},
		{"plaintext secret", func(b *config.BackupConfig) { s3(b); b.S3.SecretAccessKey = "hunter2" }, "encrypted with the master key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			tt.modify(&cfg.Backup)
			err := cfg.Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Validate() = %v, want error mentioning %q", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q reveals the secret", err)
			}
		})
	}
}

func TestBackupRetentionAndCheckSettings(t *testing.T) {
	def := config.Default().Backup
	if def.Retention != (config.BackupRetentionConfig{KeepLast: 3, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 6}) || def.Retention.KeepsEverything() {
		t.Errorf("default retention = %+v", def.Retention)
	}
	if def.Check != (config.BackupCheckConfig{IntervalDays: 7, ReadDataPercent: 10}) || def.Check.Interval() != 7*24*time.Hour {
		t.Errorf("default check = %+v", def.Check)
	}
	path := writeFile(t, "backup:\n  retention:\n    keep_last: 1\n    keep_daily: 0\n  check:\n    interval_days: 1\n")
	cfg, err := config.Load(path, env(map[string]string{
		"PRESSFLEET_BACKUP_RETENTION_KEEP_HOURLY":   "24",
		"PRESSFLEET_BACKUP_RETENTION_KEEP_YEARLY":   "2",
		"PRESSFLEET_BACKUP_CHECK_READ_DATA_PERCENT": "0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	// Keys the file leaves out keep their defaults.
	if want := (config.BackupRetentionConfig{KeepLast: 1, KeepHourly: 24, KeepWeekly: 4, KeepMonthly: 6, KeepYearly: 2}); cfg.Backup.Retention != want {
		t.Errorf("Retention = %+v, want %+v", cfg.Backup.Retention, want)
	}
	if want := (config.BackupCheckConfig{IntervalDays: 1}); cfg.Backup.Check != want {
		t.Errorf("Check = %+v, want %+v", cfg.Backup.Check, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	if _, err := config.Load(path, env(map[string]string{"PRESSFLEET_BACKUP_RETENTION_KEEP_LAST": "all"})); err == nil ||
		!strings.Contains(err.Error(), "PRESSFLEET_BACKUP_RETENTION_KEEP_LAST") {
		t.Errorf("Load of a keep_last that isn't a number = %v", err)
	}
	if !(config.BackupRetentionConfig{}).KeepsEverything() {
		t.Error("an empty policy doesn't keep everything")
	}

	for _, tt := range []struct {
		name string
		edit func(*config.BackupConfig)
		want string // empty: valid
	}{
		{"keep nothing is keep everything", func(b *config.BackupConfig) { b.Retention = config.BackupRetentionConfig{} }, ""},
		{"negative", func(b *config.BackupConfig) { b.Retention.KeepWeekly = -1 }, "backup.retention.keep_weekly"},
		{"too many", func(b *config.BackupConfig) { b.Retention.KeepYearly = 10001 }, "backup.retention.keep_yearly"},
		{"check every day", func(b *config.BackupConfig) { b.Check.IntervalDays = 1 }, ""},
		{"no interval", func(b *config.BackupConfig) { b.Check.IntervalDays = 0 }, "backup.check.interval_days"},
		{"long interval", func(b *config.BackupConfig) { b.Check.IntervalDays = 366 }, "backup.check.interval_days"},
		{"read all", func(b *config.BackupConfig) { b.Check.ReadDataPercent = 100 }, ""},
		{"read more than all", func(b *config.BackupConfig) { b.Check.ReadDataPercent = 101 }, "backup.check.read_data_percent"},
		{"read negative", func(b *config.BackupConfig) { b.Check.ReadDataPercent = -5 }, "backup.check.read_data_percent"},
	} {
		cfg := config.Default()
		tt.edit(&cfg.Backup)
		err := cfg.Validate()
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: Validate() = %v, want nil", tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: Validate() = %v, want error mentioning %q", tt.name, err, tt.want)
		}
	}
}
