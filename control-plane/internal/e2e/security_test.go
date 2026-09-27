package e2e_test

import (
	"context"
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
	"PRESSFLEET_PASSWORD_HASHING", "PRESSFLEET_DISABLE_APPLICATION_PASSWORDS") as $c) {
	echo $c, "=", defined($c) ? var_export(constant($c), true) : "-", "\n";
}`

// TestSecuritySetReachesExistingSites is the acceptance test of #108's
// "a change reaches existing sites after a control plane restart" (design
// doc §6.1): a site provisioned with the default security set gets the new
// settings, in a recreated php container, once the control plane restarts
// with changed configuration.
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
	phpContainer := func() string {
		t.Helper()
		c, err := siteProject.ServiceContainer(ctx, "php")
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}

	// The §6.1 defaults.
	if got, want := constants(), strings.Join([]string{
		"DISALLOW_FILE_EDIT=true",
		"PRESSFLEET_BLOCK_USER_ENUMERATION=true",
		"PRESSFLEET_PASSWORD_POLICY_MIN_LENGTH=12",
		"PRESSFLEET_PASSWORD_HASHING=-",
		"PRESSFLEET_DISABLE_APPLICATION_PASSWORDS=-",
	}, "\n"); got != want {
		t.Errorf("constants after provisioning:\n%s\nwant:\n%s", got, want)
	}
	phpBefore := phpContainer()

	p.Restart(t, map[string]string{
		"PRESSFLEET_WORDPRESS_SECURITY_DISABLE_FILE_EDIT":             "false",
		"PRESSFLEET_WORDPRESS_SECURITY_PASSWORD_POLICY_MIN_LENGTH":    "16",
		"PRESSFLEET_WORDPRESS_SECURITY_PASSWORD_HASHING":              "argon2id",
		"PRESSFLEET_WORDPRESS_SECURITY_DISABLE_APPLICATION_PASSWORDS": "true",
	})
	waitJob(t, p, reconcileJob(t, p, site.ID))

	if got, want := constants(), strings.Join([]string{
		"DISALLOW_FILE_EDIT=-",
		"PRESSFLEET_BLOCK_USER_ENUMERATION=true",
		"PRESSFLEET_PASSWORD_POLICY_MIN_LENGTH=16",
		"PRESSFLEET_PASSWORD_HASHING='argon2id'",
		"PRESSFLEET_DISABLE_APPLICATION_PASSWORDS=true",
	}, "\n"); got != want {
		t.Errorf("constants after the restart:\n%s\nwant:\n%s", got, want)
	}
	if phpContainer() == phpBefore {
		t.Error("php was not recreated, so it can't see the new wp-config.php")
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
