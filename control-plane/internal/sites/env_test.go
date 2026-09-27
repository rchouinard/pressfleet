package sites_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/objectcachetest"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/provisionertest"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/sitedbtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const testPreviewBase = "preview.example.test"

// testNow is inside the support window of every PHP branch in testManifest.
var testNow = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

const testManifest = `{
  "registry": "registry.example/pf",
  "php": [
    {"version": "8.4", "activeSupportEnds": "2026-12-31", "securitySupportEnds": "2028-12-31"},
    {"version": "8.3", "activeSupportEnds": "2025-12-31", "securitySupportEnds": "2027-12-31"},
    {"version": "8.1", "activeSupportEnds": "2023-11-25", "securitySupportEnds": "2025-12-31"}
  ],
  "wordpress": {"latest": {"version": "7.1.2", "sha1": "761b8101538f0631a0bfc4fba7bc4abeea92f81c"}, "pinned": []}
}`

// wordpress simulates WordPress in the fake provisioner's php containers:
// `core is-installed` fails with exit code 1 until `core install` ran.
type wordpress struct {
	mu        sync.Mutex
	installed map[string]bool
	installs  map[string][][]string
	// failInstall, if set, is returned by `core install` (after installing,
	// if installAnyway is set).
	failInstall   error
	installAnyway bool
}

func (w *wordpress) wpcli(siteID string, args []string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	switch {
	case slices.Equal(args, []string{"core", "is-installed"}):
		if w.installed[siteID] {
			return "", nil
		}
		return "", &provisioner.ExecError{ExitCode: 1}
	case len(args) > 2 && args[0] == "core" && args[1] == "install":
		w.installs[siteID] = append(w.installs[siteID], slices.Clone(args))
		if w.failInstall != nil && !w.installAnyway {
			return "", w.failInstall
		}
		w.installed[siteID] = true
		return "Success: WordPress installed successfully.", w.failInstall
	}
	return "", nil
}

func (w *wordpress) installCalls(siteID string) [][]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.installs[siteID])
}

type env struct {
	db    *store.DB
	svc   *sites.Service
	prov  *provisionertest.Fake
	dbs   *sitedbtest.Fake
	cache *objectcachetest.Fake
	wp    *wordpress
	sec   secrets.Store
	pool  *jobs.Pool
	group string
}

func newEnv(t *testing.T, db *store.DB) *env {
	t.Helper()
	return newEnvWith(t, db, testPreviewBase)
}

func newEnvWith(t *testing.T, db *store.DB, previewBase string) *env {
	t.Helper()
	cat, err := catalog.Parse([]byte(testManifest))
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		db:    db,
		prov:  provisionertest.New(),
		dbs:   sitedbtest.New(),
		cache: objectcachetest.New(),
		wp:    &wordpress{installed: map[string]bool{}, installs: map[string][][]string{}},
		sec:   secrets.NewStore(db, secretstest.Keyring(t, 1)),
	}
	e.prov.WPCLI = e.wp.wpcli
	e.svc, err = sites.New(sites.Options{
		DB:                db,
		Secrets:           e.sec,
		Catalog:           cat,
		PreviewBaseDomain: previewBase,
		Provisioner:       e.prov,
		Databases:         e.dbs,
		ObjectCaches:      e.cache,
		Logger:            storetest.Logger(),
		Now:               func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Retries run at once, so tests drive a job to its end with RunNext.
	e.pool = jobs.NewPool(db, jobs.Options{
		Logger:  storetest.Logger(),
		Backoff: func(int) time.Duration { return 0 },
		Secrets: e.sec,
	})
	for _, h := range e.svc.Handlers() {
		if err := e.pool.Register(h); err != nil {
			t.Fatal(err)
		}
	}
	g := &store.ClientGroup{Name: "Acme " + id.New()}
	if err := store.NewClientGroups(db).Create(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	e.group = g.ID
	return e
}

// create creates a site called name with defaults, failing the test on
// error.
func (e *env) create(t *testing.T, name string) (store.Site, store.Job) {
	t.Helper()
	site, job, err := e.svc.Create(context.Background(), sites.NewSite{
		ClientGroupID: e.group, Name: name, AdminEmail: "owner@example.com",
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return site, job
}

// runJobs runs queued jobs until none is left, returning how many attempts
// ran.
func (e *env) runJobs(t *testing.T) int {
	t.Helper()
	n := 0
	for ; n < 100; n++ {
		ran, err := e.pool.RunNext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ran {
			return n
		}
	}
	t.Fatal("jobs never finished")
	return n
}

func (e *env) job(t *testing.T, jobID string) store.JobDetail {
	t.Helper()
	d, err := store.NewJobs(e.db).Detail(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) site(t *testing.T, siteID string) store.Site {
	t.Helper()
	s, err := store.NewSites(e.db).Get(context.Background(), siteID)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// jobLog joins a job's log lines, for checks that a secret stayed out.
func jobLog(d store.JobDetail) string {
	var b strings.Builder
	for _, l := range d.Logs {
		b.WriteString(l.Message)
		b.WriteByte('\n')
	}
	for _, s := range d.Steps {
		b.WriteString(s.Error)
		b.WriteByte('\n')
	}
	b.WriteString(d.Job.Error)
	b.Write(d.Job.Result)
	b.Write(d.Job.Data)
	return b.String()
}
