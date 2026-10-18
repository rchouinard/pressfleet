package sites_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// imageEnv is a coreEnv whose sites run WordPress 7.2.1, the latest, so
// the core rollout leaves them alone, and where new builds of the images
// can be published and marked broken.
type imageEnv struct {
	*coreEnv
	tags catalog.Images
	// broken builds answer HTTP requests with 500.
	broken map[string]bool
}

func newImageEnv(t *testing.T, db *store.DB, opts ...func(*sites.Options)) *imageEnv {
	t.Helper()
	e := &imageEnv{coreEnv: newCoreEnv(t, db, opts...), broken: map[string]bool{}}
	cat, err := catalog.Parse([]byte(coreManifest))
	if err != nil {
		t.Fatal(err)
	}
	if e.tags, err = cat.Images("8.4", "7.2.1"); err != nil {
		t.Fatal(err)
	}
	e.status = func(siteID string) int {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.broken[e.runs(siteID)[provisioner.ComponentPHP]] {
			return 500
		}
		return 200
	}
	return e
}

// build returns the reference of build n of the image tag.
func build(tag string, n int) string {
	repo := tag[:strings.LastIndexByte(tag, ':')]
	return fmt.Sprintf("%s@sha256:%064x", repo, n)
}

// runs returns the builds the site's containers are rendered with.
func (e *imageEnv) runs(siteID string) map[provisioner.Component]string {
	rt, _ := e.prov.Site(siteID)
	return rt.Config.PinnedImages
}

// checkRuns checks that the site runs builds php and nginx, both in its
// containers and its pins, and is running.
func (e *imageEnv) checkRuns(t *testing.T, siteID, php, nginx string) {
	t.Helper()
	got := e.runs(siteID)
	if got[provisioner.ComponentPHP] != php || got[provisioner.ComponentNginx] != nginx {
		t.Errorf("site %s runs %v, want php %s and nginx %s", siteID, got, php, nginx)
	}
	pins, err := store.NewImagePins(e.db).Site(context.Background(), siteID)
	if err != nil || pins[e.tags.PHP] != php || pins[e.tags.Nginx] != nginx {
		t.Errorf("site %s pins = %v, %v", siteID, pins, err)
	}
	if site := e.site(t, siteID); site.Status != store.SiteRunning {
		t.Errorf("site %s is %s (%s), want running", siteID, site.Status, site.StatusMessage)
	}
}

