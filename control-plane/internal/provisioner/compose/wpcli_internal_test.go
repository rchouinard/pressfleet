package compose

import (
	"slices"
	"testing"
	"time"
)

func TestWPCLIArgv(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		args    []string
		want    []string
	}{
		{"no args", 10 * time.Second, nil,
			[]string{"exec", "--no-TTY", "--user", "20001:20001", "--", "php", "timeout", "--kill-after=10s", "11s", "/usr/local/bin/wp"}},
		{"rounds up", 2500 * time.Millisecond, []string{"option", "get", "home"},
			[]string{"exec", "--no-TTY", "--user", "20001:20001", "--", "php", "timeout", "--kill-after=10s", "4s", "/usr/local/bin/wp", "option", "get", "home"}},
		{"expired", -time.Second, []string{"--info"},
			[]string{"exec", "--no-TTY", "--user", "20001:20001", "--", "php", "timeout", "--kill-after=10s", "1s", "/usr/local/bin/wp", "--info"}},
		{"hostile args stay separate and unchanged", time.Minute,
			[]string{"--", "--user=0", "; rm -rf /", "$(id)", "`id`", "a\nb", "-T", ""},
			[]string{"exec", "--no-TTY", "--user", "20001:20001", "--", "php", "timeout", "--kill-after=10s", "61s", "/usr/local/bin/wp",
				"--", "--user=0", "; rm -rf /", "$(id)", "`id`", "a\nb", "-T", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := wpCLIArgv("20001:20001", tt.timeout, tt.args); !slices.Equal(got, tt.want) {
				t.Errorf("argv = %q, want %q", got, tt.want)
			}
		})
	}
}
