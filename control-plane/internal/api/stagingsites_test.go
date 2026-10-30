package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// stagingEnv has a production site and a staging site linked to it. The
// tests link two provisioned sites directly, to set every staging field.
type stagingEnv struct {
	*siteEnv
	db         *store.DB
	admin      http.Handler
	operator   http.Handler
	prod, stg  store.Site
	refreshed  time.Time
	pushedAt   time.Time
	snapshotID string
}

func newStagingEnv(t *testing.T, db *store.DB) *stagingEnv {
	t.Helper()
	ctx := context.Background()
	const userID = "01j9zq3v6t8k4m2n5p7r9s1x3y"
	e := &stagingEnv{siteEnv: newSiteEnv(t, db, testPreviewBase), db: db, snapshotID: "9f8e7d6c"}
	e.refreshed = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	e.pushedAt = e.refreshed.Add(time.Hour)
	now := time.Now()
	if _, err := db.ExecContext(ctx, "INSERT INTO users (id, email, password_hash, role, created_at, updated_at) VALUES (?, ?, 'x', 'admin', ?, ?)",
		userID, "admin@example.com", now, now); err != nil {
		t.Fatal(err)
	}
	group := &store.ClientGroup{Name: "Acme"}
	if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	provision := func(name string) store.Site {
		t.Helper()
		site, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: group.ID, Name: name, AdminEmail: "o@example.com", PHPVersion: "8.4"})
		if err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		return site
	}
	e.prod, e.stg = provision("Acme"), provision("Acme staging")
	if _, err := db.ExecContext(ctx,
		"UPDATE sites SET parent_site_id = ?, mail_disabled = ?, staging_refreshed_at = ?, staging_snapshot_id = ?, staging_pushed_at = ? WHERE id = ?",
		e.prod.ID, true, e.refreshed, e.snapshotID, e.pushedAt, e.stg.ID); err != nil {
		t.Fatal(err)
	}
	as := func(role store.Role) http.Handler {
		return newHandler(t, api.Options{DB: db, Sites: e.svc, Backups: e.backups, Authenticator: &fakeAuth{principal: &auth.Principal{
			UserID: userID, Role: role, Method: auth.MethodSession, SessionID: "test",
		}}})
	}
	e.admin, e.operator = as(store.RoleAdmin), as(store.RoleOperator)
	return e
}

func (e *stagingEnv) request(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, request{authed: true, method: method, target: target, body: body})
}

