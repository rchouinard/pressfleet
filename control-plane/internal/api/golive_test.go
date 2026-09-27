package api_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/domains"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// fakeDNS fails the DNS check for the domains in wrong.
type fakeDNS struct {
	mu    sync.Mutex
	wrong map[string]string
	err   error
}

func (f *fakeDNS) Check(_ context.Context, names []string) ([]domains.Problem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []domains.Problem
	for _, n := range names {
		if p, ok := f.wrong[n]; ok {
			out = append(out, domains.Problem{Domain: n, Detail: p})
		}
	}
	return out, nil
}

// TestGoLiveEndpoint covers POST /api/v1/sites/{id}/go-live (#45): the DNS
// check's problem type, a retry after DNS propagated, validation, the
// state checks, the role, and the audit log.
func TestGoLiveEndpoint(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newSiteEnv(t, db, testPreviewBase)
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc})
		now := time.Now()
		if _, err := db.ExecContext(ctx, "INSERT INTO users (id, email, password_hash, role, created_at, updated_at) VALUES (?, ?, 'x', 'admin', ?, ?)",
			"01j9zq3v6t8k4m2n5p7r9s1x3y", "admin@example.com", now, now); err != nil {
			t.Fatal(err)
		}
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		site, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: group.ID, Name: "Acme", AdminEmail: "o@example.com", PHPVersion: "8.4"})
		if err != nil {
			t.Fatal(err)
		}
		target := "/api/v1/sites/" + site.ID + "/go-live"
		body := `{"primary_domain":"www.acme.example","aliases":["acme.example"]}`

		// Not running yet.
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: target, body: body}); w.Code != http.StatusConflict {
			t.Errorf("while provisioning: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)

		// DNS not there yet: 422 with the DNS problem type, nothing changed.
		e.dns.wrong["acme.example"] = "it does not resolve (no such domain, or no A or AAAA record). Point its A (and AAAA) records at 203.0.113.7."
		w := do(t, h, request{authed: true, method: http.MethodPost, target: target, body: body})
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("DNS failure: %d %s", w.Code, w.Body)
		}
		p := decodeProblem(t, w)
		if p.Type != api.DNSCheckProblemType || p.Title != "DNS check failed" || len(p.Errors) != 1 ||
			p.Errors[0].Field != "/aliases" || !strings.HasPrefix(p.Errors[0].Detail, "acme.example: it does not resolve") {
			t.Errorf("problem = %+v", p)
		}
		if got, _ := store.NewSites(db).Get(ctx, site.ID); got.PrimaryDomain != "" || got.Status != store.SiteRunning {
			t.Errorf("a failed DNS check changed the site: %+v", got)
		}
		e.dns.err = domains.ErrHostUnknown
		w = do(t, h, request{authed: true, method: http.MethodPost, target: target, body: body})
		if p := decodeProblem(t, w); w.Code != http.StatusUnprocessableEntity || p.Type != api.DNSCheckProblemType || !strings.Contains(p.Detail, "public_ip") {
			t.Errorf("unknown host: %d %s", w.Code, w.Body)
		}
		e.dns.err = nil

		// Validation.
		for _, tc := range []struct{ body, field string }{
			{`{"primary_domain":"https://www.acme.example/"}`, "/primary_domain"},
			{`{"primary_domain":"www.acme.example","aliases":["www.acme.example"]}`, "/aliases/0"},
			{`{"primary_domain":"x.` + testPreviewBase + `"}`, "/primary_domain"},
		} {
			w := do(t, h, request{authed: true, method: http.MethodPost, target: target, body: tc.body})
			p := decodeProblem(t, w)
			if w.Code != http.StatusUnprocessableEntity || p.Type != "about:blank" || len(p.Errors) != 1 || p.Errors[0].Field != tc.field {
				t.Errorf("%s: %d %s", tc.body, w.Code, w.Body)
			}
		}
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: target, body: `{}`}); w.Code != http.StatusUnprocessableEntity && w.Code != http.StatusBadRequest {
			t.Errorf("no primary domain: %d %s", w.Code, w.Body)
		}

		// DNS propagated: the retry is accepted and the job succeeds.
		delete(e.dns.wrong, "acme.example")
		w = do(t, h, request{authed: true, method: http.MethodPost, target: target, body: body})
		if w.Code != http.StatusAccepted {
			t.Fatalf("go-live: %d %s", w.Code, w.Body)
		}
		acc := decodeJSON[gen.SiteAccepted](t, w.Body.Bytes())
		if acc.Site.PrimaryDomain == nil || *acc.Site.PrimaryDomain != "www.acme.example" || !slices.Equal(acc.Site.Aliases, []string{"acme.example"}) ||
			acc.Site.Status != gen.SiteStatusUpdating || acc.Job.Type != sites.GoLiveJobType || w.Header().Get("Location") != "/api/v1/jobs/"+acc.Job.ID ||
			acc.Site.URL == nil || *acc.Site.URL != "https://www.acme.example" {
			t.Errorf("accepted = %s", w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodPost, target: target, body: body}); w.Code != http.StatusConflict {
			t.Errorf("while the job waits: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)
		w = do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + site.ID})
		if got := decodeJSON[gen.Site](t, w.Body.Bytes()); got.Status != gen.SiteStatusRunning || *got.PrimaryDomain != "www.acme.example" {
			t.Errorf("after go-live: %s", w.Body)
		}

		events, err := store.NewAuditEvents(db).List(ctx, store.AuditFilter{TargetType: "site", TargetID: site.ID})
		if err != nil {
			t.Fatal(err)
		}
		var results []string
		for _, ev := range events.Events {
			if ev.Action == "site.domains.manage" {
				results = append(results, string(ev.Result))
			}
		}
		if !slices.Contains(results, "success") || !slices.Contains(results, "failure") {
			t.Errorf("audit results = %v", results)
		}

		// Viewers can't.
		viewer := &auth.Principal{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "v"}
		hv := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{principal: viewer}, Sites: e.svc})
		if w := do(t, hv, request{authed: true, method: http.MethodPost, target: target, body: body}); w.Code != http.StatusForbidden {
			t.Errorf("viewer: %d %s", w.Code, w.Body)
		}
	})
}
