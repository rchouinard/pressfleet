package e2e_test

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// TestControlPlaneRunsNoRestic checks the goal of #178 and #191: the control
// plane process never runs restic, whose memory grows with the repository,
// so its memory limit doesn't depend on how much is backed up. restic is
// still in the image, because the image is also the task image, so the check
// is of the control plane container's process list, sampled while the
// control plane
//
//   - creates a client group's repository and backs a site up twice,
//   - prunes and checks the repository (retention and maintenance jobs),
//   - and, after a restart, measures the repository's size for the storage
//     metrics, which it does as soon as it starts.
//
// A positive control comes first: the same sampling does see a restic that
// something runs in the container, and the control plane's own docker
// processes, so an empty answer isn't the watch failing to look.
func TestControlPlaneRunsNoRestic(t *testing.T) {
	projectDir := t.TempDir()
	var siteProject compose.Project
	// Registered before Start, so it runs after the platform teardown: see
	// TestSiteCreateDelete.
	t.Cleanup(func() {
		if siteProject.Name == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if res, err := siteProject.Resources(ctx); err == nil && !res.Empty() {
			t.Logf("removing leftover site project %s", siteProject.Name)
			if err := siteProject.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
				t.Errorf("removing leftover site project %s: %v", siteProject.Name, err)
			}
		}
	})
	p := e2e.Start(t)
	ctx := t.Context()

	t.Run("the watch sees restic", func(t *testing.T) {
		w := p.WatchControlPlane(t)
		// restic retries a REST server that doesn't answer, so it stays in
		// the process list until timeout ends it.
		_, _ = p.Project.Exec(ctx, e2e.Service, "timeout", "4", "restic", "--no-cache", "-r", "rest:http://192.0.2.1:8000/", "snapshots")
		seen, samples := w.Stop()
		if seen["restic"] == 0 || seen["pressfleet"] == 0 {
			t.Fatalf("the watch did not see restic and pressfleet in %d samples: %v", samples, seen)
		}
		t.Logf("%d samples; seen: %v", samples, seen)
	})

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

	groupID := createGroup(t, p, "E2E No Restic")
	created := createSite(t, p, groupID, "E2E No Restic", php)
	siteProject = compose.Project{Name: compose.SiteProjectName(created.Site.ID), Dir: projectDir}
	waitJob(t, p, created.Job.ID)

	w := p.WatchControlPlane(t)

	backup := func() {
		t.Helper()
		resp, err := p.Client.CreateSiteBackupWithResponse(ctx, created.Site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil {
			t.Fatalf("back up: status %d: %s", resp.StatusCode(), resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
	}
	// The first backup creates the repository (restic init, in a task).
	backup()
	backup()

	for _, command := range []string{"prune", "check"} {
		out, err := p.Project.Exec(ctx, e2e.Service, "pressfleet", "repository", command, groupID)
		if err != nil {
			t.Fatalf("repository %s: %v: %s", command, err, out)
		}
		m := regexp.MustCompile(`job (\S+) for`).FindStringSubmatch(string(out))
		if m == nil {
			t.Fatalf("repository %s printed no job: %s", command, out)
		}
		waitJob(t, p, m[1])
	}

	// The storage metrics collector measures every repository when the
	// control plane starts (`restic stats`, in a task).
	// A changed setting makes Compose recreate the container.
	p.Restart(t, map[string]string{"PRESSFLEET_METRICS_INTERVAL_SECONDS": "20"})
	waitRepositorySize(t, p, groupID)

	seen, samples := w.Stop()
	t.Logf("%d samples of the control plane's processes; seen: %v", samples, seen)
	if seen["restic"] != 0 {
		t.Errorf("the control plane ran restic in %d of %d samples: %v", seen["restic"], samples, slices.Sorted(maps.Keys(seen)))
	}
	if seen["docker"] == 0 || samples < 20 {
		t.Errorf("the watch looks broken: %d samples, saw %v", samples, seen)
	}
}

// waitRepositorySize waits for the storage collector's measurement of the
// group's repository to be queryable.
func waitRepositorySize(t *testing.T, p *e2e.Platform, groupID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	for {
		start := time.Now().Add(-15 * time.Minute)
		r, err := p.Client.QueryPlatformMetricsWithResponse(ctx, &client.QueryPlatformMetricsParams{
			Metric: &[]string{"restic_repository_size"}, ClientGroupID: &groupID, Start: &start,
		})
		if err == nil && r.JSON200 != nil {
			for _, m := range r.JSON200.Metrics {
				for _, s := range m.Series {
					if len(s.Points) > 0 {
						return
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no restic_repository_size for client group %s: %v", groupID, err)
		case <-time.After(5 * time.Second):
		}
	}
}
