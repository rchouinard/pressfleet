package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// testLifecycle drives a site through every lifecycle method: create,
// converge, stop, start, updates that do and don't recreate components
// (while running and while stopped), and delete. Each method is idempotent,
// and a site that doesn't exist is ErrSiteNotFound.
func testLifecycle(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	cfg := f.create(ctx, t)
	siteID := cfg.ID
	f.install(ctx, t, cfg, "Lifecycle")
	if got := f.wp(ctx, t, cfg, "option", "get", "blogname"); got != "Lifecycle" {
		t.Errorf("blogname = %q", got)
	}

	// Create again converges without recreating anything.
	phpStart, nginxStart := f.procStart(ctx, t, cfg, provisioner.ComponentPHP), f.procStart(ctx, t, cfg, provisioner.ComponentNginx)
	if st, err := f.d.CreateSite(ctx, cfg); err != nil || st.Phase != provisioner.SiteRunning {
		t.Fatalf("second CreateSite = %+v, %v", st, err)
	}
	if f.procStart(ctx, t, cfg, provisioner.ComponentPHP) != phpStart || f.procStart(ctx, t, cfg, provisioner.ComponentNginx) != nginxStart {
		t.Error("converging an unchanged site recreated components")
	}
	if st, err := f.d.UpdateSite(ctx, cfg); err != nil || st.Phase != provisioner.SiteRunning {
		t.Fatalf("UpdateSite without changes = %+v, %v", st, err)
	}
	if f.procStart(ctx, t, cfg, provisioner.ComponentPHP) != phpStart || f.procStart(ctx, t, cfg, provisioner.ComponentNginx) != nginxStart {
		t.Error("an update without changes recreated components")
	}
	if err := f.d.StartSite(ctx, siteID); err != nil {
		t.Errorf("StartSite on a running site: %v", err)
	}

	// Stop and start, each twice.
	for range 2 {
		if err := f.d.StopSite(ctx, siteID); err != nil {
			t.Fatalf("StopSite: %v", err)
		}
		f.wantPhase(ctx, t, cfg, provisioner.SiteStopped)
	}
	for range 2 {
		if err := f.d.StartSite(ctx, siteID); err != nil {
			t.Fatalf("StartSite: %v", err)
		}
		f.wantPhase(ctx, t, cfg, provisioner.SiteRunning)
	}
	if got := f.wp(ctx, t, cfg, "option", "get", "blogname"); got != "Lifecycle" {
		t.Errorf("after restart, blogname = %q: data was lost", got)
	}

	// A resource change recreates the components with the new limits.
	before := f.memoryLimit(ctx, t, cfg, provisioner.ComponentPHP)
	cfg.Resources.MemoryLimitMB = 640
	if st, err := f.d.UpdateSite(ctx, cfg); err != nil || st.Phase != provisioner.SiteRunning {
		t.Fatalf("UpdateSite = %+v, %v", st, err)
	}
	if after := f.memoryLimit(ctx, t, cfg, provisioner.ComponentPHP); after <= before {
		t.Errorf("php memory limit = %d after raising the site's from 512 to 640 MiB, was %d", after, before)
	}

	// A PHP version change swaps the php image.
	cfg.PHPVersion = env.AltPHPVersion
	if _, err := f.d.UpdateSite(ctx, cfg); err != nil {
		t.Fatalf("UpdateSite to PHP %s: %v", env.AltPHPVersion, err)
	}
	version := func() string {
		return f.wp(ctx, t, cfg, "--skip-wordpress", "eval", "echo PHP_MAJOR_VERSION, '.', PHP_MINOR_VERSION;")
	}
	if got := version(); got != env.AltPHPVersion {
		t.Errorf("PHP version = %s after switching to %s", got, env.AltPHPVersion)
	}
	f.wantPhase(ctx, t, cfg, provisioner.SiteRunning)
	if got := f.wp(ctx, t, cfg, "option", "get", "blogname"); got != "Lifecycle" {
		t.Errorf("after the PHP version changed, blogname = %q", got)
	}

	// New salts change only wp-config.php: php is recreated to see the new
	// file, and nginx keeps running.
	phpStart, nginxStart = f.procStart(ctx, t, cfg, provisioner.ComponentPHP), f.procStart(ctx, t, cfg, provisioner.ComponentNginx)
	oldSalt := f.wp(ctx, t, cfg, "eval", "echo AUTH_KEY;")
	cfg.WPSaltsSecretRef = f.newSalts(t, cfg, "v2")
	if _, err := f.d.UpdateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if f.procStart(ctx, t, cfg, provisioner.ComponentPHP) == phpStart {
		t.Error("php was not recreated after wp-config.php changed")
	}
	if f.procStart(ctx, t, cfg, provisioner.ComponentNginx) != nginxStart {
		t.Error("nginx was recreated although only wp-config.php changed")
	}
	if got := f.wp(ctx, t, cfg, "eval", "echo AUTH_KEY;"); got == oldSalt {
		t.Error("php still sees the old salts")
	}

	// Updating a stopped site keeps it stopped; the change applies on start.
	if err := f.d.StopSite(ctx, siteID); err != nil {
		t.Fatal(err)
	}
	cfg.Resources.MemoryLimitMB = 512
	if st, err := f.d.UpdateSite(ctx, cfg); err != nil || st.Phase != provisioner.SiteStopped {
		t.Fatalf("UpdateSite on a stopped site = %+v, %v", st, err)
	}
	if err := f.d.StartSite(ctx, siteID); err != nil {
		t.Fatal(err)
	}
	if got := f.memoryLimit(ctx, t, cfg, provisioner.ComponentPHP); got != before {
		t.Errorf("php memory limit = %d after lowering the site's to 512 MiB while stopped, want %d", got, before)
	}
	if got := f.wp(ctx, t, cfg, "option", "get", "blogname"); got != "Lifecycle" {
		t.Errorf("blogname = %q at the end", got)
	}

	// Delete removes everything, and deleting again succeeds.
	if err := f.d.DeleteSite(ctx, siteID); err != nil {
		t.Fatal(err)
	}
	f.wantNoLeftovers(ctx, t, siteID, true)
	if err := f.d.DeleteSite(ctx, siteID); err != nil {
		t.Errorf("second DeleteSite: %v", err)
	}
	f.wantMissing(ctx, t, siteID)

	// Re-creating a deleted site starts it from nothing: its database
	// tables are the platform's to remove, so the data may remain, but the
	// runtime is new and runs.
	if st, err := f.d.CreateSite(ctx, cfg); err != nil || st.Phase != provisioner.SiteRunning {
		t.Errorf("CreateSite after delete = %+v, %v", st, err)
	}
}

