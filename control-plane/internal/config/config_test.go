package config_test

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if cfg != config.Default() {
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
	}
	if cfg != want {
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
