package buildinfo

import (
	"runtime"
	"runtime/debug"
	"testing"
)

func TestRead(t *testing.T) {
	got := Read("", "")
	if got.Version != "dev" {
		t.Errorf("Version = %q, want dev", got.Version)
	}
	if got.GoVersion != runtime.Version() || got.OS != runtime.GOOS || got.Arch != runtime.GOARCH {
		t.Errorf("toolchain = %s %s/%s", got.GoVersion, got.OS, got.Arch)
	}

	got = Read("v1.2.3", "0123456789abcdef")
	if got.Version != "v1.2.3" || got.Commit != "0123456789abcdef" || got.Modified != nil {
		t.Errorf("ldflags build = %+v", got)
	}
}

func TestFromSettings(t *testing.T) {
	tests := []struct {
		name     string
		settings []debug.BuildSetting
		commit   string
		modified *bool
	}{
		{name: "no vcs"},
		{
			name:     "clean",
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}, {Key: "vcs.modified", Value: "false"}},
			commit:   "abc", modified: new(false),
		},
		{
			name:     "dirty",
			settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}, {Key: "vcs.modified", Value: "true"}},
			commit:   "abc", modified: new(true),
		},
		{
			name:     "modified without revision",
			settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var i Info
			i.fromSettings(tt.settings)
			if i.Commit != tt.commit {
				t.Errorf("Commit = %q, want %q", i.Commit, tt.commit)
			}
			if (i.Modified == nil) != (tt.modified == nil) || (i.Modified != nil && *i.Modified != *tt.modified) {
				t.Errorf("Modified = %v, want %v", i.Modified, tt.modified)
			}
		})
	}
}

func TestString(t *testing.T) {
	dirty := true
	tests := []struct {
		info Info
		want string
	}{
		{Info{Version: "dev", GoVersion: "go1.27.1", OS: "linux", Arch: "amd64"}, "pressfleet dev (go1.27.1, linux/amd64)"},
		{
			Info{Version: "v1.0.0", Commit: "0123456789abcdef", Modified: &dirty, GoVersion: "go1.27.1", OS: "linux", Arch: "amd64"},
			"pressfleet v1.0.0 (commit 0123456789ab-dirty, go1.27.1, linux/amd64)",
		},
	}
	for _, tt := range tests {
		if got := tt.info.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}
