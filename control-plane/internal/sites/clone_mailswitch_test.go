package sites_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// cloneImages returns the image tags the production site's versions select,
// which its staging site uses too.
func cloneImages(t *testing.T, prod store.Site) catalog.Images {
	t.Helper()
	cat, err := catalog.Parse([]byte(restoreManifest))
	if err != nil {
		t.Fatal(err)
	}
	imgs, err := cat.Images(prod.PHPVersion, prod.WPCoreVersion)
	if err != nil {
		t.Fatal(err)
	}
	return imgs
}

// opIndex returns the index in calls of the first call of op with the first
// argument arg, or -1.
func opIndex(calls []provisionertest.Call, op, arg string) int {
	return slices.IndexFunc(calls, func(c provisionertest.Call) bool {
		return c.Op == op && len(c.Args) > 0 && c.Args[0] == arg
	})
}

func restores(e *restoreEnv) int {
	n := 0
	for _, c := range e.engine.Calls() {
		if c.Op == "Restore" {
			n++
		}
	}
	return n
}

// The clone pulls the images before pinning them, so a host's cached build
// of a tag from before the mail switch isn't what the staging site gets, and
// checks the running image before the restore.
func TestCloneMailSwitchPullsThenChecks(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCloneEnv(t, db)
		prod := e.productionSite(t, "Acme")
		imgs := cloneImages(t, prod)
		// The host caches an old build; the registry has a newer one.
		e.prov.SetLocalImage(imgs.PHP, build(imgs.PHP, 1))
		e.prov.SetLocalImage(imgs.Nginx, build(imgs.Nginx, 1))
		e.prov.PublishImage(imgs.PHP, build(imgs.PHP, 2))
		e.prov.PublishImage(imgs.Nginx, build(imgs.Nginx, 2))
		from := len(e.prov.Calls())

		restoresAtCheck := -1
		e.prov.MailSwitch = func(string) bool {
			restoresAtCheck = restores(e)
			return true
		}
		out := e.createStaging(t, prod.ID, sites.NewStaging{})
		e.runJobs(t)
		if d := e.job(t, out.Job.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("clone job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
		}

		calls := e.prov.Calls()[from:]
		for _, image := range []string{imgs.PHP, imgs.Nginx} {
			pull, pin := opIndex(calls, "PullImage", image), opIndex(calls, "PinImage", image)
			if pull < 0 || pin < 0 || pull > pin {
				t.Errorf("%s: PullImage at %d, PinImage at %d; want the pull first", image, pull, pin)
			}
		}
		pins, err := store.NewImagePins(db).Site(context.Background(), out.Site.ID)
		if err != nil || pins[imgs.PHP] != build(imgs.PHP, 2) || pins[imgs.Nginx] != build(imgs.Nginx, 2) {
			t.Errorf("the staging site's pins = %v, %v; want the pulled builds", pins, err)
		}
		if restoresAtCheck != 0 {
			t.Errorf("the mail switch was checked after %d restores, want it checked before any", restoresAtCheck)
		}
		if n := restores(e); n != 1 {
			t.Errorf("%d restores, want 1", n)
		}
	})
}

// A site image without the mail switch stops the clone before any of
// production's data reaches the staging site, and the rollback removes what
// the job created.
func TestCloneFailsWithoutMailSwitch(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCloneEnv(t, db)
		prod := e.productionSite(t, "Acme")
		prodData := e.data(prod.ID)
		out := e.createStaging(t, prod.ID, sites.NewStaging{})
		e.prov.MailSwitch = func(siteID string) bool { return siteID != out.Site.ID }
		e.runJobs(t)

		d := e.requireRolledBack(t, out, prod, prodData)
		// The pull succeeded, so the message names the image, says to get a
		// current build, and doesn't mention a failed pull.
		for _, want := range []string{
			sites.ErrMailSwitchMissing.Error(), cloneImages(t, prod).PHP, "doesn't include the outbound mail switch",
			"real email", "Get a current build of that image onto this host", "try again",
			"Updating the production site to a newer WordPress release",
		} {
			if !strings.Contains(d.Job.Error, want) {
				t.Errorf("job error %q lacks %q", d.Job.Error, want)
			}
		}
		if strings.Contains(d.Job.Error, "Pulling it failed") {
			t.Errorf("job error %q mentions a failed pull", d.Job.Error)
		}
		if !strings.Contains(e.site(t, out.Site.ID).StatusMessage, "Get a current build") {
			t.Errorf("status message = %q", e.site(t, out.Site.ID).StatusMessage)
		}
		// Permanent: one attempt, and nothing of production's arrived.
		if n := restores(e); n != 0 {
			t.Errorf("%d restores, want none", n)
		}
		if got := e.wpCalls(out.Site.ID, ""); len(got) != 0 {
			t.Errorf("WP-CLI calls on the staging site: %q", got)
		}
		for _, s := range d.Steps {
			if s.Name == "restore" && s.State != store.StepFailed && s.State != store.StepRolledBack {
				t.Errorf("restore step is %s", s.State)
			}
			if s.Name == "restart_site" && s.State != store.StepPending && s.State != store.StepRolledBack {
				t.Errorf("restart_site step is %s, want it never to have run", s.State)
			}
		}
	})
}

// A failed pull is a warning, and the check decides: a host that can't pull
// still clones if its image has the switch, and doesn't if it lacks it.
func TestCloneMailSwitchWhenPullFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		present bool
	}{{"switch present", true}, {"switch missing", false}} {
		t.Run(tc.name, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newCloneEnv(t, db)
				prod := e.productionSite(t, "Acme")
				prodData := e.data(prod.ID)
				e.prov.Err = func(op, _ string) error {
					if op == "PullImage" {
						return errors.New("no registry")
					}
					return nil
				}
				out := e.createStaging(t, prod.ID, sites.NewStaging{})
				e.prov.MailSwitch = func(siteID string) bool { return tc.present || siteID != out.Site.ID }
				e.runJobs(t)

				d := e.job(t, out.Job.ID)
				warned := slices.ContainsFunc(d.Logs, func(l store.JobLog) bool {
					return l.Level == "warn" && strings.Contains(l.Message, "Couldn't pull") && strings.Contains(l.Message, "no registry")
				})
				if !warned {
					t.Errorf("no pull warning in the log:\n%s", jobLog(d))
				}
				if tc.present {
					if d.Job.State != store.JobSucceeded {
						t.Fatalf("clone job: %s %s\n%s", d.Job.State, d.Job.Error, jobLog(d))
					}
					return
				}
				e.requireRolledBack(t, out, prod, prodData)
				if !strings.Contains(d.Job.Error, sites.ErrMailSwitchMissing.Error()) || restores(e) != 0 {
					t.Errorf("job error = %q, restores = %d", d.Job.Error, restores(e))
				}
				// It still names the image and the production update, and
				// says the pull failed and why.
				for _, want := range []string{cloneImages(t, prod).PHP, "Get a current build", "Pulling it failed in this job: no registry", "Updating the production site"} {
					if !strings.Contains(d.Job.Error, want) {
						t.Errorf("job error %q lacks %q", d.Job.Error, want)
					}
				}
			})
		})
	}
}
