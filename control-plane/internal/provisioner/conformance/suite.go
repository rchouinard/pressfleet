package conformance

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// Run runs the conformance suite against the driver that env.New builds.
// Each group of checks is a subtest, with a fresh driver and fresh sites, so
// `go test -run 'TestConformance/Tasks'` runs one.
func Run(t *testing.T, env Env) {
	t.Helper()
	for _, v := range []string{env.PHPVersion, env.AltPHPVersion} {
		if _, ok := env.Images[v]; !ok {
			t.Fatalf("conformance: Env.Images has no images for PHP %q", v)
		}
	}
	if env.New == nil || env.WPCoreVersion == "" || env.Database.Database == "" {
		t.Fatal("conformance: Env.New, WPCoreVersion, and Database are required")
	}
	for _, g := range []struct {
		name string
		run  func(*testing.T, Env)
	}{
		{"Lifecycle", testLifecycle},
		{"Maintenance", testMaintenance},
		{"ExecWPCLI", testExecWPCLI},
		{"PurgeCache", testPurgeCache},
		{"ReloadCode", testReloadCode},
		{"ProbeHTTP", testProbeHTTP},
		{"Environment", testEnvironment},
		{"ObjectCache", testObjectCache},
		{"PinImage", testPinImage},
		{"Isolation", testIsolation},
		{"Tasks", testTasks},
		{"RepositoryTasks", testRepositoryTasks},
		{"Sessions", testSessions},
		{"Logs", testLogs},
		{"Stats", testStats},
		{"RenderSnapshots", testRenderSnapshots},
	} {
		t.Run(g.name, func(t *testing.T) { g.run(t, env) })
	}
}

// fixture is one test's driver and the sites it created.
type fixture struct {
	env   Env
	setup Setup
	inst  Instance
	d     provisioner.Driver
}

// Sites need UIDs and table prefixes of their own: the UID range is shared
// by every driver sharing a host, and the database by every site.
var siteCounter atomic.Int64

// newFixture builds the test's driver.
func newFixture(t *testing.T, env Env) *fixture {
	t.Helper()
	layout := config.Layout{Root: t.TempDir()}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	f := &fixture{env: env}
	f.setup = Setup{Layout: layout, Secrets: &Secrets{}, Images: ImageSet(env.Images)}
	f.inst = env.New(t, f.setup)
	f.d = f.inst.Driver
	if f.d == nil || f.inst.Exec == nil || f.inst.Get == nil {
		t.Fatal("conformance: Instance needs Driver, Exec, and Get")
	}
	return f
}

// context10m returns a context for the whole test.
func context10m(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// phpImage is the image that tasks and sessions run: the site's own.
func (f *fixture) phpImage() string { return f.env.Images[f.env.PHPVersion].PHP }

// siteConfig returns the configuration of a new site, with its secrets
// stored. Nothing exists in the driver until CreateSite.
func (f *fixture) siteConfig(t *testing.T) *provisioner.SiteConfig {
	t.Helper()
	n := int(siteCounter.Add(1))
	siteID := id.New()
	db, err := sitedb.MarshalCredentials(f.env.Database)
	if err != nil {
		t.Fatal(err)
	}
	salts, err := wpconfig.MarshalSalts(wpconfig.GenerateSalts())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &provisioner.SiteConfig{
		ID:               siteID,
		ClientGroupID:    id.New(),
		PreviewDomain:    "acme" + strconv.Itoa(n) + ".preview.example.com",
		PHPVersion:       f.env.PHPVersion,
		WPCoreVersion:    f.env.WPCoreVersion,
		Resources:        provisioner.DefaultResourceLimits(),
		DBSecretRef:      siteID + "-db",
		WPSaltsSecretRef: siteID + "-salts",
		UID:              provisioner.MinSiteUID + 7000 + n,
		TablePrefix:      "pfc" + strconv.Itoa(n) + "_",
	}
	f.setup.Secrets.put(cfg.DBSecretRef, db)
	f.setup.Secrets.put(cfg.WPSaltsSecretRef, salts)
	return cfg
}

// newSalts replaces the site's salts and returns the new reference.
func (f *fixture) newSalts(t *testing.T, cfg *provisioner.SiteConfig, suffix string) string {
	t.Helper()
	salts, err := wpconfig.MarshalSalts(wpconfig.GenerateSalts())
	if err != nil {
		t.Fatal(err)
	}
	ref := cfg.ID + "-salts-" + suffix
	f.setup.Secrets.put(ref, salts)
	return ref
}

// create makes a site and deletes it when the test ends, checking that
// nothing of it is left.
func (f *fixture) create(ctx context.Context, t *testing.T, mutate ...func(*provisioner.SiteConfig)) *provisioner.SiteConfig {
	t.Helper()
	cfg := f.siteConfig(t)
	for _, m := range mutate {
		m(cfg)
	}
	f.cleanupSite(t, cfg.ID)
	st, err := f.d.CreateSite(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if st.ID != cfg.ID || st.Phase != provisioner.SiteRunning {
		t.Fatalf("CreateSite status = %+v, want running", st)
	}
	return cfg
}

func (f *fixture) cleanupSite(t *testing.T, siteID string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := f.d.DeleteSite(ctx, siteID); err != nil {
			t.Errorf("cleanup: DeleteSite %s: %v", siteID, err)
			return
		}
		f.wantNoLeftovers(ctx, t, siteID, true)
	})
}

// wantNoLeftovers fails the test if the driver reports objects of the owner.
func (f *fixture) wantNoLeftovers(ctx context.Context, t *testing.T, owner string, deleted bool) {
	t.Helper()
	if f.inst.Leftovers == nil {
		return
	}
	left, err := f.inst.Leftovers(ctx, owner, deleted)
	if err != nil {
		t.Errorf("listing leftovers of %s: %v", owner, err)
	} else if len(left) > 0 {
		t.Errorf("left behind for %s (site deleted: %v): %q", owner, deleted, left)
	}
}

