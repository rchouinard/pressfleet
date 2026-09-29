package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/deploy"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/objectcachetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

// newDeployments returns a deployment service on the site environment's
// fakes. Its job handler isn't registered: these tests only queue jobs.
func newDeployments(t *testing.T, db *store.DB, e *siteEnv, up *uploads.Service) *deploy.Service {
	t.Helper()
	svc, err := deploy.New(deploy.Options{
		DB: db, Uploads: up, Provisioner: e.prov, Tasks: e.prov, ObjectCaches: objectcachetest.New(), Health: e.svc,
		Layout: config.Layout{Root: t.TempDir()}, Logger: storetest.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestDeployEndpoint(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		up := newUploadService(t, db)
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc, Uploads: up,
			Deployments: newDeployments(t, db, e, up)})
		const admin = "01j9zq3v6t8k4m2n5p7r9s1x3y" // fakeAuth's user
		now := time.Now()
		if _, err := db.ExecContext(ctx, "INSERT INTO users (id, email, password_hash, role, created_at, updated_at) VALUES (?, ?, 'x', 'admin', ?, ?)",
			admin, "admin@example.com", now, now); err != nil {
			t.Fatal(err)
		}
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		newSite := func(name string) store.Site {
			site, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: group.ID, Name: name, AdminEmail: "o@example.com", PHPVersion: "8.4"})
			if err != nil {
				t.Fatal(err)
			}
			return site
		}
		site, other := newSite("Acme"), newSite("Other")
		upload := func(siteID string, complete bool) string {
			t.Helper()
			data := []byte("not really a zip")
			sum := sha256.Sum256(data)
			u, err := up.Create(ctx, uploads.NewUpload{Purpose: store.UploadSiteDeploy, SiteID: siteID, SizeBytes: int64(len(data)),
				SHA256: hex.EncodeToString(sum[:]), CreatedByUserID: admin})
			if err != nil {
				t.Fatal(err)
			}
			if !complete {
				return u.ID
			}
			if _, err := up.Write(ctx, u.ID, 0, bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			if _, err := up.Complete(ctx, u.ID, ""); err != nil {
				t.Fatal(err)
			}
			return u.ID
		}
		target := func(siteID string) string { return "/api/v1/sites/" + siteID + "/deploy" }
		body := func(uploadID, kind, name string, activate bool) string {
			b := `{"upload_id":"` + uploadID + `","kind":"` + kind + `","name":"` + name + `"`
			if activate {
				b += `,"activate":true`
			}
			return b + "}"
		}
		post := func(siteID, b string) *httpResponse {
			w := do(t, h, request{authed: true, method: http.MethodPost, target: target(siteID), body: b})
			return &httpResponse{code: w.Code, body: w.Body.String(), location: w.Header().Get("Location")}
		}

		ready := upload(site.ID, true)
		// Still provisioning.
		if w := post(site.ID, body(ready, "theme", "acme", false)); w.code != http.StatusConflict {
			t.Errorf("deploy while provisioning: %d %s", w.code, w.body)
		}
		e.runJobs(t)
		if _, err := e.svc.Stop(ctx, other.ID, admin); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)

		w := post(site.ID, body(ready, "theme", "acme", true))
		if w.code != http.StatusAccepted {
			t.Fatalf("deploy: %d %s", w.code, w.body)
		}
		job := decodeJSON[gen.JobAccepted](t, []byte(w.body)).Job
		if job.Type != deploy.JobType || job.Target.ID != site.ID || w.location != "/api/v1/jobs/"+job.ID {
			t.Errorf("deploy response: %s (Location %s)", w.body, w.location)
		}
		if u, err := up.Get(ctx, ready); err != nil || u.Status != store.UploadInUse || u.JobID != job.ID {
			t.Errorf("upload after deploy = %+v, %v", u, err)
		}

		// The same upload again: in use.
		if w := post(site.ID, body(ready, "theme", "acme", false)); w.code != http.StatusConflict {
			t.Errorf("deploy with an upload in use: %d %s", w.code, w.body)
		}
		for name, tc := range map[string]struct {
			siteID, body string
			want         int
		}{
			"incomplete upload":          {site.ID, body(upload(site.ID, false), "plugin", "acme", false), http.StatusConflict},
			"another site's upload":      {site.ID, body(upload(other.ID, true), "plugin", "acme", false), http.StatusUnprocessableEntity},
			"unknown upload":             {site.ID, body("01j9zq3v6t8k4m2n5p7r9s1x3z", "plugin", "acme", false), http.StatusUnprocessableEntity},
			"activating a mu-plugin":     {site.ID, body(upload(site.ID, true), "mu-plugin", "acme", true), http.StatusUnprocessableEntity},
			"hidden name":                {site.ID, body(upload(site.ID, true), "theme", ".acme", false), http.StatusUnprocessableEntity},
			"unknown kind":               {site.ID, body(upload(site.ID, true), "widget", "acme", false), http.StatusUnprocessableEntity},
			"missing site":               {"01j9zq3v6t8k4m2n5p7r9s1x40", body(upload(site.ID, true), "theme", "acme", false), http.StatusNotFound},
			"site that isn't running":    {other.ID, body(upload(other.ID, true), "theme", "acme", false), http.StatusConflict},
			"path traversal in the name": {site.ID, body(upload(site.ID, true), "theme", "../acme", false), http.StatusUnprocessableEntity},
		} {
			if w := post(tc.siteID, tc.body); w.code != tc.want {
				t.Errorf("%s: %d %s, want %d", name, w.code, w.body, tc.want)
			}
		}
		// Nothing but the one deployment was queued.
		list, err := store.NewJobs(db).List(ctx, store.JobFilter{Types: []string{deploy.JobType}})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Jobs) != 1 {
			t.Errorf("deploy jobs = %d, want 1", len(list.Jobs))
		}
	})
}

type httpResponse struct {
	code           int
	body, location string
}
