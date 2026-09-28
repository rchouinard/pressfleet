package e2e_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// TestSiteBackup checks on-demand backups (#52) on a real platform, with the
// default local repositories. It provisions a site, puts a file in its
// wp-content and an option in its database, backs it up through the API,
// and checks:
//
//   - the job succeeds and the backup is listed as succeeded, with its
//     snapshot ID, sizes, and the site's WordPress version;
//   - the snapshot, read with plain restic from an export of the client
//     group's repository (`pressfleet repository export`), holds the file
//     under /site/wp-content and the option in /transfer/database.sql, and
//     neither WordPress core nor wp-config.php; its tags and host name
//     follow the documented layout;
//   - the transfer directory holding the dump is gone;
//   - a second backup of the site is listed too, newest first;
//   - with the default retention policy (the last 3 snapshots, and one a
//     day, which keeps the oldest too while there are fewer days than
//     keep_daily), a prune after three more backups removes the second
//     snapshot and marks its backup expired, a check of the pruned
//     repository passes, and the site can still be backed up into it (#54).
func TestSiteBackup(t *testing.T) {
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

	groupID := createGroup(t, p, "E2E Backup Agency")
	created := createSite(t, p, groupID, "E2E Backup", php)
	site := created.Site
	siteProject = compose.Project{Name: compose.SiteProjectName(site.ID), Dir: projectDir}
	waitJob(t, p, created.Job.ID)

	const marker = "pf-e2e-backup-marker-7f3a"
	if out, err := siteProject.ExecService(ctx, "php", "sh", "-c",
		"mkdir -p /var/www/wordpress/wp-content/uploads/pf && echo "+marker+" > /var/www/wordpress/wp-content/uploads/pf/marker.txt"); err != nil {
		t.Fatalf("writing to wp-content: %v: %s", err, out)
	}
	if out, err := siteProject.ExecService(ctx, "php", "wp", "option", "update", "pf_backup_marker", marker); err != nil {
		t.Fatalf("wp option update: %v: %s", err, out)
	}

	backup := func(t *testing.T) client.Job {
		t.Helper()
		resp, err := p.Client.CreateSiteBackupWithResponse(ctx, site.ID)
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil {
			t.Fatalf("back up: status %d: %s", resp.StatusCode(), resp.Body)
		}
		start := time.Now()
		waitJob(t, p, resp.JSON202.Job.ID)
		t.Logf("backup job %s finished in %s", resp.JSON202.Job.ID, time.Since(start).Round(time.Millisecond))
		return resp.JSON202.Job
	}
	list := func(t *testing.T) []client.Backup {
		t.Helper()
		resp, err := p.Client.ListSiteBackupsWithResponse(ctx, site.ID, &client.ListSiteBackupsParams{})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON200 == nil {
			t.Fatalf("list backups: status %d: %s", resp.StatusCode(), resp.Body)
		}
		return resp.JSON200.Items
	}

	job := backup(t)
	backups := list(t)
	if len(backups) != 1 {
		t.Fatalf("backups = %+v", backups)
	}
	b := backups[0]
	if b.Status != client.BackupStatusSucceeded || b.Kind != client.Manual || b.SnapshotID == nil || len(*b.SnapshotID) != 64 ||
		b.WpCoreVersion != site.WpCoreVersion || b.SizeBytes == nil || *b.SizeBytes == 0 || b.AddedBytes == nil ||
		b.FileCount == nil || *b.FileCount == 0 || b.JobID == nil || *b.JobID != job.ID || b.Message != "" {
		t.Fatalf("backup = %+v", b)
	}
	snapshot := *b.SnapshotID
	t.Logf("snapshot %s: %d files, %d bytes, %d bytes added", snapshot[:8], *b.FileCount, *b.SizeBytes, *b.AddedBytes)

	t.Run("transfer directory removed", func(t *testing.T) {
		out, err := p.Project.Exec(ctx, e2e.Service, "find", filepath.Join(p.DataDir, "tmp"), "-mindepth", "1", "-name", "backup-*")
		if err != nil {
			t.Fatal(err)
		}
		if left := strings.TrimSpace(string(out)); left != "" {
			t.Errorf("left in tmp/: %s", left)
		}
	})

	t.Run("snapshot contents", func(t *testing.T) {
		// Read the repository as a client would after offboarding: an
		// export, opened with plain restic and the password file.
		export := filepath.Join(p.DataDir, "tmp", "pf-e2e-export")
		if out, err := p.Project.Exec(ctx, e2e.Service, "pressfleet", "repository", "export", groupID, export); err != nil {
			t.Fatalf("repository export: %v: %s", err, out)
		}
		restic := func(args ...string) string {
			t.Helper()
			out, err := p.Project.Exec(ctx, e2e.Service, append([]string{"restic", "--no-cache",
				"-r", filepath.Join(export, repository.ExportRepositoryDir),
				"--password-file", filepath.Join(export, repository.ExportPasswordFile)}, args...)...)
			if err != nil {
				t.Fatalf("restic %s: %v: %s", args[0], err, out)
			}
			return string(out)
		}

		var snaps []struct {
			ID       string   `json:"id"`
			Hostname string   `json:"hostname"`
			Paths    []string `json:"paths"`
			Tags     []string `json:"tags"`
		}
		if err := json.Unmarshal([]byte(restic("snapshots", "--json", "--tag", "site:"+site.ID)), &snaps); err != nil {
			t.Fatal(err)
		}
		if len(snaps) != 1 || snaps[0].ID != snapshot {
			t.Fatalf("snapshots = %+v", snaps)
		}
		s := snaps[0]
		wantTags := []string{"site:" + site.ID, "wp-core:" + site.WpCoreVersion, "kind:manual", "backup:" + b.ID}
		slices.Sort(wantTags)
		slices.Sort(s.Tags)
		if s.Hostname != "site-"+site.ID || !slices.Equal(s.Paths, []string{"/site/wp-content", "/transfer/database.sql"}) ||
			!slices.Equal(s.Tags, wantTags) {
			t.Errorf("snapshot = %+v, want tags %q", s, wantTags)
		}

		var files []string
		for line := range strings.Lines(restic("ls", "--json", snapshot)) {
			var n struct {
				Type string `json:"type"`
				Path string `json:"path"`
			}
			if err := json.Unmarshal([]byte(line), &n); err != nil {
				t.Fatalf("restic ls line %q: %v", line, err)
			}
			if n.Type == "file" {
				files = append(files, n.Path)
			}
		}
		t.Logf("%d files in the snapshot", len(files))
		for _, f := range files {
			if !strings.HasPrefix(f, "/site/wp-content/") && f != "/transfer/database.sql" {
				t.Errorf("snapshot holds %s", f)
			}
			if base := filepath.Base(f); base == "wp-config.php" || strings.Contains(f, "/wp-includes/") || strings.Contains(f, "/wp-admin/") {
				t.Errorf("snapshot holds WordPress core or its configuration: %s", f)
			}
		}
		for _, want := range []string{"/site/wp-content/uploads/pf/marker.txt", "/transfer/database.sql"} {
			if !slices.Contains(files, want) {
				t.Errorf("snapshot lacks %s", want)
			}
		}
		// The default plugins are in wp-content.
		if !slices.ContainsFunc(files, func(f string) bool { return strings.HasPrefix(f, "/site/wp-content/plugins/") }) {
			t.Error("snapshot has no plugins")
		}
		if got := restic("dump", snapshot, "/site/wp-content/uploads/pf/marker.txt"); got != marker+"\n" {
			t.Errorf("marker file = %q", got)
		}
		dump := restic("dump", snapshot, "/transfer/database.sql")
		if !strings.Contains(dump, "'pf_backup_marker','"+marker+"'") || !strings.Contains(dump, "CREATE TABLE") ||
			strings.Contains(dump, "CREATE DATABASE") || strings.Contains(dump, "DEFINER=") {
			t.Errorf("database dump (%d bytes) lacks the option or has statements that tie it to a database:\n%.2000s", len(dump), dump)
		}
	})

	t.Run("second backup", func(t *testing.T) {
		backup(t)
		backups := list(t)
		if len(backups) != 2 || backups[1].ID != b.ID || backups[0].Status != client.BackupStatusSucceeded ||
			backups[0].SnapshotID == nil || *backups[0].SnapshotID == snapshot {
			t.Errorf("backups = %+v", backups)
		}
		// Unchanged files deduplicate: the second backup adds little.
		if backups[0].AddedBytes != nil && *backups[0].AddedBytes >= *b.AddedBytes {
			t.Errorf("second backup added %d bytes, the first %d", *backups[0].AddedBytes, *b.AddedBytes)
		}
	})

	t.Run("retention and check", func(t *testing.T) {
		// The control plane process prunes and checks a repository that
		// the sites' backup tasks, each with its own UID, wrote to.
		maintain := func(t *testing.T, command string) {
			t.Helper()
			out, err := p.Project.Exec(ctx, e2e.Service, "pressfleet", "repository", command, groupID)
			if err != nil {
				t.Fatalf("repository %s: %v: %s", command, err, out)
			}
			m := regexp.MustCompile(`job (\S+) for`).FindStringSubmatch(string(out))
			if m == nil {
				t.Fatalf("repository %s printed no job: %s", command, out)
			}
			start := time.Now()
			waitJob(t, p, m[1])
			job := finalJob(t, p, m[1])
			t.Logf("%s job %s finished in %s: %v", command, m[1], time.Since(start).Round(time.Millisecond), job.Result)
		}
		for range 3 {
			backup(t)
		}
		maintain(t, "prune")
		backups := list(t)
		if len(backups) != 5 {
			t.Fatalf("backups = %+v", backups)
		}
		for i, bk := range backups {
			// Newest first: the second backup is next to last.
			if expired := bk.ExpiredAt != nil; expired != (i == 3) {
				t.Errorf("backup %d (%s) expired at %v", i, bk.ID, bk.ExpiredAt)
			}
		}
		maintain(t, "check")
		// New files the prune wrote don't keep the next backup task out.
		backup(t)
	})
}
