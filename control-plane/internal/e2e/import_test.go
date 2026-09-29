package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// TestSiteImport checks site import (#112) on a real platform. The source
// is a stock WordPress install exported the way an agency exports a site
// from another host: a site is provisioned and given a post with an image,
// a second user, a must-use plugin of another host (Kinsta), and that
// host's page cache drop-in (advanced-cache.php, which would stop every
// request); its URLs are moved to http://old-host.e2e.test and its tables
// to the prefix pf7_, a view with a DEFINER is added, and its wp-content is
// archived with tar and its database dumped with mariadb-dump --databases
// (so the dump has CREATE DATABASE and USE). Then:
//
//  1. importing the archive and dump creates a site that serves the post
//     and the image on its preview domain over HTTPS, with its users, its
//     table prefix, the platform's object cache drop-in instead of the old
//     host's drop-ins, a warning about the Kinsta must-use plugin, and
//     every URL rewritten; the site can go live;
//  2. a multisite dump, an unreadable dump, and a dump from a WordPress
//     newer than the newest site image are rejected by the job before a
//     site is created.
func TestSiteImport(t *testing.T) {
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
	groupID := createGroup(t, p, "E2E Import Agency")

	project := func(siteID string) compose.Project {
		proj := compose.Project{Name: compose.SiteProjectName(siteID), Dir: projectDir}
		projects = append(projects, proj)
		return proj
	}
	run := func(t *testing.T, proj compose.Project, command ...string) string {
		t.Helper()
		out, err := proj.ExecService(ctx, "php", command...)
		if err != nil {
			t.Fatalf("%s: %v: %s", strings.Join(command, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	wp := func(t *testing.T, proj compose.Project, args ...string) string {
		t.Helper()
		return run(t, proj, append([]string{"wp"}, args...)...)
	}
	rootSQL := func(t *testing.T, args ...string) []byte {
		t.Helper()
		out, err := p.Project.Exec(ctx, "mariadb", append([]string{"sh", "-c",
			`p="$(cat /run/secrets/mariadb_root_password)"; prog="$1"; shift; exec "$prog" --user=root --password="$p" "$@"`, "sh"}, args...)...)
		if err != nil {
			t.Fatalf("%s: %v", args[0], err)
		}
		return out
	}

	// The source site and its export.
	created := createSite(t, p, groupID, "E2E Import Source", php)
	source := created.Site
	srcProj := project(source.ID)
	waitJob(t, p, created.Job.ID)
	const content = "/var/www/wordpress/wp-content"
	run(t, srcProj, "php", "-r", `$i = imagecreatetruecolor(16, 16); imagefill($i, 0, 0, 0x3366cc); imagepng($i, "/tmp/pf-import.png");`)
	media := wp(t, srcProj, "media", "import", "/tmp/pf-import.png", "--porcelain")
	imageURL := wp(t, srcProj, "post", "get", media, "--field=guid")
	const title = "PF E2E imported post 7c41"
	postID := wp(t, srcProj, "post", "create", "--post_status=publish", "--post_title="+title,
		`--post_content=<p>Imported content.</p><img src="`+imageURL+`" />`, "--porcelain")
	wp(t, srcProj, "user", "create", "pf-editor", "editor@e2e.test", "--role=editor")
	run(t, srcProj, "sh", "-c", "mkdir -p "+content+"/mu-plugins && echo '<?php // Kinsta' > "+content+"/mu-plugins/kinsta-mu-plugins.php"+
		" && echo \"<?php die( 'the old host page cache' );\" > "+content+"/advanced-cache.php")
	const oldHost = "old-host.e2e.test"
	wp(t, srcProj, "search-replace", "https://"+source.PreviewDomain, "http://"+oldHost, "--all-tables", "--skip-columns=guid")
	wp(t, srcProj, "search-replace", "https://"+source.PreviewDomain, "http://"+oldHost, "--all-tables", "--include-columns=guid")
	archive, err := srcProj.ExecService(ctx, "php", "tar", "-czf", "-", "-C", "/var/www/wordpress", "wp-content")
	if err != nil {
		t.Fatalf("archiving wp-content: %v", err)
	}
	// Another table prefix, the way WordPress documents changing it.
	srcDB := "pf_" + source.ID
	var renames []string
	for _, tbl := range strings.Fields(string(rootSQL(t, "mariadb", "--batch", "--skip-column-names", "-e",
		"SELECT table_name FROM information_schema.tables WHERE table_schema = '"+srcDB+"' AND table_name LIKE 'wp\\_%'"))) {
		renames = append(renames, fmt.Sprintf("`%s` TO `pf7_%s`", tbl, strings.TrimPrefix(tbl, "wp_")))
	}
	rootSQL(t, "mariadb", srcDB, "-e", "RENAME TABLE "+strings.Join(renames, ", ")+";"+
		"UPDATE pf7_options SET option_name = 'pf7_user_roles' WHERE option_name = 'wp_user_roles';"+
		"UPDATE pf7_usermeta SET meta_key = CONCAT('pf7_', SUBSTRING(meta_key, 4)) WHERE meta_key LIKE 'wp\\_%';"+
		"CREATE DEFINER = 'root'@'localhost' VIEW pf7_e2e_titles AS SELECT post_title FROM pf7_posts;")
	dump := rootSQL(t, "mariadb-dump", "--single-transaction", "--hex-blob", "--databases", srcDB)
	if !bytes.Contains(dump, []byte("DEFINER=`root`@`localhost`")) || !bytes.Contains(dump, []byte("USE `"+srcDB+"`")) {
		t.Fatalf("the source dump lacks the DEFINER and USE the import must remove:\n%.2000s", dump)
	}
	t.Logf("source: %d bytes of wp-content archive, %d bytes of dump", len(archive), len(dump))

	upload := func(t *testing.T, data []byte) string {
		t.Helper()
		sum := sha256.Sum256(data)
		sumHex := hex.EncodeToString(sum[:])
		resp, err := p.Client.CreateUploadWithResponse(ctx, client.CreateUploadJSONRequestBody{
			Purpose: client.UploadPurposeSiteImport, ClientGroupID: &groupID, SizeBytes: int64(len(data)), Sha256: &sumHex,
		})
		if err != nil || resp.JSON201 == nil {
			t.Fatalf("create upload: %v %s", err, resp.Body)
		}
		u := resp.JSON201
		for off := 0; off < len(data); off += int(u.MaxChunkBytes) {
			end := min(off+int(u.MaxChunkBytes), len(data))
			c, err := p.Client.AppendUploadChunkWithBodyWithResponse(ctx, u.ID, &client.AppendUploadChunkParams{Offset: int64(off)},
				"application/octet-stream", bytes.NewReader(data[off:end]))
			if err != nil || c.JSON200 == nil {
				t.Fatalf("chunk at %d: %v %s", off, err, c.Body)
			}
		}
		done, err := p.Client.CompleteUploadWithResponse(ctx, u.ID, client.CompleteUploadJSONRequestBody{})
		if err != nil || done.JSON200 == nil {
			t.Fatalf("complete upload: %v %s", err, done.Body)
		}
		return u.ID
	}
	importSite := func(t *testing.T, archive, dump []byte) client.SiteImportAccepted {
		t.Helper()
		resp, err := p.Client.ImportSiteWithResponse(ctx, client.ImportSiteJSONRequestBody{
			ClientGroupID: groupID, ArchiveUploadID: upload(t, archive), DumpUploadID: upload(t, dump), PhpVersion: &php,
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil {
			t.Fatalf("import: status %d: %s", resp.StatusCode(), resp.Body)
		}
		project(resp.JSON202.SiteID)
		return *resp.JSON202
	}
	groupSites := func(t *testing.T) []string {
		t.Helper()
		resp, err := p.Client.ListSitesWithResponse(ctx, &client.ListSitesParams{ClientGroupID: &groupID})
		if err != nil || resp.JSON200 == nil {
			t.Fatalf("list sites: %v %s", err, resp.Body)
		}
		var ids []string
		for _, s := range resp.JSON200.Items {
			ids = append(ids, s.ID)
		}
		return ids
	}

	t.Run("import", func(t *testing.T) {
		start := time.Now()
		accepted := importSite(t, archive, dump)
		waitJob(t, p, accepted.Job.ID)
		t.Logf("import job %s finished in %s", accepted.Job.ID, time.Since(start).Round(time.Millisecond))
		site := getSite(t, p, accepted.SiteID)
		proj := project(site.ID)
		preview := "https://" + site.PreviewDomain
		if site.Status != client.SiteStatusRunning || site.Name != "E2E Import Source" || site.AdminCredentialsAvailable {
			t.Errorf("imported site: %+v", site)
		}

		if got := wp(t, proj, "eval", `global $table_prefix; echo $table_prefix;`); got != "pf7_" {
			t.Errorf("table prefix = %q", got)
		}
		for _, option := range []string{"home", "siteurl"} {
			if got := wp(t, proj, "option", "get", option); got != preview {
				t.Errorf("%s = %q, want %q", option, got, preview)
			}
		}
		users := strings.Fields(wp(t, proj, "user", "list", "--field=user_login"))
		if !slices.Contains(users, "pf-editor") || len(users) != 2 {
			t.Errorf("users = %v", users)
		}
		if got := wp(t, proj, "user", "get", "pf-editor", "--field=roles"); got != "editor" {
			t.Errorf("pf-editor's roles = %q", got)
		}
		if n := wp(t, proj, "search-replace", oldHost, "unused.invalid", "--dry-run", "--all-tables-with-prefix", "--skip-columns=guid", "--format=count"); n != "0" {
			t.Errorf("%s values still name the old host", n)
		}
		files := run(t, proj, "sh", "-c", "cd "+content+" && ls -A . mu-plugins")
		if strings.Contains(files, "advanced-cache.php") || !strings.Contains(files, "kinsta-mu-plugins.php") {
			t.Errorf("wp-content:\n%s", files)
		}
		if got := wp(t, proj, "eval", `echo get_plugin_data( WP_CONTENT_DIR . "/object-cache.php" )["Name"];`); got != "Redis Object Cache Drop-In" {
			t.Errorf("object-cache.php is %q", got)
		}
		if owners := run(t, proj, "sh", "-c", "find "+content+" -mindepth 1 ! -user \"$(id -u)\" | head -5"); owners != "" {
			t.Errorf("files not owned by the site's user: %s", owners)
		}

		job, err := p.Client.GetJobWithResponse(ctx, accepted.Job.ID)
		if err != nil || job.JSON200 == nil {
			t.Fatal(err)
		}
		if !bytes.Contains(job.Body, []byte(`Must-use plugin \"kinsta-mu-plugins.php\" is Kinsta's`)) {
			t.Errorf("the job didn't warn about the Kinsta must-use plugin: %s", job.Body)
		}
		if job.JSON200.Result["table_prefix"] != "pf7_" || job.JSON200.Result["source_url"] != "http://"+oldHost {
			t.Errorf("job result = %v", job.JSON200.Result)
		}

		// The content is served on the preview domain.
		hc := p.IngressClient(t)
		_, body := waitIngress(t, hc, preview+"/?p="+postID, nil, http.StatusOK)
		newImage := strings.Replace(imageURL, "https://"+source.PreviewDomain, preview, 1)
		if !strings.Contains(body, title) || !strings.Contains(body, newImage) {
			t.Errorf("the post page lacks its title or %s:\n%.3000s", newImage, body)
		}
		resp, _ := ingressGet(t, hc, newImage, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: %s", newImage, resp.Status)
		}

		// And it goes live like any other site.
		skip := true
		live, err := p.Client.GoLiveSiteWithResponse(ctx, site.ID, client.GoLiveSiteJSONRequestBody{
			PrimaryDomain: "www.imported.e2e.test", SkipDNSCheck: &skip,
		})
		if err != nil || live.JSON202 == nil {
			t.Fatalf("go live: %v %s", err, live.Body)
		}
		waitJob(t, p, live.JSON202.Job.ID)
		_, body = waitIngress(t, hc, "https://www.imported.e2e.test/?p="+postID, nil, http.StatusOK)
		if !strings.Contains(body, title) || !strings.Contains(body, "https://www.imported.e2e.test/wp-content/uploads/") {
			t.Errorf("the live post page:\n%.3000s", body)
		}
	})

	t.Run("rejected before a site is created", func(t *testing.T) {
		before := groupSites(t)
		garbage := make([]byte, 4096)
		if _, err := rand.Read(garbage); err != nil {
			t.Fatal(err)
		}
		tooNew := regexp.MustCompile(`'db_version','\d+'`).ReplaceAll(dump, []byte(`'db_version','99999999'`))
		if bytes.Equal(tooNew, dump) {
			t.Fatal("found no db_version in the dump")
		}
		for _, tt := range []struct {
			name string
			dump []byte
			want string
		}{
			{"multisite", append(bytes.Clone(dump), "\nCREATE TABLE `pf7_blogs` (blog_id int);\n"...), "multisite"},
			{"unreadable", garbage, "invalid SQL dump"},
			{"too new", tooNew, "newer than the newest site image's"},
		} {
			t.Run(tt.name, func(t *testing.T) {
				accepted := importSite(t, archive, tt.dump)
				job := finalJob(t, p, accepted.Job.ID)
				if job.State != client.JobStateFailed || job.Error == nil || !strings.Contains(*job.Error, tt.want) {
					t.Errorf("job %s: %s, want failed with %q", job.State, deref(job.Error), tt.want)
				}
				for _, step := range job.Steps {
					if step.Name != "validate" && step.State != client.JobStepStatePending {
						t.Errorf("step %s is %s", step.Name, step.State)
					}
				}
				resp, err := p.Client.GetSiteWithResponse(ctx, accepted.SiteID)
				if err != nil || resp.StatusCode() != http.StatusNotFound {
					t.Errorf("GET the rejected import's site: %v %d", err, resp.StatusCode())
				}
			})
		}
		if after := groupSites(t); !slices.Equal(after, before) {
			t.Errorf("sites went from %v to %v", before, after)
		}
	})
}
