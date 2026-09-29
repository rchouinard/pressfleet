package e2e_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// TestSiteDeploy checks code deployments (#113) on a real platform, with a
// theme whose home page says which version it is:
//
//   - v1, a zip, is uploaded through the upload API and deployed with an
//     API token, activated; the site serves it, its files belong to the
//     site's user, and no staging or previous copy is left behind;
//   - v2, a tar.gz, replaces it while a client on the site's network asks
//     for the home page as fast as it can: every answer must be all v1 or
//     all v2, never a mix, an error, or a missing theme;
//   - v3 throws on the home page, so its health check fails: the job must
//     fail and roll back, leaving v2 serving and nothing behind.
func TestSiteDeploy(t *testing.T) {
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

	groupID := createGroup(t, p, "E2E Deploy Agency")
	created := createSite(t, p, groupID, "E2E Deploy", php)
	proj := compose.Project{Name: compose.SiteProjectName(created.Site.ID), Dir: projectDir}
	projects = append(projects, proj)
	waitJob(t, p, created.Job.ID)
	site := getSite(t, p, created.Site.ID)

	// CI deploys with an API token.
	tok, err := p.Client.CreateAPITokenWithResponse(ctx, client.CreateAPITokenJSONRequestBody{Name: "ci", Scope: client.Full})
	if err != nil {
		t.Fatal(err)
	}
	if tok.JSON201 == nil {
		t.Fatalf("create token: status %d: %s", tok.StatusCode(), tok.Body)
	}
	ci := p.BearerClient(t, redact.Secret(tok.JSON201.Token))

	const themes = "/var/www/wordpress/wp-content/themes"
	sh := func(script string) string {
		t.Helper()
		out, err := proj.ExecService(ctx, "php", "sh", "-c", script)
		if err != nil {
			t.Fatalf("sh -c %q: %v: %s", script, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	theme := func(version, index string) map[string]string {
		return map[string]string{
			"pf-e2e-theme/style.css": "/*\nTheme Name: PF E2E " + version + "\n*/\n",
			"pf-e2e-theme/index.php": index,
		}
	}
	serves := func(version string) string { return `<?php echo "pf-e2e-theme ` + version + `";` }
	deploy := func(data []byte, activate bool) client.Job {
		t.Helper()
		sum := sha256.Sum256(data)
		sumHex := hex.EncodeToString(sum[:])
		u, err := ci.CreateUploadWithResponse(ctx, client.CreateUploadJSONRequestBody{
			Purpose: client.UploadPurposeSiteDeploy, SiteID: &site.ID, SizeBytes: int64(len(data)), Sha256: &sumHex,
		})
		if err != nil || u.JSON201 == nil {
			t.Fatalf("create upload: %v %s", err, u.Body)
		}
		chunk, err := ci.AppendUploadChunkWithBodyWithResponse(ctx, u.JSON201.ID, &client.AppendUploadChunkParams{Offset: 0},
			"application/octet-stream", bytes.NewReader(data))
		if err != nil || chunk.JSON200 == nil {
			t.Fatalf("send upload: %v %s", err, chunk.Body)
		}
		if done, err := ci.CompleteUploadWithResponse(ctx, u.JSON201.ID, client.CompleteUploadJSONRequestBody{}); err != nil || done.JSON200 == nil {
			t.Fatalf("complete upload: %v %s", err, done.Body)
		}
		resp, err := ci.DeploySiteCodeWithResponse(ctx, site.ID, client.DeploySiteCodeJSONRequestBody{
			UploadID: u.JSON201.ID, Kind: client.Theme, Name: "pf-e2e-theme", Activate: &activate,
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.JSON202 == nil {
			t.Fatalf("deploy: status %d: %s", resp.StatusCode(), resp.Body)
		}
		return resp.JSON202.Job
	}
	home := func() string {
		t.Helper()
		out := fetch(t, images.PHP, site, "/?pf-e2e="+fmt.Sprint(time.Now().UnixNano()), "X-Forwarded-Proto: https")
		_, body, _ := strings.Cut(out, "\r\n\r\n")
		return strings.TrimSpace(body)
	}
	clean := func() {
		t.Helper()
		if left := sh("find " + themes + " -mindepth 1 -maxdepth 1 -name '.pressfleet-*'"); left != "" {
			t.Errorf("left in themes/: %s", left)
		}
		if owners := sh("find " + themes + "/pf-e2e-theme ! -user \"$(id -u)\" | head -5"); owners != "" {
			t.Errorf("files not owned by the site's user: %s", owners)
		}
	}

	// v1: a zip, activated.
	start := time.Now()
	job := deploy(zipBytes(t, theme("v1", serves("v1"))), true)
	waitJob(t, p, job.ID)
	took := time.Since(start)
	t.Logf("deployed v1 in %s", took.Round(time.Millisecond))
	if got := home(); got != "pf-e2e-theme v1" {
		t.Errorf("home page after v1 = %q", got)
	}
	if got := sh("wp option get stylesheet"); got != "pf-e2e-theme" {
		t.Errorf("active theme = %q", got)
	}
	clean()

	// v2: a tar.gz, while a client keeps asking for the home page.
	pollFor := min(2*took+15*time.Second, 2*time.Minute)
	script := fmt.Sprintf(`end=$(( $(date +%%s) + %d )); i=0
while [ "$(date +%%s)" -lt "$end" ]; do
  i=$((i+1))
  curl -s --max-time 10 -H 'Host: %s' -H 'X-Forwarded-Proto: https' -w ' %%{http_code}\n' "http://nginx:8080/?pf-poll=$i" || echo ' curl-failed'
done`, int(pollFor.Seconds()), site.PreviewDomain)
	polled := make(chan []byte, 1)
	go func() {
		out, err := compose.RunContainer(ctx, nil, compose.RunOptions{
			Image: images.PHP, Network: compose.SiteProjectName(site.ID), Entrypoint: "sh", Command: []string{"-c", script},
		})
		if err != nil {
			t.Errorf("polling the home page: %v", err)
		}
		polled <- out
	}()
	time.Sleep(3 * time.Second) // the poller is up
	job = deploy(tarGzBytes(t, theme("v2", serves("v2"))), false)
	waitJob(t, p, job.ID)
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(<-polled)), "\n") {
		counts[line]++
	}
	t.Logf("home page answers during the v2 deployment: %v", counts)
	if counts["pf-e2e-theme v1 200"] == 0 || counts["pf-e2e-theme v2 200"] == 0 || len(counts) != 2 {
		t.Errorf("answers during the deployment: %v; want only whole v1 and v2 pages, both", counts)
	}
	if got := home(); got != "pf-e2e-theme v2" {
		t.Errorf("home page after v2 = %q", got)
	}
	clean()

	// v3 breaks the home page: rolled back to v2.
	job = deploy(zipBytes(t, theme("v3", `<?php throw new Error("pf-e2e boom");`)), false)
	final := finalJob(t, p, job.ID)
	if final.State != client.JobStateFailed || !strings.Contains(deref(final.Error), "health check") {
		t.Fatalf("broken deployment: %s %q", final.State, deref(final.Error))
	}
	t.Logf("the broken deployment failed as intended: %s", deref(final.Error))
	if got := home(); got != "pf-e2e-theme v2" {
		t.Errorf("home page after the rollback = %q", got)
	}
	if got := sh("grep -c 'Theme Name: PF E2E v2' " + themes + "/pf-e2e-theme/style.css"); got != "1" {
		t.Errorf("style.css after the rollback isn't v2's")
	}
	clean()
	if got := getSite(t, p, site.ID); got.Status != client.SiteStatusRunning {
		t.Errorf("site after the rollback: %s", got.Status)
	}
}

func zipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGzBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
