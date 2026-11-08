package sites_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/objectcachetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitecron"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

func TestSlugify(t *testing.T) {
	for _, tt := range []struct{ name, want string }{
		{"Acme", "acme"},
		{"Acme Marketing Site", "acme-marketing-site"},
		{"  --Acme__Corp!!  ", "acme-corp"},
		{"Café Crème", "cafe-creme"},
		{"Ｆｕｌｌｗｉｄｔｈ ２０２６", "fullwidth-2026"},
		{"日本語", "site"},
		{"!!!", "site"},
		{"", "site"},
		{strings.Repeat("a", 60), strings.Repeat("a", 40)},
		{strings.Repeat("a", 39) + " b", strings.Repeat("a", 39)},
	} {
		if got := sites.Slugify(tt.name); got != tt.want {
			t.Errorf("Slugify(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		str := func(s string) *string { return &s }
		num := func(n int) *int { return &n }
		for _, tt := range []struct {
			name   string
			req    sites.NewSite
			fields []string
		}{
			{"blank name", sites.NewSite{Name: "   "}, []string{"/name"}},
			{"unknown PHP", sites.NewSite{Name: "x", PHPVersion: "7.4"}, []string{"/php_version"}},
			{"PHP past end of life", sites.NewSite{Name: "x", PHPVersion: "8.1"}, []string{"/php_version"}},
			{"CPU", sites.NewSite{Name: "x", CPU: str("0.1")}, []string{"/resources/cpu"}},
			{"CPU not a number", sites.NewSite{Name: "x", CPU: str("lots")}, []string{"/resources/cpu"}},
			{"memory", sites.NewSite{Name: "x", MemoryLimitMB: num(128), MemoryReservationMB: num(16)},
				[]string{"/resources/memory_limit_mb", "/resources/memory_reservation_mb"}},
			{"reservation above limit", sites.NewSite{Name: "x", MemoryLimitMB: num(512), MemoryReservationMB: num(1024)},
				[]string{"/resources/memory_reservation_mb"}},
			{"unknown client group", sites.NewSite{Name: "x", ClientGroupID: id.New()}, []string{"/client_group_id"}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				req := tt.req
				req.AdminEmail = "owner@example.com"
				if req.ClientGroupID == "" {
					req.ClientGroupID = e.group
				}
				_, _, err := e.svc.Create(ctx, req)
				var ve *sites.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %v, want a ValidationError", err)
				}
				var got []string
				for _, f := range ve.Fields {
					got = append(got, f.Field)
				}
				if !slices.Equal(got, tt.fields) {
					t.Errorf("fields = %v, want %v (%v)", got, tt.fields, err)
				}
			})
		}

		// Without a preview base domain nothing can be created.
		noPreview := newEnvWith(t, db, "")
		if _, _, err := noPreview.svc.Create(ctx, sites.NewSite{ClientGroupID: e.group, Name: "x", AdminEmail: "a@example.com"}); !errors.Is(err, sites.ErrNoPreviewDomain) {
			t.Errorf("without a preview base domain: err = %v", err)
		}

		// Rejected requests leave nothing behind.
		for _, table := range []string{"sites", "secrets", "jobs", "schedules"} {
			if n := e.count(t, "SELECT COUNT(*) FROM "+table); n != 0 {
				t.Errorf("%d rows in %s after rejected requests", n, table)
			}
		}
		if n := e.count(t, "SELECT last_uid FROM site_uid_counter"); n != provisioner.MinSiteUID-1 {
			t.Errorf("rejected requests used up UIDs: counter at %d", n)
		}
	})
}