func TestStagingSiteReadsBack(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newStagingEnv(t, db)

		w := e.request(t, e.admin, http.MethodGet, "/api/v1/sites/"+e.stg.ID, "")
		expectStatus(t, w, http.StatusOK, "get staging site")
		got := decodeJSON[gen.Site](t, w.Body.Bytes())
		if got.Environment != gen.Staging || got.ParentSiteID == nil || *got.ParentSiteID != e.prod.ID || got.Staging == nil {
			t.Fatalf("staging site = %s", w.Body)
		}
		st := got.Staging
		if st.ProductionSiteID != e.prod.ID || st.OutboundMail || st.LastSnapshotID == nil || *st.LastSnapshotID != e.snapshotID ||
			st.LastRefreshedAt == nil || !st.LastRefreshedAt.Equal(e.refreshed) || st.LastPushedAt == nil || !st.LastPushedAt.Equal(e.pushedAt) {
			t.Errorf("staging = %+v", st)
		}

		w = e.request(t, e.admin, http.MethodGet, "/api/v1/sites/"+e.prod.ID, "")
		prod := decodeJSON[gen.Site](t, w.Body.Bytes())
		if prod.Environment != gen.Production || prod.ParentSiteID != nil || prod.Staging != nil ||
			!strings.Contains(w.Body.String(), `"parent_site_id":null`) || !strings.Contains(w.Body.String(), `"staging":null`) {
			t.Errorf("production site = %s", w.Body)
		}

		// A staging site that was never refreshed has no times.
		if _, err := db.ExecContext(context.Background(),
			"UPDATE sites SET staging_refreshed_at = NULL, staging_snapshot_id = NULL, staging_pushed_at = NULL, mail_disabled = ? WHERE id = ?", false, e.stg.ID); err != nil {
			t.Fatal(err)
		}
		w = e.request(t, e.admin, http.MethodGet, "/api/v1/sites/"+e.stg.ID, "")
		if st := decodeJSON[gen.Site](t, w.Body.Bytes()).Staging; st == nil || !st.OutboundMail || st.LastRefreshedAt != nil ||
			st.LastSnapshotID != nil || st.LastPushedAt != nil || !strings.Contains(w.Body.String(), `"last_pushed_at":null`) {
			t.Errorf("fresh staging site = %s", w.Body)
		}

		for _, tc := range []struct {
			query string
			want  []string
		}{
			{"", []string{e.prod.ID, e.stg.ID}},
			{"?environment=staging", []string{e.stg.ID}},
			{"?environment=production", []string{e.prod.ID}},
			{"?parent_site_id=" + e.prod.ID, []string{e.stg.ID}},
			{"?parent_site_id=" + e.stg.ID, nil},
		} {
			w := e.request(t, e.admin, http.MethodGet, "/api/v1/sites"+tc.query, "")
			expectStatus(t, w, http.StatusOK, "list "+tc.query)
			var ids []string
			for _, s := range decodeJSON[gen.SiteList](t, w.Body.Bytes()).Items {
				ids = append(ids, s.ID)
			}
			slices.Sort(ids)
			slices.Sort(tc.want)
			if !slices.Equal(ids, tc.want) {
				t.Errorf("list %q = %v, want %v", tc.query, ids, tc.want)
			}
		}
		if w := e.request(t, e.admin, http.MethodGet, "/api/v1/sites?environment=qa", ""); w.Code != http.StatusBadRequest {
			t.Errorf("bad environment: %d %s", w.Code, w.Body)
		}
	})
}

func TestStagingSiteOutboundMail(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newStagingEnv(t, db)

		w := e.request(t, e.admin, http.MethodPatch, "/api/v1/sites/"+e.prod.ID, `{"outbound_mail":true}`)
		expectStatus(t, w, http.StatusUnprocessableEntity, "outbound mail on production")
		if !strings.Contains(w.Body.String(), "/outbound_mail") {
			t.Errorf("body = %s", w.Body)
		}

		w = e.request(t, e.operator, http.MethodPatch, "/api/v1/sites/"+e.stg.ID, `{"outbound_mail":true}`)
		expectStatus(t, w, http.StatusAccepted, "outbound mail on staging")
		acc := decodeJSON[gen.SiteAccepted](t, w.Body.Bytes())
		if acc.Site.Staging == nil || !acc.Site.Staging.OutboundMail || acc.Site.Status != gen.SiteStatusUpdating {
			t.Errorf("accepted = %s", w.Body)
		}
		e.runJobs(t)
		if s, _ := e.prov.Site(e.stg.ID); s.Config.DisableMail {
			t.Error("the site was re-rendered with mail still off")
		}
		w = e.request(t, e.admin, http.MethodGet, "/api/v1/sites/"+e.stg.ID, "")
		if got := decodeJSON[gen.Site](t, w.Body.Bytes()); got.Status != gen.SiteStatusRunning || got.Staging == nil || !got.Staging.OutboundMail {
			t.Errorf("site after the update = %s", w.Body)
		}
	})
}

func TestStagingSiteRefusesDomains(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newStagingEnv(t, db)
		w := e.request(t, e.admin, http.MethodPost, "/api/v1/sites/"+e.stg.ID+"/go-live", `{"primary_domain":"www.acme.example","skip_dns_check":true}`)
		expectStatus(t, w, http.StatusUnprocessableEntity, "go-live of a staging site")
		w = e.request(t, e.admin, http.MethodPut, "/api/v1/sites/"+e.stg.ID+"/aliases", `{"aliases":["acme.example"],"skip_dns_check":true}`)
		expectStatus(t, w, http.StatusUnprocessableEntity, "aliases of a staging site")
		if got, _ := store.NewSites(db).Get(context.Background(), e.stg.ID); got.Status != store.SiteRunning || got.PrimaryDomain != "" {
			t.Errorf("staging site = %+v", got)
		}
	})
}