// wp runs WP-CLI through the driver and returns its trimmed output.
func (f *fixture) wp(ctx context.Context, t *testing.T, cfg *provisioner.SiteConfig, args ...string) string {
	t.Helper()
	out, err := f.d.ExecWPCLI(ctx, cfg.ID, args)
	if err != nil {
		t.Fatalf("wp %q: %v", args, err)
	}
	return strings.TrimSpace(out)
}

// exec runs a command in a component, bypassing the driver.
func (f *fixture) exec(ctx context.Context, t *testing.T, cfg *provisioner.SiteConfig, c provisioner.Component, argv ...string) string {
	t.Helper()
	out, err := f.inst.Exec(ctx, cfg.ID, c, argv...)
	if err != nil {
		t.Fatalf("exec in %s %q: %v", c, argv, err)
	}
	return strings.TrimSpace(out)
}

// install installs WordPress, waiting for the database to accept the site.
func (f *fixture) install(ctx context.Context, t *testing.T, cfg *provisioner.SiteConfig, title string) {
	t.Helper()
	args := []string{"core", "install", "--url=http://" + cfg.PreviewDomain, "--title=" + title,
		"--admin_user=admin", "--admin_password=test-admin-pw", "--admin_email=admin@example.com", "--skip-email"}
	var err error
	for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if _, err = f.d.ExecWPCLI(ctx, cfg.ID, args); err == nil {
			return
		}
		var ee *provisioner.ExecError
		if errors.As(err, &ee) && strings.Contains(ee.Stderr, "already installed") {
			return
		}
	}
	t.Fatalf("installing WordPress: %v", err)
}

// wantPhase checks the site's observed phase.
func (f *fixture) wantPhase(ctx context.Context, t *testing.T, cfg *provisioner.SiteConfig, want provisioner.SitePhase) {
	t.Helper()
	st, err := f.d.GetSite(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	if st.Phase != want {
		t.Fatalf("phase = %s (%s), want %s", st.Phase, st.Message, want)
	}
}

// procStart identifies the instance of a component's main process: it
// changes when the component is recreated or restarted.
func (f *fixture) procStart(ctx context.Context, t *testing.T, cfg *provisioner.SiteConfig, c provisioner.Component) string {
	t.Helper()
	return f.exec(ctx, t, cfg, c, "sh", "-c", `cut -d' ' -f22 /proc/1/stat`)
}

// memoryLimit returns a component's memory limit as Stats reports it.
func (f *fixture) memoryLimit(ctx context.Context, t *testing.T, cfg *provisioner.SiteConfig, c provisioner.Component) uint64 {
	t.Helper()
	stats, err := f.d.Stats(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	for _, s := range stats {
		if s.Component == c {
			return s.MemoryLimitBytes
		}
	}
	t.Fatalf("Stats has no %s sample: %+v", c, stats)
	return 0
}

// get fetches a path of the site's web server as a visitor of its preview
// domain, retrying while the web server finds the php component again after
// it was recreated.
func (f *fixture) get(ctx context.Context, t *testing.T, cfg *provisioner.SiteConfig, path string) *HTTPResponse {
	t.Helper()
	var resp *HTTPResponse
	var err error
	for deadline := time.Now().Add(45 * time.Second); ; time.Sleep(time.Second) {
		resp, err = f.inst.Get(ctx, cfg.ID, cfg.PreviewDomain, path)
		if err == nil && resp.StatusCode != http.StatusBadGateway && resp.StatusCode != http.StatusServiceUnavailable || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// putFile writes a file as the site, in the php component. Content goes in
// as an argument, never through a shell command line.
func (f *fixture) putFile(ctx context.Context, t *testing.T, cfg *provisioner.SiteConfig, path, content string) {
	t.Helper()
	f.exec(ctx, t, cfg, provisioner.ComponentPHP, "sh", "-c", `printf '%s' "$1" > "$2"`, "sh", content, path)
}

// openDir makes a new directory under parent that any user can write: the
// suite doesn't run as a site's UID or root, so it opens the directory up
// instead of giving it away.
func (f *fixture) openDir(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name+"-"+id.New())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // G302: the task's user is not the test's
		t.Fatal(err)
	}
	return dir
}

func (f *fixture) transferDir(t *testing.T) string {
	t.Helper()
	return f.openDir(t, f.setup.Layout.TmpDir(), "transfer")
}

func (f *fixture) repositoryDir(t *testing.T) string {
	t.Helper()
	return f.openDir(t, f.setup.Layout.BackupRepositoriesDir(), "repo")
}

// wantErr checks an error with errors.Is.
func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want %v", what, err, want)
	}
}

// eventually polls until check returns "" or the timeout passes, and fails
// the test with the last answer.
func eventually(t *testing.T, timeout time.Duration, what string, check func() string) {
	t.Helper()
	var last string
	for deadline := time.Now().Add(timeout); ; time.Sleep(500 * time.Millisecond) {
		if last = check(); last == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %s", what, last)
		}
	}
}

// objectCacheFor returns the suite's object cache credentials for a site.
func objectCacheFor(env Env, siteID string) (redact.Secret, objectcache.Credentials, error) {
	c := *env.ObjectCache
	c.KeyPrefix = objectcache.KeyPrefix(siteID)
	s, err := objectcache.MarshalCredentials(c)
	if err != nil {
		return "", c, fmt.Errorf("marshal object cache credentials: %w", err)
	}
	return s, c, nil
}