func TestProvisioning(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site, job := e.create(t, "Acme Marketing")

		// The request's transaction recorded everything the job needs.
		if site.Slug != "acme-marketing" || site.PreviewDomain != "acme-marketing."+testPreviewBase ||
			site.Status != store.SiteProvisioning || site.PHPVersion != "8.4" || site.WPCoreVersion != "7.1.2" ||
			site.UID != provisioner.MinSiteUID || site.CPULimit != provisioner.DefaultCPU ||
			site.MemoryLimitMB != provisioner.DefaultMemoryLimitMB || site.MemoryReservationMB != provisioner.DefaultMemoryReservationMB ||
			!strings.HasPrefix(site.AdminUsername, "admin-") || site.AdminEmail != "owner@example.com" {
			t.Errorf("site = %+v", site)
		}
		if job.Type != sites.CreateJobType || job.TargetType != store.JobTargetSite || job.TargetID != site.ID ||
			job.LockKey != store.SiteLockKey(site.ID) || job.State != store.JobQueued {
			t.Errorf("job = %+v", job)
		}
		salts, err := e.sec.Get(ctx, secrets.Ref(site.WPSaltsSecretRef))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wpconfig.UnmarshalSalts(salts); err != nil {
			t.Errorf("stored salts: %v", err)
		}
		password, err := e.sec.Get(ctx, secrets.Ref(site.AdminPasswordSecretRef))
		if err != nil || len(password.Reveal()) < 20 {
			t.Fatalf("admin password: %v", err)
		}
		if _, err := store.NewSchedules(db).Get(ctx, sitecron.ScheduleName(site.ID)); err != nil {
			t.Errorf("cron schedule: %v", err)
		}
		if sch, err := store.NewSchedules(db).Get(ctx, sitebackup.ScheduleName(site.ID)); err != nil || sch.Interval != 24*time.Hour {
			t.Errorf("backup schedule: %+v, %v", sch, err)
		}
		if _, err := e.svc.TakeAdminCredentials(ctx, site.ID); !errors.Is(err, sites.ErrNotReady) {
			t.Errorf("credentials before provisioning: err = %v, want ErrNotReady", err)
		}

		e.runJobs(t)
		d := e.job(t, job.ID)
		if d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		var steps []string
		for _, s := range d.Steps {
			steps = append(steps, s.Name)
		}
		if want := []string{"prepare", "create_database", "create_object_cache", "start_stack", "install_wordpress", "configure_plugins", "finish"}; !slices.Equal(steps, want) {
			t.Errorf("steps = %v", steps)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteRunning || got.StatusMessage != "" {
			t.Errorf("site after provisioning: %+v", got)
		}

		// The runtime got the site's configuration, by reference.
		rt, ok := e.prov.Site(site.ID)
		if !ok || rt.Phase != provisioner.SiteRunning {
			t.Fatalf("provisioner site = %+v, %v", rt, ok)
		}
		cfg := rt.Config
		if cfg.UID != site.UID || cfg.WPSaltsSecretRef != site.WPSaltsSecretRef || cfg.PreviewDomain != site.PreviewDomain ||
			cfg.DBSecretRef == "" || cfg.PHPVersion != "8.4" || cfg.ClientGroupID != e.group ||
			cfg.RedisSecretRef != objectcachetest.SecretRef(site.ID) {
			t.Errorf("site config = %+v", cfg)
		}
		if _, ok := e.dbs.Data(site.ID); !ok {
			t.Error("no site database")
		}
		if !e.cache.HasUser(site.ID) {
			t.Error("no object cache user")
		}

		// WordPress was installed once, at the preview URL, with the stored
		// password, without mail.
		installs := e.wp.installCalls(site.ID)
		if len(installs) != 1 {
			t.Fatalf("install calls = %v", installs)
		}
		for _, want := range []string{
			"--url=https://acme-marketing." + testPreviewBase, "--title=Acme Marketing", "--admin_user=" + site.AdminUsername,
			"--prompt=admin_password", "--admin_email=owner@example.com", "--skip-email",
		} {
			if !slices.Contains(installs[0], want) {
				t.Errorf("install args %q lack %q", installs[0], want)
			}
		}
		// The password goes on standard input, never in argv, which a
		// driver may log (the Kubernetes API server's audit log).
		var stdin []string
		for _, c := range e.prov.Calls() {
			if c.Op == "ExecWPCLI" && c.SiteID == site.ID && strings.Contains(strings.Join(c.Args, " "), password.Reveal()) {
				t.Errorf("WP-CLI argv holds the admin password: %q", c.Args)
			}
			if c.Op == "ExecWPCLI" && c.SiteID == site.ID && len(c.Args) > 1 && c.Args[0] == "core" && c.Args[1] == "install" {
				stdin = append(stdin, c.Stdin)
			}
		}
		if !slices.Equal(stdin, []string{password.Reveal() + "\n"}) {
			t.Errorf("install standard input = %q, want the password and a newline", stdin)
		}
		if strings.Contains(jobLog(d), password.Reveal()) {
			t.Error("the admin password appears in the job's log, errors, data, or result")
		}

		// The credentials can be taken exactly once.
		creds, err := e.svc.TakeAdminCredentials(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if creds.Password != password || creds.Username != site.AdminUsername || creds.Email != "owner@example.com" ||
			creds.LoginURL != "https://acme-marketing."+testPreviewBase+"/wp-login.php" {
			t.Errorf("credentials = %+v", creds)
		}
		if _, err := e.svc.TakeAdminCredentials(ctx, site.ID); !errors.Is(err, sites.ErrCredentialsGone) {
			t.Errorf("second retrieval: err = %v, want ErrCredentialsGone", err)
		}
		if _, err := e.sec.Get(ctx, secrets.Ref(site.AdminPasswordSecretRef)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("the password secret still exists: %v", err)
		}
		if _, err := e.svc.TakeAdminCredentials(ctx, id.New()); !errors.Is(err, sites.ErrNotFound) {
			t.Errorf("missing site: err = %v, want ErrNotFound", err)
		}

		// A second site with the same name gets the next slug.
		second, _ := e.create(t, "ACME marketing!")
		if second.Slug != "acme-marketing-2" || second.PreviewDomain != "acme-marketing-2."+testPreviewBase || second.UID != site.UID+1 {
			t.Errorf("second site = %+v", second)
		}
	})
}

