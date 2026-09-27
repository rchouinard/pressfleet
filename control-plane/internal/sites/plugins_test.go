package sites_test

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
	"github.com/rchouinard/pressfleet/images"
)

// TestDefaultPluginsAreBundled checks images/shared/plugins.json, which the
// site-php image build reads: every plugin the platform installs is pinned
// there with a version and a SHA-256.
func TestDefaultPluginsAreBundled(t *testing.T) {
	var lock struct {
		Plugins []struct {
			Slug    string `json:"slug"`
			Version string `json:"version"`
			SHA256  string `json:"sha256"`
		} `json:"plugins"`
	}
	dec := json.NewDecoder(bytes.NewReader(images.Plugins()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&lock); err != nil {
		t.Fatal(err)
	}
	slugRE := regexp.MustCompile(`^[a-z0-9-]+$`)
	versionRE := regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)
	sha256RE := regexp.MustCompile(`^[0-9a-f]{64}$`)
	pinned := map[string]bool{}
	for _, p := range lock.Plugins {
		if !slugRE.MatchString(p.Slug) || !versionRE.MatchString(p.Version) || !sha256RE.MatchString(p.SHA256) {
			t.Errorf("malformed entry %+v", p)
		}
		if pinned[p.Slug] {
			t.Errorf("%s is listed twice", p.Slug)
		}
		pinned[p.Slug] = true
	}
	for _, slug := range []string{sites.PluginRedisCache} {
		if !pinned[slug] {
			t.Errorf("%s is not in images/shared/plugins.json", slug)
		}
	}
}

const (
	listPlugins = "plugin list --format=json --fields=name,status,title --skip-update-check"
	redisDropIn = "Redis Object Cache Drop-In"
)

func bundleCheck(slug string) string {
	return "eval echo is_readable( '" + sites.BundledPluginDir + "/" + slug + ".zip' ) ? 'yes' : 'no';"
}

func install(slug string) string {
	return "plugin install " + sites.BundledPluginDir + "/" + slug + ".zip"
}

func TestProvisioningInstallsDefaultPlugins(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site, job := e.create(t, "Plugins")
		e.runJobs(t)
		if d := e.job(t, job.ID); d.Job.State != store.JobSucceeded {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		want := []string{
			listPlugins,
			bundleCheck(sites.PluginRedisCache), install(sites.PluginRedisCache),
			"plugin activate " + sites.PluginRedisCache,
			"redis enable",
		}
		if got := e.wp.pluginCalls(site.ID); !slices.Equal(got, want) {
			t.Errorf("WP-CLI calls:\n got %q\nwant %q", got, want)
		}
		st := e.wp.state(site.ID)
		if st.plugins[sites.PluginRedisCache] != "active" || st.dropin != redisDropIn {
			t.Errorf("plugins = %v, drop-in %q", st.plugins, st.dropin)
		}
		if st.plugins["akismet"] != "inactive" {
			t.Errorf("the platform touched another plugin: %v", st.plugins)
		}
	})
}

