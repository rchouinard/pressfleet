package e2e_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
)

// securityConstants prints the security set's wp-config.php constants as
// PHP sees them, one "NAME=value" line each ("-" when undefined).
const securityConstants = `foreach (array("DISALLOW_FILE_EDIT", "PRESSFLEET_BLOCK_USER_ENUMERATION", "PRESSFLEET_PASSWORD_POLICY_MIN_LENGTH",
	"PRESSFLEET_PASSWORD_HASHING", "PRESSFLEET_DISABLE_APPLICATION_PASSWORDS", "PRESSFLEET_REQUIRE_TWO_FACTOR") as $c) {
	echo $c, "=", defined($c) ? var_export(constant($c), true) : "-", "\n";
}`

// TestSecuritySetReachesExistingSites is the acceptance test of #108's
// "a change reaches existing sites after a control plane restart" (design
// doc §6.1), and of the control plane half of #109: a site provisioned with
// the default security set gets the new settings, in recreated php and
// nginx containers, once the control plane restarts with changed
// configuration.
func TestSecuritySetReachesExistingSites(t *testing.T) {
	// See TestSiteCreateDelete for why the site project uses an empty
	// directory and is cleaned up here.
	projectDir := t.TempDir()
	var siteProject compose.Project
	t.Cleanup(func() {
		if siteProject.Name == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if res, err := siteProject.Resources(ctx); err == nil && !res.Empty() {
			if err := siteProject.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
				t.Errorf("removing site project %s: %v", siteProject.Name, err)
			}
		}
	})
	p := e2e.Start(t)
	ctx := t.Context()

	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	php, ok := cat.DefaultPHPVersion(time.Now())
	if !ok {
		t.Fatal("no default PHP version")
	}
	images, err := cat.Images(php, cat.LatestWordPress().Version)
	if err != nil {
		t.Fatal(err)
	}
	e2e.EnsureImages(t, images.PHP, images.Nginx)

	created := createSite(t, p, createGroup(t, p, "Security Agency"), "Security Site", php)
	site := created.Site
	siteProject = compose.Project{Name: compose.SiteProjectName(site.ID), Dir: projectDir}
	waitJob(t, p, created.Job.ID)

	constants := func() string {
		t.Helper()
		out, err := siteProject.ExecService(ctx, "php", "wp", "eval", securityConstants)
		if err != nil {
			t.Fatalf("reading wp-config.php constants: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	container := func(service string) string {
		t.Helper()
		c, err := siteProject.ServiceContainer(ctx, service)
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	// The nginx half (#109) is the site-nginx container's environment.
	nginxEnv := func() string {
		t.Helper()
		out, err := siteProject.ExecService(ctx, "nginx", "printenv",
			"NGINX_LOGIN_RATE_LIMIT", "NGINX_LOGIN_RATE", "NGINX_LOGIN_BURST", "NGINX_SECURITY_HEADERS")
		if err != nil {
			t.Fatalf("reading the nginx environment: %v", err)
		}
		return strings.Join(strings.Fields(string(out)), " ")
	}

	// The §6.1 defaults.
	if got, want := constants(), strings.Join([]string{
		"DISALLOW_FILE_EDIT=true",
		"PRESSFLEET_BLOCK_USER_ENUMERATION=true",
		"PRESSFLEET_PASSWORD_POLICY_MIN_LENGTH=12",
		"PRESSFLEET_PASSWORD_HASHING=-",
		"PRESSFLEET_DISABLE_APPLICATION_PASSWORDS=-",
		"PRESSFLEET_REQUIRE_TWO_FACTOR=-",
	}, "\n"); got != want {
		t.Errorf("constants after provisioning:\n%s\nwant:\n%s", got, want)
	}
	if got, want := nginxEnv(), "on 10r/m 10 on"; got != want {
		t.Errorf("nginx environment after provisioning = %q, want %q", got, want)
	}
	phpBefore, nginxBefore := container("php"), container("nginx")
	// Two-factor is off by default, so the site has no Two Factor plugin.
	wp := func(args ...string) (string, error) {
		t.Helper()
		out, err := siteProject.ExecService(ctx, "php", append([]string{"wp"}, args...)...)
		return strings.TrimSpace(string(out)), err
	}
	if _, err := wp("plugin", "is-installed", "two-factor"); err == nil {
		t.Error("the Two Factor plugin is installed although two-factor is off")
	}

	p.Restart(t, map[string]string{
		"PRESSFLEET_WORDPRESS_SECURITY_DISABLE_FILE_EDIT":             "false",
		"PRESSFLEET_WORDPRESS_SECURITY_PASSWORD_POLICY_MIN_LENGTH":    "16",
		"PRESSFLEET_WORDPRESS_SECURITY_PASSWORD_HASHING":              "argon2id",
		"PRESSFLEET_WORDPRESS_SECURITY_DISABLE_APPLICATION_PASSWORDS": "true",
		"PRESSFLEET_WORDPRESS_SECURITY_LOGIN_RATE_LIMIT_BURST":        "5",
		"PRESSFLEET_WORDPRESS_SECURITY_HEADERS":                       "false",
		"PRESSFLEET_WORDPRESS_SECURITY_TWO_FACTOR":                    "required_for_admins",
	})
	waitJob(t, p, reconcileJob(t, p, site.ID))

	if got, want := constants(), strings.Join([]string{
		"DISALLOW_FILE_EDIT=-",
		"PRESSFLEET_BLOCK_USER_ENUMERATION=true",
		"PRESSFLEET_PASSWORD_POLICY_MIN_LENGTH=16",
		"PRESSFLEET_PASSWORD_HASHING='argon2id'",
		"PRESSFLEET_DISABLE_APPLICATION_PASSWORDS=true",
		"PRESSFLEET_REQUIRE_TWO_FACTOR=true",
	}, "\n"); got != want {
		t.Errorf("constants after the restart:\n%s\nwant:\n%s", got, want)
	}
	if container("php") == phpBefore {
		t.Error("php was not recreated, so it can't see the new wp-config.php")
	}
	if got, want := nginxEnv(), "on 10r/m 5 off"; got != want {
		t.Errorf("nginx environment after the restart = %q, want %q", got, want)
	}
	if container("nginx") == nginxBefore {
		t.Error("nginx was not recreated")
	}
	// Required two-factor (#43): reconciling installed and activated the
	// Two Factor plugin, and the administrator provisioning created must
	// now set up a second factor.
	if _, err := wp("plugin", "is-active", "two-factor"); err != nil {
		t.Errorf("the Two Factor plugin isn't active after requiring two-factor: %v", err)
	}
	admin, err := wp("user", "list", "--role=administrator", "--field=user_login")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := wp("eval", fmt.Sprintf(`echo pressfleet_two_factor_setup_required( get_user_by( "login", %q ) ) ? "yes" : "no";`, admin)); err != nil || got != "yes" {
		t.Errorf("administrator %s must set up two-factor = %q (%v), want yes", admin, got, err)
	}
	if got := getSite(t, p, site.ID); got.Status != client.SiteStatusRunning {
		t.Errorf("site after reconciling: %+v", got)
	}
}

// reconcileJob waits for the control plane to queue the site's
// site.reconcile job after a restart and returns its ID.
func reconcileJob(t *testing.T, p *e2e.Platform, siteID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for {
		resp, err := p.Client.ListJobsWithResponse(ctx, &client.ListJobsParams{TargetID: &siteID})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON200 == nil {
			t.Fatalf("list jobs: status %d: %s", resp.StatusCode(), resp.Body)
		}
		for _, j := range resp.JSON200.Items {
			if j.Type == sites.ReconcileJobType {
				return j.ID
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no %s job for site %s", sites.ReconcileJobType, siteID)
		case <-time.After(time.Second):
		}
	}
}
