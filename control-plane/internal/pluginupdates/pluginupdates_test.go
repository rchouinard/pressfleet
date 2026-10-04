package pluginupdates_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs/jobstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/pluginupdates"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const (
	pluginsJSON = `[{"name":"woocommerce","title":"WooCommerce","version":"9.1.0","update_version":"9.2.1"},` +
		`{"name":"akismet","title":"Akismet Anti-spam","version":"5.3","update_version":"5.4"}]`
	themesJSON = `[{"name":"twentytwentyfive","title":"Twenty Twenty-Five","version":"1.0","update_version":"1.1"}]`
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    []store.PendingUpdate
		wantErr bool
	}{
		{
			name: "sorted by name",
			out:  pluginsJSON + "\n",
			want: []store.PendingUpdate{
				{Kind: "plugin", Name: "akismet", Title: "Akismet Anti-spam", Version: "5.3", UpdateVersion: "5.4"},
				{Kind: "plugin", Name: "woocommerce", Title: "WooCommerce", Version: "9.1.0", UpdateVersion: "9.2.1"},
			},
		},
		{name: "none", out: "[]", want: []store.PendingUpdate{}},
		{
			name: "notices before the JSON",
			out:  "PHP Notice:  Undefined index: foo in /var/www/html/wp-content/plugins/x/x.php on line 3\n[not json\n" + `[{"name":"x","version":"1","update_version":"2"}]`,
			want: []store.PendingUpdate{{Kind: "plugin", Name: "x", Version: "1", UpdateVersion: "2"}},
		},
		{
			name: "numeric and null versions",
			out:  `[{"name":"x","title":"X","version":2,"update_version":null}]`,
			want: []store.PendingUpdate{{Kind: "plugin", Name: "x", Title: "X", Version: "2"}},
		},
		{name: "empty output", out: "", wantErr: true},
		{name: "not JSON", out: "Error: something", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pluginupdates.Parse("plugin", tt.out)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse err = %v, want error %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestArgsOnlyList guards the decision that the platform never runs an
// update (#92): the only WP-CLI commands are list commands.
func TestArgsOnlyList(t *testing.T) {
	for _, kind := range []string{store.UpdateKindPlugin, store.UpdateKindTheme} {
		args := pluginupdates.Args(kind)
		if args[0] != kind || args[1] != "list" || slices.Contains(args, "update") || !slices.Contains(args, "--update=available") {
			t.Errorf("Args(%s) = %q", kind, args)
		}
	}
}

type fixture struct {
	db    *store.DB
	prov  *provisionertest.Fake
	clock *jobstest.Clock
	pool  *jobs.Pool
}

func newFixture(t *testing.T, db *store.DB) *fixture {
	t.Helper()
	f := &fixture{db: db, prov: provisionertest.New(), clock: jobstest.NewClock(time.Now().Add(2 * pluginupdates.Interval))}
	f.pool = jobs.NewPool(db, jobs.Options{Clock: f.clock, Logger: storetest.Logger()})
	if err := f.pool.Register(pluginupdates.Handler(db, f.prov)); err != nil {
		t.Fatal(err)
	}
	return f
}

// addSite inserts a site row with the given status and, unless phase is
// empty, creates its runtime in the fake with that phase.
func (f *fixture) addSite(t *testing.T, status string, phase provisioner.SitePhase) string {
	t.Helper()
	ctx := context.Background()
	group := &store.ClientGroup{Name: "group-" + id.New()}
	if err := store.NewClientGroups(f.db).Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	siteID := id.New()
	uid, err := store.AllocateSiteUID(ctx, f.db)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := f.db.ExecContext(ctx, `INSERT INTO sites
		(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
		VALUES (?, ?, 'Site', ?, ?, ?, '8.4', '7.1.2', ?, ?, ?)`,
		siteID, group.ID, siteID, status, siteID+".preview.example", uid, now, now); err != nil {
		t.Fatal(err)
	}
	if phase == "" {
		return siteID
	}
	cfg := &provisioner.SiteConfig{
		ID: siteID, ClientGroupID: group.ID, PreviewDomain: siteID + ".preview.example",
		PHPVersion: "8.4", WPCoreVersion: "7.1.2", DBSecretRef: "db", WPSaltsSecretRef: "salts",
		UID: uid, Resources: provisioner.DefaultResourceLimits(),
	}
	if _, err := f.prov.CreateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if phase == provisioner.SiteStopped {
		if err := f.prov.StopSite(ctx, siteID); err != nil {
			t.Fatal(err)
		}
	}
	return siteID
}

func (f *fixture) enqueue(t *testing.T, siteID string) store.Job {
	t.Helper()
	j, err := pluginupdates.NewService(f.db).Check(context.Background(), siteID, "")
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// run runs the next job and returns its final detail.
func (f *fixture) run(t *testing.T, jobID string) store.JobDetail {
	t.Helper()
	ctx := context.Background()
	if ran, err := f.pool.RunNext(ctx); err != nil || !ran {
		t.Fatalf("RunNext = %v, %v", ran, err)
	}
	d, err := store.NewJobs(f.db).Detail(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (f *fixture) wpcliCalls(siteID string) [][]string {
	var out [][]string
	for _, c := range f.prov.Calls() {
		if c.Op == "ExecWPCLI" && c.SiteID == siteID {
			out = append(out, c.Args)
		}
	}
	return out
}

func logText(d store.JobDetail) string {
	var b strings.Builder
	for _, l := range d.Logs {
		b.WriteString(l.Level + ": " + l.Message + "\n")
	}
	return b.String()
}

func listOutput(_ string, args []string) (string, error) {
	if args[0] == "plugin" {
		return pluginsJSON, nil
	}
	return themesJSON, nil
}

// TestScheduledCheckStoresReport follows a check from the schedule to the
// stored report.
func TestScheduledCheckStoresReport(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db)
		siteID := f.addSite(t, "running", provisioner.SiteRunning)
		f.prov.WPCLI = listOutput
		if err := pluginupdates.EnsureSchedule(ctx, db, siteID); err != nil {
			t.Fatal(err)
		}
		sch, err := store.NewSchedules(db).Get(ctx, pluginupdates.ScheduleName(siteID))
		if err != nil {
			t.Fatal(err)
		}
		if sch.Interval != pluginupdates.Interval || sch.LockKey != "site:"+siteID || sch.MaxAttempts != 1 || sch.MissedRunPolicy != store.MissedCoalesce {
			t.Errorf("schedule = %+v", sch)
		}
		sched := jobs.NewScheduler(db, jobs.SchedulerOptions{Clock: jobstest.NewClock(sch.NextRunAt), Logger: storetest.Logger()})
		if n, err := sched.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("scheduler enqueued %d jobs, %v; want 1", n, err)
		}
		page, err := store.NewJobs(db).List(ctx, store.JobFilter{TargetType: "site", TargetID: siteID})
		if err != nil || len(page.Jobs) != 1 {
			t.Fatalf("jobs for site = %+v, %v", page.Jobs, err)
		}

		d := f.run(t, page.Jobs[0].ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s\n%s", d.Job.State, d.Job.Error, logText(d))
		}
		want := [][]string{pluginupdates.Args("plugin"), pluginupdates.Args("theme")}
		if got := f.wpcliCalls(siteID); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("WP-CLI calls = %q, want %q", got, want)
		}
		if !strings.Contains(logText(d), "updates pending: 2 plugins, 1 theme") {
			t.Errorf("log lacks the summary:\n%s", logText(d))
		}
		rep, err := pluginupdates.NewService(db).Report(ctx, siteID)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Plugins != 2 || rep.Themes != 1 || rep.CheckedAt.IsZero() || len(rep.Updates) != 3 ||
			rep.Updates[0].Name != "akismet" || rep.Updates[2].Kind != store.UpdateKindTheme {
			t.Errorf("report = %+v", rep)
		}

		// Updating everything clears the report at the next check.
		f.prov.WPCLI = func(string, []string) (string, error) { return "[]", nil }
		d = f.run(t, f.enqueue(t, siteID).ID)
		if d.Job.State != store.JobSucceeded || !strings.Contains(logText(d), "no plugin or theme updates pending") {
			t.Fatalf("job %s: %s\n%s", d.Job.State, d.Job.Error, logText(d))
		}
		counts, err := pluginupdates.NewService(db).Counts(ctx)
		if err != nil || counts[siteID].Total() != 0 || counts[siteID].CheckedAt.IsZero() {
			t.Errorf("counts = %+v, %v", counts, err)
		}
	})
}

