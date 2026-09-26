package api_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/sitedbtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const testPreviewBase = "preview.example.test"

// implementedSiteOps are the site operations that no longer answer 501.
var implementedSiteOps = map[string]bool{
	"createSite":                   true,
	"deleteSite":                   true,
	"getSite":                      true,
	"retrieveSiteAdminCredentials": true,
}

// siteEnv is a site service on fakes, with a job pool to run its jobs.
type siteEnv struct {
	svc  *sites.Service
	prov *provisionertest.Fake
	dbs  *sitedbtest.Fake
	pool *jobs.Pool
}

func newSiteEnv(t *testing.T, db *store.DB, previewBase string) *siteEnv {
	t.Helper()
	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	sec := secrets.NewStore(db, secretstest.Keyring(t, 1))
	e := &siteEnv{prov: provisionertest.New(), dbs: sitedbtest.New()}
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
	e.svc, err = sites.New(sites.Options{
		DB: db, Secrets: sec, Catalog: cat, PreviewBaseDomain: previewBase,
		Provisioner: e.prov, Databases: e.dbs, Logger: storetest.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.pool = jobs.NewPool(db, jobs.Options{Logger: storetest.Logger(), Backoff: func(int) time.Duration { return 0 }, Secrets: sec})
	for _, h := range e.svc.Handlers() {
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
