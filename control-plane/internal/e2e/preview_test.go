package e2e_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// ingressWait bounds how long a change of a site's routes may take to show
// through Traefik. Traefik picks up container changes from Docker events
// within a second or two; this leaves room for a slow CI runner.
const ingressWait = 30 * time.Second

// ingressGet sends one GET through Traefik with the ingress client and
// returns the response with its body read.
func ingressGet(t *testing.T, hc *http.Client, url string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp, string(body)
}

// waitIngress repeats a GET through Traefik until it answers with one of
// the wanted statuses, and fails the test if that takes longer than
// ingressWait. Routes appear, change, and disappear a moment after the
// containers or files behind them change.
func waitIngress(t *testing.T, hc *http.Client, url string, header http.Header, want ...int) (*http.Response, string) {
	t.Helper()
	deadline := time.Now().Add(ingressWait)
	for {
		resp, body := ingressGet(t, hc, url, header)
		if slices.Contains(want, resp.StatusCode) {
			t.Logf("GET %s: %s", url, resp.Status)
			return resp, body
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s: status %d after %s, want %v:\n%.500s", url, resp.StatusCode, ingressWait, want, body)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestSitePreview checks what the platform does on a site's preview domain
// beyond serving it (#36 is covered by TestSiteCreateDelete): optional
// password protection (#107). On one site, it:
//
//  1. turns protection on and checks that the preview domain answers 401
//     without credentials or with wrong ones, and 200 with the generated
//     ones; that ACME HTTP-01 challenges and WP-CLI (which WP-Cron and
//     cache purges use) don't go through it; and that the plaintext
//     password is in no label, log, or API response but the one that
//     generated it;
//  2. regenerates the password: only the new one works;
//  3. turns protection off: the preview domain is open again;
//  4. turns maintenance mode (#44) on and off: 503 with Retry-After and the
//     maintenance page, also while the site is stopped, with WP-CLI still
//     working, then the site again.
func TestSitePreview(t *testing.T) {
	projectDir := t.TempDir()
	var siteProject compose.Project
	// Registered before Start, so it runs after the platform teardown: see
	// TestSiteCreateDelete.
	t.Cleanup(func() {
		if siteProject.Name == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if res, err := siteProject.Resources(ctx); err == nil && !res.Empty() {
			t.Logf("removing leftover site project %s", siteProject.Name)
			if err := siteProject.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
				t.Errorf("removing leftover site project %s: %v", siteProject.Name, err)
			}
		}
	})
	p := e2e.Start(t)
	ctx := t.Context()

	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	php, ok := cat.DefaultPHPVersion(time.Now())
	if !ok {
		t.Fatal("no default PHP version")
	}
	images, err := cat.Images(php, cat.LatestWordPress().Version)
	if err != nil {
		t.Fatal(err)
	}
	e2e.EnsureImages(t, images.PHP, images.Nginx)

	created := createSite(t, p, createGroup(t, p, "E2E Preview Agency"), "E2E Preview", php)
	site := created.Site
	siteProject = compose.Project{Name: compose.SiteProjectName(site.ID), Dir: projectDir}
	waitJob(t, p, created.Job.ID)

	hc := p.IngressClient(t)
	previewURL := "https://" + site.PreviewDomain + "/"
	waitIngress(t, hc, previewURL, nil, http.StatusOK)
	basic := func(user, password string) http.Header {
		return http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+password))}}
	}
	update := func(t *testing.T, body client.UpdateSiteJSONRequestBody) client.SiteAccepted {
		t.Helper()
		resp, err := p.Client.UpdateSiteWithResponse(ctx, site.ID, body)
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil {
			t.Fatalf("update site: status %d: %s", resp.StatusCode(), resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
		return *resp.JSON202
	}

	var creds client.PreviewCredentials
	t.Run("protection on", func(t *testing.T) {
		on := true
		acc := update(t, client.UpdateSiteJSONRequestBody{PreviewProtection: &client.PreviewProtectionUpdate{Enabled: &on}})
		if acc.PreviewCredentials == nil || acc.PreviewCredentials.Username != site.Slug || len(acc.PreviewCredentials.Password) < 20 {
			t.Fatalf("enabling returned %+v", acc.PreviewCredentials)
		}
		creds = *acc.PreviewCredentials

		resp, _ := waitIngress(t, hc, previewURL, nil, http.StatusUnauthorized)
		if !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") || resp.Header.Get("X-Robots-Tag") != "noindex" {
			t.Errorf("401 headers: %v", resp.Header)
		}
		if resp, _ := ingressGet(t, hc, previewURL, basic(creds.Username, "wrong")); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("wrong password: status %d", resp.StatusCode)
		}
		resp, home := ingressGet(t, hc, previewURL, basic(creds.Username, creds.Password))
		if resp.StatusCode != http.StatusOK || !strings.Contains(home, "<title>E2E Preview") {
			t.Errorf("with the credentials: status %d:\n%.300s", resp.StatusCode, home)
		}
		// The Authorization header doesn't reach WordPress, which would
		// try it as an application password and refuse the REST API.
		if resp, body := ingressGet(t, hc, previewURL+"wp-json/", basic(creds.Username, creds.Password)); resp.StatusCode != http.StatusOK {
			t.Errorf("REST API with the credentials: status %d:\n%.300s", resp.StatusCode, body)
		}
	})

	t.Run("platform operations don't go through protection", func(t *testing.T) {
		// Traefik answers ACME HTTP-01 challenges on port 80 before any
		// router: an unknown token is its 404, not a 401 or a redirect.
		resp, _ := ingressGet(t, hc, "http://"+site.PreviewDomain+"/.well-known/acme-challenge/pf-e2e-token", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("ACME challenge path: status %d, want Traefik's 404", resp.StatusCode)
		}
		// WP-Cron runs through WP-CLI (package sitecron), as do the other
		// tasks the control plane runs in a site; cache purges use the
		// filesystem.
		if out, err := siteProject.ExecService(ctx, "php", "wp", "cron", "event", "run", "--due-now"); err != nil {
			t.Errorf("wp cron event run: %v: %s", err, out)
		}
	})

	t.Run("the password is only in the response that generated it", func(t *testing.T) {
		nginx, err := siteProject.ServiceContainer(ctx, "nginx")
		if err != nil {
			t.Fatal(err)
		}
		users := nginx.Labels["traefik.http.middlewares."+compose.SiteProjectName(site.ID)+"-preview-auth.basicauth.users"]
		hash := strings.TrimPrefix(users, creds.Username+":")
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(creds.Password)) != nil {
			t.Errorf("the basic auth label %q doesn't hold the password's hash", users)
		}
		var places []string
		for label, v := range nginx.Labels {
			if strings.Contains(v, creds.Password) {
				places = append(places, "label "+label)
			}
		}
		for name, project := range map[string]compose.Project{"platform": p.Project, "site": siteProject} {
			logs, err := project.Logs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(logs), creds.Password) {
				places = append(places, name+" logs")
			}
		}
		got, err := p.Client.GetSiteWithResponse(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.JSON200 == nil || !got.JSON200.PreviewProtection.Enabled {
			t.Errorf("get site: status %d: %s", got.StatusCode(), got.Body)
		}
		audit, err := p.Client.ListAuditEventsWithResponse(ctx, &client.ListAuditEventsParams{})
		if err != nil {
			t.Fatal(err)
		}
		jobs, err := p.Client.ListJobsWithResponse(ctx, &client.ListJobsParams{})
		if err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string][]byte{"site": got.Body, "audit log": audit.Body, "jobs": jobs.Body} {
			if bytes.Contains(body, []byte(creds.Password)) {
				places = append(places, name+" API response")
			}
		}
		if len(places) > 0 {
			t.Errorf("the plaintext password is in: %v", places)
		}
	})

	t.Run("regenerating replaces the password", func(t *testing.T) {
		resp, err := p.Client.RegenerateSitePreviewPasswordWithResponse(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil {
			t.Fatalf("regenerate: status %d: %s", resp.StatusCode(), resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
		fresh := resp.JSON202.Credentials
		if fresh.Password == creds.Password || fresh.Username != creds.Username {
			t.Errorf("regenerated credentials = %+v", fresh)
		}
		waitIngress(t, hc, previewURL, basic(fresh.Username, fresh.Password), http.StatusOK)
		if resp, _ := ingressGet(t, hc, previewURL, basic(creds.Username, creds.Password)); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("the old password: status %d", resp.StatusCode)
		}
	})

	t.Run("protection off", func(t *testing.T) {
		off := false
		if acc := update(t, client.UpdateSiteJSONRequestBody{PreviewProtection: &client.PreviewProtectionUpdate{Enabled: &off}}); acc.PreviewCredentials != nil {
			t.Errorf("disabling returned credentials")
		}
		waitIngress(t, hc, previewURL, nil, http.StatusOK)
	})

	// Maintenance mode (#44): the domain answers 503 with Retry-After and
	// the maintenance page within seconds, also while the site is stopped,
	// WP-CLI keeps working, and turning it off serves the site again.
	setMaintenance := func(t *testing.T, enabled bool) {
		t.Helper()
		resp, err := p.Client.SetSiteMaintenanceWithResponse(ctx, site.ID, client.SetSiteMaintenanceJSONRequestBody{Enabled: enabled})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil || resp.JSON202.Site.Maintenance != enabled {
			t.Fatalf("set maintenance %v: status %d: %s", enabled, resp.StatusCode(), resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
	}
	t.Run("maintenance mode", func(t *testing.T) {
		start := time.Now()
		setMaintenance(t, true)
		resp, page := waitIngress(t, hc, previewURL+"?x=1", nil, http.StatusServiceUnavailable)
		t.Logf("maintenance page after %s", time.Since(start).Round(100*time.Millisecond))
		if resp.Header.Get("Retry-After") == "" || !strings.Contains(page, "Down for maintenance") {
			t.Errorf("maintenance response: %v\n%.300s", resp.Header, page)
		}
		if out, err := siteProject.ExecService(ctx, "php", "wp", "core", "is-installed"); err != nil {
			t.Errorf("WP-CLI in maintenance mode: %v: %s", err, out)
		}
		// The page doesn't depend on the site's containers.
		stop, err := p.Client.StopSiteWithResponse(ctx, site.ID)
		if err != nil || stop.JSON202 == nil {
			t.Fatalf("stop: %v %s", err, stop.Body)
		}
		waitJob(t, p, stop.JSON202.Job.ID)
		if resp, _ := ingressGet(t, hc, previewURL, nil); resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("stopped site in maintenance mode: status %d", resp.StatusCode)
		}
		startResp, err := p.Client.StartSiteWithResponse(ctx, site.ID)
		if err != nil || startResp.JSON202 == nil {
			t.Fatalf("start: %v %s", err, startResp.Body)
		}
		waitJob(t, p, startResp.JSON202.Job.ID)
		if resp, _ := ingressGet(t, hc, previewURL, nil); resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("restarted site in maintenance mode: status %d", resp.StatusCode)
		}

		start = time.Now()
		setMaintenance(t, false)
		waitIngress(t, hc, previewURL, nil, http.StatusOK)
		t.Logf("site back after %s", time.Since(start).Round(100*time.Millisecond))
	})

	deleted, err := p.Client.DeleteSiteWithResponse(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.JSON202 == nil {
		t.Fatalf("delete site: status %d: %s", deleted.StatusCode(), deleted.Body)
	}
	waitJob(t, p, deleted.JSON202.Job.ID)
}