// TestSkips checks that checks of sites that aren't serving succeed without
// a report, and say why.
func TestSkips(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		tests := []struct {
			name     string
			status   string
			phase    provisioner.SitePhase
			wantExec bool
			wantLog  string
		}{
			{"stopped", "stopped", provisioner.SiteStopped, false, "skipped: site is stopped"},
			{"provisioning", "provisioning", "", false, "skipped: site is provisioning"},
			{"deleting", "deleting", provisioner.SiteRunning, false, "skipped: site is deleting"},
			{"containers stopped", "running", provisioner.SiteStopped, true, "containers aren't running"},
			{"containers missing", "failed", "", true, "containers don't exist"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				f := newFixture(t, db)
				f.prov.WPCLI = listOutput
				siteID := f.addSite(t, tt.status, tt.phase)
				d := f.run(t, f.enqueue(t, siteID).ID)
				if d.Job.State != store.JobSucceeded {
					t.Fatalf("job %s: %s\n%s", d.Job.State, d.Job.Error, logText(d))
				}
				if got := len(f.wpcliCalls(siteID)) > 0; got != tt.wantExec {
					t.Errorf("WP-CLI called = %v, want %v", got, tt.wantExec)
				}
				if !strings.Contains(logText(d), tt.wantLog) {
					t.Errorf("log lacks %q:\n%s", tt.wantLog, logText(d))
				}
				if _, err := store.NewUpdateReports(db).Get(ctx, siteID); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("report after a skipped check: err = %v, want ErrNotFound", err)
				}
			})
		}
	})
}

