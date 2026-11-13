package compose

import (
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

func TestWPCLIArgv(t *testing.T) {
	prefix := []string{"exec", "--no-TTY", "--user", "20001:20001", "--", "php"}
	tests := []struct {
		name    string
		timeout time.Duration
		args    []string
		secs    string
	}{
		{"no args", 10 * time.Second, nil, "11"},
		{"rounds up", 2500 * time.Millisecond, []string{"option", "get", "home"}, "4"},
		{"expired", -time.Second, []string{"--info"}, "1"},
		{"hostile args stay separate and unchanged", time.Minute,
			[]string{"--", "--user=0", "; rm -rf /", "$(id)", "`id`", "a\nb", "-T", ""}, "61"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wpCLIArgv("20001:20001", "call1", tt.timeout, tt.args)
			want := slices.Concat(prefix, []string{"sh", "-c", got[8], "pressfleet-wpcli", "call1", tt.secs, "10", "/usr/local/bin/wp"}, tt.args)
			if !slices.Equal(got, want) {
				t.Errorf("argv = %q, want %q", got, want)
			}
			// The script is the one the provisioner package tests.
			if run := provisioner.WPCLIRunArgv("call1", tt.timeout, "x"); got[8] != run[2] {
				t.Error("the script differs from provisioner.WPCLIRunArgv's")
			}
		})
	}
}

func TestTimedArgv(t *testing.T) {
	got := timedArgv("20001:20001", 2500*time.Millisecond, "php", "-r", "x")
	want := []string{"exec", "--no-TTY", "--user", "20001:20001", "--", "php", "timeout", "--kill-after=10s", "4s", "php", "-r", "x"}
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
}
