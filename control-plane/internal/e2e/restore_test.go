package e2e_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// TestSiteRestore checks restores (#55) on a real platform with the local
// backup backend. It runs restoreChecks on one site: a restore brings back
// the backed-up files and database, and a restore whose WordPress can't
// load rolls back. In between, a snapshot of the site is restored into
// another site of the same client group, whose URLs must be rewritten to
// its own. TestSiteBackupS3 runs restoreChecks with the s3 backend.
func TestSiteRestore(t *testing.T) {
	projectDir := t.TempDir()
	var projects []compose.Project
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
	})
	p := e2e.Start(t)

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

	groupID := createGroup(t, p, "E2E Restore Agency")
	newSite := func(name string) (client.Site, compose.Project) {
		t.Helper()
		created := createSite(t, p, groupID, name, php)
		proj := compose.Project{Name: compose.SiteProjectName(created.Site.ID), Dir: projectDir}
		projects = append(projects, proj)
		waitJob(t, p, created.Job.ID)
		return getSite(t, p, created.Site.ID), proj
	}
	site, proj := newSite("E2E Restore")
	rc := restoreChecks{p: p, site: site, proj: proj}
	snapshot := rc.setUp(t)

	t.Run("same site", func(t *testing.T) { rc.sameSite(t, snapshot) })

	t.Run("into another site", func(t *testing.T) {
		other, otherProj := newSite("E2E Restore Copy")
		job := restoreSite(t, p, other.ID, snapshot, site.ID)
		waitJob(t, p, job.ID)
		want := "https://" + other.PreviewDomain
		for _, option := range []string{"home", "siteurl"} {
			if got := siteWP(t, otherProj, "option", "get", option); got != want {
				t.Errorf("%s = %q, want %q", option, got, want)
			}
		}
		if got := siteWP(t, otherProj, "option", "get", "pf_restore_marker"); got != "before" {
			t.Errorf("pf_restore_marker = %q", got)
		}
		if got := siteSh(t, otherProj, "cat "+wpContent+"/uploads/pf/keep.txt"); got != restoreKeep {
			t.Errorf("keep.txt = %q", got)
		}
		if owners := siteSh(t, otherProj, "find "+wpContent+" -mindepth 1 ! -user \"$(id -u)\" | head -5"); owners != "" {
			t.Errorf("files not owned by the target site's user: %s", owners)
		}
		// A dry run counts what still names the source's domain.
		if n := siteWP(t, otherProj, "search-replace", "//"+site.PreviewDomain, "//unused.invalid", "--dry-run",
			"--all-tables-with-prefix", "--skip-columns=guid", "--format=count"); n != "0" {
			t.Errorf("%s values still name the source's domain", n)
		}
	})

	t.Run("rollback", rc.rollback)
}

// wpContent is wp-content in a site's php container.
const wpContent = "/var/www/wordpress/wp-content"

// restoreKeep is the content of the file restoreChecks.setUp backs up.
const restoreKeep = "pf-e2e-restore-keep-2b1c"

// restoreChecks are the restore checks of the M3 acceptance criteria (#63).
// TestSiteRestore and TestSiteBackupS3 run them on a site each, so they
// hold with both backup backends: the backup, the pre-restore backup, and
// the rollback's restore all go through the backend under test.
type restoreChecks struct {
	p    *e2e.Platform
	site client.Site
	proj e2e.SiteExec
}

// setUp gives the site the state to come back to, a file in wp-content and
// an option, backs it up, and returns the snapshot.
func (rc restoreChecks) setUp(t *testing.T) string {
	t.Helper()
	siteSh(t, rc.proj, "mkdir -p "+wpContent+"/uploads/pf && echo "+restoreKeep+" > "+wpContent+"/uploads/pf/keep.txt")
	siteWP(t, rc.proj, "option", "update", "pf_restore_marker", "before")
	snapshot := backupSite(t, rc.p, rc.site.ID)
	t.Logf("snapshot %s", snapshot[:8])
	return snapshot
}

