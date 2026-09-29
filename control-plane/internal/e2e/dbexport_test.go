package e2e_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// exportTTL is the export lifetime TestDatabaseExport configures, short
// enough to watch an export expire.
const exportTTL = 45 * time.Second

// stockImport is a shell script that starts a stock MariaDB server inside
// a throwaway container (no network, data in /tmp), imports the SQL on
// standard input into a new database, and runs the query in $1 there. It
// runs as nobody, without grant tables: the point is only that the dump
// imports into a server that knows nothing about the platform.
const stockImport = `set -e
mariadb-install-db --no-defaults --datadir=/tmp/db --skip-test-db --auth-root-authentication-method=normal >/tmp/install.log 2>&1 || { cat /tmp/install.log >&2; exit 1; }
mariadbd --no-defaults --datadir=/tmp/db --socket=/tmp/mysqld.sock --pid-file=/tmp/mysqld.pid --skip-networking --skip-grant-tables >/tmp/mysqld.log 2>&1 &
i=0
until mariadb-admin --no-defaults --socket=/tmp/mysqld.sock ping >/dev/null 2>&1; do
  i=$((i+1)); [ $i -lt 60 ] || { cat /tmp/mysqld.log >&2; exit 1; }; sleep 1
done
mariadb --no-defaults --socket=/tmp/mysqld.sock -e 'CREATE DATABASE site'
mariadb --no-defaults --socket=/tmp/mysqld.sock --binary-mode site
mariadb --no-defaults --socket=/tmp/mysqld.sock --batch --skip-column-names site -e "$1"
`

