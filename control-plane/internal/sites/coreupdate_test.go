package sites_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/coreupdate"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// coreManifest is the manifest after WordPress 7.2 came out: 7.1 is kept
// for sites held on it or awaiting approval, and got a security release.
const coreManifest = `{
  "registry": "registry.example/pf",
  "php": [{"version": "8.4", "activeSupportEnds": "2026-12-31", "securitySupportEnds": "2028-12-31"}],
  "wordpress": {
    "latest": {"version": "7.2.1", "sha1": "761b8101538f0631a0bfc4fba7bc4abeea92f81c", "published": "2026-05-01"},
    "pinned": [{"version": "7.1.4", "sha1": "1111111111111111111111111111111111111111", "published": "2026-05-01"}]
  }
}`

// schemas are the database schema versions (db_version) of the cores.
var schemas = map[string]string{"7.1.2": "60000", "7.1.4": "60000", "7.2.1": "61000"}

// coreEnv simulates WordPress cores in the fake provisioner: `wp core
// version` reports the core of the site's images (or what reports says,
// for broken images), `wp core update-db` sets the database's db_version
// to the core's schema, and the database is the restore simulation's.
type coreEnv struct {
	*restoreEnv

	mu sync.Mutex
	// reports maps a core version to what its images report, for images
	// that aren't what they claim.
	reports map[string]string
	// status, if set, answers HTTP probes.
	status func(siteID string) int
}

func newCoreEnv(t *testing.T, db *store.DB, opts ...func(*sites.Options)) *coreEnv {
	t.Helper()
	e := &coreEnv{reports: map[string]string{}}
	opts = append([]func(*sites.Options){func(o *sites.Options) {
		o.CoreUpdates.HealthTimeout = 20 * time.Millisecond
		o.CoreUpdates.HealthInterval = time.Millisecond
	}}, opts...)
	e.restoreEnv = newRestoreEnvWith(t, db, coreManifest, opts...)
	e.prov.WPCLI = e.wpcli
	e.prov.Probe = func(siteID string, _ provisioner.ProbeRequest) (*provisioner.ProbeResponse, error) {
		e.mu.Lock()
		f := e.status
		e.mu.Unlock()
		if f == nil {
			return &provisioner.ProbeResponse{StatusCode: 200}, nil
		}
		return &provisioner.ProbeResponse{StatusCode: f(siteID)}, nil
	}
	return e
}

func (e *coreEnv) imageCore(siteID string) string {
	rt, _ := e.prov.Site(siteID)
	return rt.Config.WPCoreVersion
}

func (e *coreEnv) wpcli(siteID string, args []string) (string, error) {
	core := e.imageCore(siteID)
	e.mu.Lock()
	reported, broken := e.reports[core]
	e.mu.Unlock()
	switch strings.Join(args, " ") {
	case "core version":
		if broken {
			return reported + "\n", nil
		}
		return core + "\n", nil
	case "option get db_version":
		return e.option(siteID, "db_version") + "\n", nil
	case "core update-db":
		e.setOption(siteID, "db_version", schemas[core])
		return "Success: WordPress database upgraded.", nil
	}
	return e.restoreEnv.wpcli(siteID, args)
}

func (e *coreEnv) option(siteID, name string) string {
	for line := range strings.Lines(e.data(siteID)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), name+"="); ok {
			return v
		}
	}
	return ""
}

func (e *coreEnv) setOption(siteID, name, value string) {
	var out []string
	for line := range strings.Lines(e.data(siteID)) {
		if line = strings.TrimSpace(line); !strings.HasPrefix(line, name+"=") {
			out = append(out, line)
		}
	}
	out = append(out, name+"="+value)
	e.dbs.SetData(siteID, []byte(strings.Join(out, "\n")+"\n"))
}

