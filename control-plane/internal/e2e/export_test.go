package e2e_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// TestSiteExport checks site exports (#133, #134, design doc §5.7) on a real
// platform with the local backup backend. A site gets a post, a second
// user, and a file in uploads, and is backed up; then the post's title, an
// option, and the files change. Then:
//
//  1. a live export yields wp-content.tar.gz and database.sql.gz: every
//     path in the archive starts with wp-content/, the platform's drop-ins
//     (its object-cache.php) are left out, the dump holds the site's data
//     and no database password, and a download resumes with a Range
//     request; importing the two files creates a site with the same posts,
//     users, table prefix, and files;
//  2. an export of the backup taken before the change holds the earlier
//     content, and leaves the live site as it is;
//  3. once the site is deleted, its backup is listed under its client group
//     and can still be exported.
func TestSiteExport(t *testing.T) {
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
	project := func(siteID string) compose.Project {
		proj := compose.Project{Name: compose.SiteProjectName(siteID), Dir: projectDir}
		projects = append(projects, proj)
		return proj
	}

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
	groupID := createGroup(t, p, "E2E Export Site Agency")

	created := createSite(t, p, groupID, "E2E Export Site", php)
	site := created.Site
	proj := project(site.ID)
	waitJob(t, p, created.Job.ID)

	// The state the backup keeps.
	const before, after = "PF E2E export before 5d2a", "PF E2E export after 5d2a"
	postID := siteWP(t, proj, "post", "create", "--post_status=publish", "--post_title="+before, "--porcelain")
	siteWP(t, proj, "user", "create", "pf-exporter", "exporter@e2e.test", "--role=editor")
	siteSh(t, proj, "mkdir -p "+wpContent+"/uploads/pf && echo kept > "+wpContent+"/uploads/pf/kept.txt")
	backupID := backupRecord(t, p, site.ID, backupSite(t, p, site.ID))

	// The live site moves on.
	siteWP(t, proj, "post", "update", postID, "--post_title="+after)
	siteSh(t, proj, "echo added > "+wpContent+"/uploads/pf/added.txt")
	if got := siteSh(t, proj, "ls "+wpContent); !strings.Contains(got, "object-cache.php") {
		t.Fatalf("the site has no object cache drop-in to leave out:\n%s", got)
	}

	var liveArchive, liveDump []byte
	t.Run("live export", func(t *testing.T) {
		resp, err := p.Client.ExportSiteWithResponse(ctx, site.ID)
		if err != nil || resp.JSON202 == nil {
			t.Fatalf("export: %v %s", err, resp.Body)
		}
		exp := waitExport(t, p, resp.JSON202.Job.ID)
		if exp.Kind != client.ExportKindSite || exp.SiteSlug != site.Slug || exp.Source.TablePrefix != "wp_" ||
			exp.Source.SiteURL != "https://"+site.PreviewDomain || exp.Source.PhpVersion != php || len(exp.Files) != 2 {
			t.Errorf("export = %+v", exp)
		}
		liveArchive = downloadResumed(t, p, exp.ID, client.WpContentTarGz)
		liveDump = gunzipBytes(t, download(t, p, exp.ID, client.DatabaseSQLGz))

		files := untarGzip(t, liveArchive)
		for name := range files {
			if !strings.HasPrefix(name, "wp-content/") {
				t.Errorf("archive entry %q is outside wp-content/", name)
			}
		}
		for _, dropIn := range []string{"object-cache.php", "advanced-cache.php", "db.php"} {
			if _, ok := files["wp-content/"+dropIn]; ok {
				t.Errorf("the archive has the platform's %s", dropIn)
			}
		}
		if files["wp-content/uploads/pf/kept.txt"] != "kept\n" || files["wp-content/uploads/pf/added.txt"] != "added\n" {
			t.Errorf("uploads/pf in the archive: kept %q, added %q", files["wp-content/uploads/pf/kept.txt"], files["wp-content/uploads/pf/added.txt"])
		}
		if !bytes.Contains(liveDump, []byte(after)) || !bytes.Contains(liveDump, []byte("pf-exporter")) {
			t.Errorf("the dump lacks the post or the user")
		}
		if bytes.Contains(liveDump, []byte("CREATE DATABASE")) || bytes.Contains(liveDump, []byte("DB_PASSWORD")) {
			t.Errorf("the dump has a CREATE DATABASE or a password")
		}
	})

	t.Run("round trip", func(t *testing.T) {
		if liveArchive == nil {
			t.Skip("no live export")
		}
		resp, err := p.Client.ImportSiteWithResponse(ctx, client.ImportSiteJSONRequestBody{
			ClientGroupID: groupID, ArchiveUploadID: uploadFile(t, p, groupID, liveArchive),
			DumpUploadID: uploadFile(t, p, groupID, gzipBytes(t, liveDump)), PhpVersion: &php,
		})
		if err != nil || resp.JSON202 == nil {
			t.Fatalf("import: %v %s", err, resp.Body)
		}
		imported := project(resp.JSON202.SiteID)
		waitJob(t, p, resp.JSON202.Job.ID)
		if got := siteWP(t, imported, "post", "get", postID, "--field=post_title"); got != after {
			t.Errorf("imported post = %q", got)
		}
		for _, q := range [][]string{
			{"post", "list", "--post_type=any", "--format=ids", "--orderby=ID"},
			{"user", "list", "--field=user_login", "--orderby=login"},
			{"eval", `global $table_prefix; echo $table_prefix;`},
		} {
			if got, want := siteWP(t, imported, q...), siteWP(t, proj, q...); got != want {
				t.Errorf("wp %s: imported %q, original %q", strings.Join(q, " "), got, want)
			}
		}
		if got := siteSh(t, imported, "cat "+wpContent+"/uploads/pf/kept.txt "+wpContent+"/uploads/pf/added.txt"); got != "kept\nadded" {
			t.Errorf("imported uploads/pf = %q", got)
		}
	})

	t.Run("snapshot export", func(t *testing.T) {
		resp, err := p.Client.ExportSiteBackupWithResponse(ctx, site.ID, backupID)
		if err != nil || resp.JSON202 == nil {
			t.Fatalf("export: %v %s", err, resp.Body)
		}
		exp := waitExport(t, p, resp.JSON202.Job.ID)
		if exp.Kind != client.ExportKindSnapshot || exp.BackupID == nil || *exp.BackupID != backupID || exp.Source.TablePrefix != "wp_" ||
			exp.Source.SiteURL != "https://"+site.PreviewDomain || exp.Source.TakenAt == nil {
			t.Errorf("export = %+v", exp)
		}
		files := untarGzip(t, download(t, p, exp.ID, client.WpContentTarGz))
		if _, ok := files["wp-content/uploads/pf/added.txt"]; ok || files["wp-content/uploads/pf/kept.txt"] != "kept\n" {
			t.Errorf("the snapshot's archive has uploads/pf %v", keysWithPrefix(files, "wp-content/uploads/pf/"))
		}
		if _, ok := files["wp-content/object-cache.php"]; ok {
			t.Error("the snapshot's archive has the platform's object-cache.php")
		}
		dump := gunzipBytes(t, download(t, p, exp.ID, client.DatabaseSQLGz))
		if !bytes.Contains(dump, []byte(before)) || bytes.Contains(dump, []byte(after)) {
			t.Errorf("the snapshot's dump doesn't hold the earlier post title")
		}
		// The live site is as it was.
		if got := siteWP(t, proj, "post", "get", postID, "--field=post_title"); got != after {
			t.Errorf("live post after the snapshot export = %q", got)
		}
	})

	t.Run("deleted site", func(t *testing.T) {
		del, err := p.Client.DeleteSiteWithResponse(ctx, site.ID)
		if err != nil || del.JSON202 == nil {
			t.Fatalf("delete: %v %s", err, del.Body)
		}
		waitJob(t, p, del.JSON202.Job.ID)
		deleted := true
		list, err := p.Client.ListClientGroupBackupsWithResponse(ctx, groupID, &client.ListClientGroupBackupsParams{Deleted: &deleted})
		if err != nil || list.JSON200 == nil {
			t.Fatalf("list: %v %s", err, list.Body)
		}
		var found bool
		for _, b := range list.JSON200.Items {
			found = found || (b.ID == backupID && b.SiteSlug == site.Slug && b.SiteName == site.Name)
		}
		if !found {
			t.Fatalf("the deleted site's backup isn't listed: %s", list.Body)
		}
		resp, err := p.Client.ExportClientGroupBackupWithResponse(ctx, groupID, backupID)
		if err != nil || resp.JSON202 == nil {
			t.Fatalf("export: %v %s", err, resp.Body)
		}
		exp := waitExport(t, p, resp.JSON202.Job.ID)
		dump := gunzipBytes(t, download(t, p, exp.ID, client.DatabaseSQLGz))
		if !bytes.Contains(dump, []byte(before)) {
			t.Error("the deleted site's dump doesn't hold its post")
		}
		if files := untarGzip(t, download(t, p, exp.ID, client.WpContentTarGz)); files["wp-content/uploads/pf/kept.txt"] != "kept\n" {
			t.Errorf("the deleted site's archive lacks uploads/pf/kept.txt")
		}
	})
}

