package api_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/backuptest"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/objectcachetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/sitedbtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

const testPreviewBase = "preview.example.test"

// implementedSiteOps are the site operations that no longer answer 501.
var implementedSiteOps = map[string]bool{
	"getSiteCore":                   true,
	"setSiteCorePolicy":             true,
	"updateSiteCore":                true,
	"getCoreRollout":                true,
	"resumeCoreRollout":             true,
	"listSites":                     true,
	"createSite":                    true,
	"getSite":                       true,
	"updateSite":                    true,
	"regenerateSitePreviewPassword": true,
	"setSiteMaintenance":            true,
	"goLiveSite":                    true,
	"setSiteAliases":                true,
	"deleteSite":                    true,
	"startSite":                     true,
	"stopSite":                      true,
	"purgeSiteCache":                true,
	"retrieveSiteAdminCredentials":  true,
	"listSiteBackups":               true,
	"createSiteBackup":              true,
	"getSiteBackupSchedule":         true,
	"setSiteBackupSchedule":         true,
	"restoreSite":                   true,
	"importSite":                    true,
	"exportSiteDatabase":            true,
	"downloadSiteDatabaseExport":    true,
}

func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// siteEnv is a site service and a backup service on fakes, with a job pool
// to run their jobs.
type siteEnv struct {
	dns     *fakeDNS
	svc     *sites.Service
	prov    *provisionertest.Fake
	dbs     *sitedbtest.Fake
	engine  *backuptest.Fake
	backups *sitebackup.Service
	uploads *uploads.Service
	pool    *jobs.Pool
}