// TestReconcilePlugins covers how a site.reconcile job converges the
// default plugin stack of a site that has drifted from it.
func TestReconcilePlugins(t *testing.T) {
	tests := []struct {
		name  string
		drift func(*wpSite)
		// unbundled plugins are missing from the site image.
		unbundled []string
		wantCalls []string
		wantWarn  string
		want      wpSite
	}{
		{
			name:      "configured site costs one call",
			drift:     func(*wpSite) {},
			wantCalls: []string{listPlugins},
			want:      wpSite{plugins: map[string]string{sites.PluginRedisCache: "active"}, dropin: redisDropIn},
		},
		{
			name:      "deactivated plugin is activated",
			drift:     func(s *wpSite) { s.plugins[sites.PluginRedisCache] = "inactive" },
			wantCalls: []string{listPlugins, "plugin activate " + sites.PluginRedisCache},
			want:      wpSite{plugins: map[string]string{sites.PluginRedisCache: "active"}, dropin: redisDropIn},
		},
		{
			name: "deleted plugin and drop-in are reinstalled",
			drift: func(s *wpSite) {
				delete(s.plugins, sites.PluginRedisCache)
				s.dropin = ""
			},
			wantCalls: []string{
				listPlugins, bundleCheck(sites.PluginRedisCache), install(sites.PluginRedisCache),
				"plugin activate " + sites.PluginRedisCache, "redis enable",
			},
			want: wpSite{plugins: map[string]string{sites.PluginRedisCache: "active"}, dropin: redisDropIn},
		},
		{
			name:      "another object cache drop-in is left alone",
			drift:     func(s *wpSite) { s.dropin = "W3 Total Cache Object Cache" },
			wantCalls: []string{listPlugins},
			wantWarn:  "another object cache drop-in",
			want:      wpSite{plugins: map[string]string{sites.PluginRedisCache: "active"}, dropin: "W3 Total Cache Object Cache"},
		},
		{
			name: "plugin missing from the image is skipped",
			drift: func(s *wpSite) {
				delete(s.plugins, sites.PluginRedisCache)
				s.dropin = ""
			},
			unbundled: []string{sites.PluginRedisCache},
			wantCalls: []string{listPlugins, bundleCheck(sites.PluginRedisCache)},
			wantWarn:  "doesn't bundle the redis-cache plugin",
			want:      wpSite{plugins: map[string]string{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				e := newEnv(t, db)
				site := e.running(t, "Drift")
				e.wp.edit(site.ID, tt.drift)
				for _, slug := range tt.unbundled {
					e.wp.unbundled[slug] = true
				}
				e.wp.pluginCalls(site.ID)
				if _, err := e.svc.EnqueueReconcile(ctx); err != nil {
					t.Fatal(err)
				}
				e.runJobs(t)
				jobs := e.reconcileJobs(t)
				if len(jobs) != 1 {
					t.Fatalf("reconcile jobs = %d", len(jobs))
				}
				d := e.job(t, jobs[0].ID)
				if d.Job.State != store.JobSucceeded {
					t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
				}
				if got := e.wp.pluginCalls(site.ID); !slices.Equal(got, tt.wantCalls) {
					t.Errorf("WP-CLI calls:\n got %q\nwant %q", got, tt.wantCalls)
				}
				warned := false
				for _, l := range d.Logs {
					if l.Level == "warn" && strings.Contains(strings.ToLower(l.Message), strings.ToLower(tt.wantWarn)) {
						warned = true
					}
				}
				if tt.wantWarn != "" && !warned {
					t.Errorf("no warning containing %q in %s", tt.wantWarn, jobLog(d))
				}
				st := e.wp.state(site.ID)
				delete(st.plugins, "akismet")
				if !maps.Equal(st.plugins, tt.want.plugins) || st.dropin != tt.want.dropin {
					t.Errorf("state = %v %q, want %v %q", st.plugins, st.dropin, tt.want.plugins, tt.want.dropin)
				}
				if got := e.site(t, site.ID); got.Status != store.SiteRunning || got.StatusMessage != "" {
					t.Errorf("site = %s %q", got.Status, got.StatusMessage)
				}
			})
		})
	}
}

func TestReconcilePluginsSkipsStoppedSites(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Stopped")
		if _, err := e.svc.Stop(ctx, site.ID, ""); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		e.wp.pluginCalls(site.ID)
		if _, err := e.svc.EnqueueReconcile(ctx); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if got := e.wp.pluginCalls(site.ID); len(got) != 0 {
			t.Errorf("WP-CLI calls on a stopped site: %q", got)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteStopped || got.StatusMessage != "" {
			t.Errorf("site = %s %q", got.Status, got.StatusMessage)
		}
	})
}

// TestPluginFailureFailsProvisioning checks that a plugin step that keeps
// failing (here the object cache refusing the site's user) rolls the new
// site back like any other step.
func TestPluginFailureFailsProvisioning(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		e.wp.failCommand = func(args []string) error {
			if slices.Equal(args, []string{"redis", "enable"}) {
				return &provisioner.ExecError{ExitCode: 1, Stderr: "Error: Object cache could not be enabled. Redis server is unreachable: WRONGPASS"}
			}
			return nil
		}
		site, job := e.create(t, "No Cache")
		e.runJobs(t)
		d := e.job(t, job.ID)
		if d.Job.State != store.JobFailed || !strings.Contains(d.Job.Error, "enabling the object cache") {
			t.Fatalf("job %s: %s", d.Job.State, d.Job.Error)
		}
		if _, ok := e.prov.Site(site.ID); ok {
			t.Error("the stack is still there")
		}
		if got := e.site(t, site.ID); got.Status != store.SiteFailed || !strings.Contains(got.StatusMessage, "WRONGPASS") {
			t.Errorf("site = %s %q", got.Status, got.StatusMessage)
		}
	})
}
