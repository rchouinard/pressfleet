package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestUploadsSettings(t *testing.T) {
	def := config.Default().Uploads
	if def.MaxSizeBytes() != 10<<30 || def.Expiry() != 24*time.Hour {
		t.Errorf("default uploads = %+v", def)
	}
	path := writeFile(t, "uploads:\n  max_size_mb: 512\n")
	cfg, err := config.Load(path, env(map[string]string{"PRESSFLEET_UPLOADS_EXPIRY_HOURS": "2"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (config.UploadsConfig{MaxSizeMB: 512, ExpiryHours: 2}); cfg.Uploads != want {
		t.Errorf("Uploads = %+v, want %+v", cfg.Uploads, want)
	}
	if cfg.Uploads.MaxSizeBytes() != 512<<20 || cfg.Uploads.Expiry() != 2*time.Hour {
		t.Errorf("MaxSizeBytes = %d, Expiry = %s", cfg.Uploads.MaxSizeBytes(), cfg.Uploads.Expiry())
	}
	cfg, err = config.Load(path, env(map[string]string{"PRESSFLEET_UPLOADS_MAX_SIZE_MB": "20"}))
	if err != nil || cfg.Uploads.MaxSizeMB != 20 {
		t.Errorf("Load with PRESSFLEET_UPLOADS_MAX_SIZE_MB = %+v, %v", cfg.Uploads, err)
	}
	if _, err := config.Load(path, env(map[string]string{"PRESSFLEET_UPLOADS_MAX_SIZE_MB": "10G"})); err == nil ||
		!strings.Contains(err.Error(), "PRESSFLEET_UPLOADS_MAX_SIZE_MB") {
		t.Errorf("Load of a size that isn't a number = %v", err)
	}

	for _, tt := range []struct {
		name string
		edit func(*config.UploadsConfig)
		want string // empty: valid
	}{
		{"defaults", func(*config.UploadsConfig) {}, ""},
		{"smallest", func(u *config.UploadsConfig) { u.MaxSizeMB, u.ExpiryHours = 1, 1 }, ""},
		{"largest", func(u *config.UploadsConfig) { u.MaxSizeMB, u.ExpiryHours = 1<<20, 720 }, ""},
		{"no size", func(u *config.UploadsConfig) { u.MaxSizeMB = 0 }, "uploads.max_size_mb"},
		{"too large", func(u *config.UploadsConfig) { u.MaxSizeMB = 1<<20 + 1 }, "uploads.max_size_mb"},
		{"no expiry", func(u *config.UploadsConfig) { u.ExpiryHours = 0 }, "uploads.expiry_hours"},
		{"long expiry", func(u *config.UploadsConfig) { u.ExpiryHours = 721 }, "uploads.expiry_hours"},
	} {
		cfg := config.Default()
		tt.edit(&cfg.Uploads)
		err := cfg.Validate()
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: Validate() = %v, want nil", tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: Validate() = %v, want error mentioning %q", tt.name, err, tt.want)
		}
	}
}