// imageJobs returns the site's image update jobs, oldest first.
func (e *imageEnv) imageJobs(t *testing.T, siteID string) []store.Job {
	t.Helper()
	page, err := store.NewJobs(e.db).List(context.Background(), store.JobFilter{
		Types: []string{sites.ImageUpdateJobType}, TargetType: store.JobTargetSite, TargetID: siteID,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := page.Jobs
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (e *imageEnv) rolloutState(t *testing.T) sites.RolloutStatus {
	t.Helper()
	ro, err := e.svc.Rollouts().Rollout(context.Background(), sites.ImageRolloutName)
	if err != nil {
		t.Fatal(err)
	}
	return ro
}

func TestImageRolloutSiteBySite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newImageEnv(t, db)
		var ids []string
		for _, name := range []string{"A", "B", "C"} {
			ids = append(ids, e.siteOn(t, name, "7.2.1").ID)
		}
		// Every site is pinned to the builds the driver had when it was
		// rendered.
		oldPHP, oldNginx := e.runs(ids[0])[provisioner.ComponentPHP], e.runs(ids[0])[provisioner.ComponentNginx]
		for _, id := range ids {
			e.checkRuns(t, id, oldPHP, oldNginx)
		}
		// Nothing to roll out yet.
		e.rollout(t)
		if n := len(e.imageJobs(t, ids[0])); n != 0 {
			t.Fatalf("%d image update jobs without new builds", n)
		}

		// New builds of both images, with the same WordPress core.
		newPHP, newNginx := build(e.tags.PHP, 2), build(e.tags.Nginx, 2)
		e.prov.SetLocalImage(e.tags.PHP, newPHP)
		e.prov.SetLocalImage(e.tags.Nginx, newNginx)
		if err := e.svc.Rollouts().EnqueueRollout(context.Background()); err != nil {
			t.Fatal(err)
		}
		// One site at a time: however many passes run, never more than
		// one site is updating or has an image update waiting.
		for range 100 {
			ran, err := e.pool.RunNext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !ran {
				break
			}
			if err := e.svc.Rollouts().EnqueueRollout(context.Background()); err != nil {
				t.Fatal(err)
			}
			active, err := store.NewJobs(e.db).List(context.Background(), store.JobFilter{
				States: []store.JobState{store.JobQueued, store.JobRunning}, Types: []string{sites.ImageUpdateJobType},
			})
			if err != nil {
				t.Fatal(err)
			}
			updating := 0
			for _, id := range ids {
				if e.site(t, id).Status == store.SiteUpdating {
					updating++
				}
			}
			if len(active.Jobs) > 1 || updating > 1 {
				t.Fatalf("%d image updates waiting and %d sites updating at once", len(active.Jobs), updating)
			}
		}
		var last time.Time
		for i, id := range ids {
			e.checkRuns(t, id, newPHP, newNginx)
			jobs := e.imageJobs(t, id)
			if len(jobs) != 1 || jobs[0].State != store.JobSucceeded {
				t.Fatalf("image update jobs of site %d = %+v", i, jobs)
			}
			// Oldest site first, each after the one before finished.
			if jobs[0].StartedAt.Before(last) {
				t.Errorf("site %d updated before site %d finished", i, i-1)
			}
			last = jobs[0].FinishedAt
			res := jobResult(t, jobs[0])
			if res["php"] != newPHP || res["nginx"] != newNginx {
				t.Errorf("job result = %v", res)
			}
			// Same core: nothing to back up, nothing for update-db.
			if n := len(e.coreJobs(t, id)); n != 0 {
				t.Errorf("%d core update jobs", n)
			}
		}
		if ro := e.rolloutState(t); ro.Halted() || ro.ConsecutiveFailures != 0 || len(ro.Failures) != 0 {
			t.Errorf("rollout = %+v", ro)
		}
		builds := e.svc.Rollouts().ImageBuilds()
		if len(builds) != 2 || builds[0].Build != newNginx || builds[1].Build != newPHP {
			t.Errorf("image builds = %+v", builds)
		}
		if engineOps := e.engineOps(0); len(engineOps) != 0 {
			t.Errorf("backup engine calls = %q, want none", engineOps)
		}
	})
}

func TestImageRolloutHaltsOnRepeatedFailures(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newImageEnv(t, db, func(o *sites.Options) { o.CoreUpdates.MaxFailures = 2 })
		var ids []string
		for _, name := range []string{"A", "B", "C"} {
			ids = append(ids, e.siteOn(t, name, "7.2.1").ID)
		}
		oldPHP, nginx := e.runs(ids[0])[provisioner.ComponentPHP], e.runs(ids[0])[provisioner.ComponentNginx]

		// A PHP build that fails its health check.
		broken := build(e.tags.PHP, 2)
		e.broken[broken] = true
		e.prov.SetLocalImage(e.tags.PHP, broken)
		e.rollout(t)
		for i, id := range ids {
			want := 1
			if i == 2 {
				want = 0 // the rollout halted before it
			}
			jobs := e.imageJobs(t, id)
			if len(jobs) != want {
				t.Fatalf("site %d: %d image update jobs, want %d", i, len(jobs), want)
			}
			if want == 1 && (jobs[0].State != store.JobFailed || !strings.Contains(jobs[0].Error, "500")) {
				t.Errorf("site %d: job %s: %s", i, jobs[0].State, jobs[0].Error)
			}
			// Reverted to the previous build.
			e.checkRuns(t, id, oldPHP, nginx)
			if msg := e.site(t, id).StatusMessage; want == 1 && !strings.Contains(msg, "reverted to the previous builds") {
				t.Errorf("site %d status message = %q", i, msg)
			}
		}
		ro := e.rolloutState(t)
		if !ro.Halted() || ro.ConsecutiveFailures != 2 || len(ro.Failures) != 2 || ro.Failures[0].Target != "php="+broken {
			t.Fatalf("rollout = %+v; want halted after 2 failures", ro)
		}
		// Halted: another pass does nothing.
		e.rollout(t)
		if n := len(e.imageJobs(t, ids[2])); n != 0 {
			t.Fatalf("the halted rollout updated a site")
		}

		// Resuming tries the site that wasn't, but not the failed ones
		// with the same build.
		if _, err := e.svc.Rollouts().ResumeRollout(context.Background(), sites.ImageRolloutName); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		for i, id := range ids {
			if n := len(e.imageJobs(t, id)); n != 1 {
				t.Errorf("site %d: %d image update jobs after resuming, want 1", i, n)
			}
		}
		if ro := e.rolloutState(t); ro.Halted() || ro.ConsecutiveFailures != 1 || len(ro.Failures) != 3 {
			t.Errorf("rollout after resuming = %+v", ro)
		}

		// A fixed build rolls out to every site, the failed ones too.
		fixed := build(e.tags.PHP, 3)
		e.prov.SetLocalImage(e.tags.PHP, fixed)
		e.rollout(t)
		for _, id := range ids {
			e.checkRuns(t, id, fixed, nginx)
		}
		if ro := e.rolloutState(t); ro.Halted() || ro.ConsecutiveFailures != 0 || len(ro.Failures) != 0 {
			t.Errorf("rollout after the fix = %+v", ro)
		}
	})
}