// TestDeleteStagingAuthorization covers the action DELETE picks from its
// target: an Operator deletes staging sites, an Admin production sites and
// backups.
func TestDeleteStagingAuthorization(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newStagingEnv(t, db)
		lastAction := func() (string, store.AuditResult) {
			t.Helper()
			ev := allEvents(t, db)[0]
			return ev.Action, ev.Result
		}

		// An Operator can't delete a production site, with or without its
		// staging sites, nor a staging site's backups.
		for _, target := range []string{e.prod.ID, e.stg.ID + "?delete_backups=true"} {
			w := e.request(t, e.operator, http.MethodDelete, "/api/v1/sites/"+target, "")
			expectStatus(t, w, http.StatusForbidden, "operator deleting "+target)
		}
		if a, r := lastAction(); a != "site.delete_with_backups" || r != store.AuditDenied {
			t.Errorf("last event = %s %s", a, r)
		}
		w := e.request(t, e.operator, http.MethodDelete, "/api/v1/sites/"+e.prod.ID, "")
		expectStatus(t, w, http.StatusForbidden, "operator deleting production")
		if a, r := lastAction(); a != "site.delete" || r != store.AuditDenied {
			t.Errorf("last event = %s %s", a, r)
		}

		// Deleting a production site that has staging sites is refused
		// before anything is queued, with the staging sites named.
		w = e.request(t, e.admin, http.MethodDelete, "/api/v1/sites/"+e.prod.ID, "")
		expectStatus(t, w, http.StatusConflict, "admin deleting production with a staging site")
		if !strings.Contains(w.Body.String(), e.stg.Slug) || !strings.Contains(w.Body.String(), e.stg.ID) {
			t.Errorf("409 doesn't name the staging site: %s", w.Body)
		}
		if got, _ := store.NewSites(db).Get(context.Background(), e.prod.ID); got.Status != store.SiteRunning {
			t.Errorf("production status = %s", got.Status)
		}
		var queued int
		if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM jobs WHERE type = ?", sites.DeleteJobType).Scan(&queued); err != nil || queued != 0 {
			t.Errorf("%d deletion jobs queued, %v", queued, err)
		}

		// An Operator deletes the staging site.
		w = e.request(t, e.operator, http.MethodDelete, "/api/v1/sites/"+e.stg.ID, "")
		expectStatus(t, w, http.StatusAccepted, "operator deleting staging")
		if a, r := lastAction(); a != "site.staging.delete" || r != store.AuditSuccess {
			t.Errorf("last event = %s %s", a, r)
		}
		e.runJobs(t)
		if _, err := store.NewSites(db).Get(context.Background(), e.stg.ID); err == nil {
			t.Error("the staging site is still there")
		}

		// Now the Admin can delete production.
		w = e.request(t, e.admin, http.MethodDelete, "/api/v1/sites/"+e.prod.ID, "")
		expectStatus(t, w, http.StatusAccepted, "admin deleting production")
	})
}