func TestProvisioningRetriesAreIdempotent(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		// The stack fails to start the first time, and the install's
		// outcome is lost once after it succeeded (as if the worker died).
		failed := map[string]bool{}
		e.prov.Err = func(op, _ string) error {
			if op == "CreateSite" && !failed[op] {
				failed[op] = true
				return errors.New("docker daemon hiccup")
			}
			return nil
		}
		e.wp.failInstall, e.wp.installAnyway = errors.New("connection reset"), true
		site, job := e.create(t, "Retry")
		attempts := e.runJobs(t)
		d := e.job(t, job.ID)
		if d.Job.State != store.JobSucceeded || attempts != 3 {
			t.Fatalf("job %s after %d attempts: %s", d.Job.State, attempts, d.Job.Error)
		}
		if n := len(e.wp.installCalls(site.ID)); n != 1 {
			t.Errorf("WordPress installed %d times, want once", n)
		}
		creates := 0
		for _, c := range e.dbs.Calls() {
			if c.Op == "CreateDatabase" {
				creates++
			}
		}
		if creates != 1 {
			t.Errorf("CreateDatabase called %d times", creates)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteRunning {
			t.Errorf("site = %+v", got)
		}
	})
}

func TestFailedProvisioningLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.wp.failInstall = &provisioner.ExecError{ExitCode: 1, Stderr: "Error: the database is on fire"}
		site, job := e.create(t, "Doomed")
		e.runJobs(t)

		d := e.job(t, job.ID)
		if d.Job.State != store.JobFailed {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		if _, ok := e.prov.Site(site.ID); ok {
			t.Error("the stack is still there")
		}
		if _, ok := e.dbs.Data(site.ID); ok {
			t.Error("the database is still there")
		}
		if e.cache.HasUser(site.ID) {
			t.Error("the object cache user is still there")
		}
		// The salts and the admin password were the only secrets (the fake
		// database and object cache managers store none).
		if n := e.count(t, "SELECT COUNT(*) FROM secrets"); n != 0 {
			t.Errorf("%d secrets left", n)
		}
		if _, err := store.NewSchedules(db).Get(ctx, sitecron.ScheduleName(site.ID)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("cron schedule: err = %v, want ErrNotFound", err)
		}
		if _, err := store.NewSchedules(db).Get(ctx, sitebackup.ScheduleName(site.ID)); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("backup schedule: err = %v, want ErrNotFound", err)
		}
		// The site stays, failed, with the reason.
		got := e.site(t, site.ID)
		if got.Status != store.SiteFailed || !strings.Contains(got.StatusMessage, "database is on fire") ||
			got.WPSaltsSecretRef != "" || got.AdminPasswordSecretRef != "" {
			t.Errorf("site = %+v", got)
		}
		if _, err := e.svc.TakeAdminCredentials(ctx, site.ID); !errors.Is(err, sites.ErrCredentialsGone) {
			t.Errorf("credentials of a failed site: err = %v, want ErrCredentialsGone", err)
		}
	})
}

