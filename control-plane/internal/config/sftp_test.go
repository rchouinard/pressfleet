package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

func TestSFTPSettings(t *testing.T) {
	d := config.Default().SFTP
	if d.Port != 2222 || d.Listen != ":2222" || !d.Enabled() || d.IdleTimeout() != 15*time.Minute || d.MaxSessionLength() != 8*time.Hour {
		t.Errorf("default SFTP config = %+v", d)
	}
	path := writeFile(t, "sftp:\n  port: 2022\n  idle_timeout_minutes: 5\n")
	cfg, err := config.Load(path, env(map[string]string{
		"PRESSFLEET_SFTP_MAX_SESSION_HOURS": "2",
		"PRESSFLEET_SFTP_LISTEN":            "",
		"PRESSFLEET_SFTP_HOST_KEY_FILE":     "/etc/pressfleet/ssh_host_key",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := config.SFTPConfig{Port: 2022, HostKeyFile: "/etc/pressfleet/ssh_host_key", IdleTimeoutMinutes: 5, MaxSessionHours: 2}
	if cfg.SFTP != want || cfg.SFTP.Enabled() {
		t.Errorf("SFTP config = %+v, want %+v", cfg.SFTP, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
	if _, err := config.Load(path, env(map[string]string{"PRESSFLEET_SFTP_PORT": "ssh"})); err == nil {
		t.Error("non-numeric PRESSFLEET_SFTP_PORT accepted")
	}
	tests := []struct {
		name string
		edit func(*config.SFTPConfig)
		want string
	}{
		{"port zero", func(s *config.SFTPConfig) { s.Port = 0 }, "sftp.port"},
		{"port too big", func(s *config.SFTPConfig) { s.Port = 70000 }, "sftp.port"},
		{"port of Traefik", func(s *config.SFTPConfig) { s.Port = 443 }, "sftp.port"},
		{"bad listen", func(s *config.SFTPConfig) { s.Listen = "2222" }, "sftp.listen"},
		{"relative host key", func(s *config.SFTPConfig) { s.HostKeyFile = "key" }, "sftp.host_key_file"},
		{"no idle timeout", func(s *config.SFTPConfig) { s.IdleTimeoutMinutes = 0 }, "sftp.idle_timeout_minutes"},
		{"long idle timeout", func(s *config.SFTPConfig) { s.IdleTimeoutMinutes = config.MaxSFTPIdleTimeoutMinutes + 1 }, "sftp.idle_timeout_minutes"},
		{"no session length", func(s *config.SFTPConfig) { s.MaxSessionHours = 0 }, "sftp.max_session_hours"},
		{"long sessions", func(s *config.SFTPConfig) { s.MaxSessionHours = config.MaxSFTPSessionHours + 1 }, "sftp.max_session_hours"},
	}
	for _, tt := range tests {
		cfg := config.Default()
		tt.edit(&cfg.SFTP)
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: Validate() = %v, want an error about %s", tt.name, err, tt.want)
		}
	}
}