// TestDatabaseExport checks database exports (#114, design doc §5.8) on a
// real platform whose exports expire after exportTTL. It stops a site (so
// nothing changes its data), exports its database with a full API token,
// and checks that:
//
//   - the job's result links to the download, which needs credentials
//     (401 without, 403 with a read-only token) and works with the token
//     and with the admin's session;
//   - the download is a gzipped dump that imports into a stock MariaDB,
//     where every table's checksum matches the site's database;
//   - once the export expires, its link returns 404 and its file is gone;
//   - the audit log has the export and every download.
func TestDatabaseExport(t *testing.T) {
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
	p.Restart(t, map[string]string{"PRESSFLEET_DATABASE_EXPORT_TTL_SECONDS": "45"})
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
	platformImages, err := platform.DefaultImages("e2e")
	if err != nil {
		t.Fatal(err)
	}
	e2e.EnsureImages(t, images.PHP, images.Nginx, platformImages.MariaDB)

	groupID := createGroup(t, p, "E2E Export Agency")
	created := createSite(t, p, groupID, "E2E Export", php)
	site := created.Site
	siteProject = compose.Project{Name: compose.SiteProjectName(site.ID), Dir: projectDir}
	waitJob(t, p, created.Job.ID)

	// Data that must survive the trip: quotes, a backslash, and non-ASCII
	// text.
	const marker = `pf-e2e-export "quoted" it's \ ünïcødé ✓`
	if out, err := siteProject.ExecService(ctx, "php", "wp", "option", "update", "pf_export_marker", marker); err != nil {
		t.Fatalf("wp option update: %v: %s", err, out)
	}
	stopped, err := p.Client.StopSiteWithResponse(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.JSON202 == nil {
		t.Fatalf("stop: status %d: %s", stopped.StatusCode(), stopped.Body)
	}
	waitJob(t, p, stopped.JSON202.Job.ID)

	token := func(scope client.TokenScope) *client.ClientWithResponses {
		t.Helper()
		resp, err := p.Client.CreateAPITokenWithResponse(ctx, client.CreateAPITokenJSONRequestBody{Name: "e2e " + string(scope), Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON201 == nil {
			t.Fatalf("create token: status %d: %s", resp.StatusCode(), resp.Body)
		}
		return p.BearerClient(t, redact.Secret(resp.JSON201.Token))
	}
	full, readOnly := token(client.Full), token(client.ReadOnly)

	exported, err := full.ExportSiteDatabaseWithResponse(ctx, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if exported.JSON202 == nil {
		t.Fatalf("export: status %d: %s", exported.StatusCode(), exported.Body)
	}
	start := time.Now()
	waitJob(t, p, exported.JSON202.Job.ID)
	job := finalJob(t, p, exported.JSON202.Job.ID)
	t.Logf("export job %s finished in %s: %v", job.ID, time.Since(start).Round(time.Millisecond), job.Result)
	exportID, _ := job.Result["export_id"].(string)
	downloadURL, _ := job.Result["download_url"].(string)
	if exportID == "" || downloadURL != "/api/v1/sites/"+site.ID+"/database/exports/"+exportID+"/download" {
		t.Fatalf("job result = %v", job.Result)
	}

	download := func(t *testing.T, c *client.ClientWithResponses) *client.DownloadSiteDatabaseExportResponse {
		t.Helper()
		resp, err := c.DownloadSiteDatabaseExportWithResponse(ctx, site.ID, exportID)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	var dump []byte
	t.Run("download", func(t *testing.T) {
		if resp := download(t, p.NewClient(t)); resp.StatusCode() != http.StatusUnauthorized {
			t.Errorf("without credentials: status %d: %s", resp.StatusCode(), resp.Body)
		}
		if resp := download(t, readOnly); resp.StatusCode() != http.StatusForbidden {
			t.Errorf("with a read-only token: status %d: %s", resp.StatusCode(), resp.Body)
		}
		var bodies [][]byte
		for name, c := range map[string]*client.ClientWithResponses{"token": full, "session": p.Client} {
			resp := download(t, c)
			if resp.StatusCode() != http.StatusOK {
				t.Fatalf("with the %s: status %d: %s", name, resp.StatusCode(), resp.Body)
			}
			h := resp.HTTPResponse.Header
			if ct, cd := h.Get("Content-Type"), h.Get("Content-Disposition"); ct != "application/gzip" ||
				!strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, site.Slug) {
				t.Errorf("with the %s: Content-Type %q, Content-Disposition %q", name, ct, cd)
			}
			bodies = append(bodies, resp.Body)
		}
		if !bytes.Equal(bodies[0], bodies[1]) {
			t.Error("the token and the session downloaded different files")
		}
		zr, err := gzip.NewReader(bytes.NewReader(bodies[0]))
		if err != nil {
			t.Fatal(err)
		}
		if dump, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
		t.Logf("downloaded %d bytes, %d uncompressed", len(bodies[0]), len(dump))
		if !bytes.Contains(dump, []byte("CREATE TABLE")) || bytes.Contains(dump, []byte("CREATE DATABASE")) {
			t.Errorf("dump (%d bytes) isn't a plain dump of one database:\n%.2000s", len(dump), dump)
		}
	})

	t.Run("imports into stock MariaDB", func(t *testing.T) {
		if dump == nil {
			t.Skip("no dump")
		}
		// The site's tables and their checksums, read as root on the
		// platform's MariaDB.
		siteQuery := func(query string) string {
			t.Helper()
			out, err := p.Project.Exec(ctx, "mariadb", "sh", "-c",
				`exec mariadb --user=root --password="$(cat /run/secrets/mariadb_root_password)" --batch --skip-column-names "$1" -e "$2"`,
				"sh", "pf_"+site.ID, query)
			if err != nil {
				t.Fatalf("querying the site's database: %v: %s", err, out)
			}
			return string(out)
		}
		tables := strings.Fields(siteQuery("SHOW TABLES"))
		t.Logf("comparing %d tables", len(tables))
		if !slices.Contains(tables, "wp_options") || !slices.Contains(tables, "wp_posts") {
			t.Fatalf("site tables = %q", tables)
		}
		query := "CHECKSUM TABLE `" + strings.Join(tables, "`, `") + "`; SELECT option_value FROM wp_options WHERE option_name = 'pf_export_marker'"
		want := normalizeChecksums(siteQuery(query))

		out, err := compose.RunContainer(ctx, nil, compose.RunOptions{
			Image: platformImages.MariaDB, Entrypoint: "sh",
			Command: []string{"-c", stockImport, "sh", query},
			Stdin:   bytes.NewReader(dump),
		})
		if err != nil {
			t.Fatalf("importing into a stock MariaDB: %v: %s", err, out)
		}
		got := normalizeChecksums(string(out))
		if got != want {
			t.Errorf("imported database:\n%s\nsite database:\n%s", got, want)
		}
		if !strings.Contains(got, "ünïcødé") {
			t.Errorf("the marker option is missing:\n%s", got)
		}
	})

	t.Run("expiry", func(t *testing.T) {
		exports := filepath.Join(p.DataDir, "exports")
		files := func() string {
			t.Helper()
			out, err := p.Project.Exec(ctx, e2e.Service, "find", exports, "-mindepth", "1")
			if err != nil {
				t.Fatalf("listing %s: %v: %s", exports, err, out)
			}
			return strings.TrimSpace(string(out))
		}
		if f := files(); !strings.Contains(f, exportID) {
			t.Fatalf("export file missing before expiry: %q", f)
		}
		deadline := time.Now().Add(exportTTL + time.Minute)
		for {
			resp := download(t, p.Client)
			if resp.StatusCode() == http.StatusNotFound {
				break
			}
			if resp.StatusCode() != http.StatusOK || time.Now().After(deadline) {
				t.Fatalf("download while waiting for expiry: status %d", resp.StatusCode())
			}
			time.Sleep(5 * time.Second)
		}
		t.Logf("export expired %s after it was requested", time.Since(start).Round(time.Second))
		if f := files(); f != "" {
			t.Errorf("left in %s after expiry: %s", exports, f)
		}
	})

	t.Run("audit log", func(t *testing.T) {
		count := map[string]int{}
		for _, action := range []string{"site.database.export", "site.database.export.download"} {
			resp, err := p.Client.ListAuditEventsWithResponse(ctx, &client.ListAuditEventsParams{Action: &action, TargetID: &site.ID})
			if err != nil {
				t.Fatal(err)
			}
			if resp.JSON200 == nil {
				t.Fatalf("audit log: status %d: %s", resp.StatusCode(), resp.Body)
			}
			for _, ev := range resp.JSON200.Items {
				count[ev.Action+":"+string(ev.Result)]++
			}
		}
		// Downloads: the token's and the session's, the read-only token's
		// denial, and at least the one that found the export expired.
		if count["site.database.export:success"] != 1 || count["site.database.export.download:success"] < 2 ||
			count["site.database.export.download:denied"] != 1 || count["site.database.export.download:failure"] < 1 {
			t.Errorf("audit events = %v", count)
		}
	})
}

// normalizeChecksums drops the database name from CHECKSUM TABLE output
// ("pf_x.wp_posts\t123" becomes "wp_posts\t123"), so the site's database
// and the imported one compare equal.
func normalizeChecksums(out string) string {
	var b strings.Builder
	for line := range strings.Lines(out) {
		if db, rest, ok := strings.Cut(line, "."); ok && !strings.ContainsAny(db, " \t") {
			line = rest
		}
		b.WriteString(line)
	}
	return b.String()
}