// wantMissing checks that every method that takes a site ID reports a site
// that doesn't exist as ErrSiteNotFound (DeleteSite succeeds).
func (f *fixture) wantMissing(ctx context.Context, t *testing.T, siteID string) {
	t.Helper()
	cfg := f.siteConfig(t)
	cfg.ID = siteID
	_, err := f.d.GetSite(ctx, siteID)
	wantErr(t, "GetSite", err, provisioner.ErrSiteNotFound)
	_, err = f.d.UpdateSite(ctx, cfg)
	wantErr(t, "UpdateSite", err, provisioner.ErrSiteNotFound)
	wantErr(t, "StartSite", f.d.StartSite(ctx, siteID), provisioner.ErrSiteNotFound)
	wantErr(t, "StopSite", f.d.StopSite(ctx, siteID), provisioner.ErrSiteNotFound)
	wantErr(t, "PurgeCache", f.d.PurgeCache(ctx, siteID), provisioner.ErrSiteNotFound)
	wantErr(t, "ReloadCode", f.d.ReloadCode(ctx, siteID), provisioner.ErrSiteNotFound)
	wantErr(t, "SetMaintenance", f.d.SetMaintenance(ctx, siteID, true), provisioner.ErrSiteNotFound)
	_, err = f.d.ExecWPCLI(ctx, siteID, []string{"--info"})
	wantErr(t, "ExecWPCLI", err, provisioner.ErrSiteNotFound)
	_, err = f.d.Stats(ctx, siteID)
	wantErr(t, "Stats", err, provisioner.ErrSiteNotFound)
	_, err = f.d.ProbeHTTP(ctx, siteID, provisioner.ProbeRequest{Host: "acme.preview.example.com", Path: "/"})
	wantErr(t, "ProbeHTTP", err, provisioner.ErrSiteNotFound)
	_, err = f.d.RunTask(ctx, siteID, provisioner.Task{Image: f.phpImage(), Command: []string{"true"}})
	wantErr(t, "RunTask", err, provisioner.ErrSiteNotFound)
	err = f.d.RunSession(ctx, siteID, provisioner.Session{Image: f.phpImage(), Command: []string{"true"}, Stdin: strings.NewReader(""), Stdout: &strings.Builder{}})
	wantErr(t, "RunSession", err, provisioner.ErrSiteNotFound)
	for _, err := range f.d.Logs(ctx, siteID, provisioner.LogOptions{}) {
		wantErr(t, "Logs", err, provisioner.ErrSiteNotFound)
		break
	}
}

