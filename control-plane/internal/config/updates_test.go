package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestUpdatesSettings(t *testing.T) {
	def := config.Default()
	if u := def.Updates; u.HasWindow() || u.MaxFailures != 3 || u.Core.MajorAutoApproveDays != 0 || u.Core.HoldWarningDays != 90 || u.Images.CheckHours != 6 {
		t.Errorf("default updates = %+v", u)
	}
	if def.ImageManifestFile != "" {
		t.Errorf("default image_manifest_file = %q", def.ImageManifestFile)
	}
	path := writeFile(t, "updates:\n  window_start: \"01:30\"\n  window_hours: 3\n  core:\n    major_auto_approve_days: 14\n"+
		"image_manifest_file: /etc/pressfleet/manifest.json\n")
	cfg, err := config.Load(path, env(map[string]string{
		"PRESSFLEET_UPDATES_MAX_FAILURES":           "5",
		"PRESSFLEET_UPDATES_CORE_HOLD_WARNING_DAYS": "30",
		"PRESSFLEET_UPDATES_IMAGES_CHECK_HOURS":     "0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	u := cfg.Updates
	want := config.UpdatesConfig{WindowStart: "01:30", WindowHours: 3, MaxFailures: 5,
		Core: config.CoreUpdatesConfig{MajorAutoApproveDays: 14, HoldWarningDays: 30}, Images: config.ImageUpdatesConfig{CheckHours: 0}}
	if u != want || !u.HasWindow() || u.Start() != 90*time.Minute || u.Window() != 3*time.Hour {
		t.Errorf("Updates = %+v, want %+v", u, want)
	}
	if cfg.ImageManifestFile != "/etc/pressfleet/manifest.json" {
		t.Errorf("image_manifest_file = %q", cfg.ImageManifestFile)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	cfg, err = config.Load(path, env(map[string]string{
		"PRESSFLEET_UPDATES_WINDOW_START":                 "",
		"PRESSFLEET_UPDATES_WINDOW_HOURS":                 "6",
		"PRESSFLEET_UPDATES_CORE_MAJOR_AUTO_APPROVE_DAYS": "0",
		"PRESSFLEET_IMAGE_MANIFEST_FILE":                  "/srv/manifest.json",
	}))
	if err != nil || cfg.Updates.HasWindow() || cfg.Updates.WindowHours != 6 || cfg.Updates.Core.MajorAutoApproveDays != 0 ||
		cfg.ImageManifestFile != "/srv/manifest.json" {
		t.Errorf("from env: %+v, %q, %v", cfg.Updates, cfg.ImageManifestFile, err)
	}
	if _, err := config.Load(path, env(map[string]string{"PRESSFLEET_UPDATES_MAX_FAILURES": "three"})); err == nil {
		t.Error("Load accepted max_failures that isn't a number")
	}

	for _, tt := range []struct {
		name   string
		change func(*config.Config)
		want   string // empty: valid
	}{
		{"no window", func(c *config.Config) { c.Updates.WindowStart = "" }, ""},
		{"window", func(c *config.Config) { c.Updates.WindowStart, c.Updates.WindowHours = "23:00", 24 }, ""},
		{"bad window start", func(c *config.Config) { c.Updates.WindowStart = "3:00" }, "updates.window_start"},
		{"window too long", func(c *config.Config) { c.Updates.WindowHours = 25 }, "updates.window_hours"},
		{"no failures allowed", func(c *config.Config) { c.Updates.MaxFailures = 0 }, "updates.max_failures"},
		{"negative delay", func(c *config.Config) { c.Updates.Core.MajorAutoApproveDays = -1 }, "updates.core.major_auto_approve_days"},
		{"no hold warning", func(c *config.Config) { c.Updates.Core.HoldWarningDays = 0 }, "updates.core.hold_warning_days"},
		{"weekly image checks", func(c *config.Config) { c.Updates.Images.CheckHours = 168 }, ""},
		{"image checks too rare", func(c *config.Config) { c.Updates.Images.CheckHours = 169 }, "updates.images.check_hours"},
		{"negative image checks", func(c *config.Config) { c.Updates.Images.CheckHours = -1 }, "updates.images.check_hours"},
		{"relative manifest", func(c *config.Config) { c.ImageManifestFile = "manifest.json" }, "image_manifest_file"},
	} {
		cfg := config.Default()
		tt.change(&cfg)
		err := cfg.Validate()
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: Validate() = %v, want nil", tt.name, err)
		case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
			t.Errorf("%s: Validate() = %v, want error mentioning %q", tt.name, err, tt.want)
		}
	}
}