func TestProvisioningStopsForSiteBeingDeleted(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site, job := e.create(t, "Gone")
		if _, err := store.NewSites(db).SetStatus(ctx, site.ID, store.SiteDeleting, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobFailed || !strings.Contains(d.Job.Error, "deleting") {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		// Nothing was provisioned, and the deletion's status stands.
		if len(e.prov.Calls()) != 0 || len(e.dbs.Calls()) != 0 {
			t.Errorf("provisioner calls %v, database calls %v", e.prov.Calls(), e.dbs.Calls())
		}
		if got := e.site(t, site.ID); got.Status != store.SiteDeleting {
			t.Errorf("site = %+v", got)
		}
	})
}

func TestConcurrentCreationsGetDistinctSlugs(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		const n = 3
		slugs := make(chan string, n)
		errs := make(chan error, n)
		var wg sync.WaitGroup
		for range n {
			wg.Go(func() {
				site, _, err := e.svc.Create(context.Background(), sites.NewSite{ClientGroupID: e.group, Name: "Same", AdminEmail: "a@example.com"})
				if err != nil {
					errs <- err
					return
				}
				slugs <- site.Slug
			})
		}
		wg.Wait()
		close(slugs)
		close(errs)
		for err := range errs {
			t.Errorf("Create: %v", err)
		}
		var got []string
		for s := range slugs {
			got = append(got, s)
		}
		slices.Sort(got)
		if want := []string{"same", "same-2", "same-3"}; !slices.Equal(got, want) {
			t.Errorf("slugs = %v, want %v", got, want)
		}
	})
}

type fakeAttacher struct {
	calls []string
	errs  map[string]error
}

func (f *fakeAttacher) AttachSharedServices(_ context.Context, siteID string) error {
	f.calls = append(f.calls, siteID)
	return f.errs[siteID]
}

func TestReattachSharedServices(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		running, _ := e.create(t, "Running")
		missing, _ := e.create(t, "Missing")
		broken, _ := e.create(t, "Broken")
		deleting, _ := e.create(t, "Deleting")
		if _, err := store.NewSites(db).SetStatus(ctx, deleting.ID, store.SiteDeleting, ""); err != nil {
			t.Fatal(err)
		}
		a := &fakeAttacher{errs: map[string]error{
			missing.ID: provisioner.ErrSiteNotFound,
			broken.ID:  errors.New("mariadb container not found"),
		}}
		e.svc.ReattachSharedServices(ctx, a)
		want := []string{running.ID, missing.ID, broken.ID}
		slices.Sort(want)
		if !slices.Equal(a.calls, want) {
			t.Errorf("attached %v, want %v (every site but the one being deleted)", a.calls, want)
		}
	})
}
