package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/images"
)

// stagingPrimary is the production site's primary domain in TestStagingSites.
const stagingPrimary = "www.staging.e2e.test"

// TestStagingSites automates the staging milestone's "done when" (#209,
// design doc §5.4, §5.11) on one production site and its staging site, on a
// platform with certificates from Pebble. The subtests run in order and build
// on each other: each moves the two sites on, as an operator would.
//
// It is the first test that runs clone, refresh, and push against real
// WordPress, restic, and Compose. Unit tests cover the refusals and races that
// are awkward to stage here (see the sites and jobs packages).
func TestStagingSites(t *testing.T) {
	projectDir := t.TempDir()
	var projects []compose.Project
	var tags []string
	// Registered before Start, so it runs after the platform teardown: see
	// TestSiteCreateDelete.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, proj := range projects {
			if res, err := proj.Resources(ctx); err == nil && !res.Empty() {
				t.Logf("removing leftover site project %s", proj.Name)
				if err := proj.Down(ctx, compose.DownOptions{Volumes: true}); err != nil {
					t.Errorf("removing leftover site project %s: %v", proj.Name, err)
				}
			}
		}
		for _, tag := range tags {
			if err := compose.RemoveImage(ctx, nil, tag); err != nil {
				t.Errorf("removing %s: %v", tag, err)
			}
		}
	})
	p := e2e.StartWithPebble(t)
	ctx := t.Context()

	cat, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	php, ok := cat.DefaultPHPVersion(time.Now())
	if !ok {
		t.Fatal("no default PHP version")
	}
	latest := cat.LatestWordPress().Version
	base, err := cat.Images(php, latest)
	if err != nil {
		t.Fatal(err)
	}
	e2e.EnsureImages(t, base.PHP, base.Nginx)
	e2e.EnsureStagingImages(t, base.PHP)

	f := &stagingFlow{p: p, hc: p.IngressClient(t)}

	// The production site, live, with content that names its URLs.
	groupID := createGroup(t, p, "E2E Staging Agency")
	created := createSite(t, p, groupID, "E2E Staging Production", php)
	f.prod = compose.Project{Name: compose.SiteProjectName(created.Site.ID), Dir: projectDir}
	projects = append(projects, f.prod)
	waitJob(t, p, created.Job.ID)
	f.prodID = created.Site.ID
	skipDNS := false
	live, err := p.Client.GoLiveSiteWithResponse(ctx, f.prodID, client.GoLiveSiteJSONRequestBody{PrimaryDomain: stagingPrimary, SkipDNSCheck: &skipDNS})
	if err != nil || live.JSON202 == nil {
		t.Fatalf("go-live: %v %s", err, live.Body)
	}
	waitJob(t, p, live.JSON202.Job.ID)
	const primaryURL = "https://" + stagingPrimary
	prodPost := siteWP(t, f.prod, "post", "create", "--post_status=publish", "--post_title=PF production launch post",
		`--post_content=<a href="`+primaryURL+`/about/">About</a>`, "--porcelain")
	prodGUID := siteWP(t, f.prod, "post", "get", prodPost, "--field=guid")
	siteWP(t, f.prod, "option", "update", "pf_e2e_links", `{"home":"`+primaryURL+`/","n":1}`, "--format=json")
	siteSh(t, f.prod, "mkdir -p "+wpContent+"/uploads/pf-e2e && echo production > "+wpContent+"/uploads/pf-e2e/from-production.txt")
	waitIngress(t, f.hc, primaryURL+"/", nil, http.StatusOK)

	var previewAuth http.Header
	var cloneDatabase, cloneUploads time.Time

	if !t.Run("clone", func(t *testing.T) {
		resp, err := p.Client.CreateStagingSiteWithResponse(ctx, f.prodID, client.CreateStagingSiteJSONRequestBody{})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil {
			t.Fatalf("create staging site: status %d: %s", resp.StatusCode(), resp.Body)
		}
		f.stagID = resp.JSON202.Site.ID
		f.stag = compose.Project{Name: compose.SiteProjectName(f.stagID), Dir: projectDir}
		projects = append(projects, f.stag)
		creds := resp.JSON202.PreviewCredentials
		if creds == nil {
			t.Fatalf("no preview credentials: %s", resp.Body)
		}
		previewAuth = http.Header{"Authorization": {"Basic " + base64.StdEncoding.EncodeToString([]byte(creds.Username+":"+creds.Password))}}
		start := time.Now()
		waitJob(t, p, resp.JSON202.Job.ID)
		t.Logf("cloned in %s", time.Since(start).Round(time.Second))

		stag := getSite(t, p, f.stagID)
		f.stagHost = stag.PreviewDomain
		if stag.Environment != client.Staging || stag.ParentSiteID == nil || *stag.ParentSiteID != f.prodID ||
			stag.ClientGroupID != groupID || stag.Status != client.SiteStatusRunning || stag.PhpVersion != php || !stag.PreviewProtection.Enabled {
			t.Errorf("staging site: %+v", stag)
		}
		if stag.Staging == nil || stag.Staging.OutboundMail || stag.Staging.LastRefreshedAt == nil {
			t.Fatalf("staging state: %+v", stag.Staging)
		}
		cloneDatabase, cloneUploads = requireAsOf(t, stag, "the clone")

		// It serves production's content on its own domain, with its own URLs,
		// behind the preview password.
		stagURL := "https://" + f.stagHost
		if res, _ := waitIngress(t, f.hc, stagURL+"/", nil, http.StatusUnauthorized); res.StatusCode != http.StatusUnauthorized {
			t.Errorf("without the preview password: status %d", res.StatusCode)
		}
		res, home := waitIngress(t, f.hc, stagURL+"/", previewAuth, http.StatusOK)
		if !strings.Contains(home, "PF production launch post") || !strings.Contains(home, stagURL+"/") || strings.Contains(home, stagingPrimary) {
			t.Errorf("the staging home page isn't production's content with its own URLs:\n%.2000s", home)
		}
		if got := res.Header.Get("X-Robots-Tag"); got != "noindex" {
			t.Errorf("X-Robots-Tag = %q", got)
		}
		for _, option := range []string{"home", "siteurl"} {
			if got := siteWP(t, f.stag, "option", "get", option); got != stagURL {
				t.Errorf("%s = %q, want %q", option, got, stagURL)
			}
		}
		if got := siteWP(t, f.stag, "post", "get", prodPost, "--field=post_content"); !strings.Contains(got, stagURL+"/about/") {
			t.Errorf("post content = %q", got)
		}
		if got := siteWP(t, f.stag, "option", "get", "pf_e2e_links", "--format=json"); !strings.Contains(got, `"https:\/\/`+f.stagHost+`\/"`) {
			t.Errorf("serialized option = %s", got)
		}
		if got := siteWP(t, f.stag, "post", "get", prodPost, "--field=guid"); got != prodGUID {
			t.Errorf("GUID = %q, want %q", got, prodGUID)
		}
		f.checkNoHost(t, f.stag, stagingPrimary)
		if got := siteSh(t, f.stag, "cat "+wpContent+"/uploads/pf-e2e/from-production.txt"); got != "production" {
			t.Errorf("uploaded file = %q", got)
		}
		// Copied files belong to the staging site's own user.
		if owners := siteSh(t, f.stag, "find "+wpContent+" -mindepth 1 ! -user \"$(id -u)\" | head -5"); owners != "" {
			t.Errorf("files not owned by the staging site's user: %s", owners)
		}

		f.checkEnvironment(t, f.stag, "staging")
		f.checkEnvironment(t, f.prod, "production")
		f.checkMail(t)
		f.checkDefaultPlugins(t, f.stag)

		// No network path between the two sites' containers.
		a, b := siteNetwork(t, f.stag), siteNetwork(t, f.prod)
		for _, dir := range []struct{ from, to siteNet }{{a, b}, {b, a}} {
			results := probe(t, dir.from.project, []string{
				// Positive controls: the probe works.
				"nginx:8080", compose.MariaDBAlias + ":3306", compose.RedisAlias + ":6379",
				dir.to.names["nginx"] + ":8080", dir.to.names["php"] + ":9000",
				dir.to.ips["nginx"] + ":8080", dir.to.ips["php"] + ":9000",
			})
			for i, r := range results {
				t.Logf("from %s: %s open=%v ip=%q %s", dir.from.project.Name, r.target, r.open, r.ip, r.detail)
				if want := i < 3; r.open != want {
					t.Errorf("from %s: %s open = %v, want %v (%s)", dir.from.project.Name, r.target, r.open, want, r.detail)
				}
			}
		}
	}) {
		t.FailNow() // Everything after this needs the staging site.
	}

	// Pushing code: the plugin comes along, nothing in the database moves.
	const widget = "pf-e2e-widget"
	widgetFile := wpContent + "/plugins/" + widget + "/" + widget + ".php"
	widgetSource := "<?php\n/* Plugin Name: PF E2E Widget */\n"

	t.Run("code push", func(t *testing.T) {
		job := f.deployPlugin(t, widget, widgetSource, true)
		waitJob(t, p, job.ID)
		if got := siteWP(t, f.stag, "plugin", "get", widget, "--field=status"); got != "active" {
			t.Errorf("the deployed plugin on the staging site is %s", got)
		}
		post := siteWP(t, f.prod, "post", "create", "--post_status=publish", "--post_title=PF written on production", "--porcelain")
		siteSh(t, f.prod, "echo kept > "+wpContent+"/uploads/pf-e2e/kept-by-code-push.txt")
		before := f.database(t, f.prodID)
		settings := getSite(t, p, f.prodID)

		final := f.push(t, client.StagingCopyFilesCode, false)
		if final.State != client.JobStateSucceeded {
			t.Fatalf("code push: %s: %s", final.State, deref(final.Error))
		}
		f.checkUnchanged(t, before, f.database(t, f.prodID))
		if got := siteWP(t, f.prod, "plugin", "get", widget, "--field=status"); got != "inactive" {
			t.Errorf("the pushed plugin on production is %q, want inactive: activation is in the database", got)
		}
		if got := siteWP(t, f.prod, "post", "get", post, "--field=post_title"); got != "PF written on production" {
			t.Errorf("production's post = %q", got)
		}
		if got := siteSh(t, f.prod, "cat "+wpContent+"/uploads/pf-e2e/kept-by-code-push.txt"); got != "kept" {
			t.Errorf("production's upload = %q", got)
		}
		f.checkSettingsKept(t, settings)
		if w := jobWarnings(final); len(w) != 0 {
			t.Errorf("warnings from a push that removed nothing: %q", w)
		}
		if res, _ := waitIngress(t, f.hc, primaryURL+"/", nil, http.StatusOK); res.Header.Get("X-Robots-Tag") != "" {
			t.Errorf("production sends X-Robots-Tag: %s", res.Header.Get("X-Robots-Tag"))
		}
		f.checkPushResult(t, final, client.PreRestore)
		// A code push leaves the copy times alone, and records the push.
		stag := getSite(t, p, f.stagID)
		if d, u := requireAsOf(t, stag, "a code push"); !d.Equal(cloneDatabase) || !u.Equal(cloneUploads) {
			t.Errorf("after a code push: database_as_of %s (was %s), uploads_as_of %s (was %s)", d, cloneDatabase, u, cloneUploads)
		}
		if stag.Staging.LastPushedAt == nil {
			t.Errorf("the push isn't recorded on the staging site: %+v", stag.Staging)
		}
	})

	t.Run("a code push repairs a missing default plugin", func(t *testing.T) {
		siteWP(t, f.stag, "plugin", "delete", sites.PluginNginxHelper)
		before := f.database(t, f.prodID)
		final := f.push(t, client.StagingCopyFilesCode, false)
		if final.State != client.JobStateSucceeded {
			t.Fatalf("code push: %s: %s", final.State, deref(final.Error))
		}
		if got := siteWP(t, f.prod, "plugin", "get", sites.PluginNginxHelper, "--field=status"); got != "active" {
			t.Errorf("%s on production is %q, want active", sites.PluginNginxHelper, got)
		}
		warnings := jobWarnings(final)
		if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, sites.PluginNginxHelper) }) {
			t.Errorf("no warning names %s: %q", sites.PluginNginxHelper, warnings)
		}
		// Reinstalling a plugin may clear WordPress's cached plugin update list,
		// which is a transient (#204); nothing else may change.
		f.checkUnchanged(t, before, f.database(t, f.prodID))
	})

	t.Run("a code-only refresh", func(t *testing.T) {
		siteSh(t, f.stag, "echo staging > "+wpContent+"/uploads/pf-e2e/from-staging.txt")
		post := siteWP(t, f.stag, "post", "create", "--post_status=publish", "--post_title=PF kept by a code refresh", "--porcelain")
		before := getSite(t, p, f.stagID)
		final := f.refresh(t, client.StagingCopyFilesCode, false)
		if final.State != client.JobStateSucceeded {
			t.Fatalf("code refresh: %s: %s", final.State, deref(final.Error))
		}
		// Production's plugins are back, and nothing else moved.
		if out, err := f.stag.ExecService(ctx, "php", "wp", "plugin", "get", sites.PluginNginxHelper, "--field=name"); err != nil {
			t.Errorf("the code refresh didn't bring %s back: %v: %s", sites.PluginNginxHelper, err, out)
		}
		if got := siteWP(t, f.stag, "post", "get", post, "--field=post_title"); got != "PF kept by a code refresh" {
			t.Errorf("post on the staging site = %q", got)
		}
		if got := siteSh(t, f.stag, "cat "+wpContent+"/uploads/pf-e2e/from-staging.txt"); got != "staging" {
			t.Errorf("upload on the staging site = %q", got)
		}
		after := getSite(t, p, f.stagID)
		bd, bu := requireAsOf(t, before, "before the refresh")
		ad, au := requireAsOf(t, after, "a code-only refresh")
		if !ad.Equal(bd) || !au.Equal(bu) {
			t.Errorf("a code-only refresh moved the copy times: database_as_of %s to %s, uploads_as_of %s to %s", bd, ad, bu, au)
		}
		if after.Staging.LastRefreshedAt == nil || !after.Staging.LastRefreshedAt.After(*before.Staging.LastRefreshedAt) {
			t.Errorf("last_refreshed_at %v, was %v", after.Staging.LastRefreshedAt, before.Staging.LastRefreshedAt)
		}
		f.checkEnvironment(t, f.stag, "staging")
	})

	t.Run("a failed push rolls production back", func(t *testing.T) {
		// A plugin active on production, whose new code on the staging site
		// fatals on load. The push changes production's files, and the WP-CLI
		// check after them fails.
		siteWP(t, f.prod, "plugin", "activate", widget)
		post := siteWP(t, f.prod, "post", "create", "--post_status=publish", "--post_title=PF survives the failed push", "--porcelain")
		siteSh(t, f.stag, "printf '%s\\n' 'throw new Error(\"pf-e2e boom\");' >> "+widgetFile)
		before := f.database(t, f.prodID)
		settings := getSite(t, p, f.prodID)

		final := f.push(t, client.StagingCopyFilesCode, false)
		if final.State != client.JobStateFailed {
			t.Fatalf("push of code that fatals: %s: %s", final.State, deref(final.Error))
		}
		t.Logf("the push failed as intended: %s", deref(final.Error))
		if !strings.Contains(deref(final.Error), "can't load the pushed code") {
			t.Errorf("job error = %q", deref(final.Error))
		}
		if len(deref(final.Error)) > 2000 {
			t.Errorf("job error is %d bytes long", len(deref(final.Error)))
		}
		if got := siteSh(t, f.prod, "cat "+widgetFile); strings.Contains(got, "throw") {
			t.Errorf("the broken plugin is still on production:\n%s", got)
		}
		if got := siteWP(t, f.prod, "post", "get", post, "--field=post_title"); got != "PF survives the failed push" {
			t.Errorf("post after the rollback = %q", got)
		}
		if got := siteWP(t, f.prod, "plugin", "get", widget, "--field=status"); got != "active" {
			t.Errorf("%s after the rollback is %q", widget, got)
		}
		f.checkUnchanged(t, before, f.database(t, f.prodID))
		f.checkSettingsKept(t, settings)
		got := getSite(t, p, f.prodID)
		if got.Status != client.SiteStatusRunning || !got.Maintenance || !strings.Contains(deref(got.StatusMessage), "rolled back") {
			t.Errorf("production after the rollback: %s, maintenance %v, message %q", got.Status, got.Maintenance, deref(got.StatusMessage))
		}
		if id, _ := final.Result["pre_push_backup_id"].(string); id == "" || !strings.Contains(deref(got.StatusMessage), id) {
			t.Errorf("the failed push's result %v doesn't name the backup its status message does: %q", final.Result, deref(got.StatusMessage))
		}
		res, _ := waitIngress(t, f.hc, primaryURL+"/", nil, http.StatusServiceUnavailable)
		if res.Header.Get("Retry-After") == "" {
			t.Error("the maintenance page has no Retry-After")
		}

		// The operator checks the site and turns maintenance mode off.
		f.setMaintenance(t, false)
		waitIngress(t, f.hc, primaryURL+"/", nil, http.StatusOK)
		siteSh(t, f.stag, "printf '%s' '"+widgetSource+"' > "+widgetFile)
		// The message keeps saying what happened, but no longer claims the site is
		// in maintenance mode or asks to turn it off: that is the live flag's to say.
		off := getSite(t, p, f.prodID)
		message := deref(off.StatusMessage)
		if off.Maintenance {
			t.Error("production is still in maintenance mode after it was turned off")
		}
		for _, stale := range []string{"maintenance mode", "turn it off"} {
			if strings.Contains(strings.ToLower(message), stale) {
				t.Errorf("the status message still says %q with maintenance mode off: %q", stale, message)
			}
		}
		backupID, _ := final.Result["pre_push_backup_id"].(string)
		if !strings.Contains(message, "failed") || !strings.Contains(message, "rolled back") || backupID == "" || !strings.Contains(message, backupID) {
			t.Errorf("the status message no longer says the push failed and was rolled back to backup %q: %q", backupID, message)
		}
	})

	t.Run("database push and undo", func(t *testing.T) {
		stagURL := "https://" + f.stagHost
		stagPost := siteWP(t, f.stag, "post", "create", "--post_status=publish", "--post_title=PF written on staging",
			`--post_content=<a href="`+stagURL+`/contact/">Contact</a>`, "--porcelain")
		stagGUID := siteWP(t, f.stag, "post", "get", stagPost, "--field=guid")
		siteWP(t, f.stag, "option", "update", "pf_e2e_links", `{"home":"`+stagURL+`/","n":2}`, "--format=json")
		lost := siteWP(t, f.prod, "post", "create", "--post_status=publish", "--post_title=PF lost to the database push", "--porcelain")
		beforeStag := getSite(t, p, f.stagID)
		settings := getSite(t, p, f.prodID)
		beforeDB := f.database(t, f.prodID)

		final := f.push(t, client.StagingCopyFilesNone, true)
		if final.State != client.JobStateSucceeded {
			t.Fatalf("database push: %s: %s", final.State, deref(final.Error))
		}
		for _, option := range []string{"home", "siteurl"} {
			if got := siteWP(t, f.prod, "option", "get", option); got != primaryURL {
				t.Errorf("%s = %q, want %q", option, got, primaryURL)
			}
		}
		if got := siteWP(t, f.prod, "post", "get", stagPost, "--field=post_content"); !strings.Contains(got, primaryURL+"/contact/") {
			t.Errorf("post content = %q", got)
		}
		if got := siteWP(t, f.prod, "post", "get", stagPost, "--field=guid"); got != stagGUID {
			t.Errorf("GUID = %q, want %q (GUIDs aren't rewritten)", got, stagGUID)
		}
		if got := siteWP(t, f.prod, "option", "get", "pf_e2e_links", "--format=json"); !strings.Contains(got, `"https:\/\/`+stagingPrimary+`\/"`) || !strings.Contains(got, `"n":2`) {
			t.Errorf("serialized option = %s", got)
		}
		f.checkNoHost(t, f.prod, f.stagHost)
		if out, err := f.prod.ExecService(ctx, "php", "wp", "post", "get", lost, "--field=ID"); err == nil {
			t.Errorf("post %s written on production after the staging copy survived a database push: %s", lost, out)
		}
		_, home := waitIngress(t, f.hc, primaryURL+"/", nil, http.StatusOK)
		if !strings.Contains(home, "PF written on staging") || strings.Contains(home, f.stagHost) {
			t.Errorf("the home page doesn't serve staging's content with production's URLs:\n%.2000s", home)
		}
		f.checkSettingsKept(t, settings)
		f.checkEnvironment(t, f.prod, "production")
		f.checkDefaultPlugins(t, f.prod)
		// The push sets the database's copy time, and only that.
		afterStag := getSite(t, p, f.stagID)
		bd, bu := requireAsOf(t, beforeStag, "before the push")
		ad, au := requireAsOf(t, afterStag, "a database push")
		if !ad.After(bd) || !au.Equal(bu) {
			t.Errorf("after a database push: database_as_of %s (was %s), uploads_as_of %s (was %s)", ad, bd, au, bu)
		}

		// Restoring the pre-push backup undoes it.
		backup := f.checkPushResult(t, final, client.PreRestore)
		job := restoreSite(t, p, f.prodID, *backup.SnapshotID, "")
		waitJob(t, p, job.ID)
		if got := siteWP(t, f.prod, "post", "get", lost, "--field=post_title"); got != "PF lost to the database push" {
			t.Errorf("post after the undo = %q", got)
		}
		if out, err := f.prod.ExecService(ctx, "php", "wp", "post", "get", stagPost, "--field=post_title"); err == nil && strings.TrimSpace(string(out)) == "PF written on staging" {
			t.Error("staging's post is still there after the undo")
		}
		f.checkUnchanged(t, beforeDB, f.database(t, f.prodID))
		if got := getSite(t, p, f.prodID); got.Status != client.SiteStatusRunning || got.Maintenance {
			t.Errorf("production after the undo: %s, maintenance %v", got.Status, got.Maintenance)
		}
		_, home = waitIngress(t, f.hc, primaryURL+"/", nil, http.StatusOK)
		if !strings.Contains(home, "PF lost to the database push") || strings.Contains(home, "PF written on staging") {
			t.Errorf("the home page after the undo:\n%.2000s", home)
		}
	})

	t.Run("all-files push and undo", func(t *testing.T) {
		const onlyProd = wpContent + "/uploads/pf-e2e/only-on-production.txt"
		const onlyStag = wpContent + "/uploads/pf-e2e/only-on-staging.txt"
		siteSh(t, f.prod, "echo media > "+onlyProd)
		siteSh(t, f.stag, "echo media > "+onlyStag)
		beforeStag := getSite(t, p, f.stagID)
		settings := getSite(t, p, f.prodID)
		before := f.database(t, f.prodID)

		final := f.push(t, client.StagingCopyFilesAll, false)
		if final.State != client.JobStateSucceeded {
			t.Fatalf("all-files push: %s: %s", final.State, deref(final.Error))
		}
		if got := siteSh(t, f.prod, "ls "+wpContent+"/uploads/pf-e2e"); strings.Contains(got, "only-on-production") || !strings.Contains(got, "only-on-staging") {
			t.Errorf("production's uploads after an all-files push:\n%s", got)
		}
		// Production keeps its settings.
		f.checkSettingsKept(t, settings)
		f.checkEnvironment(t, f.prod, "production")
		f.checkUnchanged(t, before, f.database(t, f.prodID))
		if owners := siteSh(t, f.prod, "find "+wpContent+" -mindepth 1 ! -user \"$(id -u)\" | head -5"); owners != "" {
			t.Errorf("files not owned by production's user: %s", owners)
		}
		f.checkDefaultPlugins(t, f.prod)
		if got := siteWP(t, f.prod, "redis", "status"); !strings.Contains(got, "Connected") {
			t.Errorf("wp redis status on production:\n%s", got)
		}
		// All files set the uploads' copy time, and only that.
		afterStag := getSite(t, p, f.stagID)
		bd, bu := requireAsOf(t, beforeStag, "before the push")
		ad, au := requireAsOf(t, afterStag, "an all-files push")
		if !ad.Equal(bd) || !au.After(bu) {
			t.Errorf("after an all-files push: database_as_of %s (was %s), uploads_as_of %s (was %s)", ad, bd, au, bu)
		}

		backup := f.checkPushResult(t, final, client.PreRestore)
		job := restoreSite(t, p, f.prodID, *backup.SnapshotID, "")
		waitJob(t, p, job.ID)
		if got := siteSh(t, f.prod, "ls "+wpContent+"/uploads/pf-e2e"); !strings.Contains(got, "only-on-production") || strings.Contains(got, "only-on-staging") {
			t.Errorf("production's uploads after the undo:\n%s", got)
		}
		f.checkSettingsKept(t, settings)
	})

	t.Run("database-only refresh", func(t *testing.T) {
		const stagingOnly = "pf-e2e-staging-only"
		job := f.deployPlugin(t, stagingOnly, "<?php\n/* Plugin Name: PF E2E Staging Only */\n", true)
		waitJob(t, p, job.ID)
		fresh := siteWP(t, f.prod, "post", "create", "--post_status=publish", "--post_title=PF new on production", "--porcelain")
		mine := siteWP(t, f.stag, "post", "create", "--post_status=publish", "--post_title=PF replaced by the refresh", "--porcelain")
		before := getSite(t, p, f.stagID)

		final := f.refresh(t, client.StagingCopyFilesNone, true)
		if final.State != client.JobStateSucceeded {
			t.Fatalf("database refresh: %s: %s", final.State, deref(final.Error))
		}
		if got := siteWP(t, f.stag, "post", "get", fresh, "--field=post_title"); got != "PF new on production" {
			t.Errorf("production's new post on the staging site = %q", got)
		}
		if got, err := f.stag.ExecService(ctx, "php", "wp", "post", "get", mine, "--field=post_title"); err == nil && strings.TrimSpace(string(got)) == "PF replaced by the refresh" {
			t.Error("the staging site's own post survived a database refresh")
		}
		// It keeps its plugins' code: the database is production's, so a plugin
		// only it has is no longer active.
		if got := siteWP(t, f.stag, "plugin", "get", stagingOnly, "--field=status"); got != "inactive" {
			t.Errorf("%s after the refresh is %q, want present and inactive", stagingOnly, got)
		}
		stagURL := "https://" + f.stagHost
		for _, option := range []string{"home", "siteurl"} {
			if got := siteWP(t, f.stag, "option", "get", option); got != stagURL {
				t.Errorf("%s = %q, want %q", option, got, stagURL)
			}
		}
		f.checkNoHost(t, f.stag, stagingPrimary)
		f.checkDefaultPlugins(t, f.stag)
		// It keeps its settings.
		f.checkEnvironment(t, f.stag, "staging")
		f.checkMail(t)
		after := getSite(t, p, f.stagID)
		if !after.PreviewProtection.Enabled || after.Staging.OutboundMail {
			t.Errorf("settings after the refresh: protection %v, mail %v", after.PreviewProtection.Enabled, after.Staging.OutboundMail)
		}
		waitIngress(t, f.hc, stagURL+"/", nil, http.StatusUnauthorized)
		_, home := waitIngress(t, f.hc, stagURL+"/", previewAuth, http.StatusOK)
		if !strings.Contains(home, "PF new on production") || strings.Contains(home, stagingPrimary) {
			t.Errorf("the staging home page after the refresh:\n%.2000s", home)
		}
		bd, bu := requireAsOf(t, before, "before the refresh")
		ad, au := requireAsOf(t, after, "a database-only refresh")
		if !ad.After(bd) || !au.Equal(bu) {
			t.Errorf("after a database-only refresh: database_as_of %s (was %s), uploads_as_of %s (was %s)", ad, bd, au, bu)
		}
	})

	t.Run("a plain restore clears the copy times", func(t *testing.T) {
		snapshot := backupSite(t, p, f.stagID)
		job := restoreSite(t, p, f.stagID, snapshot, "")
		waitJob(t, p, job.ID)
		got := getSite(t, p, f.stagID)
		if got.Staging == nil || got.Staging.DatabaseAsOf != nil || got.Staging.UploadsAsOf != nil {
			t.Errorf("staging state after a restore: %+v", got.Staging)
		}
		f.checkEnvironment(t, f.stag, "staging")
	})

	t.Run("the core guard refuses a database push", func(t *testing.T) {
		// A newer branch needs a stand-in release, as in TestCoreUpdate: the
		// suite's images are all of one WordPress release.
		parts := strings.Split(latest, ".")
		minor, err := strconv.Atoi(parts[1])
		if err != nil || len(parts) != 3 {
			t.Fatalf("the latest release %s isn't x.y.z", latest)
		}
		branch := parts[0] + "." + strconv.Itoa(minor+1)
		newer := branch + ".0"
		registry := "pfe2e-staging.local/" + strings.ToLower(p.Project.Name)
		testCat, err := catalog.Parse(stagingManifest(t, registry, latest, newer))
		if err != nil {
			t.Fatal(err)
		}
		const versionPHP = "/var/www/wordpress/wp-includes/version.php"
		for _, release := range []struct{ version, run string }{
			{latest, ""},
			{newer, `sed -i "s/^\$wp_version = '` + latest + `';/\$wp_version = '` + newer + `';/" ` + versionPHP},
		} {
			imgs, err := testCat.Images(php, release.version)
			if err != nil {
				t.Fatal(err)
			}
			for _, img := range []struct{ tag, from string }{{imgs.PHP, base.PHP}, {imgs.Nginx, base.Nginx}} {
				dockerfile := "FROM " + img.from + "\n"
				if release.run != "" {
					dockerfile += "USER root\nRUN " + release.run + "\nUSER 65534:65534\n"
				}
				tags = append(tags, img.tag)
				if err := compose.BuildImage(ctx, nil, img.tag, dockerfile); err != nil {
					t.Fatal(err)
				}
				// The control plane pins the builds it runs under a tag of their
				// own, in the same repository, which has to go too.
				id, err := compose.ImageID(ctx, nil, img.tag)
				if err != nil {
					t.Fatal(err)
				}
				tags = append(tags, compose.PinTag(img.tag[:strings.LastIndexByte(img.tag, ':')], id))
			}
		}
		hold := true
		if resp, err := p.Client.SetSiteCorePolicyWithResponse(ctx, f.prodID, client.SetSiteCorePolicyJSONRequestBody{Hold: &hold}); err != nil || resp.JSON200 == nil {
			t.Fatalf("holding production's core: %v %s", err, resp.Body)
		}
		manifestFile := filepath.Join(p.DataDir, "e2e-manifest-staging.json")
		if err := os.WriteFile(manifestFile, stagingManifest(t, registry, latest, newer), 0o644); err != nil {
			t.Fatal(err)
		}
		p.Restart(t, map[string]string{"PRESSFLEET_IMAGE_MANIFEST_FILE": manifestFile})

		// The staging site follows its own policy (§5.11).
		before := coreJobs(t, p, f.stagID)
		resp, err := p.Client.SetSiteCorePolicyWithResponse(ctx, f.stagID, client.SetSiteCorePolicyJSONRequestBody{ApprovedBranch: &branch})
		if err != nil || resp.JSON200 == nil {
			t.Fatalf("approving %s on the staging site: %v %s", branch, err, resp.Body)
		}
		waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		var update string
		for update == "" {
			for _, j := range coreJobs(t, p, f.stagID) {
				if !slices.Contains(before, j) {
					update = j
					break
				}
			}
			select {
			case <-waitCtx.Done():
				t.Fatal("the rollout queued no core update of the staging site")
			case <-time.After(2 * time.Second):
			}
		}
		waitJob(t, p, update)
		if got := getSite(t, p, f.stagID); got.WpCoreVersion != newer {
			t.Fatalf("the staging site runs WordPress %s, want %s", got.WpCoreVersion, newer)
		}
		f.waitRunning(t, f.prodID)
		prodBefore := getSite(t, p, f.prodID)
		if prodBefore.WpCoreVersion != latest {
			t.Fatalf("production, held, runs WordPress %s, want %s", prodBefore.WpCoreVersion, latest)
		}

		refused, err := p.Client.PushStagingSiteWithResponse(ctx, f.stagID, client.PushStagingSiteJSONRequestBody{Files: client.StagingCopyFilesNone, Database: true})
		if err != nil {
			t.Fatal(err)
		}
		prob := refused.ApplicationProblemJSON422
		if prob == nil || prob.Errors == nil || !slices.ContainsFunc(*prob.Errors, func(e client.FieldError) bool {
			return e.Field == "/database" && strings.Contains(e.Detail, "holds it on branch")
		}) {
			t.Fatalf("a database push against a hold: status %d: %s", refused.StatusCode(), refused.Body)
		}
		if got := getSite(t, p, f.prodID); got.Status != client.SiteStatusRunning || got.WpCoreVersion != latest {
			t.Errorf("production after the refusal: %s on %s", got.Status, got.WpCoreVersion)
		}
	})

	t.Run("deletion", func(t *testing.T) {
		// Refused while the staging site exists, before anything is removed.
		refused, err := p.Client.DeleteSiteWithResponse(ctx, f.prodID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if refused.StatusCode() != http.StatusConflict {
			t.Fatalf("deleting a production site with a staging site: status %d: %s", refused.StatusCode(), refused.Body)
		}
		if got := getSite(t, p, f.prodID); got.Status != client.SiteStatusRunning {
			t.Errorf("production after the refused deletion: %s", got.Status)
		}

		// An Operator deletes the staging site; deleting production stays Admin.
		operator := newUser(t, p, "operator@e2e.test", client.Operator)
		denied, err := operator.DeleteSiteWithResponse(ctx, f.prodID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if denied.StatusCode() != http.StatusForbidden {
			t.Errorf("an Operator deleting a production site: status %d: %s", denied.StatusCode(), denied.Body)
		}
		deleted, err := operator.DeleteSiteWithResponse(ctx, f.stagID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if deleted.JSON202 == nil {
			t.Fatalf("an Operator deleting a staging site: status %d: %s", deleted.StatusCode(), deleted.Body)
		}
		waitJob(t, p, deleted.JSON202.Job.ID)
		f.checkGone(t, f.stagID, f.stag)
		if res, _ := waitIngress(t, f.hc, "https://"+f.stagHost+"/", previewAuth, http.StatusNotFound); res.StatusCode != http.StatusNotFound {
			t.Errorf("the staging preview domain answers %d", res.StatusCode)
		}

		// Production carries on, and can now go too.
		_, home := waitIngress(t, f.hc, primaryURL+"/", nil, http.StatusOK)
		if !strings.Contains(home, "PF production launch post") {
			t.Errorf("production after the staging site's deletion:\n%.2000s", home)
		}
		gone, err := p.Client.DeleteSiteWithResponse(ctx, f.prodID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if gone.JSON202 == nil {
			t.Fatalf("deleting production: status %d: %s", gone.StatusCode(), gone.Body)
		}
		waitJob(t, p, gone.JSON202.Job.ID)
		f.checkGone(t, f.prodID, f.prod)
	})
}

// stagingFlow is the two sites of TestStagingSites.
type stagingFlow struct {
	p              *e2e.Platform
	hc             *http.Client
	prodID, stagID string
	prod, stag     compose.Project
	stagHost       string
}

// push pushes the staging site and returns the push job once it ended. The
// request's own promises are checked here: it answers with the production
// site, updating, and a job waiting for the staging site's backup.
func (f *stagingFlow) push(t *testing.T, files client.StagingCopyFiles, database bool) client.Job {
	t.Helper()
	resp, err := f.p.Client.PushStagingSiteWithResponse(t.Context(), f.stagID, client.PushStagingSiteJSONRequestBody{Files: files, Database: database})
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON202 == nil {
		t.Fatalf("push: status %d: %s", resp.StatusCode(), resp.Body)
	}
	accepted := resp.JSON202
	if accepted.Site.ID != f.prodID || accepted.Site.Status != client.SiteStatusUpdating {
		t.Errorf("the push answered with site %s, %s", accepted.Site.ID, accepted.Site.Status)
	}
	if accepted.Job.Type != sites.PushJobType || accepted.Job.AfterJobID == nil {
		t.Errorf("push job: type %s, after %v", accepted.Job.Type, accepted.Job.AfterJobID)
	}
	start := time.Now()
	final := finalJob(t, f.p, accepted.Job.ID)
	t.Logf("push (files %s, database %v): %s in %s", files, database, final.State, time.Since(start).Round(time.Second))
	return f.withLogs(t, final)
}

// refresh refreshes the staging site with a fresh backup of production and
// returns the refresh job once it ended.
func (f *stagingFlow) refresh(t *testing.T, files client.StagingCopyFiles, database bool) client.Job {
	t.Helper()
	resp, err := f.p.Client.RefreshStagingSiteWithResponse(t.Context(), f.stagID, client.RefreshStagingSiteJSONRequestBody{Files: files, Database: database})
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON202 == nil {
		t.Fatalf("refresh: status %d: %s", resp.StatusCode(), resp.Body)
	}
	start := time.Now()
	final := finalJob(t, f.p, resp.JSON202.Job.ID)
	t.Logf("refresh (files %s, database %v): %s in %s", files, database, final.State, time.Since(start).Round(time.Second))
	return f.withLogs(t, final)
}

// withLogs logs what a failed copy job said, for reading a failed test.
func (f *stagingFlow) withLogs(t *testing.T, job client.Job) client.Job {
	t.Helper()
	if job.State != client.JobStateSucceeded {
		for _, s := range job.Steps {
			t.Logf("step %s: %s", s.Name, s.State)
		}
	}
	return job
}

// jobWarnings returns the warnings in a job's steps' logs.
func jobWarnings(job client.Job) []string {
	var out []string
	for _, s := range job.Steps {
		if s.Log == nil {
			continue
		}
		for _, e := range *s.Log {
			if e.Level == client.JobLogEntryLevelWarn {
				out = append(out, e.Message)
			}
		}
	}
	return out
}

// checkPushResult checks what a finished push's result names, and returns the
// pre-push backup.
func (f *stagingFlow) checkPushResult(t *testing.T, job client.Job, kind client.BackupKind) client.Backup {
	t.Helper()
	id, _ := job.Result["pre_push_backup_id"].(string)
	if id == "" {
		t.Fatalf("the push's result names no pre-push backup: %v", job.Result)
	}
	for _, key := range []string{"snapshot_id", "backup_id"} {
		if s, _ := job.Result[key].(string); s == "" {
			t.Errorf("the push's result has no %s: %v", key, job.Result)
		}
	}
	list, err := f.p.Client.ListSiteBackupsWithResponse(t.Context(), f.prodID, &client.ListSiteBackupsParams{})
	if err != nil || list.JSON200 == nil {
		t.Fatalf("list backups: %v", err)
	}
	for _, b := range list.JSON200.Items {
		if b.ID == id {
			if b.Kind != kind || b.Status != client.BackupStatusSucceeded || b.SnapshotID == nil {
				t.Fatalf("pre-push backup = %+v", b)
			}
			return b
		}
	}
	t.Fatalf("pre-push backup %s isn't among production's backups", id)
	return client.Backup{}
}

// requireAsOf returns a staging site's copy times, which must both be set.
func requireAsOf(t *testing.T, site client.Site, when string) (database, uploads time.Time) {
	t.Helper()
	if site.Staging == nil || site.Staging.DatabaseAsOf == nil || site.Staging.UploadsAsOf == nil {
		t.Fatalf("after %s, the copy times aren't both set: %+v", when, site.Staging)
	}
	return *site.Staging.DatabaseAsOf, *site.Staging.UploadsAsOf
}

// checkEnvironment checks what WordPress itself says about the environment,
// and that wp-config.php agrees: only a staging site names it, and has the mail
// switch.
func (f *stagingFlow) checkEnvironment(t *testing.T, proj compose.Project, want string) {
	t.Helper()
	if got := siteWP(t, proj, "eval", "echo wp_get_environment_type();"); got != want {
		t.Errorf("%s: wp_get_environment_type() = %q, want %q", proj.Name, got, want)
	}
	config := siteSh(t, proj, "cat /var/www/wordpress/wp-config.php")
	staging := want == "staging"
	if strings.Contains(config, "'WP_ENVIRONMENT_TYPE', 'staging'") != staging {
		t.Errorf("%s: wp-config.php names the staging environment: %v, want %v", proj.Name, !staging, staging)
	}
	if strings.Contains(config, "PRESSFLEET_DISABLE_MAIL") != staging {
		t.Errorf("%s: wp-config.php has the mail switch: %v, want %v", proj.Name, !staging, staging)
	}
}

// mailProbe sends a message with wp_mail() and says whether it got as far as
// PHPMailer. Nothing is listening for it, and nothing needs to: on production
// the site's relay isn't configured, so the message goes no further than
// PHPMailer either way, which is the positive control.
const mailProbe = `$reached = false;
add_action("phpmailer_init", function () use (&$reached) { $reached = true; });
$sent = wp_mail("someone@e2e.test", "pf-e2e", "pf-e2e");
echo json_encode(array("sent" => $sent, "reached_phpmailer" => $reached)), "\n";`

// checkMail checks that wp_mail() on the staging site reports success, never
// reaches PHPMailer, and logs the suppression without the address, and that on
// production it does reach PHPMailer.
func (f *stagingFlow) checkMail(t *testing.T) {
	t.Helper()
	run := func(proj compose.Project) string {
		t.Helper()
		out, err := proj.ExecService(t.Context(), "php", "sh", "-c", `wp eval "$1" 2>&1`, "sh", mailProbe)
		if err != nil {
			t.Fatalf("mail probe on %s: %v: %s", proj.Name, err, out)
		}
		return string(out)
	}
	stag := run(f.stag)
	if !strings.Contains(stag, `"sent":true`) || !strings.Contains(stag, `"reached_phpmailer":false`) {
		t.Errorf("wp_mail() on the staging site:\n%s", stag)
	}
	if !strings.Contains(stag, "suppressed a message to 1 recipient(s)") || strings.Contains(stag, "someone@e2e.test") {
		t.Errorf("the staging site's mail log:\n%s", stag)
	}
	if prod := run(f.prod); !strings.Contains(prod, `"reached_phpmailer":true`) {
		t.Errorf("wp_mail() on production doesn't reach PHPMailer:\n%s", prod)
	}
}

// checkDefaultPlugins checks the default plugins are there and active.
func (f *stagingFlow) checkDefaultPlugins(t *testing.T, proj compose.Project) {
	t.Helper()
	for _, slug := range []string{sites.PluginNginxHelper, sites.PluginRedisCache} {
		if got := siteWP(t, proj, "plugin", "get", slug, "--field=status"); got != "active" {
			t.Errorf("%s: %s is %q", proj.Name, slug, got)
		}
	}
	if got := siteSh(t, proj, "test -e "+wpContent+"/object-cache.php && echo yes"); got != "yes" {
		t.Errorf("%s: no object cache drop-in", proj.Name)
	}
}

// checkNoHost checks the site's database doesn't name a host outside GUIDs,
// which are never rewritten. A dry run counts what still names it.
func (f *stagingFlow) checkNoHost(t *testing.T, proj compose.Project, host string) {
	t.Helper()
	if n := siteWP(t, proj, "search-replace", host, "unused.invalid", "--dry-run",
		"--all-tables-with-prefix", "--skip-columns=guid", "--format=count"); n != "0" {
		out, _ := proj.ExecService(t.Context(), "php", "wp", "search-replace", host, "unused.invalid", "--dry-run",
			"--all-tables-with-prefix", "--skip-columns=guid", "--log", "--no-color")
		t.Errorf("%s: %s values still name %s:\n%.3000s", proj.Name, n, host, out)
	}
}

// checkSettingsKept checks that production kept the settings a push never
// carries.
func (f *stagingFlow) checkSettingsKept(t *testing.T, before client.Site) {
	t.Helper()
	got := getSite(t, f.p, f.prodID)
	if got.PhpVersion != before.PhpVersion || deref(got.PrimaryDomain) != deref(before.PrimaryDomain) ||
		!slices.Equal(got.Aliases, before.Aliases) || got.PreviewProtection.Enabled != before.PreviewProtection.Enabled ||
		got.PreviewDomain != before.PreviewDomain || got.Resources != before.Resources || got.Environment != client.Production ||
		got.Staging != nil {
		t.Errorf("production's settings changed:\nbefore %+v\nafter  %+v", before, got)
	}
}

func (f *stagingFlow) setMaintenance(t *testing.T, enabled bool) {
	t.Helper()
	resp, err := f.p.Client.SetSiteMaintenanceWithResponse(t.Context(), f.prodID, client.SetSiteMaintenanceJSONRequestBody{Enabled: enabled})
	if err != nil || resp.JSON202 == nil {
		t.Fatalf("maintenance %v: %v %s", enabled, err, resp.Body)
	}
	waitJob(t, f.p, resp.JSON202.Job.ID)
}

// waitRunning waits until no job is working on the site.
func (f *stagingFlow) waitRunning(t *testing.T, siteID string) {
	t.Helper()
	deadline := time.Now().Add(jobTimeout)
	for {
		got := getSite(t, f.p, siteID)
		if got.Status == client.SiteStatusRunning {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("site %s is still %s", siteID, got.Status)
		}
		time.Sleep(2 * time.Second)
	}
}

// deployPlugin deploys a one-file plugin to the staging site through the deploy
// API and returns the job.
func (f *stagingFlow) deployPlugin(t *testing.T, name, source string, activate bool) client.Job {
	t.Helper()
	ctx := t.Context()
	data := zipBytes(t, map[string]string{name + "/" + name + ".php": source})
	sum := sha256.Sum256(data)
	sumHex := hex.EncodeToString(sum[:])
	u, err := f.p.Client.CreateUploadWithResponse(ctx, client.CreateUploadJSONRequestBody{
		Purpose: client.UploadPurposeSiteDeploy, SiteID: &f.stagID, SizeBytes: int64(len(data)), Sha256: &sumHex,
	})
	if err != nil || u.JSON201 == nil {
		t.Fatalf("create upload: %v %s", err, u.Body)
	}
	chunk, err := f.p.Client.AppendUploadChunkWithBodyWithResponse(ctx, u.JSON201.ID, &client.AppendUploadChunkParams{Offset: 0},
		"application/octet-stream", bytes.NewReader(data))
	if err != nil || chunk.JSON200 == nil {
		t.Fatalf("send upload: %v %s", err, chunk.Body)
	}
	if done, err := f.p.Client.CompleteUploadWithResponse(ctx, u.JSON201.ID, client.CompleteUploadJSONRequestBody{}); err != nil || done.JSON200 == nil {
		t.Fatalf("complete upload: %v %s", err, done.Body)
	}
	resp, err := f.p.Client.DeploySiteCodeWithResponse(ctx, f.stagID, client.DeploySiteCodeJSONRequestBody{
		UploadID: u.JSON201.ID, Kind: client.Plugin, Name: name, Activate: &activate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON202 == nil {
		t.Fatalf("deploy: status %d: %s", resp.StatusCode(), resp.Body)
	}
	return resp.JSON202.Job
}

// database reads a site's posts and options as root on the platform MariaDB, so
// looking doesn't load WordPress, which could write to them. Left out are
// transients, which are caches (a push without the database may clear
// update_plugins when it reinstalls a default plugin, #204), and what WP-Cron
// writes when it runs: its schedule (cron), and recovery_keys, from its daily
// clean-up. The site's cron job queues on the site's lock key behind a push, so
// it can run between two readings without having anything to do with the push.
func (f *stagingFlow) database(t *testing.T, siteID string) map[string]string {
	t.Helper()
	var proj compose.Project
	if siteID == f.prodID {
		proj = f.prod
	} else {
		proj = f.stag
	}
	prefix := siteWP(t, proj, "db", "prefix")
	query := fmt.Sprintf("SELECT CONCAT('post ', ID), CONCAT_WS(' | ', post_status, post_type, post_title, post_name, post_modified_gmt, MD5(post_content), guid) FROM `pf_%[1]s`.`%[2]sposts`; "+
		"SELECT CONCAT('option ', option_name), CONCAT_WS(' | ', autoload, option_value) FROM `pf_%[1]s`.`%[2]soptions` "+
		"WHERE option_name NOT LIKE '\\_transient\\_%%' AND option_name NOT LIKE '\\_site\\_transient\\_%%' AND option_name NOT IN ('cron', 'recovery_keys');", siteID, prefix)
	out, err := f.p.Project.Exec(t.Context(), "mariadb", "sh", "-c",
		`exec mariadb --user=root --password="$(cat /run/secrets/mariadb_root_password)" --batch --skip-column-names -e "$1"`, "sh", query)
	if err != nil {
		t.Fatalf("reading the database of %s: %v", siteID, err)
	}
	rows := map[string]string{}
	for line := range strings.Lines(string(out)) {
		key, value, _ := strings.Cut(strings.TrimRight(line, "\n"), "\t")
		rows[key] = value
	}
	if len(rows) < 50 {
		t.Fatalf("only %d rows in the database of %s:\n%s", len(rows), siteID, out)
	}
	return rows
}

// checkUnchanged reports every post and option that differs. Each is a finding:
// the code path under test promises to leave them alone.
func (f *stagingFlow) checkUnchanged(t *testing.T, before, after map[string]string) {
	t.Helper()
	keys := slices.Sorted(maps.Keys(before))
	for key := range after {
		if _, ok := before[key]; !ok {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		b, inBefore := before[key]
		a, inAfter := after[key]
		switch {
		case !inAfter:
			t.Errorf("%s is gone, was %.300s", key, b)
		case !inBefore:
			t.Errorf("%s is new: %.300s", key, a)
		case a != b:
			t.Errorf("%s changed:\nbefore %.600s\nafter  %.600s", key, b, a)
		}
	}
}

// checkGone checks nothing is left of a deleted site: containers, network,
// volumes, files, database, object cache user, or record.
func (f *stagingFlow) checkGone(t *testing.T, siteID string, proj compose.Project) {
	t.Helper()
	ctx := t.Context()
	res, err := proj.Resources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Empty() {
		t.Errorf("%s left resources behind: %+v", proj.Name, res)
	}
	for _, service := range []string{"mariadb", "redis", "traefik"} {
		c, err := f.p.Project.ServiceContainer(ctx, service)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := c.Networks[proj.Name]; ok {
			t.Errorf("%s is still on network %s", service, proj.Name)
		}
	}
	if n := siteDatabase(t, f.p, siteID); n != (dbObjects{}) {
		t.Errorf("site %s left in MariaDB: %+v", siteID, n)
	}
	for _, u := range cacheUsers(t, f.p) {
		if strings.Contains(u, siteID) {
			t.Errorf("object cache user %s is left", u)
		}
	}
	dir := filepath.Join(f.p.DataDir, "sites", siteID)
	if out, err := f.p.Project.Exec(ctx, e2e.Service, "sh", "-c", `if test -e "$1"; then echo yes; else echo no; fi`, "sh", dir); err != nil {
		t.Fatal(err)
	} else if strings.TrimSpace(string(out)) != "no" {
		t.Errorf("%s is left", dir)
	}
	got, err := f.p.Client.GetSiteWithResponse(ctx, siteID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StatusCode() != http.StatusNotFound {
		t.Errorf("get deleted site %s: status %d", siteID, got.StatusCode())
	}
}

// stagingManifest returns the embedded version manifest with another registry,
// and two WordPress releases with made-up checksums: the stand-in images aren't
// built from a tarball, and a held site's current release has to stay in it.
func stagingManifest(t *testing.T, registry, current, newer string) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(images.Manifest(), &m); err != nil {
		t.Fatal(err)
	}
	m["registry"] = registry
	release := func(v string) map[string]any { return map[string]any{"version": v, "sha1": strings.Repeat("0", 40)} }
	m["wordpress"] = map[string]any{"latest": release(newer), "pinned": []any{release(current)}}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