// sameSite changes the site's files and database after the setUp backup
// (a new post, a new file, the backed-up file deleted, the option changed)
// and restores the snapshot, which must bring back exactly the earlier
// state, with the restored files owned by the site's user, the site out of
// maintenance mode, a pre-restore backup listed, and nothing left in tmp/.
func (rc restoreChecks) sameSite(t *testing.T, snapshot string) {
	ctx := t.Context()
	p, proj := rc.p, rc.proj
	postID := siteWP(t, proj, "post", "create", "--post_title=PF E2E after backup", "--post_status=publish", "--porcelain")
	siteSh(t, proj, "echo new > "+wpContent+"/uploads/pf/new.txt && rm "+wpContent+"/uploads/pf/keep.txt")
	siteWP(t, proj, "option", "update", "pf_restore_marker", "after")

	start := time.Now()
	job := restoreSite(t, p, rc.site.ID, snapshot, "")
	waitJob(t, p, job.ID)
	t.Logf("restore job %s finished in %s", job.ID, time.Since(start).Round(time.Millisecond))

	if out, err := proj.ExecService(ctx, "php", "wp", "post", "get", postID, "--field=ID"); err == nil {
		t.Errorf("post %s created after the backup still exists: %s", postID, out)
	}
	if got := siteWP(t, proj, "option", "get", "pf_restore_marker"); got != "before" {
		t.Errorf("pf_restore_marker = %q, want before", got)
	}
	if files := siteSh(t, proj, "ls "+wpContent+"/uploads/pf"); files != "keep.txt" {
		t.Errorf("uploads/pf holds %q, want keep.txt alone", files)
	}
	if got := siteSh(t, proj, "cat "+wpContent+"/uploads/pf/keep.txt"); got != restoreKeep {
		t.Errorf("keep.txt = %q", got)
	}
	// The restored files belong to the site's user, like the rest (the
	// volume's root, which the restore leaves alone, is the image's).
	if owners := siteSh(t, proj, "find "+wpContent+" -mindepth 1 ! -user \"$(id -u)\" | head -5"); owners != "" {
		t.Errorf("files not owned by the site's user: %s", owners)
	}
	got := getSite(t, p, rc.site.ID)
	if got.Status != client.SiteStatusRunning || got.Maintenance || (got.StatusMessage != nil && *got.StatusMessage != "") {
		t.Errorf("site after the restore: %s, maintenance %v, message %v", got.Status, got.Maintenance, got.StatusMessage)
	}
	list, err := p.Client.ListSiteBackupsWithResponse(ctx, rc.site.ID, &client.ListSiteBackupsParams{})
	if err != nil || list.JSON200 == nil {
		t.Fatal(err)
	}
	if b := list.JSON200.Items[0]; b.Kind != client.PreRestore || b.Status != client.BackupStatusSucceeded {
		t.Errorf("newest backup = %+v, want the pre-restore backup", b)
	}
	out, err := p.ExecControlPlane(ctx, nil, "find", p.DataDir+"/tmp", "-mindepth", "1", "-name", "restore-*")
	if err != nil {
		t.Fatal(err)
	}
	if left := strings.TrimSpace(string(out)); left != "" {
		t.Errorf("left in tmp/: %s", left)
	}
}

// rollback backs up the site with an active plugin that throws, so the
// restored WordPress can't load (the backup itself doesn't run WordPress),
// makes the site healthy again with a new post, and restores the broken
// snapshot. The job must fail after the data was replaced and roll the
// site back to its pre-restore backup: the post is there, the plugin is
// harmless, and the site is running, in maintenance mode, with a status
// message saying so.
func (rc restoreChecks) rollback(t *testing.T) {
	p, proj := rc.p, rc.proj
	const plugin = wpContent + "/plugins/pf-e2e-boom/pf-e2e-boom.php"
	siteSh(t, proj, "mkdir -p "+wpContent+"/plugins/pf-e2e-boom && printf '%s\\n' '<?php' '/* Plugin Name: PF E2E Boom */' > "+plugin)
	siteWP(t, proj, "plugin", "activate", "pf-e2e-boom")
	siteSh(t, proj, "printf '%s\\n' 'throw new Error(\"pf-e2e boom\");' >> "+plugin)
	broken := backupSite(t, p, rc.site.ID)
	siteSh(t, proj, "printf '%s\\n' '<?php' '/* Plugin Name: PF E2E Boom */' > "+plugin)
	postID := siteWP(t, proj, "post", "create", "--post_title=PF E2E survives", "--post_status=publish", "--porcelain")

	job := restoreSite(t, p, rc.site.ID, broken, "")
	final := finalJob(t, p, job.ID)
	if final.State != client.JobStateFailed {
		t.Fatalf("restore job %s: %s %v", job.ID, final.State, final.Error)
	}
	t.Logf("the restore failed as intended: %s", deref(final.Error))
	if got := siteWP(t, proj, "post", "get", postID, "--field=post_title"); got != "PF E2E survives" {
		t.Errorf("post after the rollback = %q", got)
	}
	if got := siteSh(t, proj, "cat "+plugin); strings.Contains(got, "throw") {
		t.Errorf("the broken plugin is back after the rollback:\n%s", got)
	}
	got := getSite(t, p, rc.site.ID)
	if got.Status != client.SiteStatusRunning || !got.Maintenance || got.StatusMessage == nil ||
		!strings.Contains(*got.StatusMessage, "rolled back to its pre-restore backup") {
		t.Errorf("site after the rollback: %s, maintenance %v, message %q", got.Status, got.Maintenance, deref(got.StatusMessage))
	}
}