// siteOn provisions a running site on WordPress version, with a database
// of that core's schema.
func (e *coreEnv) siteOn(t *testing.T, name, version string) store.Site {
	t.Helper()
	site := e.runningSite(t, name, "db_version="+schemas[version])
	ctx := context.Background()
	if _, err := store.NewSites(e.db).SetWPCoreVersion(ctx, site.ID, version); err != nil {
		t.Fatal(err)
	}
	rt, _ := e.prov.Site(site.ID)
	cfg := rt.Config
	cfg.WPCoreVersion = version
	if _, err := e.prov.UpdateSite(ctx, &cfg); err != nil {
		t.Fatal(err)
	}
	return e.site(t, site.ID)
}

// rollout runs a rollout pass and everything it queues.
func (e *coreEnv) rollout(t *testing.T) {
	t.Helper()
	if err := e.svc.EnqueueRollout(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.runJobs(t)
}

// coreJobs returns the site's core update jobs, oldest first.
func (e *coreEnv) coreJobs(t *testing.T, siteID string) []store.Job {
	t.Helper()
	page, err := store.NewJobs(e.db).List(context.Background(), store.JobFilter{
		Types: []string{sites.CoreUpdateJobType}, TargetType: store.JobTargetSite, TargetID: siteID,
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

// checkCore checks that the site runs version in its record and both of
// its images, and is running.
func (e *coreEnv) checkCore(t *testing.T, siteID, version string) {
	t.Helper()
	site := e.site(t, siteID)
	if site.WPCoreVersion != version || e.imageCore(siteID) != version {
		t.Errorf("site on WordPress %s, images on %s; want %s", site.WPCoreVersion, e.imageCore(siteID), version)
	}
	if site.Status != store.SiteRunning {
		t.Errorf("site is %s (%s), want running", site.Status, site.StatusMessage)
	}
	// Both images come from the one core version (§5.5): nginx and php
	// always run the same core.
	cat, err := catalog.Parse([]byte(coreManifest))
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := e.prov.Site(siteID)
	imgs, err := cat.Images(rt.Config.PHPVersion, rt.Config.WPCoreVersion)
	if err != nil || !strings.HasSuffix(imgs.PHP, "-wp"+version) || !strings.HasSuffix(imgs.Nginx, ":wp"+version) {
		t.Errorf("images = %+v, %v; want both on %s", imgs, err, version)
	}
}

func (e *coreEnv) setPolicy(t *testing.T, siteID string, u sites.CorePolicyUpdate) {
	t.Helper()
	if _, err := e.svc.SetCorePolicy(context.Background(), siteID, u); err != nil {
		t.Fatal(err)
	}
}

func TestCoreRolloutMinorReleases(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCoreEnv(t, db)
		held := e.siteOn(t, "Held", "7.1.2")
		free := e.siteOn(t, "Free", "7.1.2")
		e.setPolicy(t, held.ID, sites.CorePolicyUpdate{Hold: true})

		e.rollout(t)

		// A held site gets its branch's security release, but not the
		// next major; a site awaiting approval neither.
		for _, site := range []store.Site{held, free} {
			e.checkCore(t, site.ID, "7.1.4")
			jobs := e.coreJobs(t, site.ID)
			if len(jobs) != 1 || jobs[0].State != store.JobSucceeded {
				t.Fatalf("core update jobs of %s = %+v", site.Name, jobs)
			}
			res := jobResult(t, jobs[0])
			if res["from"] != "7.1.2" || res["to"] != "7.1.4" || res["pre_update_backup_id"] == "" {
				t.Errorf("job result = %v", res)
			}
			rec, err := store.NewBackupRecords(e.db).Get(context.Background(), res["pre_update_backup_id"])
			if err != nil || rec.Kind != store.BackupPreUpdate || rec.Status != store.BackupSucceeded || rec.WPCoreVersion != "7.1.2" {
				t.Errorf("pre-update backup = %+v, %v", rec, err)
			}
			st, err := e.svc.CoreStatus(context.Background(), site.ID)
			if err != nil || st.Plan.Target != "" || st.Plan.Pending == nil || st.Plan.Pending.Version != "7.2.1" {
				t.Errorf("status after the rollout = %+v, %v", st.Plan, err)
			}
		}
		if st, _ := e.svc.CoreStatus(context.Background(), held.ID); st.Plan.Pending.Reason != coreupdate.PendingHeld {
			t.Errorf("held site's pending reason = %s", st.Plan.Pending.Reason)
		}
		if st, _ := e.svc.CoreStatus(context.Background(), free.ID); st.Plan.Pending.Reason != coreupdate.PendingAwaitingApproval {
			t.Errorf("other site's pending reason = %s", st.Plan.Pending.Reason)
		}
		// Nothing more to do.
		e.rollout(t)
		if n := len(e.coreJobs(t, free.ID)); n != 1 {
			t.Errorf("%d core update jobs after a second rollout, want 1", n)
		}
	})
}

func TestCoreMajorNeedsApproval(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCoreEnv(t, db)
		site := e.siteOn(t, "Acme", "7.1.4")
		other := e.siteOn(t, "Other", "7.1.4")
		e.rollout(t)
		if len(e.coreJobs(t, site.ID)) != 0 {
			t.Fatal("a major release rolled out without approval")
		}
		if _, _, err := e.svc.UpdateCore(context.Background(), site.ID, ""); !errors.Is(err, sites.ErrNoCoreUpdate) {
			t.Errorf("UpdateCore without approval: err = %v, want ErrNoCoreUpdate", err)
		}

		// Approving the site queues a rollout pass, which updates it.
		e.setPolicy(t, site.ID, sites.CorePolicyUpdate{ApprovedBranch: "7.2"})
		e.runJobs(t)
		e.checkCore(t, site.ID, "7.2.1")
		if got := e.option(site.ID, "db_version"); got != schemas["7.2.1"] {
			t.Errorf("db_version = %s, want the new schema", got)
		}
		e.checkCore(t, other.ID, "7.1.4")

		// Approving the client group updates its other sites; a site's hold
		// wins over it.
		held := e.siteOn(t, "Held", "7.1.4")
		e.setPolicy(t, held.ID, sites.CorePolicyUpdate{Hold: true})
		if _, err := e.svc.SetClientGroupCorePolicy(context.Background(), e.group, sites.CorePolicyUpdate{ApprovedBranch: "7.2"}); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		e.checkCore(t, other.ID, "7.2.1")
		e.checkCore(t, held.ID, "7.1.4")
	})
}

