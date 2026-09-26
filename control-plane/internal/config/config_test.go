package config_test

import (
	"errors"
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
		DataDir: "/srv/file",                                      // file overrides default
		Listen:  ":9999",                                          // env overrides file
		Log:     config.LogConfig{Level: "debug", Format: "json"}, // file and env
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
	for _, want := range []string{"data_dir", "listen", "log.level", "log.format"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}
