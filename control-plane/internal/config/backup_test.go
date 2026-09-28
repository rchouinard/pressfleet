package config_test

import (
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

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
	want := config.BackupConfig{Backend: "s3", S3: config.BackupS3Config{
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
	if err != nil || cfg.Backup != (config.BackupConfig{Backend: "local"}) {
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