func TestImageRolloutIgnoresDeletedSites(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newImageEnv(t, db, func(o *sites.Options) { o.CoreUpdates.MaxFailures = 1 })
		site := e.siteOn(t, "A", "7.2.1")
		// The site's deletion starts while its update checks its health.
		e.status = func(siteID string) int {
			if _, err := store.NewSites(db).SetStatus(context.Background(), siteID, store.SiteDeleting, "", store.SiteUpdating); err != nil {
				t.Error(err)
			}
			return 500
		}
		e.prov.SetLocalImage(e.tags.PHP, build(e.tags.PHP, 2))
		e.rollout(t)
		jobs := e.imageJobs(t, site.ID)
		if len(jobs) != 1 || jobs[0].State != store.JobFailed {
			t.Fatalf("image update jobs = %+v, want one failed", jobs)
		}
		if ro := e.rolloutState(t); ro.Halted() || ro.ConsecutiveFailures != 0 || len(ro.Failures) != 0 {
			t.Errorf("rollout = %+v; a deleted site's failure counted", ro)
		}
	})
}

func TestImageRolloutHaltLiftsOnNewBuild(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newImageEnv(t, db, func(o *sites.Options) { o.CoreUpdates.MaxFailures = 1 })
		site := e.siteOn(t, "A", "7.2.1")
		nginx := e.runs(site.ID)[provisioner.ComponentNginx]
		broken := build(e.tags.PHP, 2)
		e.broken[broken] = true
		e.prov.SetLocalImage(e.tags.PHP, broken)
		e.rollout(t)
		if ro := e.rolloutState(t); !ro.Halted() {
			t.Fatalf("rollout = %+v, want halted", ro)
		}
		fixed := build(e.tags.PHP, 3)
		e.prov.SetLocalImage(e.tags.PHP, fixed)
		e.rollout(t)
		e.checkRuns(t, site.ID, fixed, nginx)
	})
}

func TestImagePinsHoldAcrossRenders(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newImageEnv(t, db)
		site := e.siteOn(t, "A", "7.2.1")
		php, nginx := e.runs(site.ID)[provisioner.ComponentPHP], e.runs(site.ID)[provisioner.ComponentNginx]
		// A new build reaches the host, but only the rollout moves the
		// site to it: re-rendering the site keeps the pinned builds.
		e.prov.SetLocalImage(e.tags.PHP, build(e.tags.PHP, 2))
		if _, err := e.svc.EnqueueReconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		e.checkRuns(t, site.ID, php, nginx)
	})
}

func TestImageRolloutPullsFromRegistry(t *testing.T) {
	for _, tt := range []struct {
		name     string
		interval time.Duration
		moves    bool
	}{{"never", 0, false}, {"every 6h", 6 * time.Hour, true}} {
		t.Run(tt.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newImageEnv(t, db, func(o *sites.Options) { o.ImageUpdates.PullInterval = tt.interval })
				site := e.siteOn(t, "A", "7.2.1")
				php, nginx := e.runs(site.ID)[provisioner.ComponentPHP], e.runs(site.ID)[provisioner.ComponentNginx]
				published := build(e.tags.Nginx, 2)
				e.prov.PublishImage(e.tags.Nginx, published)
				e.rollout(t)
				if tt.moves {
					e.checkRuns(t, site.ID, php, published)
				} else {
					e.checkRuns(t, site.ID, php, nginx)
				}
			})
		})
	}
}
