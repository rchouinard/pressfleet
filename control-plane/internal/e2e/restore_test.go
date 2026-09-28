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

// TestSiteRestore checks restores (#55) on a real platform: a site is
// backed up, changed (a new post, a new file in wp-content, a deleted one,
// a changed option), and restored through the API, which must bring back
// the earlier state exactly, with the restored files owned by the site's
// user and the site out of maintenance mode. A snapshot of the site is then
// restored into another site of the same client group, whose URLs must be
// rewritten to its own. Finally, a snapshot whose restored WordPress can't
// load (an active plugin that throws) makes the job fail after the data
// was replaced: the job must roll the site back to its pre-restore backup
// and leave it in maintenance mode.
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

	const content = "/var/www/wordpress/wp-content"
	sh := func(t *testing.T, proj compose.Project, script string) string {
		t.Helper()
		out, err := proj.ExecService(ctx, "php", "sh", "-c", script)
		if err != nil {
			t.Fatalf("sh -c %q: %v: %s", script, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	wp := func(t *testing.T, proj compose.Project, args ...string) string {
		t.Helper()
		out, err := proj.ExecService(ctx, "php", append([]string{"wp"}, args...)...)
		if err != nil {
			t.Fatalf("wp %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	backup := func(t *testing.T, siteID string) string {
		t.Helper()
		resp, err := p.Client.CreateSiteBackupWithResponse(ctx, siteID)
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil {
			t.Fatalf("back up: status %d: %s", resp.StatusCode(), resp.Body)
		}
		waitJob(t, p, resp.JSON202.Job.ID)
		list, err := p.Client.ListSiteBackupsWithResponse(ctx, siteID, &client.ListSiteBackupsParams{})
		if err != nil || list.JSON200 == nil || len(list.JSON200.Items) == 0 || list.JSON200.Items[0].SnapshotID == nil {
			t.Fatalf("list backups: %v %s", err, list.Body)
		}
		return *list.JSON200.Items[0].SnapshotID
	}
	restore := func(t *testing.T, siteID, snapshot, source string) client.Job {
		t.Helper()
		body := client.RestoreSiteJSONRequestBody{SnapshotID: snapshot}
		if source != "" {
			body.SourceSiteID = &source
		}
		resp, err := p.Client.RestoreSiteWithResponse(ctx, siteID, body)
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

	// The state to come back to.
	const keep = "pf-e2e-restore-keep-2b1c"
	sh(t, proj, "mkdir -p "+content+"/uploads/pf && echo "+keep+" > "+content+"/uploads/pf/keep.txt")
	wp(t, proj, "option", "update", "pf_restore_marker", "before")
	snapshot := backup(t, site.ID)
	t.Logf("snapshot %s", snapshot[:8])

	t.Run("same site", func(t *testing.T) {
		// Changes after the backup.
		postID := wp(t, proj, "post", "create", "--post_title=PF E2E after backup", "--post_status=publish", "--porcelain")
		sh(t, proj, "echo new > "+content+"/uploads/pf/new.txt && rm "+content+"/uploads/pf/keep.txt")
		wp(t, proj, "option", "update", "pf_restore_marker", "after")

		start := time.Now()
		job := restore(t, site.ID, snapshot, "")
		waitJob(t, p, job.ID)
		t.Logf("restore job %s finished in %s", job.ID, time.Since(start).Round(time.Millisecond))

		if out, err := proj.ExecService(ctx, "php", "wp", "post", "get", postID, "--field=ID"); err == nil {
			t.Errorf("post %s created after the backup still exists: %s", postID, out)
		}
		if got := wp(t, proj, "option", "get", "pf_restore_marker"); got != "before" {
			t.Errorf("pf_restore_marker = %q, want before", got)
		}
		files := sh(t, proj, "ls "+content+"/uploads/pf")
		if files != "keep.txt" {
			t.Errorf("uploads/pf holds %q, want keep.txt alone", files)
		}
		if got := sh(t, proj, "cat "+content+"/uploads/pf/keep.txt"); got != keep {
			t.Errorf("keep.txt = %q", got)
		}
		// The restored files belong to the site's user, like the rest (the
		// volume's root, which the restore leaves alone, is the image's).
		if owners := sh(t, proj, "find "+content+" -mindepth 1 ! -user \"$(id -u)\" | head -5"); owners != "" {
			t.Errorf("files not owned by the site's user: %s", owners)
		}
		got := getSite(t, p, site.ID)
		if got.Status != client.SiteStatusRunning || got.Maintenance || (got.StatusMessage != nil && *got.StatusMessage != "") {
			t.Errorf("site after the restore: %s, maintenance %v, message %v", got.Status, got.Maintenance, got.StatusMessage)
		}
		list, err := p.Client.ListSiteBackupsWithResponse(ctx, site.ID, &client.ListSiteBackupsParams{})
		if err != nil || list.JSON200 == nil {
			t.Fatal(err)
		}
		if b := list.JSON200.Items[0]; b.Kind != client.PreRestore || b.Status != client.BackupStatusSucceeded {
			t.Errorf("newest backup = %+v, want the pre-restore backup", b)
		}
		out, err := p.Project.Exec(ctx, e2e.Service, "find", p.DataDir+"/tmp", "-mindepth", "1", "-name", "restore-*")
		if err != nil {
			t.Fatal(err)
		}
		if left := strings.TrimSpace(string(out)); left != "" {
			t.Errorf("left in tmp/: %s", left)
		}
	})

	t.Run("into another site", func(t *testing.T) {
		other, otherProj := newSite("E2E Restore Copy")
		job := restore(t, other.ID, snapshot, site.ID)
		waitJob(t, p, job.ID)
		want := "https://" + other.PreviewDomain
		for _, option := range []string{"home", "siteurl"} {
			if got := wp(t, otherProj, "option", "get", option); got != want {
				t.Errorf("%s = %q, want %q", option, got, want)
			}
		}
		if got := wp(t, otherProj, "option", "get", "pf_restore_marker"); got != "before" {
			t.Errorf("pf_restore_marker = %q", got)
		}
		if got := sh(t, otherProj, "cat "+content+"/uploads/pf/keep.txt"); got != keep {
			t.Errorf("keep.txt = %q", got)
		}
		if owners := sh(t, otherProj, "find "+content+" -mindepth 1 ! -user \"$(id -u)\" | head -5"); owners != "" {
			t.Errorf("files not owned by the target site's user: %s", owners)
		}
		// A dry run counts what still names the source's domain.
		if n := wp(t, otherProj, "search-replace", "//"+site.PreviewDomain, "//unused.invalid", "--dry-run",
			"--all-tables-with-prefix", "--skip-columns=guid", "--format=count"); n != "0" {
			t.Errorf("%s values still name the source's domain", n)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		// A snapshot whose WordPress can't load: an active plugin that
		// throws. The backup itself doesn't run WordPress.
		const plugin = content + "/plugins/pf-e2e-boom/pf-e2e-boom.php"
		sh(t, proj, "mkdir -p "+content+"/plugins/pf-e2e-boom && printf '%s\\n' '<?php' '/* Plugin Name: PF E2E Boom */' > "+plugin)
		wp(t, proj, "plugin", "activate", "pf-e2e-boom")
		sh(t, proj, "printf '%s\\n' 'throw new Error(\"pf-e2e boom\");' >> "+plugin)
		broken := backup(t, site.ID)
		// Healthy again, with a post that must survive the rollback.
		sh(t, proj, "printf '%s\\n' '<?php' '/* Plugin Name: PF E2E Boom */' > "+plugin)
		postID := wp(t, proj, "post", "create", "--post_title=PF E2E survives", "--post_status=publish", "--porcelain")

		job := restore(t, site.ID, broken, "")
		final := finalJob(t, p, job.ID)
		if final.State != client.JobStateFailed {
			t.Fatalf("restore job %s: %s %v", job.ID, final.State, final.Error)
		}
		t.Logf("the restore failed as intended: %s", deref(final.Error))
		if got := wp(t, proj, "post", "get", postID, "--field=post_title"); got != "PF E2E survives" {
			t.Errorf("post after the rollback = %q", got)
		}
		if got := sh(t, proj, "cat "+plugin); strings.Contains(got, "throw") {
			t.Errorf("the broken plugin is back after the rollback:\n%s", got)
		}
		got := getSite(t, p, site.ID)
		if got.Status != client.SiteStatusRunning || !got.Maintenance || got.StatusMessage == nil ||
			!strings.Contains(*got.StatusMessage, "rolled back to its pre-restore backup") {
			t.Errorf("site after the rollback: %s, maintenance %v, message %q", got.Status, got.Maintenance, deref(got.StatusMessage))
		}
	})
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
