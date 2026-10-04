// Package buildinfo describes the running binary: its release version and
// the commit, toolchain, and platform it was built from.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Info is the build of the running binary.
type Info struct {
	// Version is the release version set at build time, or "dev".
	Version string
	// Commit is the full commit hash, or empty when unknown.
	Commit string
	// Modified says whether the working tree had uncommitted changes; nil
	// when unknown.
	Modified  *bool
	GoVersion string
	OS        string
	Arch      string
}

// Read returns the build of the running binary. version and commit are the
// values set with -ldflags. Image builds set the commit that way because
// they build with -buildvcs=false; otherwise the commit and its modified
// state come from the VCS stamp Go embeds.
func Read(version, commit string) Info {
	info := Info{Version: version, Commit: commit, GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if info.Version == "" {
		info.Version = "dev"
	}
	if info.Commit != "" {
		return info
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		info.fromSettings(bi.Settings)
	}
	return info
}

func (i *Info) fromSettings(settings []debug.BuildSetting) {
	var modified string
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			i.Commit = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if i.Commit != "" && modified != "" {
		m := modified == "true"
		i.Modified = &m
	}
}

// ShortCommit returns the first 12 characters of the commit, with "-dirty"
// when the tree was modified, or empty when the commit is unknown.
func (i Info) ShortCommit() string {
	if i.Commit == "" {
		return ""
	}
	c := i.Commit[:min(len(i.Commit), 12)]
	if i.Modified != nil && *i.Modified {
		c += "-dirty"
	}
	return c
}

// String formats the build on one line, as `pressfleet version` prints it.
func (i Info) String() string {
	details := ""
	if c := i.ShortCommit(); c != "" {
		details = "commit " + c + ", "
	}
	return fmt.Sprintf("pressfleet %s (%s%s, %s/%s)", i.Version, details, i.GoVersion, i.OS, i.Arch)
}
