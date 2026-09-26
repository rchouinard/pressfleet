package compose

import (
	"slices"
	"strings"
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

func TestHeadBuffer(t *testing.T) {
	b := &headBuffer{max: 5}
	for _, s := range []string{"ab", "cdef", "gh"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got := string(b.buf); got != "abcde" {
		t.Errorf("kept %q, want abcde", got)
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 4}
	if _, _ = b.Write([]byte("ab")); b.String() != "ab" {
		t.Errorf("short = %q", b.String())
	}
	for range 10 {
		if n, err := b.Write([]byte("0123456789")); n != 10 || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	got := b.String()
	if !strings.HasSuffix(got, "\n6789") || !strings.HasPrefix(got, "[98 bytes truncated]") {
		t.Errorf("String() = %q", got)
	}
	if len(b.buf) > 2*b.max {
		t.Errorf("buffer holds %d bytes, limit %d", len(b.buf), b.max)
	}
}
