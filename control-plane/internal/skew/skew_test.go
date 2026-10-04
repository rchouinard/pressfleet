package skew_test

import (
	"errors"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/skew"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want skew.Version
		ok   bool
	}{
		{"v1.2.3", skew.Version{Major: 1, Minor: 2, Patch: 3}, true},
		{"1.2.3", skew.Version{Major: 1, Minor: 2, Patch: 3}, true},
		{"v0.4.0-rc.1", skew.Version{Minor: 4, Pre: "rc.1"}, true},
		{"v1.2.3-test", skew.Version{Major: 1, Minor: 2, Patch: 3, Pre: "test"}, true},
		{"v0.1.0-3-gabc1234-dirty", skew.Version{Minor: 1, Pre: "3-gabc1234-dirty"}, true},
		{"v1.2.3+build.5", skew.Version{Major: 1, Minor: 2, Patch: 3}, true},
		{"dev", skew.Version{}, false},
		{"", skew.Version{}, false},
		{"e2e", skew.Version{}, false},
		{"abc1234", skew.Version{}, false},
		{"v1.2", skew.Version{}, false},
		{"v01.2.3", skew.Version{}, false},
		{"v1.2.3.4", skew.Version{}, false},
	}
	for _, tt := range tests {
		got, err := skew.Parse(tt.in)
		if tt.ok != (err == nil) || got != tt.want {
			t.Errorf("Parse(%q) = %+v, %v", tt.in, got, err)
		}
		if !tt.ok && !errors.Is(err, skew.ErrNotRelease) {
			t.Errorf("Parse(%q) error %v doesn't wrap ErrNotRelease", tt.in, err)
		}
	}
}

func TestVersionString(t *testing.T) {
	for in, want := range map[string]string{"1.2.3": "v1.2.3", "v0.4.0-rc.1": "v0.4.0-rc.1", "v1.0.0+meta": "v1.0.0"} {
		v, err := skew.Parse(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String(); got != want {
			t.Errorf("Parse(%q).String() = %q, want %q", in, got, want)
		}
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		name, cli, cp string
		ok            bool
		cliNewer      bool
		compatible    string
	}{
		{name: "same version", cli: "v0.3.1", cp: "v0.3.1", ok: true},
		{name: "patch skew, CLI newer", cli: "v0.3.4", cp: "v0.3.1", ok: true},
		{name: "patch skew, CLI older", cli: "v1.3.0", cp: "v1.3.9", ok: true},
		{name: "pre-release of the same minor", cli: "v0.4.0-rc.1", cp: "v0.4.2", ok: true},
		{name: "pre-1.0 minor ahead", cli: "v0.4.0", cp: "v0.3.9", cliNewer: true, compatible: "v0.3.x"},
		{name: "pre-1.0 minor behind", cli: "v0.2.5", cp: "v0.3.0", compatible: "v0.3.x"},
		{name: "post-1.0 minor one ahead", cli: "v1.5.0", cp: "v1.4.2", ok: true},
		{name: "post-1.0 minor one behind", cli: "v1.3.7", cp: "v1.4.0", ok: true},
		{name: "post-1.0 minor two ahead", cli: "v1.6.0", cp: "v1.4.0", cliNewer: true, compatible: "v1.3.x to v1.5.x"},
		{name: "post-1.0 minor two behind", cli: "v1.2.0", cp: "v1.4.0", compatible: "v1.3.x to v1.5.x"},
		{name: "post-1.0 at minor zero", cli: "v1.2.0", cp: "v1.0.0", cliNewer: true, compatible: "v1.0.x to v1.1.x"},
		{name: "1.0 CLI, 0.x control plane", cli: "v1.0.0", cp: "v0.9.0", cliNewer: true, compatible: "v0.9.x"},
		{name: "major behind", cli: "v1.9.0", cp: "v2.0.0", compatible: "v2.0.x to v2.1.x"},
		{name: "major ahead", cli: "v3.0.0", cp: "v2.9.0", cliNewer: true, compatible: "v2.8.x to v2.10.x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cli, err := skew.Parse(tt.cli)
			if err != nil {
				t.Fatal(err)
			}
			cp, err := skew.Parse(tt.cp)
			if err != nil {
				t.Fatal(err)
			}
			err = skew.Check(cli, cp)
			if tt.ok {
				if err != nil {
					t.Fatalf("Check = %v, want nil", err)
				}
				return
			}
			var se *skew.Error
			if !errors.As(err, &se) || !errors.Is(err, skew.ErrIncompatible) {
				t.Fatalf("Check = %v, want a *skew.Error wrapping ErrIncompatible", err)
			}
			if se.CLINewer() != tt.cliNewer {
				t.Errorf("CLINewer = %v, want %v", se.CLINewer(), tt.cliNewer)
			}
			if got := se.CompatibleCLIs(); got != tt.compatible {
				t.Errorf("CompatibleCLIs = %q, want %q", got, tt.compatible)
			}
		})
	}
}

func TestErrorRule(t *testing.T) {
	tests := []struct{ cli, cp, want string }{
		{"v1.0.0", "v0.9.0", "their major versions must match"},
		{"v0.2.0", "v0.3.0", "before 1.0, their minor versions must match"},
		{"v1.1.0", "v1.3.0", "the CLI's minor version must be within one of the control plane's"},
	}
	for _, tt := range tests {
		cli, _ := skew.Parse(tt.cli)
		cp, _ := skew.Parse(tt.cp)
		var se *skew.Error
		if !errors.As(skew.Check(cli, cp), &se) {
			t.Fatalf("%s vs %s: no *skew.Error", tt.cli, tt.cp)
		}
		if got := se.Rule(); got != tt.want {
			t.Errorf("%s vs %s: Rule = %q, want %q", tt.cli, tt.cp, got, tt.want)
		}
	}
}
