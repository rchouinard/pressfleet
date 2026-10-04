package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// withDeprecated swaps in test rows for the deprecation tables.
func withDeprecated(t *testing.T, keys, envVars []deprecatedKey) {
	t.Helper()
	oldKeys, oldEnv := deprecatedKeys, deprecatedEnvVars
	deprecatedKeys, deprecatedEnvVars = keys, envVars
	t.Cleanup(func() { deprecatedKeys, deprecatedEnvVars = oldKeys, oldEnv })
}

func mapEnv(vars map[string]string) LookupEnv {
	return func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
}

var testDeprecatedKeys = []deprecatedKey{
	{Old: "listen_addr", New: "listen", Since: "v0.5.0"},
	{Old: "jobs.workers", New: "jobs.concurrency", Since: "v0.5.0"},
	{Old: "logging.verbosity", New: "log.level", Since: "v0.5.0"},
	{Old: "backup.window", Since: "v0.5.0", Reason: "backups follow each client group's schedule"},
	{Old: "first_name", New: "second_name", Since: "v0.4.0"},
	{Old: "second_name", New: "data_dir", Since: "v0.5.0"},
}

func TestLoadDeprecatedKeys(t *testing.T) {
	withDeprecated(t, testDeprecatedKeys, nil)
	tests := []struct {
		name     string
		file     string
		check    func(Config) bool
		warnings []string // substrings, one per expected warning
		wantErr  error    // nil, or a sentinel; errText is checked instead when set
		errText  string
	}{
		{
			name:     "top-level rename",
			file:     "listen_addr: \":9000\"\n",
			check:    func(c Config) bool { return c.Listen == ":9000" },
			warnings: []string{"config key listen_addr was renamed to listen in v0.5.0; update "},
		},
		{
			name:     "nested rename within a section",
			file:     "jobs:\n  workers: 7\n",
			check:    func(c Config) bool { return c.Jobs.Concurrency == 7 },
			warnings: []string{"jobs.workers was renamed to jobs.concurrency"},
		},
		{
			name:     "nested rename into another section that exists",
			file:     "log:\n  format: json\nlogging:\n  verbosity: debug\n",
			check:    func(c Config) bool { return c.Log.Level == "debug" && c.Log.Format == "json" },
			warnings: []string{"logging.verbosity was renamed to log.level"},
		},
		{
			name:     "nested rename into a section that is missing",
			file:     "logging:\n  verbosity: warn\n",
			check:    func(c Config) bool { return c.Log.Level == "warn" },
			warnings: []string{"logging.verbosity was renamed to log.level"},
		},
		{
			name:     "nested rename into an empty section",
			file:     "log:\nlogging:\n  verbosity: warn\n",
			check:    func(c Config) bool { return c.Log.Level == "warn" },
			warnings: []string{"logging.verbosity was renamed to log.level"},
		},
		{
			name:     "removal",
			file:     "backup:\n  window: \"02:00\"\n",
			check:    func(c Config) bool { return reflect.DeepEqual(c.Backup, Default().Backup) },
			warnings: []string{"config key backup.window was removed in v0.5.0 (backups follow each client group's schedule) and is ignored"},
		},
		{
			name:     "renamed twice",
			file:     "first_name: /srv/data\n",
			check:    func(c Config) bool { return c.DataDir == "/srv/data" },
			warnings: []string{"first_name was renamed to second_name", "second_name was renamed to data_dir"},
		},
		{
			name:    "old and new both set",
			file:    "listen: \":8000\"\nlisten_addr: \":9000\"\n",
			wantErr: ErrDeprecatedConflict,
			errText: "config keys listen_addr and listen are both set",
		},
		{
			name:    "nested old and new both set",
			file:    "jobs:\n  workers: 7\n  concurrency: 3\n",
			wantErr: ErrDeprecatedConflict,
			errText: "config keys jobs.workers and jobs.concurrency are both set",
		},
		{
			name:    "unknown key with a rename",
			file:    "listen_addr: \":9000\"\nlistne: \":1\"\n",
			errText: "field listne not found",
		},
		{
			name:    "unknown key without a rename",
			file:    "listne: \":1\"\n",
			errText: "field listne not found",
		},
		{
			name:  "nothing deprecated",
			file:  "listen: \":9000\"\n",
			check: func(c Config) bool { return c.Listen == ":9000" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pressfleet.yaml")
			if err := os.WriteFile(path, []byte(tt.file), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path, mapEnv(nil))
			if tt.errText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errText) {
					t.Fatalf("Load() error = %v, want it to contain %q", err, tt.errText)
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Errorf("Load() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if !tt.check(cfg) {
				t.Errorf("Load() = %+v, not as expected", cfg)
			}
			checkWarnings(t, cfg.Warnings, tt.warnings)
			for _, w := range cfg.Warnings {
				if strings.Contains(w, "renamed") && !strings.Contains(w, path) {
					t.Errorf("warning %q doesn't name the file %s", w, path)
				}
			}
		})
	}
}