func TestCreateStagingSite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		e := newStagingEnv(t, db)
		target := "/api/v1/sites/" + e.prod.ID + "/staging-sites"

		// An Operator creates one from a fresh backup (acme-staging is the
		// linked site's slug already): the clone job waits
		// for the sync backup of production.
		w := e.request(t, e.operator, http.MethodPost, target, "")
		expectStatus(t, w, http.StatusAccepted, "create staging site")
		acc := decodeJSON[gen.SiteAccepted](t, w.Body.Bytes())
		if acc.Site.ParentSiteID == nil || *acc.Site.ParentSiteID != e.prod.ID || acc.Site.Status != gen.SiteStatusProvisioning ||
			acc.Site.Environment != gen.Staging || acc.Site.Slug != "acme-staging-2" || acc.Site.Name != "Acme (staging)" ||
			!acc.Site.PreviewProtection.Enabled {
			t.Errorf("site = %s", w.Body)
		}
		pc := acc.PreviewCredentials
		if pc == nil || pc.Username != "acme-staging-2" || pc.Password == "" {
			t.Fatalf("preview credentials = %+v", pc)
		}
		if acc.Job.Type != sites.CloneJobType || acc.Job.AfterJobID == nil || w.Header().Get("Location") != "/api/v1/jobs/"+acc.Job.ID {
			t.Fatalf("job = %+v, Location %q", acc.Job, w.Header().Get("Location"))
		}
		backupJob, err := store.NewJobs(db).Get(ctx, *acc.Job.AfterJobID)
		if err != nil || backupJob.TargetID != e.prod.ID || backupJob.LockKey != store.SiteLockKey(e.prod.ID) ||
			!strings.Contains(string(backupJob.Payload), `"sync"`) {
			t.Errorf("awaited job = %+v, %v", backupJob, err)
		}
		ev := allEvents(t, db)[0]
		if ev.Action != "site.staging.create" || ev.Result != store.AuditSuccess || ev.TargetID != acc.Site.ID ||
			ev.Details["parent_site_id"] != e.prod.ID || ev.Details["backup_job_id"] != backupJob.ID {
			t.Errorf("audit event = %+v", ev)
		}
		for k, v := range ev.Details {
			if strings.Contains(v, pc.Password) {
				t.Errorf("audit detail %s holds the preview password", k)
			}
		}

		// From an existing backup, with a name and resources: no backup job.
		snap, err := e.engine.Backup(ctx, e.prod.ID, backup.Options{Kind: string(store.BackupManual)})
		if err != nil {
			t.Fatal(err)
		}
		w = e.request(t, e.admin, http.MethodPost, target,
			`{"name":"QA","snapshot_id":"`+string(snap.ID)+`","resources":{"memory_limit_mb":1024}}`)
		expectStatus(t, w, http.StatusAccepted, "create staging site from a snapshot")
		acc = decodeJSON[gen.SiteAccepted](t, w.Body.Bytes())
		if acc.Site.Name != "QA" || acc.Site.Slug != "acme-staging-3" || acc.Site.Resources.MemoryLimitMb != 1024 || acc.Job.AfterJobID != nil {
			t.Errorf("accepted = %s", w.Body)
		}

		for _, tc := range []struct {
			name, target, body string
			want               int
		}{
			{"from a staging site", "/api/v1/sites/" + e.stg.ID + "/staging-sites", "", http.StatusUnprocessableEntity},
			{"unknown snapshot", target, `{"snapshot_id":"deadbeef00"}`, http.StatusUnprocessableEntity},
			{"bad resources", target, `{"resources":{"memory_limit_mb":64,"memory_reservation_mb":128}}`, http.StatusUnprocessableEntity},
			{"missing site", "/api/v1/sites/" + strings.Repeat("0", 26) + "/staging-sites", "", http.StatusNotFound},
		} {
			w := e.request(t, e.admin, http.MethodPost, tc.target, tc.body)
			expectStatus(t, w, tc.want, tc.name)
		}

		// A production site being deleted is refused.
		if _, err := db.ExecContext(ctx, "UPDATE sites SET status = ? WHERE id = ?", store.SiteDeleting, e.prod.ID); err != nil {
			t.Fatal(err)
		}
		w = e.request(t, e.admin, http.MethodPost, target, "")
		expectStatus(t, w, http.StatusConflict, "production being deleted")
		if n, err := store.NewSites(db).Children(ctx, e.prod.ID); err != nil || len(n) != 3 {
			t.Errorf("staging sites = %d, %v; want 3", len(n), err)
		}
	})
}
