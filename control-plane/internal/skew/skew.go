// Package skew implements the version skew policy between the host CLI and
// the control plane (design doc §4.7):
//
//   - Patch versions within the same minor version are always compatible.
//   - Before 1.0, the minor versions must match.
//   - From 1.0, the CLI's minor version must be within one of the control
//     plane's, in the same major version.
package skew

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// Sentinel errors.
var (
	// ErrNotRelease means a version string isn't a semantic version, as
	// with development builds ("dev", a commit hash). The policy can't be
	// applied to such a build.
	ErrNotRelease = errors.New("not a release version")
	// ErrIncompatible means two versions are outside the policy. Check
	// returns it wrapped in an *Error.
	ErrIncompatible = errors.New("version skew outside the supported policy")
)

// Version is a semantic version. Build metadata is dropped.
type Version struct {
	Major, Minor, Patch int
	// Pre is the pre-release part without its leading '-', e.g. "rc.1" or,
	// for a `git describe` build, "3-gabc1234".
	Pre string
}

// semver matches a semantic version with an optional leading "v".
var semver = regexp.MustCompile(`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z.-]+))?(?:\+[0-9A-Za-z.-]+)?$`)

// Parse parses a version such as "v1.2.3" or "1.2.3-rc.1". It returns an
// error wrapping ErrNotRelease for anything else.
func Parse(s string) (Version, error) {
	m := semver.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("%q: %w", s, ErrNotRelease)
	}
	var v Version
	for i, dst := range []*int{&v.Major, &v.Minor, &v.Patch} {
		n, err := strconv.Atoi(m[i+1])
		if err != nil { // only on overflow
			return Version{}, fmt.Errorf("%q: %w", s, ErrNotRelease)
		}
		*dst = n
	}
	v.Pre = m[4]
	return v, nil
}

// String formats the version as "v1.2.3" or "v1.2.3-rc.1".
func (v Version) String() string {
	s := fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Error describes a CLI and control plane outside the policy. It wraps
// ErrIncompatible.
type Error struct {
	CLI, ControlPlane Version
}

func (e *Error) Error() string {
	return fmt.Sprintf("host CLI %s and control plane %s are incompatible: %s", e.CLI, e.ControlPlane, e.Rule())
}

func (e *Error) Unwrap() error { return ErrIncompatible }

// Rule states the part of the policy the two versions break.
func (e *Error) Rule() string {
	switch {
	case e.CLI.Major != e.ControlPlane.Major:
		return "their major versions must match"
	case e.ControlPlane.Major == 0:
		return "before 1.0, their minor versions must match"
	default:
		return "the CLI's minor version must be within one of the control plane's"
	}
}

// CLINewer reports whether the CLI is the newer of the two, in which case
// upgrading the control plane fixes the skew. Otherwise only a different
// CLI does: the control plane can't be downgraded.
func (e *Error) CLINewer() bool {
	if e.CLI.Major != e.ControlPlane.Major {
		return e.CLI.Major > e.ControlPlane.Major
	}
	return e.CLI.Minor > e.ControlPlane.Minor
}

// CompatibleCLIs describes the CLI versions that can manage the control
// plane, e.g. "v0.4.x" or "v1.3.x to v1.5.x".
func (e *Error) CompatibleCLIs() string {
	cp := e.ControlPlane
	if cp.Major == 0 {
		return fmt.Sprintf("v0.%d.x", cp.Minor)
	}
	return fmt.Sprintf("v%d.%d.x to v%d.%d.x", cp.Major, max(cp.Minor-1, 0), cp.Major, cp.Minor+1)
}

// Check applies the policy. It returns nil if the CLI can manage the
// control plane, and an *Error otherwise.
func Check(cli, controlPlane Version) error {
	ok := cli.Major == controlPlane.Major
	if ok && cli.Major == 0 {
		ok = cli.Minor == controlPlane.Minor
	} else if ok {
		ok = cli.Minor >= controlPlane.Minor-1 && cli.Minor <= controlPlane.Minor+1
	}
	if ok {
		return nil
	}
	return &Error{CLI: cli, ControlPlane: controlPlane}
}
