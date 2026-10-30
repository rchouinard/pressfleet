package wpconfig

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func stagingParams() Params {
	p := testParams()
	p.Environment = EnvironmentStaging
	p.DisableMail = true
	return p
}

func TestRenderEnvironment(t *testing.T) {
	const (
		staging = "define( 'WP_ENVIRONMENT_TYPE', 'staging' );"
		noMail  = "define( 'PRESSFLEET_DISABLE_MAIL', true );"
	)
	for _, tc := range []struct {
		name        string
		environment string
		disableMail bool
		want        []string
	}{
		{"zero value", "", false, nil},
		{"explicit production", EnvironmentProduction, false, nil},
		{"staging", EnvironmentStaging, false, []string{staging}},
		{"staging without mail", EnvironmentStaging, true, []string{staging, noMail}},
		{"production without mail", EnvironmentProduction, true, []string{noMail}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testParams()
			p.Environment, p.DisableMail = tc.environment, tc.disableMail
			out, err := Render(p)
			if err != nil {
				t.Fatal(err)
			}
			s := string(out)
			for _, line := range []string{staging, noMail} {
				want := false
				for _, w := range tc.want {
					want = want || w == line
				}
				if got := strings.Contains(s, line); got != want {
					t.Errorf("%q present = %v, want %v", line, got, want)
				}
			}
			if section := strings.Contains(s, "** Environment **"); section != (len(tc.want) > 0) {
				t.Errorf("environment section present = %v", section)
			}
		})
	}
}

// TestRenderProductionUnchanged pins that production defaults render the
// same bytes as Params that predate the settings, which minimal.php.golden
// holds: a production site's file doesn't change, so there is no render
// version bump (design doc §4.7).
func TestRenderProductionUnchanged(t *testing.T) {
	want, err := Render(testParams())
	if err != nil {
		t.Fatal(err)
	}
	p := testParams()
	p.Environment = EnvironmentProduction
	got, err := Render(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("explicit production differs from the zero value")
	}
	// An agency's own WP_ENVIRONMENT_TYPE on a production site keeps working
	// (full.php.golden holds it).
	if _, err := Render(fullParams()); err != nil {
		t.Errorf("agency constant on a production site: %v", err)
	}
}

func TestRenderEnvironmentInvalid(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Params)
		want   string
	}{
		"unknown":      {func(p *Params) { p.Environment = "qa" }, "environment must be"},
		"case matters": {func(p *Params) { p.Environment = "Staging" }, "environment must be"},
		"agency constant on staging": {func(p *Params) {
			p.Environment = EnvironmentStaging
			p.Constants = []Constant{{Name: "wp_environment_type", Value: "local"}}
		}, "set by the platform on a staging site"},
		"agency mail constant": {func(p *Params) {
			p.Constants = []Constant{{Name: "PRESSFLEET_DISABLE_MAIL", Value: false}}
		}, "reserved for the platform"},
	} {
		t.Run(name, func(t *testing.T) {
			p := testParams()
			tc.mutate(&p)
			out, err := Render(p)
			if !errors.Is(err, ErrInvalidParams) || out != nil {
				t.Fatalf("out = %q, err = %v, want ErrInvalidParams", out, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
