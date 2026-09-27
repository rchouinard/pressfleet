package sites_test

import (
	"context"
	"encoding/json"
	"maps"
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
// `core is-installed` fails with exit code 1 until `core install` ran, and
// the plugin commands of the default plugin stack act on a per-site plugin
// list.
type wordpress struct {
	mu        sync.Mutex
	installed map[string]bool
	installs  map[string][][]string
	sites     map[string]*wpSite
	// calls records every other WP-CLI call per site.
	calls map[string][][]string
	// failInstall, if set, is returned by `core install` (after installing,
	// if installAnyway is set).
	failInstall   error
	installAnyway bool
	// unbundled lists plugins the site image doesn't bundle.
	unbundled map[string]bool
	// failCommand, if set, is consulted for every plugin-related call.
	failCommand func(args []string) error
}

// wpSite is a simulated site's plugin state.
type wpSite struct {
	plugins map[string]string // slug -> status
	dropin  string            // title of object-cache.php, "" if none
	options map[string]string // option -> JSON value
	caps    []string          // capabilities added to administrator
	// autoUpdates are the plugins whose auto-updates were turned on.
	autoUpdates []string
}

func (w *wordpress) site(siteID string) *wpSite {
	s, ok := w.sites[siteID]
	if !ok {
		s = &wpSite{plugins: map[string]string{"akismet": "inactive"}, options: map[string]string{}}
		w.sites[siteID] = s
	}
	return s
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
	w.calls[siteID] = append(w.calls[siteID], slices.Clone(args))
	if w.failCommand != nil {
		if err := w.failCommand(args); err != nil {
			return "", err
		}
	}
	s := w.site(siteID)
	switch {
	case len(args) > 1 && args[0] == "plugin" && args[1] == "list":
		list := []map[string]string{{"name": "pressfleet", "status": "must-use", "title": "PressFleet Platform"}}
		for _, slug := range slices.Sorted(maps.Keys(s.plugins)) {
			list = append(list, map[string]string{"name": slug, "status": s.plugins[slug], "title": slug})
		}
		if s.dropin != "" {
			list = append(list, map[string]string{"name": "object-cache.php", "status": "dropin", "title": s.dropin})
		}
		b, err := json.Marshal(list)
		return string(b), err
	case len(args) == 2 && args[0] == "eval" && strings.Contains(args[1], "is_readable"):
		slug, ok := bundledSlug(args[1])
		if !ok || w.unbundled[slug] {
			return "no", nil
		}
		return "yes", nil
	case len(args) == 3 && args[0] == "plugin" && args[1] == "install":
		slug, ok := bundledSlug(args[2])
		if !ok || w.unbundled[slug] {
			return "", &provisioner.ExecError{ExitCode: 1, Stderr: "Error: No plugins installed."}
		}
		if _, ok := s.plugins[slug]; ok {
			return "", &provisioner.ExecError{ExitCode: 1, Stderr: "Error: Destination folder already exists."}
		}
		s.plugins[slug] = "inactive"
		return "Success: Installed 1 of 1 plugins.", nil
	case len(args) > 2 && args[0] == "plugin" && args[1] == "activate":
		for _, slug := range args[2:] {
			if _, ok := s.plugins[slug]; !ok {
				return "", &provisioner.ExecError{ExitCode: 1, Stderr: "Error: The '" + slug + "' plugin could not be found."}
			}
			s.plugins[slug] = "active"
		}
		return "Success: Activated.", nil
	case slices.Equal(args, []string{"redis", "enable"}):
		if s.plugins[sites.PluginRedisCache] != "active" {
			return "", &provisioner.ExecError{ExitCode: 1, Stderr: "Error: 'redis' is not a registered wp command."}
		}
		s.dropin = "Redis Object Cache Drop-In"
		return "Success: Object cache enabled.", nil
	case len(args) == 4 && args[0] == "option" && args[1] == "get" && args[3] == "--format=json":
		v, ok := s.options[args[2]]
		if !ok {
			return "", &provisioner.ExecError{ExitCode: 1, Stderr: "Error: Could not get '" + args[2] + "' option. Does it exist?"}
		}
		return v, nil
	case len(args) == 5 && args[0] == "option" && args[1] == "update" && args[4] == "--format=json":
		if !json.Valid([]byte(args[3])) {
			return "", &provisioner.ExecError{ExitCode: 1, Stderr: "Error: Invalid JSON."}
		}
		s.options[args[2]] = args[3]
		return "Success: Updated.", nil
	case len(args) > 3 && args[0] == "cap" && args[1] == "add" && args[2] == "administrator":
		s.caps = append(s.caps, args[3:]...)
		return "Success: Added.", nil
	case len(args) > 3 && args[0] == "plugin" && args[1] == "auto-updates" && args[2] == "enable":
		s.autoUpdates = append(s.autoUpdates, args[3:]...)
		return "Success: Enabled.", nil
	}
	return "", nil
}

// bundledSlug extracts the slug from a path under sites.BundledPluginDir.
func bundledSlug(s string) (string, bool) {
	_, rest, ok := strings.Cut(s, sites.BundledPluginDir+"/")
	if !ok {
		return "", false
	}
	slug, _, ok := strings.Cut(rest, ".zip")
	return slug, ok
}

func (w *wordpress) installCalls(siteID string) [][]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.installs[siteID])
}

// pluginCalls returns the site's WP-CLI calls other than the core install
// commands, each joined with spaces, and forgets them.
func (w *wordpress) pluginCalls(siteID string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, c := range w.calls[siteID] {
		out = append(out, strings.Join(c, " "))
	}
	delete(w.calls, siteID)
	return out
}

// state returns a copy of the site's plugin state.
func (w *wordpress) state(siteID string) wpSite {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.site(siteID)
	return wpSite{plugins: maps.Clone(s.plugins), dropin: s.dropin, options: maps.Clone(s.options), caps: slices.Clone(s.caps), autoUpdates: slices.Clone(s.autoUpdates)}
}

// edit changes the site's plugin state, as the site owner might.
func (w *wordpress) edit(siteID string, fn func(*wpSite)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fn(w.site(siteID))
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

// newEnvWith returns an env whose service has the given preview base
// domain, after applying opts to its options.
func newEnvWith(t *testing.T, db *store.DB, previewBase string, opts ...func(*sites.Options)) *env {
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
		wp: &wordpress{
			installed: map[string]bool{}, installs: map[string][][]string{},
			sites: map[string]*wpSite{}, calls: map[string][][]string{}, unbundled: map[string]bool{},
		},
		sec: secrets.NewStore(db, secretstest.Keyring(t, 1)),
	}
	e.prov.WPCLI = e.wp.wpcli
	o := sites.Options{
		DB:                db,
		Secrets:           e.sec,
		Catalog:           cat,
		PreviewBaseDomain: previewBase,
		Provisioner:       e.prov,
		Databases:         e.dbs,
		ObjectCaches:      e.cache,
		Logger:            storetest.Logger(),
		Now:               func() time.Time { return testNow },
	}
	for _, opt := range opts {
		opt(&o)
	}
	e.svc, err = sites.New(o)
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
