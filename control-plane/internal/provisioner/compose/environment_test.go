package compose_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

const (
	stagingLine = "define( 'WP_ENVIRONMENT_TYPE', 'staging' );"
	noMailLine  = "define( 'PRESSFLEET_DISABLE_MAIL', true );"
)

// TestRenderSiteProductionUnchanged pins that a production site renders the
// files it rendered before the environment settings existed. Production is
// the zero value or the explicit "production", with mail on: none of them
// may change a byte, so the settings need no render version bump and start
// no template rollout (design doc §4.7). TestRenderSiteGolden holds the
// bytes themselves.
func TestRenderSiteProductionUnchanged(t *testing.T) {
	base := testSiteRender()
	baseDir, _ := renderSite(t, base)
	want := readRendered(t, baseDir)
	if strings.Contains(want[compose.SiteWPConfigFile], "WP_ENVIRONMENT_TYPE") ||
		strings.Contains(want[compose.SiteWPConfigFile], "PRESSFLEET_DISABLE_MAIL") {
		t.Fatal("the default site renders the environment settings")
	}

	explicit := testSiteRender()
	explicit.Site.Environment = provisioner.EnvironmentProduction
	dir, _ := renderSite(t, explicit)
	got := readRendered(t, dir)
	for name, w := range want {
		if got[name] != w {
			t.Errorf("explicit production changes %s", name)
		}
	}
}

// TestUpdateSiteEnvironment checks that a change of the environment or the
// mail switch reaches the running site like the security set does (§6.1):
// wp-config.php is bind-mounted on its own, so php is recreated.
func TestUpdateSiteEnvironment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.prov.CreateSite(ctx, f.cfg); err != nil {
		t.Fatal(err)
	}
	wpConfig := func() string {
		b, err := os.ReadFile(filepath.Join(f.siteDir(t), compose.SiteWPConfigFile))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if got := wpConfig(); strings.Contains(got, stagingLine) || strings.Contains(got, noMailLine) {
		t.Fatal("a new production site renders the settings")
	}
	f.docker.calls = nil

	steps := []struct {
		name     string
		env      provisioner.Environment
		mail     bool
		staging  bool
		noMail   bool
		commands []string
	}{
		{"unchanged", "", false, false, false, []string{upWait}},
		{"to staging with mail off", provisioner.EnvironmentStaging, true, true, true, []string{recreate, upWait}},
		{"mail on again", provisioner.EnvironmentStaging, false, true, false, []string{recreate, upWait}},
		{"mail off again", provisioner.EnvironmentStaging, true, true, true, []string{recreate, upWait}},
		{"staging site unchanged", provisioner.EnvironmentStaging, true, true, true, []string{upWait}},
		{"back to production", provisioner.EnvironmentProduction, false, false, false, []string{recreate, upWait}},
		{"mail off on production", "", true, false, true, []string{recreate, upWait}},
	}
	for _, s := range steps {
		f.cfg.Environment, f.cfg.DisableMail = s.env, s.mail
		if _, err := f.prov.UpdateSite(ctx, f.cfg); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		got := wpConfig()
		if has := strings.Contains(got, stagingLine); has != s.staging {
			t.Errorf("%s: environment line present = %v, want %v", s.name, has, s.staging)
		}
		if has := strings.Contains(got, noMailLine); has != s.noMail {
			t.Errorf("%s: mail line present = %v, want %v", s.name, has, s.noMail)
		}
		wantCommands(t, f.docker, s.commands...)
	}
}

func TestCreateSiteInvalidEnvironment(t *testing.T) {
	f := newFixture(t)
	f.cfg.Environment = "qa"
	if _, err := f.prov.CreateSite(context.Background(), f.cfg); err == nil || !strings.Contains(err.Error(), "environment must be") {
		t.Errorf("err = %v, want an environment error", err)
	}
}
