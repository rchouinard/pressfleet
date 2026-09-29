package e2e_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
)

// TestImageUpdate checks the platform image rollout (#58) with real
// containers. New builds of the site images are stand-ins derived from the
// local images of the manifest's latest release, rebuilt under the same
// tags of a registry of their own, each with its build number in
// /pf-e2e-build:
//
//   - build 1 is what the sites start on;
//   - build 2 is a good rebuild of both images;
//   - build 3 is a broken rebuild of site-php whose version.php claims
//     another WordPress release, so the health check fails.
//
// The control plane runs with a manifest naming that registry
// (image_manifest_file), updates.max_failures 1, and no registry pulls
// (the registry doesn't exist). Build 2 must reach both sites, one after
// the other; build 3 must fail on the first site, which is moved back to
// build 2, and halt the rollout before the second site.
func TestImageUpdate(t *testing.T) {
	projectDir := t.TempDir()
	var projects []compose.Project
	var cleanupImages []string
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
		for _, image := range cleanupImages {
			if err := compose.RemoveImage(ctx, nil, image); err != nil {
				t.Errorf("removing %s: %v", image, err)
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

	registry := "pfe2e-images.local/" + strings.ToLower(p.Project.Name)
	testCat, err := catalog.Parse(manifestWith(t, registry, map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	imgs, err := testCat.Images(php, latest)
	if err != nil {
		t.Fatal(err)
	}
	const versionPHP = "/var/www/wordpress/wp-includes/version.php"
	// build rebuilds the tags as build n; phpRun, if set, also runs in
	// the php image.
	build := func(n int, nginx bool, phpRun string) {
		t.Helper()
		type image struct{ tag, from, run string }
		list := []image{{imgs.PHP, base.PHP, phpRun}}
		if nginx {
			list = append(list, image{imgs.Nginx, base.Nginx, ""})
		}
		for _, img := range list {
			run := "echo " + strconv.Itoa(n) + " > /pf-e2e-build"
			if img.run != "" {
				run += " && " + img.run
			}
			dockerfile := "FROM " + img.from + "\nUSER root\nRUN " + run + "\nUSER 65534:65534\n"
			if err := compose.BuildImage(ctx, nil, img.tag, dockerfile); err != nil {
				t.Fatal(err)
			}
			id, err := compose.ImageID(ctx, nil, img.tag)
			if err != nil {
				t.Fatal(err)
			}
			repo := img.tag[:strings.LastIndexByte(img.tag, ':')]
			cleanupImages = append(cleanupImages, compose.PinTag(repo, id))
		}
	}
	build(1, true, "")
	cleanupImages = append(cleanupImages, imgs.PHP, imgs.Nginx)

	manifestFile := filepath.Join(p.DataDir, "e2e-manifest-images.json")
	if err := os.WriteFile(manifestFile, manifestWith(t, registry, map[string]string{}), 0o644); err != nil {
		t.Fatal(err)
	}
	p.Restart(t, map[string]string{
		"PRESSFLEET_IMAGE_MANIFEST_FILE":        manifestFile,
		"PRESSFLEET_UPDATES_MAX_FAILURES":       "1",
		"PRESSFLEET_UPDATES_IMAGES_CHECK_HOURS": "0",
	})

	groupID := createGroup(t, p, "E2E Images Agency")
	var siteIDs []string
	projectOf := map[string]compose.Project{}
	for _, name := range []string{"E2E Images A", "E2E Images B"} {
		created := createSite(t, p, groupID, name, php)
		proj := compose.Project{Name: compose.SiteProjectName(created.Site.ID), Dir: projectDir}
		projects = append(projects, proj)
		projectOf[created.Site.ID] = proj
		waitJob(t, p, created.Job.ID)
		siteIDs = append(siteIDs, created.Site.ID)
	}
	hc := p.IngressClient(t)
	checkBuild := func(t *testing.T, siteID string, php, nginx int) {
		t.Helper()
		for service, want := range map[string]int{"php": php, "nginx": nginx} {
			c, err := projectOf[siteID].ServiceContainer(ctx, service)
			if err != nil {
				t.Fatal(err)
			}
			// A pinned build, not the tag.
			if !c.Running || !strings.Contains(c.Image, "sha256:") {
				t.Errorf("site %s: %s runs %s (running %t), want a pinned build", siteID, service, c.Image, c.Running)
			}
			out, err := projectOf[siteID].ExecService(ctx, service, "cat", "/pf-e2e-build")
			if got := strings.TrimSpace(string(out)); err != nil || got != strconv.Itoa(want) {
				t.Errorf("site %s: %s runs build %q (%v), want %d", siteID, service, got, err, want)
			}
		}
		site := getSite(t, p, siteID)
		if site.Status != client.SiteStatusRunning {
			t.Errorf("site %s is %s (%s)", siteID, site.Status, deref(site.StatusMessage))
		}
		if resp, _ := waitIngress(t, hc, "https://"+site.PreviewDomain+"/", nil, http.StatusOK); resp.StatusCode != http.StatusOK {
			t.Errorf("site %s answers %d", siteID, resp.StatusCode)
		}
	}
	for _, id := range siteIDs {
		checkBuild(t, id, 1, 1)
	}
	resume := func(t *testing.T) {
		t.Helper()
		resp, err := p.Client.ResumeImageRolloutWithResponse(ctx)
		if err != nil || resp.JSON200 == nil {
			t.Fatalf("resume the image rollout: %v %s", err, resp.Body)
		}
	}
	rollout := func(t *testing.T) client.ImageRollout {
		t.Helper()
		resp, err := p.Client.GetImageRolloutWithResponse(ctx)
		if err != nil || resp.JSON200 == nil {
			t.Fatalf("get the image rollout: %v %s", err, resp.Body)
		}
		return *resp.JSON200
	}
	// waitImageJobs waits until the site has n finished image update jobs
	// and returns them, newest first.
	waitImageJobs := func(t *testing.T, siteID string, n int) []client.Job {
		t.Helper()
		waitCtx, cancel := context.WithTimeout(ctx, jobTimeout)
		defer cancel()
		for {
			jobs := imageJobs(t, p, siteID)
			done := 0
			for _, j := range jobs {
				if j.State == client.JobStateSucceeded || j.State == client.JobStateFailed {
					done++
				}
			}
			if done >= n {
				return jobs
			}
			select {
			case <-waitCtx.Done():
				t.Fatalf("site %s: %d finished image update jobs, want %d", siteID, done, n)
			case <-time.After(2 * time.Second):
			}
		}
	}

	t.Run("a new build rolls out site by site", func(t *testing.T) {
		build(2, true, "")
		resume(t)
		a := waitImageJobs(t, siteIDs[0], 1)
		b := waitImageJobs(t, siteIDs[1], 1)
		for _, j := range []client.Job{a[0], b[0]} {
			if j.State != client.JobStateSucceeded {
				t.Fatalf("image update job %s of %s: %s: %s", j.ID, j.Target.ID, j.State, deref(j.Error))
			}
		}
		// The older site first; the second only after the first finished.
		if a[0].FinishedAt == nil || b[0].StartedAt == nil || b[0].StartedAt.Before(*a[0].FinishedAt) {
			t.Errorf("site updates overlapped: first %v-%v, second from %v", a[0].StartedAt, a[0].FinishedAt, b[0].StartedAt)
		}
		for _, id := range siteIDs {
			checkBuild(t, id, 2, 2)
		}
		ro := rollout(t)
		if ro.Halted || ro.ConsecutiveFailures != 0 || len(ro.Images) != 2 || len(ro.Failures) != 0 {
			t.Errorf("rollout after the update: %+v", ro)
		}
	})

	t.Run("a broken build is reverted and halts the rollout", func(t *testing.T) {
		build(3, false, `sed -i "s/^\$wp_version = '`+latest+`';/\$wp_version = '0.0.1';/" `+versionPHP)
		resume(t)
		a := waitImageJobs(t, siteIDs[0], 2)
		if a[0].State != client.JobStateFailed || !strings.Contains(deref(a[0].Error), "health check") {
			t.Fatalf("image update job %s: %s: %s", a[0].ID, a[0].State, deref(a[0].Error))
		}
		// Halted after one failure (updates.max_failures 1).
		waitCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		ro := rollout(t)
		for !ro.Halted {
			select {
			case <-waitCtx.Done():
				t.Fatalf("the rollout didn't halt: %+v", ro)
			case <-time.After(2 * time.Second):
			}
			ro = rollout(t)
		}
		if ro.ConsecutiveFailures != 1 || len(ro.Failures) != 1 || ro.Failures[0].SiteID != siteIDs[0] ||
			!strings.HasPrefix(ro.Failures[0].Target, "php=") {
			t.Errorf("rollout after the failure: %+v", ro)
		}
		got := getSite(t, p, siteIDs[0])
		if !strings.Contains(deref(got.StatusMessage), "reverted to the previous builds") {
			t.Errorf("status message = %q", deref(got.StatusMessage))
		}
		checkBuild(t, siteIDs[0], 2, 2)
		// The halted rollout left the second site alone.
		if jobs := imageJobs(t, p, siteIDs[1]); len(jobs) != 1 {
			t.Errorf("second site: %d image update jobs, want 1", len(jobs))
		}
		checkBuild(t, siteIDs[1], 2, 2)
	})

	for _, id := range siteIDs {
		resp, err := p.Client.DeleteSiteWithResponse(ctx, id)
		if err != nil || resp.JSON202 == nil {
			t.Fatalf("delete site: %v %s", err, resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
	}
}

// imageJobs returns the site's image update jobs, newest first.
func imageJobs(t *testing.T, p *e2e.Platform, siteID string) []client.Job {
	t.Helper()
	resp, err := p.Client.ListJobsWithResponse(t.Context(), &client.ListJobsParams{TargetID: &siteID})
	if err != nil || resp.JSON200 == nil {
		t.Fatalf("list jobs: %v %s", err, resp.Body)
	}
	var out []client.Job
	for _, j := range resp.JSON200.Items {
		if j.Type == sites.ImageUpdateJobType {
			out = append(out, j)
		}
	}
	return out
}