// snapshotExportChecks exports the snapshot restoreChecks.setUp took, for
// TestSiteBackupS3: the archive holds the file it kept.
func snapshotExportChecks(t *testing.T, p *e2e.Platform, siteID, snapshot string) {
	resp, err := p.Client.ExportSiteBackupWithResponse(t.Context(), siteID, backupRecord(t, p, siteID, snapshot))
	if err != nil || resp.JSON202 == nil {
		t.Fatalf("export: %v %s", err, resp.Body)
	}
	exp := waitExport(t, p, resp.JSON202.Job.ID)
	files := untarGzip(t, download(t, p, exp.ID, client.WpContentTarGz))
	if got := files["wp-content/uploads/pf/keep.txt"]; got != restoreKeep+"\n" {
		t.Errorf("the snapshot's archive has keep.txt %q", got)
	}
	if dump := gunzipBytes(t, download(t, p, exp.ID, client.DatabaseSQLGz)); !bytes.Contains(dump, []byte("pf_restore_marker")) {
		t.Error("the snapshot's dump lacks pf_restore_marker")
	}
}

// backupRecord returns the ID of the site's backup of the snapshot.
func backupRecord(t *testing.T, p *e2e.Platform, siteID, snapshot string) string {
	t.Helper()
	limit := 200
	list, err := p.Client.ListSiteBackupsWithResponse(t.Context(), siteID, &client.ListSiteBackupsParams{Limit: &limit})
	if err != nil || list.JSON200 == nil {
		t.Fatalf("list backups: %v %s", err, list.Body)
	}
	for _, b := range list.JSON200.Items {
		if b.SnapshotID != nil && *b.SnapshotID == snapshot {
			return b.ID
		}
	}
	t.Fatalf("no backup of snapshot %s", snapshot)
	return ""
}