// TestDeletedSite checks that a check queued before the site was deleted
// removes the leftover schedule.
func TestDeletedSite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db)
		siteID := id.New()
		if err := pluginupdates.EnsureSchedule(ctx, db, siteID); err != nil {
			t.Fatal(err)
		}
		j, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: pluginupdates.JobType, TargetType: "site", TargetID: siteID, MaxAttempts: 1})
		if err != nil {
			t.Fatal(err)
		}
		d := f.run(t, j.ID)
		if d.Job.State != store.JobSucceeded || !strings.Contains(logText(d), "no longer exists") {
			t.Fatalf("job %s: %s\n%s", d.Job.State, d.Job.Error, logText(d))
		}
		if _, err := store.NewSchedules(db).Get(ctx, pluginupdates.ScheduleName(siteID)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("schedule after the run: %v, want ErrNotFound", err)
		}
	})
}

// TestFailureKeepsPreviousReport checks that a failed check fails the job
// at once and leaves the last good report alone.
func TestFailureKeepsPreviousReport(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		tests := []struct {
			name    string
			wpcli   func(string, []string) (string, error)
			wantErr string
			wantLog string
		}{
			{
				name: "WP-CLI fails",
				wpcli: func(string, []string) (string, error) {
					return "", &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error: boom"}
				},
				wantErr: "status 255",
				wantLog: "PHP Fatal error: boom",
			},
			{
				name: "theme list fails",
				wpcli: func(_ string, args []string) (string, error) {
					if args[0] == "theme" {
						return "", &provisioner.ExecError{ExitCode: 1, Stderr: "Error: no themes"}
					}
					return "[]", nil
				},
				wantErr: "check theme updates",
				wantLog: "Error: no themes",
			},
			{
				name:    "unexpected output",
				wpcli:   func(string, []string) (string, error) { return "Warning: something", nil },
				wantErr: "no plugin list",
				wantLog: "Warning: something",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				f := newFixture(t, db)
				siteID := f.addSite(t, "running", provisioner.SiteRunning)
				prev := store.UpdateReport{SiteID: siteID, Updates: []store.PendingUpdate{{Kind: "plugin", Name: "x", Version: "1", UpdateVersion: "2"}}}
				if err := store.NewUpdateReports(db).Put(ctx, prev); err != nil {
					t.Fatal(err)
				}
				f.prov.WPCLI = tt.wpcli
				d := f.run(t, f.enqueue(t, siteID).ID)
				if d.Job.State != store.JobFailed || d.Job.Attempts != 1 || !strings.Contains(d.Job.Error, tt.wantErr) {
					t.Fatalf("job %s after %d attempts: %q; want failed after 1 with %q", d.Job.State, d.Job.Attempts, d.Job.Error, tt.wantErr)
				}
				if !strings.Contains(logText(d), tt.wantLog) {
					t.Errorf("log lacks %q:\n%s", tt.wantLog, logText(d))
				}
				rep, err := store.NewUpdateReports(db).Get(ctx, siteID)
				if err != nil || rep.Plugins != 1 {
					t.Errorf("report after a failed check = %+v, %v; want the previous one", rep, err)
				}
				if ran, err := f.pool.RunNext(ctx); err != nil || ran {
					t.Errorf("a failed check was retried (ran %v, %v)", ran, err)
				}
			})
		}
	})
}

