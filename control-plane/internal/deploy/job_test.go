package deploy_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/deploy"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/objectcachetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

const testUser = "01j9zq3v6t8k4m2n5p7r9s1x3y"

// jobEnv runs deployment jobs against fakes: site tasks run the real
// operations (RunTaskCommand) on a temporary wp-content, and the site's
// web server answers 500 while its theme's style.css says "broken".
type jobEnv struct {
	db      *store.DB
	prov    *provisionertest.Fake
	caches  *objectcachetest.Fake
	uploads *uploads.Service
	svc     *deploy.Service
	pool    *jobs.Pool
	layout  config.Layout
	site    store.Site
	wc      string

	mu     sync.Mutex
	theme  string          // the active theme
	active map[string]bool // active plugins
	wpcli  [][]string
}

func newJobEnv(t *testing.T, db *store.DB) *jobEnv {
	t.Helper()
	ctx := context.Background()
	e := &jobEnv{db: db, prov: provisionertest.New(), caches: objectcachetest.New(), layout: config.Layout{Root: t.TempDir()},
		wc: t.TempDir(), theme: "twentytwentyfive", active: map[string]bool{}}
	now := time.Now()
	if _, err := db.ExecContext(ctx, "INSERT INTO users (id, email, password_hash, role, created_at, updated_at) VALUES (?, ?, 'x', 'operator', ?, ?)",
		testUser, "dev@example.com", now, now); err != nil {
		t.Fatal(err)
	}
	g := &store.ClientGroup{Name: "Acme"}
	if err := store.NewClientGroups(db).Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	e.site = store.Site{
		ClientGroupID: g.ID, Name: "Site", Slug: "acme", Status: store.SiteRunning, PHPVersion: "8.4", WPCoreVersion: "6.9.1",
		PreviewDomain: "acme.preview.example", MemoryReservationMB: 128, MemoryLimitMB: 512, CPULimit: "1",
		UID: 20001, WPSaltsSecretRef: "salts",
	}
	if err := store.NewSites(db).Create(ctx, &e.site); err != nil {
		t.Fatal(err)
	}
	if _, err := e.prov.CreateSite(ctx, &provisioner.SiteConfig{
		ID: e.site.ID, ClientGroupID: g.ID, PreviewDomain: e.site.PreviewDomain, PHPVersion: "8.4", WPCoreVersion: "6.9.1",
		Resources: provisioner.DefaultResourceLimits(), DBSecretRef: "db", WPSaltsSecretRef: "salts", UID: 20001,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.caches.CreateUser(ctx, e.site.ID); err != nil {
		t.Fatal(err)
	}

	e.prov.Probe = func(_ string, req provisioner.ProbeRequest) (*provisioner.ProbeResponse, error) {
		if req.Host != e.site.PreviewDomain {
			return &provisioner.ProbeResponse{StatusCode: http.StatusMisdirectedRequest}, nil
		}
		b, _ := os.ReadFile(filepath.Join(e.wc, "themes", e.activeTheme(), "style.css"))
		if strings.Contains(string(b), "broken") {
			return &provisioner.ProbeResponse{StatusCode: http.StatusInternalServerError}, nil
		}
		return &provisioner.ProbeResponse{StatusCode: http.StatusOK}, nil
	}
	e.prov.RunTaskFunc = func(_ string, task provisioner.Task) (*provisioner.TaskResult, error) {
		if !slices.Equal(task.Command[:len(deploy.TaskCommand)], deploy.TaskCommand) {
			t.Errorf("task command %q", task.Command)
		}
		args := append(slices.Clone(task.Command[len(deploy.TaskCommand):]), "--wp-content", e.wc)
		if task.TransferDir != "" {
			args = append(args, "--archive", filepath.Join(task.TransferDir, deploy.PackageFile))
		}
		var out bytes.Buffer
		if err := deploy.RunTaskCommand(args, &out); err != nil {
			code := 1
			if deploy.IsPermanentTaskError(err) {
				code = deploy.ExitPermanent
			}
			return nil, &provisioner.ExecError{ExitCode: code, Stderr: "Error: " + err.Error()}
		}
		return &provisioner.TaskResult{Stdout: out.String()}, nil
	}
	e.prov.WPCLI = e.fakeWPCLI

	var err error
	e.uploads, err = uploads.New(uploads.Options{DB: db, Dir: e.layout.UploadsDir(), MaxSize: 1 << 30, Expiry: time.Hour, Logger: storetest.Logger()})
	if err != nil {
		t.Fatal(err)
	}
	e.svc, err = deploy.New(deploy.Options{
		DB: db, Uploads: e.uploads, Provisioner: e.prov, Tasks: e.prov, ObjectCaches: e.caches, Layout: e.layout,
		Health: healthFunc(e.checkHealth), Logger: storetest.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.pool = jobs.NewPool(db, jobs.Options{Logger: storetest.Logger(), Backoff: func(int) time.Duration { return 0 }})
	if err := e.pool.Register(e.svc.Handler()); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *jobEnv) activeTheme() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.theme
}

// fakeWPCLI answers the WP-CLI commands deployments run. Code whose main
// file says "fatal" makes WordPress fail to load once it is active.
func (e *jobEnv) fakeWPCLI(_ string, args []string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.wpcli = append(e.wpcli, args)
	fatal := func(path string) bool {
		b, _ := os.ReadFile(filepath.Join(e.wc, path))
		return strings.Contains(string(b), "fatal")
	}
	crash := &provisioner.ExecError{ExitCode: 255, Stderr: "PHP Fatal error: boom\nStack trace:\n#0 ..."}
	skips := func(flag, name string) bool { return slices.Contains(args, flag+name) }
	loads := func() error {
		if !skips("--skip-themes=", e.theme) && fatal("themes/"+e.theme+"/style.css") {
			return crash
		}
		for p := range e.active {
			if !skips("--skip-plugins=", p) && fatal("plugins/"+p+"/"+p+".php") {
				return crash
			}
		}
		return nil
	}
	switch strings.Join(args[:min(3, len(args))], " ") {
	case "option get stylesheet":
		return e.theme + "\n", loads()
	case "option get home":
		return "https://" + e.site.PreviewDomain + "\n", loads()
	}
	switch {
	case len(args) >= 3 && args[0] == "theme" && args[1] == "activate":
		if err := loads(); err != nil {
			return "", err
		}
		e.theme = args[2]
		return "", loads()
	case len(args) >= 3 && args[0] == "plugin" && args[1] == "is-active":
		if !e.active[args[2]] {
			return "", &provisioner.ExecError{ExitCode: 1}
		}
		return "", nil
	case len(args) >= 3 && args[0] == "plugin" && args[1] == "activate":
		e.active[args[2]] = true
		if err := loads(); err != nil {
			delete(e.active, args[2])
			return "", err
		}
		return "", nil
	case len(args) >= 3 && args[0] == "plugin" && args[1] == "deactivate":
		delete(e.active, args[2])
		return "", loads()
	}
	return "", errors.New("unexpected wp " + strings.Join(args, " "))
}

// upload stores data as a complete upload for the site.
func (e *jobEnv) upload(t *testing.T, data []byte) string {
	t.Helper()
	ctx := context.Background()
	sum := sha256.Sum256(data)
	u, err := e.uploads.Create(ctx, uploads.NewUpload{Purpose: store.UploadSiteDeploy, SiteID: e.site.ID, Filename: "package",
		SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), CreatedByUserID: testUser})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.uploads.Write(ctx, u.ID, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.uploads.Complete(ctx, u.ID, ""); err != nil {
		t.Fatal(err)
	}
	return u.ID
}

// deploy queues a deployment, runs it, and returns the finished job.
func (e *jobEnv) deploy(t *testing.T, data []byte, kind deploy.Kind, name string, activate bool) store.Job {
	t.Helper()
	ctx := context.Background()
	job, err := e.svc.Deploy(ctx, e.site.ID, deploy.Request{UploadID: e.upload(t, data), Kind: kind, Name: name, Activate: activate, UserID: testUser})
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		ran, err := e.pool.RunNext(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			break
		}
	}
	j, err := store.NewJobs(e.db).Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// files lists what is in a kind's directory, hidden entries included.
func (e *jobEnv) files(t *testing.T, kind deploy.Kind) map[string]string {
	t.Helper()
	return tree(t, filepath.Join(e.wc, kind.Dir()))
}

func (e *jobEnv) checkClean(t *testing.T) {
	t.Helper()
	for _, kind := range []deploy.Kind{deploy.KindTheme, deploy.KindPlugin, deploy.KindMUPlugin} {
		for name := range e.files(t, kind) {
			if strings.HasPrefix(name, ".") {
				t.Errorf("left behind in %s: %s", kind.Dir(), name)
			}
		}
	}
	if entries, err := os.ReadDir(e.layout.TmpDir()); err == nil && len(entries) > 0 {
		t.Errorf("left in tmp/: %v", entries)
	}
}

func TestDeployJob(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newJobEnv(t, db)
		theme := func(version string) []member {
			return []member{{name: "acme/"}, {name: "acme/style.css", body: version}, {name: "acme/index.php", body: "<?php // " + version}}
		}

		// A first deployment, activated.
		job := e.deploy(t, zipOf(t, theme("v1")...), deploy.KindTheme, "acme", true)
		if job.State != store.JobSucceeded {
			t.Fatalf("first deployment: %s %s", job.State, job.Error)
		}
		var result map[string]any
		if err := json.Unmarshal(job.Result, &result); err != nil {
			t.Fatal(err)
		}
		if result["replaced"] != false || result["activated"] != true || result["path"] != "themes/acme" || result["files"] != float64(2) {
			t.Errorf("result = %s", job.Result)
		}
		if got := e.files(t, deploy.KindTheme); got["acme/style.css"] != "v1" || len(got) != 3 {
			t.Errorf("themes after the first deployment: %v", got)
		}
		if e.activeTheme() != "acme" {
			t.Errorf("active theme = %s", e.activeTheme())
		}
		e.checkClean(t)
		site, _ := e.prov.Site(e.site.ID)
		if site.CodeReloads != 1 || site.CachePurges != 1 || e.caches.Flushes(e.site.ID) != 1 {
			t.Errorf("reloads %d, page cache purges %d, object cache flushes %d", site.CodeReloads, site.CachePurges, e.caches.Flushes(e.site.ID))
		}
		if entries, err := os.ReadDir(e.layout.UploadsDir()); err != nil || len(entries) != 0 {
			t.Errorf("the upload wasn't released: %v %v", entries, err)
		}

		// A new version, as tar.gz, replaces it.
		job = e.deploy(t, tarGzOf(t, theme("v2")...), deploy.KindTheme, "acme", false)
		if job.State != store.JobSucceeded || !strings.Contains(string(job.Result), `"replaced":true`) {
			t.Fatalf("second deployment: %s %s %s", job.State, job.Error, job.Result)
		}
		if got := e.files(t, deploy.KindTheme); got["acme/style.css"] != "v2" || len(got) != 3 {
			t.Errorf("themes after the second deployment: %v", got)
		}
		e.checkClean(t)

		// A version that breaks the home page is rolled back.
		job = e.deploy(t, zipOf(t, theme("v3 broken")...), deploy.KindTheme, "acme", true)
		if job.State != store.JobFailed || !strings.Contains(job.Error, "health check") || !strings.Contains(job.Error, "500") {
			t.Fatalf("broken deployment: %s %s", job.State, job.Error)
		}
		if got := e.files(t, deploy.KindTheme); got["acme/style.css"] != "v2" || got["acme/index.php"] != "<?php // v2" || len(got) != 3 {
			t.Errorf("themes after the rollback: %v", got)
		}
		e.checkClean(t)

		// A new theme that breaks WordPress: activation is undone too.
		job = e.deploy(t, zipOf(t, member{name: "style.css", body: "fatal"}), deploy.KindTheme, "shiny", true)
		if job.State != store.JobFailed || !strings.Contains(job.Error, "Fatal error: boom") || strings.Contains(job.Error, "Stack trace") {
			t.Fatalf("fatal theme: %s %s", job.State, job.Error)
		}
		if e.activeTheme() != "acme" {
			t.Errorf("active theme after the rollback = %s", e.activeTheme())
		}
		if _, ok := e.files(t, deploy.KindTheme)["shiny/"]; ok {
			t.Error("the failed first deployment of shiny is still there")
		}
		e.checkClean(t)

		// A plugin that fails to activate is removed again.
		job = e.deploy(t, zipOf(t, member{name: "boom/boom.php", body: "fatal"}), deploy.KindPlugin, "boom", true)
		if job.State != store.JobFailed || !strings.Contains(job.Error, "activating the plugin") {
			t.Fatalf("fatal plugin: %s %s", job.State, job.Error)
		}
		if got := e.files(t, deploy.KindPlugin); len(got) != 0 {
			t.Errorf("plugins after the rollback: %v", got)
		}
		e.checkClean(t)

		// A must-use plugin goes into mu-plugins, which is created.
		job = e.deploy(t, zipOf(t, member{name: "loader.php", body: "<?php"}), deploy.KindMUPlugin, "acme-mu", false)
		if job.State != store.JobSucceeded {
			t.Fatalf("mu-plugin: %s %s", job.State, job.Error)
		}
		if got := e.files(t, deploy.KindMUPlugin); got["acme-mu/loader.php"] != "<?php" {
			t.Errorf("mu-plugins: %v", got)
		}

		// An unsafe package never reaches the site.
		before := len(e.prov.Calls())
		job = e.deploy(t, zipOf(t, member{name: "../../wp-config.php", body: "x"}), deploy.KindTheme, "acme", false)
		if job.State != store.JobFailed || !strings.Contains(job.Error, "outside the package") {
			t.Fatalf("unsafe package: %s %s", job.State, job.Error)
		}
		for _, c := range e.prov.Calls()[before:] {
			if c.Op == "RunTask" && !strings.Contains(strings.Join(c.Task.Command, " "), "deploy-discard") {
				t.Errorf("an unsafe package ran %q", c.Task.Command)
			}
		}
		if got := e.files(t, deploy.KindTheme); got["acme/style.css"] != "v2" {
			t.Errorf("themes after the unsafe package: %v", got)
		}
		e.checkClean(t)
	})
}

func TestDeployJobStopsForAStoppedSite(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newJobEnv(t, db)
		ctx := context.Background()
		job, err := e.svc.Deploy(ctx, e.site.ID, deploy.Request{UploadID: e.upload(t, zipOf(t, member{name: "a.php", body: "x"})),
			Kind: deploy.KindPlugin, Name: "a", UserID: testUser})
		if err != nil {
			t.Fatal(err)
		}
		// Stopped while the job waited.
		if _, err := store.NewSites(db).SetStatus(ctx, e.site.ID, store.SiteStopped, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := e.pool.RunNext(ctx); err != nil {
			t.Fatal(err)
		}
		j, err := store.NewJobs(db).Get(ctx, job.ID)
		if err != nil || j.State != store.JobFailed || !strings.Contains(j.Error, "must be running") {
			t.Errorf("job = %s %s, %v", j.State, j.Error, err)
		}
		if !id.Valid(j.ID) {
			t.Error("bad job")
		}
	})
}

// healthFunc is a deploy.HealthChecker.
type healthFunc func(ctx context.Context, site store.Site) error

func (f healthFunc) CheckHealth(ctx context.Context, site store.Site, _ string) error {
	return f(ctx, site)
}

// checkHealth does what sites.Service.CheckHealth does, once: the home page
// must answer below 500, and WordPress must load.
func (e *jobEnv) checkHealth(ctx context.Context, site store.Site) error {
	res, err := e.prov.ProbeHTTP(ctx, site.ID, provisioner.ProbeRequest{Host: site.PreviewDomain, Path: "/?pressfleet-health-check=1"})
	if err != nil {
		return err
	}
	if res.StatusCode >= 500 {
		return fmt.Errorf("HTTP request: the site answered %d", res.StatusCode)
	}
	if _, err := e.prov.ExecWPCLI(ctx, site.ID, []string{"option", "get", "home"}); err != nil {
		var ee *provisioner.ExecError
		if errors.As(err, &ee) {
			return errors.New(ee.Stderr)
		}
		return err
	}
	return nil
}
