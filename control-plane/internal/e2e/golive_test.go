package e2e_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// Domains the go-live test uses. Nothing resolves them: the test reaches
// them through Traefik with the ingress client.
const (
	goLivePrimary = "www.golive.e2e.test"
	goLiveAlias   = "golive.e2e.test"
)

// TestSiteGoLive checks the go-live workflow (#45, design doc §5.2)
// through Traefik, on one site:
//
//  1. The DNS check refuses a domain that doesn't resolve to the host
//     (e2e.PublicIP), with the DNS problem type, and changes nothing.
//  2. Going live with the check skipped (the test domains resolve
//     nowhere) routes the primary domain and serves the site there, with
//     HSTS and without noindex; rewrites WordPress's URLs, serialized
//     options included and GUIDs excepted; redirects the preview domain
//     and the alias to the primary domain, keeping path and query; and
//     recreates no container.
//  3. With preview protection on, the preview domain asks for the password
//     before it redirects.
//  4. Maintenance mode covers the primary domain and the alias.
//
// Certificates can't be checked here: the platform's ACME directory never
// resolves (e2e.ACMECAServer), so Traefik serves its default certificate.
// The M2 acceptance test with Pebble (#48) checks them.
func TestSiteGoLive(t *testing.T) {
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

	created := createSite(t, p, createGroup(t, p, "E2E Go-live Agency"), "E2E Go-live", php)
	site := created.Site
	siteProject = compose.Project{Name: compose.SiteProjectName(site.ID), Dir: projectDir}
	waitJob(t, p, created.Job.ID)

	hc := p.IngressClient(t)
	preview := site.PreviewDomain
	waitIngress(t, hc, "https://"+preview+"/", nil, http.StatusOK)
	wp := func(t *testing.T, args ...string) string {
		t.Helper()
		out, err := siteProject.ExecService(ctx, "php", append([]string{"wp"}, args...)...)
		if err != nil {
			t.Fatalf("wp %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	// Content that names the preview domain: a post, whose GUID must keep
	// it, and a serialized option.
	post := wp(t, "post", "create", "--post_type=post", "--post_status=publish", "--post_title=Go-live link",
		`--post_content=<a href="https://`+preview+`/about/">About</a>`, "--porcelain")
	guid := wp(t, "post", "get", post, "--field=guid")
	wp(t, "option", "update", "pf_e2e_links", `{"home":"https://`+preview+`/","n":1}`, "--format=json")
	nginx, err := siteProject.ServiceContainer(ctx, "nginx")
	if err != nil {
		t.Fatal(err)
	}

	goLive := func(t *testing.T, skipDNS bool) *client.GoLiveSiteResponse {
		t.Helper()
		resp, err := p.Client.GoLiveSiteWithResponse(ctx, site.ID, client.GoLiveSiteJSONRequestBody{
			PrimaryDomain: goLivePrimary,
			Aliases:       &[]string{goLiveAlias},
			SkipDNSCheck:  &skipDNS,
		})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	t.Run("the DNS check refuses domains that don't point here", func(t *testing.T) {
		resp := goLive(t, false)
		prob := resp.ApplicationProblemJSON422
		if prob == nil || prob.Type != api.DNSCheckProblemType || prob.Errors == nil || len(*prob.Errors) != 2 {
			t.Fatalf("status %d: %s", resp.StatusCode(), resp.Body)
		}
		for _, e := range *prob.Errors {
			// The names don't exist, but the lookup goes to the runner's resolver,
			// which may not answer in time. Either way the check refuses.
			resolved := strings.Contains(e.Detail, "does not resolve") || strings.Contains(e.Detail, "timed out")
			if !resolved || !strings.Contains(e.Detail, e2e.PublicIP) {
				t.Errorf("%s: %s", e.Field, e.Detail)
			}
		}
		if got := getSite(t, p, site.ID); got.PrimaryDomain != nil || len(got.Aliases) != 0 || got.Status != client.SiteStatusRunning {
			t.Errorf("a failed DNS check changed the site: %+v", got)
		}
	})

	t.Run("go live", func(t *testing.T) {
		start := time.Now()
		resp := goLive(t, true)
		if resp.JSON202 == nil {
			t.Fatalf("go-live: status %d: %s", resp.StatusCode(), resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
		got := getSite(t, p, site.ID)
		if got.PrimaryDomain == nil || *got.PrimaryDomain != goLivePrimary || len(got.Aliases) != 1 || got.Aliases[0] != goLiveAlias ||
			got.Status != client.SiteStatusRunning || got.URL == nil || *got.URL != "https://"+goLivePrimary {
			t.Errorf("site after go-live: %+v", got)
		}

		res, home := waitIngress(t, hc, "https://"+goLivePrimary+"/", nil, http.StatusOK)
		t.Logf("primary domain served %s after the request", time.Since(start).Round(100*time.Millisecond))
		if !strings.Contains(home, "https://"+goLivePrimary+"/") || strings.Contains(home, preview) {
			t.Errorf("the home page doesn't use the primary domain's URLs:\n%.2000s", home)
		}
		if got := res.Header.Get("Strict-Transport-Security"); got != "max-age=31536000" {
			t.Errorf("Strict-Transport-Security = %q", got)
		}
		if got := res.Header.Get("X-Robots-Tag"); got != "" {
			t.Errorf("the primary domain sends X-Robots-Tag: %s", got)
		}
		if res.ProtoMajor != 2 {
			t.Errorf("protocol %s, want HTTP/2", res.Proto)
		}
		if res, body := ingressGet(t, hc, "https://"+goLivePrimary+"/wp-login.php", nil); res.StatusCode != http.StatusOK || !strings.Contains(body, "user_login") {
			t.Errorf("wp-login.php on the primary domain: status %d", res.StatusCode)
		}
		if res, _ := ingressGet(t, hc, "http://"+goLivePrimary+"/x", nil); res.StatusCode/100 != 3 ||
			res.Header.Get("Location") != "https://"+goLivePrimary+"/x" {
			t.Errorf("plain HTTP: status %d, Location %q", res.StatusCode, res.Header.Get("Location"))
		}

		// WordPress's URLs, content and serialized options included, moved;
		// the GUID didn't.
		for _, opt := range []string{"home", "siteurl"} {
			if got := wp(t, "option", "get", opt); got != "https://"+goLivePrimary {
				t.Errorf("%s = %q", opt, got)
			}
		}
		if got := wp(t, "post", "get", post, "--field=post_content"); !strings.Contains(got, "https://"+goLivePrimary+"/about/") {
			t.Errorf("post content = %q", got)
		}
		if got := wp(t, "option", "get", "pf_e2e_links", "--format=json"); !strings.Contains(got, `"https:\/\/`+goLivePrimary+`\/"`) {
			t.Errorf("serialized option = %s", got)
		}
		if got := wp(t, "post", "get", post, "--field=guid"); got != guid {
			t.Errorf("GUID = %q, want %q", got, guid)
		}

		// The preview domain and the alias redirect, keeping path and query.
		for _, tc := range []struct {
			host    string
			noindex bool
			hsts    bool
		}{
			{preview, true, false},
			{goLiveAlias, false, true},
		} {
			res, _ := ingressGet(t, hc, "https://"+tc.host+"/shop/item?id=7&x=y", nil)
			if res.StatusCode != http.StatusMovedPermanently || res.Header.Get("Location") != "https://"+goLivePrimary+"/shop/item?id=7&x=y" {
				t.Errorf("%s: status %d, Location %q", tc.host, res.StatusCode, res.Header.Get("Location"))
			}
			if got := res.Header.Get("X-Robots-Tag") == "noindex"; got != tc.noindex {
				t.Errorf("%s: X-Robots-Tag %q", tc.host, res.Header.Get("X-Robots-Tag"))
			}
			if got := res.Header.Get("Strict-Transport-Security") != ""; got != tc.hsts {
				t.Errorf("%s: Strict-Transport-Security %q", tc.host, res.Header.Get("Strict-Transport-Security"))
			}
		}

		// Routing moved without recreating a container.
		after, err := siteProject.ServiceContainer(ctx, "nginx")
		if err != nil {
			t.Fatal(err)
		}
		if after.ID != nginx.ID {
			t.Error("going live recreated nginx")
		}
	})

	t.Run("a protected preview domain asks before it redirects", func(t *testing.T) {
		on := true
		resp, err := p.Client.UpdateSiteWithResponse(ctx, site.ID, client.UpdateSiteJSONRequestBody{
			PreviewProtection: &client.PreviewProtectionUpdate{Enabled: &on},
		})
		if err != nil || resp.JSON202 == nil || resp.JSON202.PreviewCredentials == nil {
			t.Fatalf("protect: %v %s", err, resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
		creds := resp.JSON202.PreviewCredentials
		res, _ := waitIngress(t, hc, "https://"+preview+"/", nil, http.StatusUnauthorized)
		if loc := res.Header.Get("Location"); loc != "" {
			t.Errorf("the 401 names the primary domain: %s", loc)
		}
		auth := http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(creds.Username+":"+creds.Password))}}
		if res, _ := ingressGet(t, hc, "https://"+preview+"/", auth); res.StatusCode != http.StatusMovedPermanently {
			t.Errorf("with the password: status %d", res.StatusCode)
		}
		if res, _ := ingressGet(t, hc, "https://"+goLivePrimary+"/", nil); res.StatusCode != http.StatusOK {
			t.Errorf("the primary domain asks for the preview password: status %d", res.StatusCode)
		}
		off := false
		resp, err = p.Client.UpdateSiteWithResponse(ctx, site.ID, client.UpdateSiteJSONRequestBody{
			PreviewProtection: &client.PreviewProtectionUpdate{Enabled: &off},
		})
		if err != nil || resp.JSON202 == nil {
			t.Fatalf("unprotect: %v %s", err, resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
		waitIngress(t, hc, "https://"+preview+"/", nil, http.StatusMovedPermanently)
	})

	t.Run("alias changes apply without downtime", func(t *testing.T) {
		const shop = "shop.golive.e2e.test"
		before, err := siteProject.ServiceContainer(ctx, "nginx")
		if err != nil {
			t.Fatal(err)
		}
		// A new alias gets the DNS check.
		resp, err := p.Client.SetSiteAliasesWithResponse(ctx, site.ID, client.SetSiteAliasesJSONRequestBody{Aliases: []string{shop}})
		if err != nil {
			t.Fatal(err)
		}
		if prob := resp.ApplicationProblemJSON422; prob == nil || prob.Type != api.DNSCheckProblemType {
			t.Fatalf("unresolved alias: status %d: %s", resp.StatusCode(), resp.Body)
		}

		// Request the primary domain continuously while the aliases change.
		done := make(chan struct{})
		failures := make(chan string, 1000)
		var requests int
		go func() {
			defer close(failures)
			for {
				select {
				case <-done:
					return
				default:
				}
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+goLivePrimary+"/", nil)
				res, err := hc.Do(req)
				requests++
				switch {
				case err != nil:
					failures <- err.Error()
				default:
					_ = res.Body.Close()
					if res.StatusCode != http.StatusOK {
						failures <- res.Status
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
		}()
		skip := true
		resp, err = p.Client.SetSiteAliasesWithResponse(ctx, site.ID, client.SetSiteAliasesJSONRequestBody{
			Aliases: []string{shop}, SkipDNSCheck: &skip,
		})
		if err != nil || resp.JSON202 == nil {
			close(done)
			t.Fatalf("set aliases: %v %s", err, resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
		res, _ := waitIngress(t, hc, "https://"+shop+"/a?b=c", nil, http.StatusMovedPermanently)
		waitIngress(t, hc, "https://"+goLiveAlias+"/", nil, http.StatusNotFound)
		close(done)
		var failed []string
		for f := range failures {
			failed = append(failed, f)
		}
		t.Logf("%d requests to the primary domain during the change", requests)
		if len(failed) > 0 {
			t.Errorf("%d of %d requests failed during the alias change: %q", len(failed), requests, failed)
		}
		if res.Header.Get("Location") != "https://"+goLivePrimary+"/a?b=c" || res.Header.Get("Strict-Transport-Security") == "" {
			t.Errorf("new alias: Location %q, HSTS %q", res.Header.Get("Location"), res.Header.Get("Strict-Transport-Security"))
		}
		after, err := siteProject.ServiceContainer(ctx, "nginx")
		if err != nil {
			t.Fatal(err)
		}
		if after.ID != before.ID {
			t.Error("changing aliases recreated nginx")
		}
		if got := getSite(t, p, site.ID); len(got.Aliases) != 1 || got.Aliases[0] != shop || got.Status != client.SiteStatusRunning {
			t.Errorf("site after the alias change: %+v", got)
		}
	})

	t.Run("maintenance mode covers the custom domains", func(t *testing.T) {
		set := func(enabled bool) {
			resp, err := p.Client.SetSiteMaintenanceWithResponse(ctx, site.ID, client.SetSiteMaintenanceJSONRequestBody{Enabled: enabled})
			if err != nil || resp.JSON202 == nil {
				t.Fatalf("maintenance %v: %v %s", enabled, err, resp.Body)
			}
			waitJob(t, p, resp.JSON202.Job.ID)
		}
		set(true)
		for _, host := range []string{goLivePrimary, "shop.golive.e2e.test", preview} {
			waitIngress(t, hc, "https://"+host+"/", nil, http.StatusServiceUnavailable)
		}
		set(false)
		waitIngress(t, hc, "https://"+goLivePrimary+"/", nil, http.StatusOK)
	})

	deleted, err := p.Client.DeleteSiteWithResponse(ctx, site.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.JSON202 == nil {
		t.Fatalf("delete site: status %d: %s", deleted.StatusCode(), deleted.Body)
	}
	waitJob(t, p, deleted.JSON202.Job.ID)
	if res, _ := waitIngress(t, hc, "https://"+goLivePrimary+"/", nil, http.StatusNotFound); res.StatusCode != http.StatusNotFound {
		t.Error("the primary domain outlived the site")
	}
}