// waitExport waits for an export job and returns its export.
func waitExport(t *testing.T, p *e2e.Platform, jobID string) client.Export {
	t.Helper()
	start := time.Now()
	waitJob(t, p, jobID)
	job, err := p.Client.GetJobWithResponse(t.Context(), jobID)
	if err != nil || job.JSON200 == nil {
		t.Fatalf("job %s: %v", jobID, err)
	}
	exportID, _ := job.JSON200.Result["export_id"].(string)
	t.Logf("export job %s (export %s) finished in %s", jobID, exportID, time.Since(start).Round(time.Millisecond))
	resp, err := p.Client.GetExportWithResponse(t.Context(), exportID)
	if err != nil || resp.JSON200 == nil {
		t.Fatalf("export %s: %v %s", exportID, err, resp.Body)
	}
	if resp.JSON200.Status != client.ExportStatusReady {
		t.Fatalf("export %s is %s", exportID, resp.JSON200.Status)
	}
	return *resp.JSON200
}

// download returns one of an export's files.
func download(t *testing.T, p *e2e.Platform, exportID string, name client.ExportFileName) []byte {
	t.Helper()
	resp, err := p.Client.DownloadExportFileWithResponse(t.Context(), exportID, name, &client.DownloadExportFileParams{})
	if err != nil || resp.StatusCode() != http.StatusOK {
		t.Fatalf("download %s of export %s: %v %d %.300s", name, exportID, err, resp.StatusCode(), resp.Body)
	}
	return resp.Body
}

// downloadResumed downloads a file whole, and again as a download that was
// interrupted and resumed with a Range request, and checks that both give
// the same bytes.
func downloadResumed(t *testing.T, p *e2e.Platform, exportID string, name client.ExportFileName) []byte {
	t.Helper()
	full, err := p.Client.DownloadExportFileWithResponse(t.Context(), exportID, name, &client.DownloadExportFileParams{})
	if err != nil || full.StatusCode() != http.StatusOK {
		t.Fatalf("download: %v %d", err, full.StatusCode())
	}
	etag := full.HTTPResponse.Header.Get("ETag")
	half := len(full.Body) / 2
	rng := fmt.Sprintf("bytes=%d-", half)
	rest, err := p.Client.DownloadExportFileWithResponse(t.Context(), exportID, name, &client.DownloadExportFileParams{Range: &rng, IfRange: &etag})
	if err != nil || rest.StatusCode() != http.StatusPartialContent {
		t.Fatalf("resumed download: %v %d", err, rest.StatusCode())
	}
	if resumed := append(slices.Clone(full.Body[:half]), rest.Body...); !bytes.Equal(resumed, full.Body) {
		t.Errorf("the resumed download (%d bytes) differs from the whole one (%d bytes)", len(resumed), len(full.Body))
	}
	return full.Body
}

// uploadFile sends data through the upload API for a site import.
func uploadFile(t *testing.T, p *e2e.Platform, groupID string, data []byte) string {
	t.Helper()
	ctx := t.Context()
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

func gunzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// untarGzip returns a gzipped tar archive's regular files.
func untarGzip(t *testing.T, b []byte) map[string]string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(gunzipBytes(t, b)))
	out := map[string]string{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		c, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = string(c)
	}
}

func keysWithPrefix(m map[string]string, prefix string) []string {
	var out []string
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