func TestCoreMajorAutoApproval(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		// Published 2026-05-01, now is 2026-06-01: 31 days.
		for _, tt := range []struct {
			days    int
			version string
		}{{60, "7.1.4"}, {14, "7.2.1"}} {
			e := newCoreEnv(t, db, func(o *sites.Options) {
				o.CoreUpdates.Policy.MajorAutoApproveAfter = time.Duration(tt.days) * 24 * time.Hour
			})
			site := e.siteOn(t, "Acme", "7.1.4")
			e.rollout(t)
			e.checkCore(t, site.ID, tt.version)
		}
	})
}

func TestCoreUpdateRevertsOnFailure(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCoreEnv(t, db)
		// The 7.2.1 images really hold 7.1.4, so the health check fails
		// after update-db changed the schema.
		e.reports["7.2.1"] = "7.1.4"
		site := e.siteOn(t, "Acme", "7.1.4")
		e.setPolicy(t, site.ID, sites.CorePolicyUpdate{ApprovedBranch: "7.2"})
		e.setOption(site.ID, "marker", "before")
		from := len(e.engine.Calls())
		e.runJobs(t)

		jobs := e.coreJobs(t, site.ID)
		if len(jobs) != 1 || jobs[0].State != store.JobFailed {
			t.Fatalf("core update jobs = %+v", jobs)
		}
		if !strings.Contains(jobs[0].Error, "health check") {
			t.Errorf("job error = %q", jobs[0].Error)
		}
		e.checkCore(t, site.ID, "7.1.4")
		got := e.site(t, site.ID)
		if !strings.Contains(got.StatusMessage, "failed and was reverted to 7.1.4") || !strings.Contains(got.StatusMessage, "pre-update backup") {
			t.Errorf("status message = %q", got.StatusMessage)
		}
		// The schema changed, so the pre-update backup was restored.
		if v := e.option(site.ID, "db_version"); v != schemas["7.1.4"] {
			t.Errorf("db_version after the revert = %s, want %s", v, schemas["7.1.4"])
		}
		var restores int
		for _, op := range e.engineOps(from) {
			if strings.HasPrefix(op, "Restore ") {
				restores++
			}
		}
		if restores != 1 {
			t.Errorf("engine calls = %q, want one restore", e.engineOps(from))
		}
		st, err := e.svc.CoreStatus(context.Background(), site.ID)
		if err != nil || st.LastFailure == nil || st.LastFailure.Target != "7.2.1" {
			t.Errorf("status = %+v, %v; want the failure", st.LastFailure, err)
		}

		// The rollout doesn't try the same release again; by hand it can.
		e.rollout(t)
		if n := len(e.coreJobs(t, site.ID)); n != 1 {
			t.Errorf("%d core update jobs after another rollout, want 1", n)
		}
		delete(e.reports, "7.2.1")
		if _, _, err := e.svc.UpdateCore(context.Background(), site.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		e.checkCore(t, site.ID, "7.2.1")
		if st, _ := e.svc.CoreStatus(context.Background(), site.ID); st.LastFailure != nil {
			t.Errorf("failure kept after a successful update: %+v", st.LastFailure)
		}
	})
}

