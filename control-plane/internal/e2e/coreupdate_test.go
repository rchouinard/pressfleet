package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/images"
)

// TestCoreUpdate checks the WordPress core rollout (#57) with real
// containers. Real newer WordPress releases would have to be downloaded
// and built, so the test derives stand-ins from the local site images of
// the manifest's latest release L, under a registry of its own:
//
//   - "L" is L retagged;
//   - "7.x.y+1" (the "good" release) is L with wp-includes/version.php
//     claiming the new version, in both images;
//   - "7.x.y+2" (the "broken" one) is L with a higher database schema
//     version ($wp_db_version) but still claiming L: `wp core update-db`
//     upgrades the database, and the health check then finds the wrong
//     core, as it would for a mislabeled image.
//
// The control plane gets a version manifest with these releases
// (image_manifest_file) and is restarted; at startup it queues a rollout
// pass, which updates the site as a minor release. The good release must
// reach both containers, and the site must pass its checks on it; the
// broken one must be reverted automatically, restoring the pre-update
// backup because the schema changed.
func TestCoreUpdate(t *testing.T) {
	projectDir := t.TempDir()
	var projects []compose.Project
	var tags []string
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, proj := range projects {
			if res, err := proj.Resources(ctx); err == nil && !res.Empty() {
				t.Logf("removing leftover site project %s", proj.Name)
				if err := proj.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
					t.Errorf("removing leftover site project %s: %v", proj.Name, err)
				}
			}
		}
		for _, tag := range tags {
			if err := compose.RemoveImage(ctx, nil, tag); err != nil {
				t.Errorf("removing %s: %v", tag, err)
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
	latest := cat.LatestWordPress().Version
	base, err := cat.Images(php, latest)
	if err != nil {
		t.Fatal(err)
	}
	e2e.EnsureImages(t, base.PHP, base.Nginx)

	// The stand-in releases.
	parts := strings.Split(latest, ".")
	if len(parts) != 3 {
		t.Fatalf("the latest release %s isn't x.y.z", latest)
	}
	z, err := strconv.Atoi(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	patch := func(n int) string { return parts[0] + "." + parts[1] + "." + strconv.Itoa(z+n) }
	good, broken := patch(1), patch(2)
	registry := "pfe2e-core.local/" + strings.ToLower(p.Project.Name)
	testCat, err := catalog.Parse(manifestWith(t, registry, map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	const versionPHP = "/var/www/wordpress/wp-includes/version.php"
	build := func(version, phpRun string) {
		t.Helper()
		imgs, err := testCat.Images(php, version)
		if err != nil {
			t.Fatal(err)
		}
		for _, img := range []struct{ tag, from string }{{imgs.PHP, base.PHP}, {imgs.Nginx, base.Nginx}} {
			dockerfile := "FROM " + img.from + "\n"
			if phpRun != "" {
				dockerfile += "USER root\nRUN " + phpRun + "\nUSER 65534:65534\n"
			}
			tags = append(tags, img.tag)
			if err := compose.BuildImage(ctx, nil, img.tag, dockerfile); err != nil {
				t.Fatal(err)
			}
		}
	}
	build(latest, "")
	build(good, `sed -i "s/^\$wp_version = '`+latest+`';/\$wp_version = '`+good+`';/" `+versionPHP)
	// Appending a digit makes the schema version larger.
	build(broken, `sed -i "s/^\(\$wp_db_version = [0-9]*\);/\19;/" `+versionPHP)
	images := func(version string) catalog.Images {
		imgs, err := testCat.Images(php, version)
		if err != nil {
			t.Fatal(err)
		}
		return imgs
	}

	groupID := createGroup(t, p, "E2E Core Agency")
	created := createSite(t, p, groupID, "E2E Core", php)
	proj := compose.Project{Name: compose.SiteProjectName(created.Site.ID), Dir: projectDir}
	projects = append(projects, proj)
	waitJob(t, p, created.Job.ID)
	site := getSite(t, p, created.Site.ID)
	if site.WpCoreVersion != latest {
		t.Fatalf("new site on WordPress %s, want %s", site.WpCoreVersion, latest)
	}
	wp := func(t *testing.T, args ...string) string {
		t.Helper()
		out, err := proj.ExecService(ctx, "php", append([]string{"wp"}, args...)...)
		if err != nil {
			t.Fatalf("wp %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	hc := p.IngressClient(t)
	checkServes := func(t *testing.T) {
		t.Helper()
		if resp, _ := waitIngress(t, hc, "https://"+site.PreviewDomain+"/", nil, http.StatusOK); resp.StatusCode != http.StatusOK {
			t.Errorf("the site answers %d", resp.StatusCode)
		}
	}
	checkImages := func(t *testing.T, version string) {
		t.Helper()
		want := images(version)
		for service, image := range map[string]string{"php": want.PHP, "nginx": want.Nginx} {
			c, err := proj.ServiceContainer(ctx, service)
			if err != nil {
				t.Fatal(err)
			}
			if !e2e.RunsImage(t, c, image) || !c.Running {
				t.Errorf("%s runs %s (running %t), want %s", service, c.Image, c.Running, image)
			}
		}
	}
	rollOut := func(t *testing.T, release string) client.Job {
		t.Helper()
		before := coreJobs(t, p, site.ID)
		// A file per release, so the environment changes and Restart
		// recreates the control plane, which reads the manifest at startup.
		manifestFile := filepath.Join(p.DataDir, "e2e-manifest-"+release+".json")
		if err := os.WriteFile(manifestFile, manifestWith(t, registry, map[string]string{"latest": release}), 0o644); err != nil {
			t.Fatal(err)
		}
		p.Restart(t, map[string]string{"PRESSFLEET_IMAGE_MANIFEST_FILE": manifestFile})
		waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		for {
			for _, j := range coreJobs(t, p, site.ID) {
				if !slices.Contains(before, j) {
					return finalJob(t, p, j)
				}
			}
			select {
			case <-waitCtx.Done():
				t.Fatalf("the rollout queued no core update of the site to %s", release)
			case <-time.After(2 * time.Second):
			}
		}
	}

	t.Run("a minor release rolls out to both containers", func(t *testing.T) {
		job := rollOut(t, good)
		if job.State != client.JobStateSucceeded {
			t.Fatalf("core update job %s: %s", job.State, deref(job.Error))
		}
		got := getSite(t, p, site.ID)
		if got.WpCoreVersion != good || got.Status != client.SiteStatusRunning {
			t.Errorf("site after the update: WordPress %s, %s (%s)", got.WpCoreVersion, got.Status, deref(got.StatusMessage))
		}
		checkImages(t, good)
		if v := wp(t, "core", "version"); v != good {
			t.Errorf("wp core version = %s, want %s", v, good)
		}
		checkServes(t)
		core, err := p.Client.GetSiteCoreWithResponse(ctx, site.ID)
		if err != nil || core.JSON200 == nil || core.JSON200.Target != nil || core.JSON200.Version != good {
			t.Errorf("site core after the update: %v %s", err, core.Body)
		}
		list, err := p.Client.ListSiteBackupsWithResponse(ctx, site.ID, &client.ListSiteBackupsParams{})
		if err != nil || list.JSON200 == nil || !slices.ContainsFunc(list.JSON200.Items, func(b client.Backup) bool {
			return string(b.Kind) == "pre_update" && b.WpCoreVersion == latest
		}) {
			t.Errorf("no pre-update backup: %v %s", err, list.Body)
		}
	})

	t.Run("a failed update reverts automatically", func(t *testing.T) {
		schema := wp(t, "option", "get", "db_version")
		job := rollOut(t, broken)
		if job.State != client.JobStateFailed || !strings.Contains(deref(job.Error), "health check") {
			t.Fatalf("core update job %s: %s", job.State, deref(job.Error))
		}
		got := getSite(t, p, site.ID)
		if got.WpCoreVersion != good || got.Status != client.SiteStatusRunning ||
			!strings.Contains(deref(got.StatusMessage), "reverted to "+good) || !strings.Contains(deref(got.StatusMessage), "pre-update backup") {
			t.Errorf("site after the revert: WordPress %s, %s (%s)", got.WpCoreVersion, got.Status, deref(got.StatusMessage))
		}
		checkImages(t, good)
		if v := wp(t, "core", "version"); v != good {
			t.Errorf("wp core version = %s, want %s", v, good)
		}
		// update-db changed the schema; restoring the pre-update backup
		// put it back.
		if v := wp(t, "option", "get", "db_version"); v != schema {
			t.Errorf("db_version after the revert = %s, want %s", v, schema)
		}
		checkServes(t)
		core, err := p.Client.GetSiteCoreWithResponse(ctx, site.ID)
		if err != nil || core.JSON200 == nil || core.JSON200.LastFailure == nil || core.JSON200.LastFailure.Version != broken {
			t.Errorf("site core after the revert: %v %s", err, core.Body)
		}
		ro, err := p.Client.GetCoreRolloutWithResponse(ctx)
		if err != nil || ro.JSON200 == nil || ro.JSON200.Halted || ro.JSON200.ConsecutiveFailures != 1 {
			t.Errorf("rollout after one failure: %v %s", err, ro.Body)
		}
	})

	resp, err := p.Client.DeleteSiteWithResponse(ctx, site.ID)
	if err != nil || resp.JSON202 == nil {
		t.Fatalf("delete site: %v %s", err, resp.Body)
	}
	waitJob(t, p, resp.JSON202.Job.ID)
}

// coreJobs returns the IDs of the site's core update jobs.
func coreJobs(t *testing.T, p *e2e.Platform, siteID string) []string {
	t.Helper()
	resp, err := p.Client.ListJobsWithResponse(t.Context(), &client.ListJobsParams{TargetID: &siteID})
	if err != nil || resp.JSON200 == nil {
		t.Fatalf("list jobs: %v %s", err, resp.Body)
	}
	var out []string
	for _, j := range resp.JSON200.Items {
		if j.Type == sites.CoreUpdateJobType {
			out = append(out, j.ID)
		}
	}
	return out
}

// manifestWith returns the embedded version manifest with another
// registry and, if set, another latest WordPress release (with a made-up
// checksum: the stand-in images aren't built from a tarball).
func manifestWith(t *testing.T, registry string, wp map[string]string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(images.Manifest(), &m); err != nil {
		t.Fatal(err)
	}
	m["registry"] = registry
	if v, ok := wp["latest"]; ok {
		m["wordpress"] = map[string]any{
			"latest": map[string]any{"version": v, "sha1": strings.Repeat("0", 40)},
			"pinned": []any{},
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