func newSiteEnv(t *testing.T, db *store.DB, previewBase string) *siteEnv {
	t.Helper()
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	sec := secrets.NewStore(db, secretstest.Keyring(t, 1))
	e := &siteEnv{prov: provisionertest.New(), dbs: sitedbtest.New(), dns: &fakeDNS{wrong: map[string]string{}}}
	installed := map[string]bool{}
	e.prov.WPCLI = func(siteID string, args []string) (string, error) {
		if slices.Equal(args, []string{"core", "is-installed"}) && !installed[siteID] {
			return "", &provisioner.ExecError{ExitCode: 1}
		}
		if len(args) > 1 && args[1] == "install" {
			installed[siteID] = true
		}
		return "", nil
	}
	e.engine = backuptest.New()
	e.backups, err = sitebackup.New(sitebackup.Options{DB: db, Engine: e.engine, Logger: storetest.Logger()})
	if err != nil {
		t.Fatal(err)
	}
	e.uploads = newUploadService(t, db)
	e.svc, err = sites.New(sites.Options{
		DB: db, Secrets: sec, Catalog: cat, PreviewBaseDomain: previewBase,
		Provisioner: e.prov, Databases: e.dbs, ObjectCaches: objectcachetest.New(), Logger: storetest.Logger(),
		DNSChecker: e.dns, Backups: e.backups, BackupEngine: e.engine,
		Uploads: e.uploads, Tasks: e.prov, TmpDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.pool = jobs.NewPool(db, jobs.Options{Logger: storetest.Logger(), Backoff: func(int) time.Duration { return 0 }, Secrets: sec})
	for _, h := range append(e.svc.Handlers(), e.backups.Handler()) {
		if err := e.pool.Register(h); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *siteEnv) runJobs(t *testing.T) {
	t.Helper()
	for range 100 {
		ran, err := e.pool.RunNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			return
		}
	}
	t.Fatal("jobs never finished")
}

func TestSitesEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc})
		// Jobs record the user who started them: fakeAuth's admin.
		now := time.Now()
		if _, err := db.ExecContext(ctx, "INSERT INTO users (id, email, password_hash, role, created_at, updated_at) VALUES (?, ?, 'x', 'admin', ?, ?)",
			"01j9zq3v6t8k4m2n5p7r9s1x3y", "admin@example.com", now, now); err != nil {
			t.Fatal(err)
		}
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}

		// Create: 202 with the site and its provisioning job.
		body := `{"client_group_id":"` + group.ID + `","name":"Acme Blog","admin_email":"owner@example.com",` +
			`"admin_username":"acme-admin","php_version":"8.4","resources":{"memory_limit_mb":1024}}`
		w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/sites", body: body})
		if w.Code != http.StatusAccepted {
			t.Fatalf("create: %d %s", w.Code, w.Body)
		}
		accepted := decodeJSON[gen.SiteAccepted](t, w.Body.Bytes())
		site, job := accepted.Site, accepted.Job
		if !id.Valid(site.ID) || site.ClientGroupID != group.ID || site.Name != "Acme Blog" || site.Slug != "acme-blog" ||
			site.PreviewDomain != "acme-blog."+testPreviewBase || site.Status != gen.SiteStatusProvisioning ||
			site.PhpVersion != "8.4" || site.Resources.MemoryLimitMb != 1024 || site.Resources.MemoryReservationMb != provisioner.DefaultMemoryReservationMB ||
			!site.AdminCredentialsAvailable || site.URL == nil || *site.URL != "https://acme-blog."+testPreviewBase ||
			site.PrimaryDomain != nil || site.Aliases == nil || site.ParentSiteID != nil {
			t.Errorf("site = %s", w.Body)
		}
		if job.Type != sites.CreateJobType || job.Target.ID != site.ID || job.Target.Type != gen.JobTargetTypeSite ||
			job.State != gen.JobStateQueued || job.CreatedBy == nil {
			t.Errorf("job = %s", w.Body)
		}
		if loc := w.Header().Get("Location"); loc != "/api/v1/jobs/"+job.ID {
			t.Errorf("Location = %q", loc)
		}

		// Get.
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + site.ID})
		if got := decodeJSON[gen.Site](t, w.Body.Bytes()); w.Code != http.StatusOK || got.ID != site.ID || got.Status != gen.SiteStatusProvisioning {
			t.Errorf("get: %d %s", w.Code, w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + id.New()}); w.Code != http.StatusNotFound {
			t.Errorf("get missing: %d %s", w.Code, w.Body)
		}

		// The credentials wait for WordPress, then are handed out once.
		creds := "/api/v1/sites/" + site.ID + "/admin-credentials"
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: creds}); w.Code != http.StatusConflict {
			t.Errorf("credentials while provisioning: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + site.ID})
		if got := decodeJSON[gen.Site](t, w.Body.Bytes()); got.Status != gen.SiteStatusRunning || !got.AdminCredentialsAvailable {
			t.Errorf("after provisioning: %s", w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodPost, target: creds})
		if w.Code != http.StatusOK {
			t.Fatalf("credentials: %d %s", w.Code, w.Body)
		}
		c := decodeJSON[gen.SiteAdminCredentials](t, w.Body.Bytes())
		if c.Username != "acme-admin" || c.Email != "owner@example.com" || len(c.Password) < 20 ||
			c.LoginURL != "https://acme-blog."+testPreviewBase+"/wp-login.php" {
			t.Errorf("credentials = %s", w.Body)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("Cache-Control = %q", cc)
		}
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: creds}); w.Code != http.StatusGone {
			t.Errorf("credentials again: %d %s", w.Code, w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + site.ID})
		if got := decodeJSON[gen.Site](t, w.Body.Bytes()); got.AdminCredentialsAvailable {
			t.Errorf("credentials still available: %s", w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/sites/" + id.New() + "/admin-credentials"}); w.Code != http.StatusNotFound {
			t.Errorf("credentials of a missing site: %d %s", w.Code, w.Body)
		}

		// The audit log names the new site, and never holds the password.
		events, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{TargetType: "site", TargetID: site.ID})
		if err != nil {
			t.Fatal(err)
		}
		var actions []string
		for _, ev := range events.Events {
			actions = append(actions, ev.Action)
		}
		if !slices.Contains(actions, "site.create") || !slices.Contains(actions, "site.credentials.retrieve") {
			t.Errorf("audit actions = %v", actions)
		}

		// Validation.
		for _, tt := range []struct {
			name, body string
			field      string
		}{
			{"unknown group", `{"client_group_id":"` + id.New() + `","name":"x","admin_email":"a@example.com"}`, "/client_group_id"},
			{"unknown PHP", `{"client_group_id":"` + group.ID + `","name":"x","admin_email":"a@example.com","php_version":"5.6"}`, "/php_version"},
			{"tiny memory", `{"client_group_id":"` + group.ID + `","name":"x","admin_email":"a@example.com","resources":{"memory_limit_mb":128}}`, "/resources/memory_limit_mb"},
		} {
			w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/sites", body: tt.body})
			if w.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s: %d %s", tt.name, w.Code, w.Body)
				continue
			}
			if p := decodeProblem(t, w); len(p.Errors) != 1 || p.Errors[0].Field != tt.field {
				t.Errorf("%s: errors = %+v", tt.name, p.Errors)
			}
		}

		// Without a preview base domain, creation is refused with a reason.
		noPreview := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: newSiteEnv(t, db, "").svc})
		w = do(t, noPreview, request{authed: true, method: http.MethodPost, target: "/api/v1/sites",
			body: `{"client_group_id":"` + group.ID + `","name":"x","admin_email":"a@example.com"}`})
		if w.Code != http.StatusConflict {
			t.Fatalf("without a preview base domain: %d %s", w.Code, w.Body)
		}
		if p := decodeProblem(t, w); !strings.Contains(p.Detail, "preview_base_domain") {
			t.Errorf("detail = %q", p.Detail)
		}

		// Delete: 202 with the job; asking again returns the same job.
		target := "/api/v1/sites/" + site.ID
		w = do(t, h, request{authed: true, method: http.MethodDelete, target: target})
		if w.Code != http.StatusAccepted {
			t.Fatalf("delete: %d %s", w.Code, w.Body)
		}
		del := decodeJSON[gen.JobAccepted](t, w.Body.Bytes()).Job
		if del.Type != sites.DeleteJobType || del.Target.ID != site.ID || w.Header().Get("Location") != "/api/v1/jobs/"+del.ID {
			t.Errorf("delete = %s (Location %q)", w.Body, w.Header().Get("Location"))
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: target})
		if got := decodeJSON[gen.Site](t, w.Body.Bytes()); got.Status != gen.SiteStatusDeleting {
			t.Errorf("status while deleting: %s", w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodDelete, target: target})
		if w.Code != http.StatusAccepted || decodeJSON[gen.JobAccepted](t, w.Body.Bytes()).Job.ID != del.ID {
			t.Errorf("delete again: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: target}); w.Code != http.StatusNotFound {
			t.Errorf("get after delete: %d %s", w.Code, w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodDelete, target: target}); w.Code != http.StatusNotFound {
			t.Errorf("delete a deleted site: %d %s", w.Code, w.Body)
		}
		if _, ok := e.prov.Site(site.ID); ok {
			t.Error("the stack survived deletion")
		}
		// The client group can go now that its site has.
		if w := do(t, h, request{authed: true, method: http.MethodDelete, target: "/api/v1/client-groups/" + group.ID}); w.Code != http.StatusNoContent {
			t.Errorf("delete the emptied group: %d %s", w.Code, w.Body)
		}
	})
}

func TestSiteManagementEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc})
		now := time.Now()
		if _, err := db.ExecContext(ctx, "INSERT INTO users (id, email, password_hash, role, created_at, updated_at) VALUES (?, ?, 'x', 'admin', ?, ?)",
			"01j9zq3v6t8k4m2n5p7r9s1x3y", "admin@example.com", now, now); err != nil {
			t.Fatal(err)
		}
		var groups [2]string
		for i := range groups {
			g := &store.ClientGroup{Name: "Group " + id.New()}
			if err := store.NewClientGroups(db).Create(ctx, g); err != nil {
				t.Fatal(err)
			}
			groups[i] = g.ID
		}
		var (
			ids    []string
			byID   = map[string]string{}
			groupB []string
		)
		for i, name := range []string{"One", "Two", "Three"} {
			site, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: groups[min(i, 1)], Name: name, AdminEmail: "o@example.com", PHPVersion: "8.4"})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, site.ID)
			byID[site.ID] = name
			if i > 0 {
				groupB = append(groupB, site.ID)
			}
		}
		e.runJobs(t)
		// Newest first is ID order, which is creation order to the
		// millisecond.
		order := func(siteIDs []string) []string {
			sorted := slices.Clone(siteIDs)
			slices.Sort(sorted)
			slices.Reverse(sorted)
			var names []string
			for _, s := range sorted {
				names = append(names, byID[s])
			}
			return names
		}
		all, inB := order(ids), order(groupB)

		list := func(query string) gen.SiteList {
			t.Helper()
			w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites" + query})
			if w.Code != http.StatusOK {
				t.Fatalf("list %q: %d %s", query, w.Code, w.Body)
			}
			return decodeJSON[gen.SiteList](t, w.Body.Bytes())
		}
		names := func(l gen.SiteList) string {
			var out []string
			for _, s := range l.Items {
				out = append(out, s.Name)
				if s.Runtime != nil || s.Aliases == nil {
					t.Errorf("listed site %s: runtime %v, aliases %v", s.Name, s.Runtime, s.Aliases)
				}
			}
			return strings.Join(out, ",")
		}

		// List: newest first, paginated, filtered.
		if got := list(""); names(got) != strings.Join(all, ",") || got.NextCursor != nil {
			t.Errorf("list = %s, next %v", names(got), got.NextCursor)
		}
		page := list("?limit=2")
		if names(page) != strings.Join(all[:2], ",") || page.NextCursor == nil {
			t.Fatalf("first page = %s, next %v", names(page), page.NextCursor)
		}
		if got := list("?limit=2&cursor=" + *page.NextCursor); names(got) != all[2] || got.NextCursor != nil {
			t.Errorf("second page = %s, next %v", names(got), got.NextCursor)
		}
		if got := list("?client_group_id=" + groups[1]); names(got) != strings.Join(inB, ",") {
			t.Errorf("group filter = %s", names(got))
		}
		if got := list("?client_group_id=" + id.New()); names(got) != "" {
			t.Errorf("unknown group = %s", names(got))
		}
		if got := list("?status=stopped"); names(got) != "" {
			t.Errorf("status filter = %s", names(got))
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites?cursor=nope"}); w.Code != http.StatusBadRequest {
			t.Errorf("bad cursor: %d %s", w.Code, w.Body)
		}

		siteID := ids[0]
		target := "/api/v1/sites/" + siteID
		get := func() gen.Site {
			t.Helper()
			w := do(t, h, request{authed: true, method: http.MethodGet, target: target})
			if w.Code != http.StatusOK {
				t.Fatalf("get: %d %s", w.Code, w.Body)
			}
			return decodeJSON[gen.Site](t, w.Body.Bytes())
		}

		// Get includes the live runtime state, and falls back to the
		// recorded status when the runtime can't be asked.
		if got := get(); got.Runtime == nil || got.Runtime.Phase != gen.SiteRuntimePhaseRunning {
			t.Errorf("runtime = %+v", got.Runtime)
		}
		e.prov.Err = func(op, _ string) error {
			if op == "GetSite" {
				return errors.New("cannot connect to the Docker daemon")
			}
			return nil
		}
		if got := get(); got.Status != gen.SiteStatusRunning || got.Runtime == nil || got.Runtime.Phase != gen.SiteRuntimePhaseUnknown ||
			got.Runtime.Message == nil || strings.Contains(*got.Runtime.Message, "daemon") {
			t.Errorf("with the runtime down: status %s, runtime %+v", got.Status, got.Runtime)
		}
		e.prov.Err = nil

		// PATCH: 202 with the desired state saved and the job queued;
		// further changes wait for it.
		w := do(t, h, request{authed: true, method: http.MethodPatch, target: target, body: `{"php_version":"8.3","resources":{"memory_limit_mb":768}}`})
		if w.Code != http.StatusAccepted {
			t.Fatalf("patch: %d %s", w.Code, w.Body)
		}
		acc := decodeJSON[gen.SiteAccepted](t, w.Body.Bytes())
		if acc.Site.Status != gen.SiteStatusUpdating || acc.Site.PhpVersion != "8.3" || acc.Site.Resources.MemoryLimitMb != 768 ||
			acc.Job.Type != sites.UpdateJobType || w.Header().Get("Location") != "/api/v1/jobs/"+acc.Job.ID {
			t.Errorf("patch = %s", w.Body)
		}
		for _, req := range []request{
			{method: http.MethodPatch, target: target, body: `{"php_version":"8.4"}`},
			{method: http.MethodPost, target: target + "/stop"},
		} {
			req.authed = true
			if w := do(t, h, req); w.Code != http.StatusConflict {
				t.Errorf("%s %s while updating: %d %s", req.method, req.target, w.Code, w.Body)
			} else if p := decodeProblem(t, w); !strings.Contains(p.Detail, "updating") {
				t.Errorf("detail = %q", p.Detail)
			}
		}
		e.runJobs(t)
		if got := get(); got.Status != gen.SiteStatusRunning || got.PhpVersion != "8.3" {
			t.Errorf("after the update: %+v", got)
		}
		if s, _ := e.prov.Site(siteID); s.Config.PHPVersion != "8.3" || s.Config.Resources.MemoryLimitMB != 768 {
			t.Errorf("provisioner config = %+v", s.Config)
		}

		// Preview password protection (#107).
		patchPreview := func(body string) gen.SiteAccepted {
			t.Helper()
			w := do(t, h, request{authed: true, method: http.MethodPatch, target: target, body: `{"preview_protection":` + body + `}`})
			if w.Code != http.StatusAccepted {
				t.Fatalf("patch preview protection %s: %d %s", body, w.Code, w.Body)
			}
			acc := decodeJSON[gen.SiteAccepted](t, w.Body.Bytes())
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
			}
			e.runJobs(t)
			return acc
		}
		previewAuth := func() *provisioner.BasicAuth {
			t.Helper()
			s, _ := e.prov.Site(siteID)
			return s.Config.PreviewAuth
		}
		checkPassword := func(auth *provisioner.BasicAuth, password string) {
			t.Helper()
			if auth == nil || bcrypt.CompareHashAndPassword([]byte(auth.PasswordHash), []byte(password)) != nil {
				t.Errorf("the provisioner's preview auth %+v doesn't match the password", auth)
			}
		}
		if got := get(); got.PreviewProtection.Enabled || got.PreviewProtection.Username != nil || previewAuth() != nil {
			t.Errorf("new site has preview protection: %+v", got.PreviewProtection)
		}
		// Turning it on the first time generates the password, returned once.
		acc = patchPreview(`{"enabled":true}`)
		creds := acc.PreviewCredentials
		if creds == nil || creds.Username != "one" || len(creds.Password) < 20 || !acc.Site.PreviewProtection.Enabled ||
			orEmpty(acc.Site.PreviewProtection.Username) != "one" || acc.Job.Type != sites.UpdateJobType {
			t.Fatalf("enabling preview protection = %+v", acc)
		}
		checkPassword(previewAuth(), creds.Password)
		if a := previewAuth(); a.Username != "one" {
			t.Errorf("preview auth username = %q", a.Username)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: target}); strings.Contains(w.Body.String(), creds.Password) {
			t.Error("GET returns the preview password")
		}
		// Off, and on again with the same credentials, which aren't shown.
		if acc := patchPreview(`{"enabled":false}`); acc.PreviewCredentials != nil || acc.Site.PreviewProtection.Enabled || previewAuth() != nil {
			t.Errorf("disabling = %+v, provisioner %+v", acc, previewAuth())
		}
		if acc := patchPreview(`{"enabled":true,"username":"client"}`); acc.PreviewCredentials != nil || orEmpty(acc.Site.PreviewProtection.Username) != "client" {
			t.Errorf("enabling again = %+v", acc)
		}
		checkPassword(previewAuth(), creds.Password)
		// A new password replaces the old one.
		w = do(t, h, request{authed: true, method: http.MethodPost, target: target + "/preview-password"})
		if w.Code != http.StatusAccepted || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("regenerate: %d %v %s", w.Code, w.Header(), w.Body)
		}
		regen := decodeJSON[gen.PreviewPasswordAccepted](t, w.Body.Bytes())
		if regen.Credentials.Username != "client" || regen.Credentials.Password == creds.Password || regen.Job.Type != sites.UpdateJobType ||
			w.Header().Get("Location") != "/api/v1/jobs/"+regen.Job.ID {
			t.Errorf("regenerate = %s", w.Body)
		}
		e.runJobs(t)
		checkPassword(previewAuth(), regen.Credentials.Password)
		var previewSecrets int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM secrets WHERE kind = ?", string(secrets.KindSitePreviewPassword)).Scan(&previewSecrets); err != nil || previewSecrets != 1 {
			t.Errorf("preview password secrets = %d, %v; want the new one only", previewSecrets, err)
		}
		for _, body := range []string{`{"preview_protection":{"username":"a:b"}}`, `{"preview_protection":{}}`} {
			if w := do(t, h, request{authed: true, method: http.MethodPatch, target: target, body: body}); w.Code != http.StatusUnprocessableEntity {
				t.Errorf("patch %s: %d %s", body, w.Code, w.Body)
			}
		}
		patchPreview(`{"enabled":false}`)

		// Maintenance mode (#44).
		for _, enabled := range []bool{true, false} {
			body := `{"enabled":false}`
			if enabled {
				body = `{"enabled":true}`
			}
			w := do(t, h, request{authed: true, method: http.MethodPut, target: target + "/maintenance", body: body})
			if w.Code != http.StatusAccepted {
				t.Fatalf("maintenance %v: %d %s", enabled, w.Code, w.Body)
			}
			acc := decodeJSON[gen.SiteAccepted](t, w.Body.Bytes())
			if acc.Site.Maintenance != enabled || acc.Site.Status != gen.SiteStatusRunning || acc.Job.Type != sites.MaintenanceJobType ||
				w.Header().Get("Location") != "/api/v1/jobs/"+acc.Job.ID {
				t.Errorf("maintenance %v = %s", enabled, w.Body)
			}
			e.runJobs(t)
			if s, _ := e.prov.Site(siteID); s.Maintenance != enabled {
				t.Errorf("provisioner maintenance = %v, want %v", s.Maintenance, enabled)
			}
			if got := get(); got.Maintenance != enabled {
				t.Errorf("get: maintenance = %v", got.Maintenance)
			}
		}
		if w := do(t, h, request{authed: true, method: http.MethodPut, target: target + "/maintenance", body: `{}`}); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("maintenance without enabled: %d %s", w.Code, w.Body)
		}

		// Validation.
		for _, tt := range []struct{ name, body, field string }{
			{"unknown PHP", `{"php_version":"5.6"}`, "/php_version"},
			{"reservation over limit", `{"resources":{"memory_reservation_mb":1024}}`, "/resources/memory_reservation_mb"},
		} {
			w := do(t, h, request{authed: true, method: http.MethodPatch, target: target, body: tt.body})
			if w.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s: %d %s", tt.name, w.Code, w.Body)
				continue
			}
			if p := decodeProblem(t, w); len(p.Errors) != 1 || p.Errors[0].Field != tt.field {
				t.Errorf("%s: errors = %+v", tt.name, p.Errors)
			}
		}
		if w := do(t, h, request{authed: true, method: http.MethodPatch, target: target, body: `{}`}); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("empty patch: %d %s", w.Code, w.Body)
		}

		// Stop and start.
		for _, step := range []struct {
			action  string
			jobType string
			status  gen.SiteStatus
			phase   provisioner.SitePhase
		}{
			{"stop", sites.StopJobType, gen.SiteStatusStopped, provisioner.SiteStopped},
			{"start", sites.StartJobType, gen.SiteStatusRunning, provisioner.SiteRunning},
		} {
			w := do(t, h, request{authed: true, method: http.MethodPost, target: target + "/" + step.action})
			if w.Code != http.StatusAccepted {
				t.Fatalf("%s: %d %s", step.action, w.Code, w.Body)
			}
			if j := decodeJSON[gen.JobAccepted](t, w.Body.Bytes()).Job; j.Type != step.jobType || w.Header().Get("Location") != "/api/v1/jobs/"+j.ID {
				t.Errorf("%s = %s", step.action, w.Body)
			}
			e.runJobs(t)
			if got := get(); got.Status != step.status {
				t.Errorf("after %s: status %s", step.action, got.Status)
			}
			if s, _ := e.prov.Site(siteID); s.Phase != step.phase {
				t.Errorf("after %s: phase %s", step.action, s.Phase)
			}
		}

		// Cache purge: 202 with the job, which purges the page cache
		// through the provisioner (and the object cache through the
		// object cache manager).
		w = do(t, h, request{authed: true, method: http.MethodPost, target: target + "/cache/purge"})
		if w.Code != http.StatusAccepted {
			t.Fatalf("purge: %d %s", w.Code, w.Body)
		}
		if j := decodeJSON[gen.JobAccepted](t, w.Body.Bytes()).Job; j.Type != sites.PurgeCacheJobType || j.Target.ID != siteID ||
			w.Header().Get("Location") != "/api/v1/jobs/"+j.ID {
			t.Errorf("purge = %s", w.Body)
		}
		e.runJobs(t)
		if s, _ := e.prov.Site(siteID); s.CachePurges != 1 {
			t.Errorf("page cache purges = %d", s.CachePurges)
		}
		if got := get(); got.Status != gen.SiteStatusRunning {
			t.Errorf("after the purge: status %s", got.Status)
		}

		// Missing sites, and sites being deleted.
		missing := "/api/v1/sites/" + id.New()
		for _, req := range []request{
			{method: http.MethodPatch, target: missing, body: `{"php_version":"8.4"}`},
			{method: http.MethodPost, target: missing + "/start"},
			{method: http.MethodPost, target: missing + "/stop"},
			{method: http.MethodPost, target: missing + "/cache/purge"},
		} {
			req.authed = true
			if w := do(t, h, req); w.Code != http.StatusNotFound {
				t.Errorf("%s %s: %d %s", req.method, req.target, w.Code, w.Body)
			}
		}
		if w := do(t, h, request{authed: true, method: http.MethodDelete, target: target}); w.Code != http.StatusAccepted {
			t.Fatalf("delete: %d %s", w.Code, w.Body)
		}
		for _, req := range []request{
			{method: http.MethodPatch, target: target, body: `{"php_version":"8.4"}`},
			{method: http.MethodPost, target: target + "/start"},
			{method: http.MethodPost, target: target + "/cache/purge"},
		} {
			req.authed = true
			if w := do(t, h, req); w.Code != http.StatusConflict {
				t.Errorf("%s %s while deleting: %d %s", req.method, req.target, w.Code, w.Body)
			}
		}
		e.runJobs(t)
	})
}