// siteSh runs a shell script in the site's php container and returns its
// trimmed output.
func siteSh(t *testing.T, proj e2e.SiteExec, script string) string {
	t.Helper()
	out, err := proj.ExecService(t.Context(), "php", "sh", "-c", script)
	if err != nil {
		t.Fatalf("sh -c %q: %v: %s", script, err, out)
	}
	return strings.TrimSpace(string(out))
}

// siteWP runs WP-CLI in the site's php container and returns its trimmed
// output.
func siteWP(t *testing.T, proj e2e.SiteExec, args ...string) string {
	t.Helper()
	out, err := proj.ExecService(t.Context(), "php", append([]string{"wp"}, args...)...)
	if err != nil {
		t.Fatalf("wp %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// backupSite backs up a site through the API and returns the new
// snapshot's ID.
func backupSite(t *testing.T, p *e2e.Platform, siteID string) string {
	t.Helper()
	ctx := t.Context()
	resp, err := p.Client.CreateSiteBackupWithResponse(ctx, siteID)
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON202 == nil {
		t.Fatalf("back up: status %d: %s", resp.StatusCode(), resp.Body)
	}
	waitJob(t, p, resp.JSON202.Job.ID)
	list, err := p.Client.ListSiteBackupsWithResponse(ctx, siteID, &client.ListSiteBackupsParams{})
	if err != nil || list.JSON200 == nil || len(list.JSON200.Items) == 0 {
		t.Fatalf("list backups: %v %s", err, list.Body)
	}
	b := list.JSON200.Items[0]
	if b.Status != client.BackupStatusSucceeded || b.SnapshotID == nil {
		t.Fatalf("backup = %+v", b)
	}
	return *b.SnapshotID
}

// restoreSite asks for a restore of snapshot into the site, from the
// source site's repository if source isn't empty, and returns its job.
func restoreSite(t *testing.T, p *e2e.Platform, siteID, snapshot, source string) client.Job {
	t.Helper()
	body := client.RestoreSiteJSONRequestBody{SnapshotID: snapshot}
	if source != "" {
		body.SourceSiteID = &source
	}
	resp, err := p.Client.RestoreSiteWithResponse(t.Context(), siteID, body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.JSON202 == nil {
		t.Fatalf("restore: status %d: %s", resp.StatusCode(), resp.Body)
	}
	if resp.JSON202.Site.Status != client.SiteStatusUpdating {
		t.Errorf("site status after the restore request = %s", resp.JSON202.Site.Status)
	}
	return resp.JSON202.Job
}

// finalJob polls a job until it reaches a final state and returns it.
func finalJob(t *testing.T, p *e2e.Platform, jobID string) client.Job {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), jobTimeout)
	defer cancel()
	for {
		resp, err := p.Client.GetJobWithResponse(ctx, jobID)
		if err != nil {
			t.Fatalf("job %s: %v", jobID, err)
		}
		if resp.JSON200 == nil {
			t.Fatalf("job %s: status %d: %s", jobID, resp.StatusCode(), resp.Body)
		}
		switch resp.JSON200.State {
		case client.JobStateSucceeded, client.JobStateFailed, client.JobStateRollbackFailed:
			return *resp.JSON200
		}
		select {
		case <-ctx.Done():
			t.Fatalf("job %s still %s: %s", jobID, resp.JSON200.State, resp.Body)
		case <-time.After(2 * time.Second):
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