// TestService covers the API's side: reports of unchecked and missing
// sites, and manual checks that don't pile up.
func TestService(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db)
		svc := pluginupdates.NewService(db)
		siteID := f.addSite(t, "running", provisioner.SiteRunning)

		rep, err := svc.Report(ctx, siteID)
		if err != nil || !rep.CheckedAt.IsZero() || rep.Updates == nil || len(rep.Updates) != 0 {
			t.Errorf("report of an unchecked site = %+v, %v", rep, err)
		}
		if _, err := svc.Report(ctx, id.New()); !errors.Is(err, pluginupdates.ErrSiteNotFound) {
			t.Errorf("report of a missing site: err = %v, want ErrSiteNotFound", err)
		}
		if _, err := svc.Check(ctx, id.New(), ""); !errors.Is(err, pluginupdates.ErrSiteNotFound) {
			t.Errorf("check of a missing site: err = %v, want ErrSiteNotFound", err)
		}

		user := createUser(ctx, t, db)
		first, err := svc.Check(ctx, siteID, user)
		if err != nil {
			t.Fatal(err)
		}
		if first.Type != pluginupdates.JobType || first.LockKey != "site:"+siteID || first.MaxAttempts != 1 || first.CreatedByUserID != user {
			t.Errorf("check job = %+v", first)
		}
		second, err := svc.Check(ctx, siteID, user)
		if err != nil || second.ID != first.ID {
			t.Errorf("second check = %s, %v; want the queued job %s", second.ID, err, first.ID)
		}
	})
}

func createUser(ctx context.Context, t *testing.T, db *store.DB) string {
	t.Helper()
	u := &store.User{Email: id.New() + "@example.com", PasswordHash: "x", Role: "admin"}
	if err := store.NewUsers(db).Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// TestReconcileSchedules checks that every site gets a schedule and
// leftovers of deleted sites go.
func TestReconcileSchedules(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		f := newFixture(t, db)
		a := f.addSite(t, "running", provisioner.SiteRunning)
		b := f.addSite(t, "stopped", "")
		gone := id.New()
		if err := pluginupdates.EnsureSchedule(ctx, db, gone); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := pluginupdates.ReconcileSchedules(ctx, db); err != nil {
				t.Fatal(err)
			}
		}
		schedules := store.NewSchedules(db)
		for _, siteID := range []string{a, b} {
			if _, err := schedules.Get(ctx, pluginupdates.ScheduleName(siteID)); err != nil {
				t.Errorf("schedule of site %s: %v", siteID, err)
			}
		}
		if _, err := schedules.Get(ctx, pluginupdates.ScheduleName(gone)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("schedule of a deleted site: %v, want ErrNotFound", err)
		}
	})
}
