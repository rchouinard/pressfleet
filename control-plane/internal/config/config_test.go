package config_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func env(vars map[string]string) config.LookupEnv {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pressfleet.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// noDefaultFile points PRESSFLEET_CONFIG at an empty file so tests never read
// a real config at config.DefaultPath.
func noDefaultFile(t *testing.T, vars map[string]string) map[string]string {
	t.Helper()
	if vars == nil {
		vars = map[string]string{}
	}
	vars[config.EnvConfigPath] = writeFile(t, "")
	return vars
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load("", env(noDefaultFile(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, config.Default()) {
		t.Errorf("Load() = %+v, want defaults %+v", cfg, config.Default())
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults do not validate: %v", err)
	}
}

func TestLoadPrecedence(t *testing.T) {
	path := writeFile(t, "data_dir: /srv/file\nlisten: \":9000\"\nlog:\n  level: debug\n")
	cfg, err := config.Load(path, env(map[string]string{
		"PRESSFLEET_LISTEN":     ":9999",
		"PRESSFLEET_LOG_FORMAT": "json",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{
		DataDir:  "/srv/file",                                      // file overrides default
		Listen:   ":9999",                                          // env overrides file
		Log:      config.LogConfig{Level: "debug", Format: "json"}, // file and env
		Database: config.DatabaseConfig{Driver: "sqlite"},          // default
		Jobs:     config.JobsConfig{Concurrency: 4},                // default
		MariaDB:  config.Default().MariaDB,                         // default
		Redis:    config.Default().Redis,                           // default
		Audit:    config.AuditConfig{RetentionDays: 365},           // default
		SMTP:     config.Default().SMTP,                            // default
		// default
		PlatformProject:   "pressfleet",
		WordPressSecurity: config.DefaultWordPressSecurity(), // default
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() = %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigPathFromEnv(t *testing.T) {
	path := writeFile(t, "data_dir: /srv/from-env-path\n")
	cfg, err := config.Load("", env(map[string]string{config.EnvConfigPath: path}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DataDir != "/srv/from-env-path" {
		t.Errorf("DataDir = %q", cfg.DataDir)
	}
}

func TestLoadErrors(t *testing.T) {
	t.Run("explicit file missing", func(t *testing.T) {
		_, err := config.Load(filepath.Join(t.TempDir(), "nope.yaml"), env(nil))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want ErrNotExist", err)
		}
	})
	t.Run("file from env missing", func(t *testing.T) {
		_, err := config.Load("", env(map[string]string{config.EnvConfigPath: "/nonexistent/pressfleet.yaml"}))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v, want ErrNotExist", err)
		}
	})
	t.Run("unknown key", func(t *testing.T) {
		_, err := config.Load(writeFile(t, "data_dri: /typo\n"), env(nil))
		if err == nil || !strings.Contains(err.Error(), "data_dri") {
			t.Errorf("err = %v, want unknown field error", err)
		}
	})
}

func TestValidate(t *testing.T) {
	cfg := config.Config{DataDir: "relative", Listen: "", Log: config.LogConfig{Level: "loud", Format: "xml"}}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil")
	}
	for _, want := range []string{"data_dir", "listen", "log.level", "log.format", "database.driver"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestLoadHostSettingsFromEnv(t *testing.T) {
	cfg, err := config.Load("", env(noDefaultFile(t, map[string]string{
		"PRESSFLEET_DASHBOARD_DOMAIN":    "fleet.example.com",
		"PRESSFLEET_PREVIEW_BASE_DOMAIN": "preview.example.com",
		"PRESSFLEET_PUBLIC_IP":           "203.0.113.7",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DashboardDomain != "fleet.example.com" || cfg.PreviewBaseDomain != "preview.example.com" || cfg.PublicIP != "203.0.113.7" {
		t.Errorf("Load() = %+v", cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestValidateHostSettings(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*config.Config)
		wantErr string
	}{
		{"unset is fine", func(*config.Config) {}, ""},
		{"domain with trailing dot", func(c *config.Config) { c.DashboardDomain = "fleet.example.com." }, ""},
		{"IPv6 public IP", func(c *config.Config) { c.PublicIP = "2001:db8::1" }, ""},
		{"URL as domain", func(c *config.Config) { c.DashboardDomain = "https://fleet.example.com" }, "dashboard_domain"},
		{"single label", func(c *config.Config) { c.DashboardDomain = "localhost" }, "dashboard_domain"},
		{"wildcard", func(c *config.Config) { c.PreviewBaseDomain = "*.preview.example.com" }, "preview_base_domain"},
		{"leading hyphen", func(c *config.Config) { c.PreviewBaseDomain = "-bad.example.com" }, "preview_base_domain"},
		{"bad IP", func(c *config.Config) { c.PublicIP = "203.0.113" }, "public_ip"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			tt.mutate(&cfg)
			err := cfg.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate() = %v, want error mentioning %s", err, tt.wantErr)
			}
		})
	}
}

func TestValidateDatabase(t *testing.T) {
	tests := []struct {
		name string
		db   config.DatabaseConfig
		want string // substring of the error; empty for valid
	}{
		{"sqlite default path", config.DatabaseConfig{Driver: "sqlite"}, ""},
		{"sqlite absolute path", config.DatabaseConfig{Driver: "sqlite", SQLitePath: "/srv/state.db"}, ""},
		{"sqlite relative path", config.DatabaseConfig{Driver: "sqlite", SQLitePath: "state.db"}, "database.sqlite_path"},
		{"postgres", config.DatabaseConfig{Driver: "postgres", PostgresDSN: "postgres://db/pressfleet"}, ""},
		{"postgres without DSN", config.DatabaseConfig{Driver: "postgres"}, "database.postgres_dsn"},
		{"unknown driver", config.DatabaseConfig{Driver: "mysql"}, "database.driver"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Database = tt.db
			err := cfg.Validate()
			if tt.want == "" && err != nil {
				t.Errorf("Validate() = %v", err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Validate() = %v, want error mentioning %s", err, tt.want)
			}
		})
	}
}

func TestDatabaseSettings(t *testing.T) {
	path := writeFile(t, "data_dir: /srv/pf\ndatabase:\n  driver: postgres\n  postgres_dsn: postgres://u:from-file@db/pf\n")
	cfg, err := config.Load(path, env(map[string]string{
		"PRESSFLEET_DATABASE_POSTGRES_DSN": "postgres://u:from-env@db/pf",
		"PRESSFLEET_DATABASE_SQLITE_PATH":  "/srv/other.db",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Database.Driver != "postgres" || cfg.Database.PostgresDSN.Reveal() != "postgres://u:from-env@db/pf" {
		t.Errorf("Database = %+v", cfg.Database)
	}
	if cfg.SQLitePath() != "/srv/other.db" {
		t.Errorf("SQLitePath() = %q", cfg.SQLitePath())
	}
	if got := fmt.Sprintf("%v", cfg); strings.Contains(got, "from-env") {
		t.Errorf("formatted config reveals the DSN: %s", got)
	}

	cfg.Database.SQLitePath = ""
	if got := cfg.SQLitePath(); got != "/srv/pf/state/pressfleet.db" {
		t.Errorf("default SQLitePath() = %q", got)
	}
}

func TestJobsSettings(t *testing.T) {
	path := writeFile(t, "jobs:\n  concurrency: 8\n")
	cfg, err := config.Load(path, env(nil))
	if err != nil || cfg.Jobs.Concurrency != 8 {
		t.Fatalf("from file: %d, %v", cfg.Jobs.Concurrency, err)
	}
	cfg, err = config.Load(path, env(map[string]string{"PRESSFLEET_JOBS_CONCURRENCY": "2"}))
	if err != nil || cfg.Jobs.Concurrency != 2 {
		t.Fatalf("from env: %d, %v", cfg.Jobs.Concurrency, err)
	}
	if _, err := config.Load(path, env(map[string]string{"PRESSFLEET_JOBS_CONCURRENCY": "many"})); err == nil {
		t.Error("non-numeric PRESSFLEET_JOBS_CONCURRENCY accepted")
	}
	for _, n := range []int{0, config.MaxJobsConcurrency + 1} {
		cfg := config.Default()
		cfg.Jobs.Concurrency = n
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "jobs.concurrency") {
			t.Errorf("concurrency %d: Validate() = %v", n, err)
		}
	}
}

func TestAuditSettings(t *testing.T) {
	if got := config.Default().Audit; got.RetentionDays != 365 || got.Retention() != 365*24*time.Hour {
		t.Errorf("default audit config = %+v", got)
	}
	path := writeFile(t, "audit:\n  retention_days: 90\n")
	cfg, err := config.Load(path, env(nil))
	if err != nil || cfg.Audit.RetentionDays != 90 {
		t.Fatalf("from file: %d, %v", cfg.Audit.RetentionDays, err)
	}
	cfg, err = config.Load(path, env(map[string]string{"PRESSFLEET_AUDIT_RETENTION_DAYS": "30"}))
	if err != nil || cfg.Audit.RetentionDays != 30 {
		t.Fatalf("from env: %d, %v", cfg.Audit.RetentionDays, err)
	}
	if _, err := config.Load(path, env(map[string]string{"PRESSFLEET_AUDIT_RETENTION_DAYS": "forever"})); err == nil {
		t.Error("non-numeric PRESSFLEET_AUDIT_RETENTION_DAYS accepted")
	}
	for _, n := range []int{0, -1, config.MaxAuditRetentionDays + 1} {
		cfg := config.Default()
		cfg.Audit.RetentionDays = n
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "audit.retention_days") {
			t.Errorf("retention %d: Validate() = %v", n, err)
		}
	}
}

func TestMasterKeySettings(t *testing.T) {
	path := writeFile(t, "master_key_file: /etc/pressfleet/master.key\n")
	cfg, err := config.Load(path, env(map[string]string{"PRESSFLEET_MASTER_KEY": "pfmk1:from-env"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MasterKeyFile != "/etc/pressfleet/master.key" || cfg.MasterKey.Reveal() != "pfmk1:from-env" {
		t.Errorf("MasterKeyFile = %q, MasterKey set = %v", cfg.MasterKeyFile, cfg.MasterKey != "")
	}
	if got := fmt.Sprintf("%+v", cfg); strings.Contains(got, "from-env") {
		t.Errorf("formatted config reveals the master key: %s", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}

	cfg, err = config.Load(path, env(map[string]string{"PRESSFLEET_MASTER_KEY_FILE": "relative.key"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "master_key_file must be an absolute path") {
		t.Errorf("Validate() = %v, want master_key_file error", err)
	}

	// The key itself can't be put in the config file.
	if _, err := config.Load(writeFile(t, "master_key: pfmk1:in-file\n"), env(nil)); err == nil {
		t.Error("master_key in the config file was accepted")
	}
}

func TestMariaDBSettings(t *testing.T) {
	path := writeFile(t, "mariadb:\n  address: db.internal:3307\n  admin_user: admin\n")
	cfg, err := config.Load(path, env(map[string]string{"PRESSFLEET_MARIADB_ADMIN_PASSWORD_FILE": "/run/secrets/other"}))
	if err != nil {
		t.Fatal(err)
	}
	want := config.MariaDBConfig{Address: "db.internal:3307", AdminUser: "admin", AdminPasswordFile: "/run/secrets/other"}
	if cfg.MariaDB != want {
		t.Errorf("MariaDB = %+v, want %+v", cfg.MariaDB, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	cfg, err = config.Load(path, env(map[string]string{"PRESSFLEET_MARIADB_ADDRESS": "[::1]:3306"}))
	if err != nil || cfg.MariaDB.Address != "[::1]:3306" {
		t.Fatalf("Load = %+v, %v", cfg.MariaDB, err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() with IPv6 address = %v", err)
	}
}

func TestRedisSettings(t *testing.T) {
	path := writeFile(t, "redis:\n  address: cache.internal:6380\n  admin_user: admin\n")
	cfg, err := config.Load(path, env(map[string]string{"PRESSFLEET_REDIS_ADMIN_PASSWORD_FILE": "/run/secrets/other"}))
	if err != nil {
		t.Fatal(err)
	}
	want := config.RedisConfig{Address: "cache.internal:6380", AdminUser: "admin", AdminPasswordFile: "/run/secrets/other"}
	if cfg.Redis != want {
		t.Errorf("Redis = %+v, want %+v", cfg.Redis, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	cfg, err = config.Load(path, env(map[string]string{"PRESSFLEET_REDIS_ADDRESS": "[::1]:6379", "PRESSFLEET_REDIS_ADMIN_USER": "root"}))
	if err != nil || cfg.Redis.Address != "[::1]:6379" || cfg.Redis.AdminUser != "root" {
		t.Fatalf("Load = %+v, %v", cfg.Redis, err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() with IPv6 address = %v", err)
	}
}

func TestValidateRedis(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*config.RedisConfig)
		want   string
	}{
		{"no port", func(r *config.RedisConfig) { r.Address = "redis" }, "redis.address"},
		{"no host", func(r *config.RedisConfig) { r.Address = ":6379" }, "redis.address"},
		{"bad port", func(r *config.RedisConfig) { r.Address = "redis:0" }, "redis.address"},
		{"empty user", func(r *config.RedisConfig) { r.AdminUser = "" }, "redis.admin_user"},
		{"user with space", func(r *config.RedisConfig) { r.AdminUser = "press fleet" }, "redis.admin_user"},
		{"relative password file", func(r *config.RedisConfig) { r.AdminPasswordFile = "secrets/pw" }, "redis.admin_password_file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			tt.modify(&cfg.Redis)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v, want error mentioning %s", err, tt.want)
			}
		})
	}
}

func TestPlatformProjectSetting(t *testing.T) {
	if got := config.Default().PlatformProject; got != "pressfleet" {
		t.Errorf("default platform_project = %q", got)
	}
	path := writeFile(t, "platform_project: pf-test\n")
	cfg, err := config.Load(path, env(nil))
	if err != nil || cfg.PlatformProject != "pf-test" {
		t.Fatalf("Load = %q, %v", cfg.PlatformProject, err)
	}
	cfg, err = config.Load(path, env(map[string]string{"PRESSFLEET_PLATFORM_PROJECT": "pfe2e_1"}))
	if err != nil || cfg.PlatformProject != "pfe2e_1" {
		t.Fatalf("Load from env = %q, %v", cfg.PlatformProject, err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	for _, bad := range []string{"", "Upper", "-dash", "has space", "dot.ted"} {
		cfg := config.Default()
		cfg.PlatformProject = bad
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "platform_project") {
			t.Errorf("Validate(%q) = %v, want a platform_project error", bad, err)
		}
	}
}

func TestSMTPSettings(t *testing.T) {
	if got := config.Default().SMTP; got.Enabled() || got.Encryption != config.SMTPEncryptionSTARTTLS {
		t.Errorf("default smtp = %+v, want disabled with starttls", got)
	}
	path := writeFile(t, "smtp:\n  host: smtp.example.com\n  username: relay\n  password: pfenc1:abc:ZGVm\n  from_email: sites@agency.example\n")
	cfg, err := config.Load(path, env(map[string]string{
		"PRESSFLEET_SMTP_PORT":       "2525",
		"PRESSFLEET_SMTP_ENCRYPTION": "none",
		"PRESSFLEET_SMTP_FROM_NAME":  "Agency Sites",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := config.SMTPConfig{
		Host: "smtp.example.com", Port: 2525, Username: "relay", Password: "pfenc1:abc:ZGVm",
		Encryption: "none", FromEmail: "sites@agency.example", FromName: "Agency Sites",
	}
	if cfg.SMTP != want {
		t.Errorf("SMTP = %+v, want %+v", cfg.SMTP, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	if _, err := config.Load(path, env(map[string]string{"PRESSFLEET_SMTP_PORT": "smtp"})); err == nil {
		t.Error("non-numeric PRESSFLEET_SMTP_PORT accepted")
	}
}

func TestSMTPEffectivePort(t *testing.T) {
	for _, tc := range []struct {
		port       int
		encryption string
		want       int
	}{
		{0, config.SMTPEncryptionSTARTTLS, 587},
		{0, config.SMTPEncryptionTLS, 465},
		{0, config.SMTPEncryptionNone, 25},
		{2525, config.SMTPEncryptionTLS, 2525},
	} {
		s := config.SMTPConfig{Host: "relay", Port: tc.port, Encryption: tc.encryption}
		if got := s.EffectivePort(); got != tc.want {
			t.Errorf("EffectivePort(%d, %s) = %d, want %d", tc.port, tc.encryption, got, tc.want)
		}
	}
}

func TestValidateSMTP(t *testing.T) {
	valid := func(s *config.SMTPConfig) {
		s.Host = "smtp.example.com"
		s.Username = "relay"
		s.Password = "pfenc1:abc:ZGVm"
	}
	tests := []struct {
		name   string
		modify func(*config.SMTPConfig)
		want   string // empty: valid
	}{
		{"disabled", func(*config.SMTPConfig) {}, ""},
		{"full", valid, ""},
		{"no auth", func(s *config.SMTPConfig) { s.Host = "10.0.0.5" }, ""},
		{"container name", func(s *config.SMTPConfig) { s.Host = "mailpit"; s.Encryption = "none" }, ""},
		{"settings without host", func(s *config.SMTPConfig) { s.Username = "relay" }, "smtp.host is required"},
		{"bad host", func(s *config.SMTPConfig) { valid(s); s.Host = "smtp://relay" }, "smtp.host"},
		{"bad port", func(s *config.SMTPConfig) { valid(s); s.Port = 70000 }, "smtp.port"},
		{"bad encryption", func(s *config.SMTPConfig) { valid(s); s.Encryption = "ssl" }, "smtp.encryption"},
		{"username only", func(s *config.SMTPConfig) { valid(s); s.Password = "" }, "set together"},
		{"plaintext password", func(s *config.SMTPConfig) { valid(s); s.Password = "hunter2" }, "encrypted with the master key"},
		{"bad from email", func(s *config.SMTPConfig) { valid(s); s.FromEmail = "Sites <s@example.com>" }, "smtp.from_email"},
		{"from name line break", func(s *config.SMTPConfig) { valid(s); s.FromName = "a\r\nBcc: x@example.com" }, "smtp.from_name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			tt.modify(&cfg.SMTP)
			err := cfg.Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Validate() = %v, want error mentioning %q", err, tt.want)
			}
			if err != nil && strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q reveals the password", err)
			}
		})
	}
}

func TestValidateMariaDB(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*config.MariaDBConfig)
		want   string
	}{
		{"no port", func(m *config.MariaDBConfig) { m.Address = "mariadb" }, "mariadb.address"},
		{"no host", func(m *config.MariaDBConfig) { m.Address = ":3306" }, "mariadb.address"},
		{"bad port", func(m *config.MariaDBConfig) { m.Address = "mariadb:99999" }, "mariadb.address"},
		{"named port", func(m *config.MariaDBConfig) { m.Address = "mariadb:mysql" }, "mariadb.address"},
		{"empty user", func(m *config.MariaDBConfig) { m.AdminUser = "" }, "mariadb.admin_user"},
		{"user with space", func(m *config.MariaDBConfig) { m.AdminUser = "ro ot" }, "mariadb.admin_user"},
		{"relative password file", func(m *config.MariaDBConfig) { m.AdminPasswordFile = "secrets/pw" }, "mariadb.admin_password_file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			tt.modify(&cfg.MariaDB)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v, want error mentioning %s", err, tt.want)
			}
		})
	}
}