// testMaintenance checks maintenance mode: it takes effect on a running and
// a stopped site, leaves WP-CLI working, survives an update (the
// SiteConfig.Maintenance field carries it), and ends when asked.
func testMaintenance(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	cfg := f.create(ctx, t)
	f.install(ctx, t, cfg, "Maintenance")
	inMaintenance := func(want bool) {
		t.Helper()
		if f.inst.InMaintenance == nil {
			return
		}
		eventually(t, 30*time.Second, "maintenance mode", func() string {
			got, err := f.inst.InMaintenance(ctx, cfg.ID)
			if err != nil {
				return err.Error()
			}
			if got != want {
				return fmt.Sprintf("in maintenance = %v, want %v", got, want)
			}
			return ""
		})
	}
	inMaintenance(false)

	if err := f.d.SetMaintenance(ctx, cfg.ID, true); err != nil {
		t.Fatalf("SetMaintenance(true): %v", err)
	}
	if err := f.d.SetMaintenance(ctx, cfg.ID, true); err != nil {
		t.Errorf("SetMaintenance(true) again: %v", err)
	}
	inMaintenance(true)
	if got := f.wp(ctx, t, cfg, "option", "get", "blogname"); got != "Maintenance" {
		t.Errorf("WP-CLI in maintenance mode: blogname = %q", got)
	}

	// An update that follows the documented rule (the config says what
	// SetMaintenance did) recreates components and keeps the mode.
	cfg.Maintenance = true
	cfg.Resources.MemoryLimitMB = 600
	if _, err := f.d.UpdateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	inMaintenance(true)
	// So does stopping and starting, and converging with CreateSite.
	if err := f.d.StopSite(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	inMaintenance(true)
	if err := f.d.StartSite(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.CreateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	inMaintenance(true)

	if err := f.d.SetMaintenance(ctx, cfg.ID, false); err != nil {
		t.Fatalf("SetMaintenance(false): %v", err)
	}
	if err := f.d.SetMaintenance(ctx, cfg.ID, false); err != nil {
		t.Errorf("SetMaintenance(false) again: %v", err)
	}
	inMaintenance(false)
	cfg.Maintenance = false
	if _, err := f.d.UpdateSite(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	inMaintenance(false)

	// Maintenance in the config of a new site starts it in maintenance.
	other := f.create(ctx, t, func(c *provisioner.SiteConfig) { c.Maintenance = true })
	if f.inst.InMaintenance != nil {
		if got, err := f.inst.InMaintenance(ctx, other.ID); err != nil || !got {
			t.Errorf("a site created in maintenance: in maintenance = %v, %v", got, err)
		}
	}
	// Deleting a site ends it.
	if err := f.d.DeleteSite(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if f.inst.InMaintenance != nil {
		if got, err := f.inst.InMaintenance(ctx, other.ID); err != nil || got {
			t.Errorf("a deleted site: in maintenance = %v, %v", got, err)
		}
	}
}

// hostileArgs are arguments a shell or the orchestrator's exec would
// interpret if they were ever parsed rather than passed through as argv.
// WP-CLI parses --name arguments itself, even after "--", and rejects ones
// the command doesn't know, so the options here are ones it accepts.
var hostileArgs = []string{
	"; rm -rf /",
	"$(touch /tmp/pwned)",
	"`touch /tmp/pwned`",
	"line1\nline2",
	`it's "quoted" \ back`,
	"*",
	"$HOME",
	"| cat /etc/passwd",
	"--",
	"--user=0",
	"-T",
	"--url=http://x/$(id)",
	"",
}

// testExecWPCLI runs WP-CLI through the driver: as the site's UID, with
// hostile arguments arriving verbatim, structured errors, a deadline that
// also ends the process in the container, and the errors for a site that
// is stopped or doesn't exist.
func testExecWPCLI(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	cfg := f.create(ctx, t)

	// It runs as the site UID.
	want := fmt.Sprintf("%d:%d", cfg.UID, cfg.UID)
	if got := f.wp(ctx, t, cfg, "eval", "--skip-wordpress", "echo posix_geteuid(), ':', posix_getegid();"); got != want {
		t.Errorf("wp runs as %q, want the site UID %s", got, want)
	}

	// Arguments arrive as WP-CLI's argv, unchanged. The PHP file prints the
	// process's argv after the file name, as JSON.
	f.putFile(ctx, t, cfg, "/tmp/argv.php",
		`<?php echo json_encode( array_slice( $_SERVER['argv'], array_search( '/tmp/argv.php', $_SERVER['argv'], true ) + 1 ) );`)
	raw := f.wp(ctx, t, cfg, append([]string{"eval-file", "--skip-wordpress", "/tmp/argv.php"}, hostileArgs...)...)
	var got []string
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("argv output %q: %v", raw, err)
	}
	if !slices.Equal(got, hostileArgs) {
		t.Errorf("wp saw argv\n %q\nwant\n %q", got, hostileArgs)
	}
	if _, err := f.inst.Exec(ctx, cfg.ID, provisioner.ComponentPHP, "test", "-e", "/tmp/pwned"); err == nil {
		t.Error("an argument was executed by a shell")
	}

	// A non-zero exit is an *ExecError with the exit code and the streams.
	_, err := f.d.ExecWPCLI(ctx, cfg.ID, []string{"eval", "--skip-wordpress", "echo 'out'; fwrite( STDERR, 'boom' ); exit( 3 );"})
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *provisioner.ExecError", err)
	}
	if ee.ExitCode != 3 || ee.Stdout != "out" || !strings.Contains(ee.Stderr, "boom") {
		t.Errorf("ExecError = %+v", ee)
	}
	// So is a WP-CLI usage error, which isn't a driver failure.
	_, err = f.d.ExecWPCLI(ctx, cfg.ID, []string{"no-such-command"})
	if !errors.As(err, &ee) || ee.ExitCode == 0 {
		t.Errorf("unknown WP-CLI command: %v, want an *ExecError", err)
	}

	// The deadline ends the call, and the process in the container ends
	// shortly after.
	short, cancelShort := context.WithTimeout(ctx, 2*time.Second)
	defer cancelShort()
	start := time.Now()
	_, err = f.d.ExecWPCLI(short, cfg.ID, []string{"eval", "--skip-wordpress", "sleep( 120 ); // pf-timeout-test"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("ExecWPCLI returned %s after its deadline", elapsed)
	}
	eventually(t, 30*time.Second, "WP-CLI after its deadline", func() string {
		procs := f.exec(ctx, t, cfg, provisioner.ComponentPHP, "sh", "-c", `cat /proc/[0-9]*/cmdline | tr '\0' ' '`)
		if strings.Contains(procs, "pf-timeout-test") {
			return "still running in the container: " + procs
		}
		return ""
	})

	// A stopped site can't run WP-CLI.
	if err := f.d.StopSite(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	_, err = f.d.ExecWPCLI(ctx, cfg.ID, []string{"--info"})
	wantErr(t, "ExecWPCLI on a stopped site", err, provisioner.ErrSiteNotRunning)
	if errors.Is(err, provisioner.ErrSiteNotFound) {
		t.Error("a stopped site is not a missing one")
	}
}

// testPurgeCache empties the page cache of a running site and of a stopped
// one: cache files the web server wrote as the site UID are deleted, and the
// cache stays usable.
func testPurgeCache(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	cfg := f.create(ctx, t)
	const cacheDir = "/var/cache/pressfleet/fastcgi"
	// nginx writes cache files as levels=1:2 subdirectories.
	seed := func() {
		t.Helper()
		f.exec(ctx, t, cfg, provisioner.ComponentNginx, "sh", "-c",
			"mkdir -p "+cacheDir+"/c/ba && echo page > "+cacheDir+"/c/ba/0123456789abcdef0123456789abcbac")
	}
	left := func() string {
		t.Helper()
		return f.exec(ctx, t, cfg, provisioner.ComponentPHP, "find", cacheDir, "-mindepth", "1")
	}

	seed()
	if err := f.d.PurgeCache(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	if got := left(); got != "" {
		t.Errorf("left in the cache of a running site: %q", got)
	}
	if err := f.d.PurgeCache(ctx, cfg.ID); err != nil {
		t.Errorf("purging an empty cache: %v", err)
	}

	seed()
	if err := f.d.StopSite(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.d.PurgeCache(ctx, cfg.ID); err != nil {
		t.Fatalf("PurgeCache on a stopped site: %v", err)
	}
	if err := f.d.StartSite(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	if got := left(); got != "" {
		t.Errorf("left in the cache of a stopped site: %q", got)
	}
	// The purge left nothing running, and the web server can still write.
	f.wantNoLeftovers(ctx, t, cfg.ID, false)
	f.exec(ctx, t, cfg, provisioner.ComponentNginx, "touch", cacheDir+"/writable")
}

// testReloadCode checks that PHP runs changed code after ReloadCode, not
// after its opcode cache lets it go, and that components keep running.
func testReloadCode(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	cfg := f.create(ctx, t)
	const file, url = "/var/www/wordpress/wp-content/pf-reload.php", "/wp-content/pf-reload.php"
	write := func(version string, mtime string) {
		t.Helper()
		f.putFile(ctx, t, cfg, file, `<?php echo "`+version+`";`)
		f.exec(ctx, t, cfg, provisioner.ComponentPHP, "touch", "-d", mtime, file)
	}
	body := func() string {
		t.Helper()
		resp := f.get(ctx, t, cfg, url)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d: %.200s", url, resp.StatusCode, resp.Body)
		}
		return resp.Body
	}

	write("v1", "2026-01-01 00:00:00")
	if got := body(); got != "v1" {
		t.Fatalf("first version = %q", got)
	}
	phpStart, nginxStart := f.procStart(ctx, t, cfg, provisioner.ComponentPHP), f.procStart(ctx, t, cfg, provisioner.ComponentNginx)
	write("v2", "2026-01-01 00:01:00")
	if err := f.d.ReloadCode(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "PHP after ReloadCode", func() string {
		resp, err := f.inst.Get(ctx, cfg.ID, cfg.PreviewDomain, url)
		switch {
		case err != nil:
			return err.Error()
		case resp.Body != "v2":
			return fmt.Sprintf("status %d, body %q, want v2", resp.StatusCode, resp.Body)
		}
		return ""
	})
	if f.procStart(ctx, t, cfg, provisioner.ComponentPHP) != phpStart || f.procStart(ctx, t, cfg, provisioner.ComponentNginx) != nginxStart {
		t.Error("ReloadCode restarted a component")
	}
	if err := f.d.ReloadCode(ctx, cfg.ID); err != nil {
		t.Errorf("second ReloadCode: %v", err)
	}

	// A stopped site has nothing cached and succeeds.
	if err := f.d.StopSite(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.d.ReloadCode(ctx, cfg.ID); err != nil {
		t.Errorf("ReloadCode on a stopped site: %v", err)
	}
}

// testProbeHTTP checks that ProbeHTTP reaches the site's web server whatever
// the site's state otherwise (maintenance mode), says the request came over
// HTTPS, follows no redirects, and reports a site that is stopped or
// doesn't exist as the errors the contract names.
func testProbeHTTP(t *testing.T, env Env) {
	f := newFixture(t, env)
	ctx := context10m(t)
	cfg := f.create(ctx, t)
	f.install(ctx, t, cfg, "Probe")
	// A file that answers with a status of its own, and reports whether it
	// was asked over HTTPS (is_ssl, from the web server's parameters).
	f.putFile(ctx, t, cfg, "/var/www/wordpress/wp-content/pf-probe.php",
		`<?php http_response_code( ( isset( $_SERVER['HTTPS'] ) && 'on' === $_SERVER['HTTPS'] ) ? 418 : 200 );`)
	f.putFile(ctx, t, cfg, "/var/www/wordpress/wp-content/pf-redirect.php",
		`<?php header( 'Location: /elsewhere', true, 302 );`)

	probe := func(path string) int {
		t.Helper()
		var err error
		var resp *provisioner.ProbeResponse
		for deadline := time.Now().Add(45 * time.Second); ; time.Sleep(time.Second) {
			resp, err = f.d.ProbeHTTP(ctx, cfg.ID, provisioner.ProbeRequest{Host: cfg.PreviewDomain, Path: path})
			if err == nil || time.Now().After(deadline) {
				break
			}
		}
		if err != nil {
			t.Fatalf("ProbeHTTP %s: %v", path, err)
		}
		return resp.StatusCode
	}
	if got := probe("/wp-content/pf-probe.php"); got != 418 {
		t.Errorf("probe of a page that answers 418 over HTTPS = %d", got)
	}
	if got := probe("/wp-content/pf-redirect.php?a=b&c=d"); got != 302 {
		t.Errorf("probe of a redirect = %d, want 302 (no redirects followed)", got)
	}
	if got := probe("/wp-content/pf-no-such-file.php"); got != 404 {
		t.Errorf("probe of a missing file = %d, want 404", got)
	}

	if err := f.d.SetMaintenance(ctx, cfg.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := probe("/wp-content/pf-probe.php"); got != 418 {
		t.Errorf("probe in maintenance mode = %d, want the site's own answer", got)
	}
	if err := f.d.SetMaintenance(ctx, cfg.ID, false); err != nil {
		t.Fatal(err)
	}

	if err := f.d.StopSite(ctx, cfg.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.d.ProbeHTTP(ctx, cfg.ID, provisioner.ProbeRequest{Host: cfg.PreviewDomain, Path: "/"})
	wantErr(t, "ProbeHTTP on a stopped site", err, provisioner.ErrSiteNotRunning)
}