func TestCoreUpdateRevertsWithoutSchemaChange(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCoreEnv(t, db)
		site := e.siteOn(t, "Acme", "7.1.2")
		// PHP fails on the new images.
		e.status = func(siteID string) int {
			if e.imageCore(siteID) == "7.1.4" {
				return 500
			}
			return 200
		}
		from := len(e.engine.Calls())
		e.rollout(t)
		jobs := e.coreJobs(t, site.ID)
		if len(jobs) != 1 || jobs[0].State != store.JobFailed || !strings.Contains(jobs[0].Error, "500") {
			t.Fatalf("core update jobs = %+v", jobs)
		}
		e.checkCore(t, site.ID, "7.1.2")
		for _, op := range e.engineOps(from) {
			if strings.HasPrefix(op, "Restore ") {
				t.Errorf("restored a backup although the schema didn't change: %q", op)
			}
		}
	})
}

func TestCoreRolloutHaltsOnRepeatedFailures(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCoreEnv(t, db, func(o *sites.Options) { o.CoreUpdates.MaxFailures = 2 })
		e.reports["7.1.4"] = "7.1.2"
		var ids []string
		for _, name := range []string{"A", "B", "C"} {
			ids = append(ids, e.siteOn(t, name, "7.1.2").ID)
		}
		e.rollout(t)
		for i, id := range ids {
			want := 1
			if i == 2 {
				want = 0 // the rollout halted before it
			}
			if n := len(e.coreJobs(t, id)); n != want {
				t.Errorf("site %d: %d core update jobs, want %d", i, n, want)
			}
			e.checkCore(t, id, "7.1.2")
		}
		ro, err := e.svc.Rollout(context.Background(), sites.CoreRolloutName)
		if err != nil || !ro.Halted() || ro.ConsecutiveFailures != 2 || len(ro.Failures) != 2 {
			t.Fatalf("rollout = %+v, %v; want halted after 2 failures", ro, err)
		}

		// Fixed images; resuming updates the site that wasn't tried, but
		// not the ones that failed with the same release.
		delete(e.reports, "7.1.4")
		if _, err := e.svc.ResumeRollout(context.Background(), sites.CoreRolloutName); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		e.checkCore(t, ids[2], "7.1.4")
		e.checkCore(t, ids[0], "7.1.2")
		if ro, _ := e.svc.Rollout(context.Background(), sites.CoreRolloutName); ro.Halted() || ro.ConsecutiveFailures != 0 {
			t.Errorf("rollout after resuming = %+v", ro)
		}
	})
}

