package provisioner_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

func TestLogClassifier(t *testing.T) {
	type want struct {
		source provisioner.LogSource
		level  provisioner.LogLevel
	}
	tests := []struct {
		name      string
		component provisioner.Component
		lines     []string
		want      []want
	}{
		{"nginx access", provisioner.ComponentNginx, []string{
			`{"time":"2026-10-01T12:00:00+00:00","status":200,"uri":"/"}`,
			`{"time":"2026-10-01T12:00:00+00:00","status":404,"uri":"/x"}`,
			`{"time":"2026-10-01T12:00:00+00:00","status":502,"uri":"/y"}`,
		}, []want{{provisioner.LogAccess, provisioner.LevelInfo}, {provisioner.LogAccess, provisioner.LevelWarn}, {provisioner.LogAccess, provisioner.LevelError}}},
		{"nginx error", provisioner.ComponentNginx, []string{
			`2026/10/01 12:00:00 [error] 7#7: *1 connect() failed`,
			`2026/10/01 12:00:00 [warn] 7#7: low`,
			`2026/10/01 12:00:00 [notice] 7#7: start`,
			`something unexpected`,
		}, []want{{provisioner.LogError, provisioner.LevelError}, {provisioner.LogError, provisioner.LevelWarn}, {provisioner.LogError, provisioner.LevelInfo}, {provisioner.LogError, provisioner.LevelInfo}}},
		{"php", provisioner.ComponentPHP, []string{
			`PHP Fatal error:  Uncaught Error in /x.php:1`,
			`PHP Warning:  Undefined variable`,
			`[01-Oct-2026 12:00:00] NOTICE: fpm is running`,
			`[01-Oct-2026 12:00:00] WARNING: [pool www] child 5 said into stderr`,
			`hello from a plugin`,
		}, []want{{provisioner.LogPHP, provisioner.LevelError}, {provisioner.LogPHP, provisioner.LevelWarn}, {provisioner.LogPHP, provisioner.LevelInfo}, {provisioner.LogPHP, provisioner.LevelWarn}, {provisioner.LogPHP, provisioner.LevelInfo}}},
		{"slow log spans lines", provisioner.ComponentPHP, []string{
			`[01-Oct-2026 12:00:00]  [pool www] pid 42`,
			`script_filename = /var/www/wordpress/index.php`,
			`[0x00007f] sleep() /var/www/wordpress/wp-content/x.php:3`,
			`[0x00007f] {main}() /var/www/wordpress/index.php:0`,
			`back to ordinary output`,
		}, []want{{provisioner.LogSlow, provisioner.LevelWarn}, {provisioner.LogSlow, provisioner.LevelWarn}, {provisioner.LogSlow, provisioner.LevelWarn}, {provisioner.LogSlow, provisioner.LevelWarn}, {provisioner.LogPHP, provisioner.LevelInfo}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := provisioner.NewLogClassifier(tt.component)
			for i, line := range tt.lines {
				s, l := c.Classify(line)
				if s != tt.want[i].source || l != tt.want[i].level {
					t.Errorf("Classify(%q) = %s, %s; want %s, %s", line, s, l, tt.want[i].source, tt.want[i].level)
				}
			}
		})
	}
}

func TestLogOptionsValidate(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		opts provisioner.LogOptions
		ok   bool
	}{
		{"zero", provisioner.LogOptions{}, true},
		{"full", provisioner.LogOptions{Sources: []provisioner.LogSource{provisioner.LogSlow}, MinLevel: provisioner.LevelWarn, Tail: 5, Since: now, Until: now.Add(time.Hour)}, true},
		{"bad source", provisioner.LogOptions{Sources: []provisioner.LogSource{"x"}}, false},
		{"bad component", provisioner.LogOptions{Components: []provisioner.Component{"redis"}}, false},
		{"bad level", provisioner.LogOptions{MinLevel: "fatal"}, false},
		{"negative tail", provisioner.LogOptions{Tail: -1}, false},
		{"empty window", provisioner.LogOptions{Since: now, Until: now}, false},
		{"follow until", provisioner.LogOptions{Follow: true, Until: now}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.opts.Validate()
			if (err == nil) != tt.ok || (err != nil && !errors.Is(err, provisioner.ErrInvalidLogOptions)) {
				t.Errorf("Validate() = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

func TestSelectLogs(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// Newest first on purpose: merging containers interleaves entries.
	var in []provisioner.LogEntry
	for i, l := range []string{"a", "b", "B", "c", "d"} {
		in = append(in, provisioner.LogEntry{Time: base.Add(time.Duration(4-i) * time.Second), Line: l, Level: provisioner.LevelInfo})
	}
	var got []string
	for _, e := range provisioner.SelectLogs(in, provisioner.LogOptions{Contains: "b", Tail: 1}) {
		got = append(got, e.Line)
	}
	if !slices.Equal(got, []string{"b"}) {
		t.Errorf("got %v", got)
	}
}