func TestApplyEnvDeprecated(t *testing.T) {
	withDeprecated(t, nil, []deprecatedKey{
		{Old: "PRESSFLEET_ADDR", New: "PRESSFLEET_LISTEN", Since: "v0.5.0"},
		{Old: "PRESSFLEET_WORKERS", New: "PRESSFLEET_JOBS_CONCURRENCY", Since: "v0.5.0"},
		{Old: "PRESSFLEET_BACKUP_WINDOW", Since: "v0.5.0", Reason: "no longer used"},
		{Old: "PRESSFLEET_A", New: "PRESSFLEET_B", Since: "v0.4.0"},
		{Old: "PRESSFLEET_B", New: "PRESSFLEET_DATA_DIR", Since: "v0.5.0"},
	})
	tests := []struct {
		name     string
		vars     map[string]string
		check    func(Config) bool
		warnings []string
		errText  string
	}{
		{
			name:     "rename",
			vars:     map[string]string{"PRESSFLEET_ADDR": ":9000"},
			check:    func(c Config) bool { return c.Listen == ":9000" },
			warnings: []string{"environment variable PRESSFLEET_ADDR was renamed to PRESSFLEET_LISTEN in v0.5.0"},
		},
		{
			name:     "rename to a parsed variable",
			vars:     map[string]string{"PRESSFLEET_WORKERS": "6"},
			check:    func(c Config) bool { return c.Jobs.Concurrency == 6 },
			warnings: []string{"PRESSFLEET_WORKERS was renamed to PRESSFLEET_JOBS_CONCURRENCY"},
		},
		{
			name:     "removal",
			vars:     map[string]string{"PRESSFLEET_BACKUP_WINDOW": "02:00"},
			check:    func(c Config) bool { return reflect.DeepEqual(c.Backup, Default().Backup) },
			warnings: []string{"PRESSFLEET_BACKUP_WINDOW was removed in v0.5.0 (no longer used) and is ignored"},
		},
		{
			name:     "renamed twice",
			vars:     map[string]string{"PRESSFLEET_A": "/srv/data"},
			check:    func(c Config) bool { return c.DataDir == "/srv/data" },
			warnings: []string{"PRESSFLEET_A was renamed", "PRESSFLEET_B was renamed"},
		},
		{
			name:    "old and new both set",
			vars:    map[string]string{"PRESSFLEET_ADDR": ":9000", "PRESSFLEET_LISTEN": ":8000"},
			errText: "environment variables PRESSFLEET_ADDR and PRESSFLEET_LISTEN are both set",
		},
		{
			name:  "nothing deprecated",
			vars:  map[string]string{"PRESSFLEET_LISTEN": ":8000"},
			check: func(c Config) bool { return c.Listen == ":8000" },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			err := ApplyEnv(&cfg, mapEnv(tt.vars))
			if tt.errText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errText) {
					t.Fatalf("ApplyEnv() error = %v, want it to contain %q", err, tt.errText)
				}
				if !errors.Is(err, ErrDeprecatedConflict) {
					t.Errorf("ApplyEnv() error = %v, want %v", err, ErrDeprecatedConflict)
				}
				return
			}
			if err != nil {
				t.Fatalf("ApplyEnv() error = %v", err)
			}
			if !tt.check(cfg) {
				t.Errorf("ApplyEnv() = %+v, not as expected", cfg)
			}
			checkWarnings(t, cfg.Warnings, tt.warnings)
		})
	}
}

// TestDeprecatedTablesWellFormed checks the real tables.
func TestDeprecatedTablesWellFormed(t *testing.T) {
	for _, d := range append(append([]deprecatedKey{}, deprecatedKeys...), deprecatedEnvVars...) {
		if d.Old == "" || d.Since == "" {
			t.Errorf("row %+v needs Old and Since", d)
		}
		if d.removed() == (d.Reason == "") {
			t.Errorf("row %+v: a removal needs a Reason and a rename must not have one", d)
		}
	}
}

func checkWarnings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("warnings = %q, want %d", got, len(want))
	}
	for i := range want {
		if !strings.Contains(got[i], want[i]) {
			t.Errorf("warning %d = %q, want it to contain %q", i, got[i], want[i])
		}
	}
}