func TestCoreRolloutWindow(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		// testNow is midnight UTC: outside 02:00-06:00, inside 23:00-01:00.
		for _, tt := range []struct {
			start   time.Duration
			version string
		}{{2 * time.Hour, "7.1.2"}, {23 * time.Hour, "7.1.4"}} {
			e := newCoreEnv(t, db, func(o *sites.Options) {
				o.CoreUpdates.Window = sites.UpdateWindow{Start: tt.start, Length: 2 * time.Hour}
			})
			site := e.siteOn(t, "Acme", "7.1.2")
			e.rollout(t)
			e.checkCore(t, site.ID, tt.version)
		}
	})
}

func TestUpdateWindowContains(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 6, 1, h, m, 0, 0, time.UTC) }
	tests := []struct {
		w    sites.UpdateWindow
		t    time.Time
		want bool
	}{
		{sites.UpdateWindow{}, at(12, 0), true},
		{sites.UpdateWindow{Start: 2 * time.Hour, Length: 4 * time.Hour}, at(2, 0), true},
		{sites.UpdateWindow{Start: 2 * time.Hour, Length: 4 * time.Hour}, at(5, 59), true},
		{sites.UpdateWindow{Start: 2 * time.Hour, Length: 4 * time.Hour}, at(6, 0), false},
		{sites.UpdateWindow{Start: 23 * time.Hour, Length: 2 * time.Hour}, at(0, 30), true},
		{sites.UpdateWindow{Start: 23 * time.Hour, Length: 2 * time.Hour}, at(1, 0), false},
		{sites.UpdateWindow{Start: 23 * time.Hour, Length: 2 * time.Hour}, at(22, 59), false},
	}
	for _, tt := range tests {
		if got := tt.w.Contains(tt.t); got != tt.want {
			t.Errorf("%+v.Contains(%s) = %t, want %t", tt.w, tt.t.Format("15:04"), got, tt.want)
		}
	}
}

func TestCorePolicyValidation(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newCoreEnv(t, db)
		site := e.siteOn(t, "Acme", "7.1.2")
		ctx := context.Background()
		for _, u := range []sites.CorePolicyUpdate{
			{Hold: true, ApprovedBranch: "7.2"},
			{ApprovedBranch: "7.2.1"},
			{ApprovedBranch: "6.9"}, // no images
		} {
			var ve *sites.ValidationError
			if _, err := e.svc.SetCorePolicy(ctx, site.ID, u); !errors.As(err, &ve) {
				t.Errorf("SetCorePolicy(%+v) error = %v, want a ValidationError", u, err)
			}
		}
		if _, err := e.svc.SetCorePolicy(ctx, "01jzzzzzzzzzzzzzzzzzzzzzzz", sites.CorePolicyUpdate{Hold: true}); !errors.Is(err, sites.ErrNotFound) {
			t.Errorf("missing site: err = %v", err)
		}
		if _, err := e.svc.SetClientGroupCorePolicy(ctx, "01jzzzzzzzzzzzzzzzzzzzzzzz", sites.CorePolicyUpdate{Hold: true}); !errors.Is(err, sites.ErrClientGroupNotFound) {
			t.Errorf("missing group: err = %v", err)
		}
		// A hold keeps the time it was placed.
		st, err := e.svc.SetCorePolicy(ctx, site.ID, sites.CorePolicyUpdate{Hold: true})
		if err != nil || !st.Policy.Hold || !st.Policy.HoldSince.Equal(testNow) {
			t.Fatalf("hold = %+v, %v", st.Policy, err)
		}
		// Following the group again.
		st, err = e.svc.SetCorePolicy(ctx, site.ID, sites.CorePolicyUpdate{})
		if err != nil || !st.Policy.IsZero() || st.Plan.Source != coreupdate.SourceNone {
			t.Errorf("cleared policy = %+v (%s), %v", st.Policy, st.Plan.Source, err)
		}
	})
}
