package compose_test

import (
	"bytes"
	"errors"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

const (
	testSiteID   = "01k5z3m8q9r7t6v5w4x3y2z1ab"
	testDBSecret = "db-$ecret'pw"
)

func testSiteRender() compose.SiteRender {
	return compose.SiteRender{
		Site: &provisioner.SiteConfig{
			ID:               testSiteID,
			ClientGroupID:    "01k5z3m8q9r7t6v5w4x3y2z1cg",
			PreviewDomain:    "acme.preview.example.com",
			PHPVersion:       "8.4",
			WPCoreVersion:    "7.1.2",
			EnableCache:      true,
			Resources:        provisioner.DefaultResourceLimits(),
			DBSecretRef:      "01k5z3m8q9r7t6v5w4x3y2z1db",
			WPSaltsSecretRef: "01k5z3m8q9r7t6v5w4x3y2z1ws",
			UID:              20042,
		},
		Images: compose.SiteImages{
			PHP:   "ghcr.io/rchouinard/pressfleet/site-php:php8.4-wp7.1.2",
			Nginx: "ghcr.io/rchouinard/pressfleet/site-nginx:wp7.1.2",
		},
		WPConfig: wpconfig.Params{
			DB: sitedb.Credentials{Database: "site_01k5z3m8", Username: "site_01k5z3m8", Password: testDBSecret},
			Salts: wpconfig.Salts{
				AuthKey: "k1", SecureAuthKey: "k2", LoggedInKey: "k3", NonceKey: "k4",
				AuthSalt: "s1", SecureAuthSalt: "s2", LoggedInSalt: "s3", NonceSalt: "s4",
			},
		},
		Security:        config.DefaultWordPressSecurity(),
		PlatformProject: "pressfleet",
	}
}

func renderSite(t *testing.T, in compose.SiteRender) (string, compose.RenderResult) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sites", in.Site.ID)
	res, err := compose.RenderSite(dir, in)
	if err != nil {
		t.Fatal(err)
	}
	return dir, res
}

func TestRenderSiteGolden(t *testing.T) {
	dir, res := renderSite(t, testSiteRender())
	if !res.ComposeChanged || !res.WPConfigChanged {
		t.Errorf("first render result = %+v, want both changed", res)
	}
	for _, tc := range []struct{ file, golden string }{
		{compose.SiteComposeFile, "site-compose.yaml.golden"},
		{compose.SiteWPConfigFile, "site-wp-config.php.golden"},
	} {
		got, err := os.ReadFile(filepath.Join(dir, tc.file))
		if err != nil {
			t.Fatal(err)
		}
		golden := filepath.Join("testdata", tc.golden)
		if *update {
			if err := os.WriteFile(golden, got, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from %s (run go test -update to accept):\n%s", tc.file, golden, got)
		}
	}
}

// siteFile is the part of compose.yaml the structural test checks.
type siteFile struct {
	Name     string
	Services map[string]struct {
		Image       string
		User        string
		Restart     string
		ReadOnly    bool `yaml:"read_only"`
		Tmpfs       []string
		CapDrop     []string `yaml:"cap_drop"`
		SecurityOpt []string `yaml:"security_opt"`
		Ports       []any
		Networks    []string
		Labels      map[string]string
		Environment map[string]string
		Volumes     []struct {
			Type, Source, Target string
			ReadOnly             bool `yaml:"read_only"`
		}
		Logging struct {
			Driver  string
			Options map[string]string
		}
		Deploy struct {
			Resources struct {
				Limits       struct{ CPUs, Memory string }
				Reservations struct{ Memory string }
			}
		}
		Healthcheck struct{ Test []string }
	}
	Networks map[string]struct {
		Name   string
		Labels map[string]string
	}
	Volumes map[string]struct {
		Labels map[string]string
	}
}

// TestRenderedSiteCompose checks the rendered file's structure rather than
// its text, so the properties that matter survive golden updates.
func TestRenderedSiteCompose(t *testing.T) {
	in := testSiteRender()
	in.Site.Resources = provisioner.ResourceLimits{CPU: "2", MemoryReservationMB: 256, MemoryLimitMB: 1024}
	dir, _ := renderSite(t, in)
	raw, err := os.ReadFile(filepath.Join(dir, compose.SiteComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	var f siteFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	project := "pf-" + testSiteID
	if f.Name != project {
		t.Errorf("name = %q", f.Name)
	}
	if len(f.Services) != 2 {
		t.Fatalf("services = %d, want nginx and php", len(f.Services))
	}
	wantLabels := map[string]string{
		"pressfleet.managed":         "true",
		"pressfleet.site-id":         testSiteID,
		"pressfleet.client-group-id": in.Site.ClientGroupID,
	}
	checkLabels := func(what string, got map[string]string) {
		t.Helper()
		for k, v := range wantLabels {
			if got[k] != v {
				t.Errorf("%s: label %s = %q, want %q", what, k, got[k], v)
			}
		}
	}
	for name, svc := range f.Services {
		if svc.User != "20042:20042" {
			t.Errorf("%s: user = %q", name, svc.User)
		}
		if svc.Restart != "unless-stopped" || !svc.ReadOnly || len(svc.Tmpfs) != 1 || svc.Tmpfs[0] != "/tmp" {
			t.Errorf("%s: restart/read_only/tmpfs = %q %v %v", name, svc.Restart, svc.ReadOnly, svc.Tmpfs)
		}
		if len(svc.CapDrop) != 1 || svc.CapDrop[0] != "ALL" || len(svc.SecurityOpt) != 1 || svc.SecurityOpt[0] != "no-new-privileges:true" {
			t.Errorf("%s: cap_drop = %v, security_opt = %v", name, svc.CapDrop, svc.SecurityOpt)
		}
		if len(svc.Ports) != 0 {
			t.Errorf("%s: publishes ports %v", name, svc.Ports)
		}
		if len(svc.Networks) != 1 || svc.Networks[0] != "site" {
			t.Errorf("%s: networks = %v", name, svc.Networks)
		}
		if svc.Logging.Driver != "json-file" || svc.Logging.Options["max-size"] == "" || svc.Logging.Options["max-file"] == "" {
			t.Errorf("%s: no log rotation: %+v", name, svc.Logging)
		}
		if len(svc.Healthcheck.Test) < 2 || svc.Healthcheck.Test[0] != "CMD" {
			t.Errorf("%s: healthcheck = %v", name, svc.Healthcheck.Test)
		}
		checkLabels(name, svc.Labels)
	}

	php, nginx := f.Services["php"], f.Services["nginx"]
	if php.Image != in.Images.PHP || nginx.Image != in.Images.Nginx {
		t.Errorf("images = %q, %q", php.Image, nginx.Image)
	}
	// Resources: 2 CPUs, 256/1024 MiB split per splitResources.
	for name, want := range map[string][3]string{"php": {"1.5", "896M", "224M"}, "nginx": {"0.5", "128M", "32M"}} {
		r := f.Services[name].Deploy.Resources
		if got := [3]string{r.Limits.CPUs, r.Limits.Memory, r.Reservations.Memory}; got != want {
			t.Errorf("%s resources = %v, want %v", name, got, want)
		}
	}
	if php.Environment["PHP_MEMORY_LIMIT"] != "256M" || php.Environment["PHP_FPM_MAX_CHILDREN"] != "13" {
		t.Errorf("php environment = %v", php.Environment)
	}

	type mount struct {
		source   string
		readOnly bool
	}
	mounts := func(svc string) map[string]mount {
		m := map[string]mount{}
		for _, v := range f.Services[svc].Volumes {
			m[v.Target] = mount{v.Type + ":" + v.Source, v.ReadOnly}
		}
		return m
	}
	wantMounts := map[string]map[string]mount{
		"php": {
			"/var/www/wordpress/wp-content":    {"volume:wp-content", false},
			"/var/cache/pressfleet/fastcgi":    {"volume:cache", false},
			"/var/www/wordpress/wp-config.php": {"bind:./wp-config.php", true},
		},
		"nginx": {
			"/var/www/wordpress/wp-content": {"volume:wp-content", true},
			"/var/cache/pressfleet/fastcgi": {"volume:cache", false},
		},
	}
	for svc, want := range wantMounts {
		got := mounts(svc)
		if len(got) != len(want) {
			t.Errorf("%s mounts = %v, want %v", svc, got, want)
		}
		for target, m := range want {
			if got[target] != m {
				t.Errorf("%s mount %s = %+v, want %+v", svc, target, got[target], m)
			}
		}
	}

	if f.Networks["site"].Name != project {
		t.Errorf("network name = %q, want %q", f.Networks["site"].Name, project)
	}
	checkLabels("network", f.Networks["site"].Labels)
	for _, v := range []string{"wp-content", "cache"} {
		vol, ok := f.Volumes[v]
		if !ok {
			t.Errorf("volume %s not declared", v)
		}
		checkLabels("volume "+v, vol.Labels)
	}

	// No secrets in compose.yaml.
	if bytes.Contains(raw, []byte("db-")) || bytes.Contains(raw, []byte("ecret")) {
		t.Error("compose.yaml contains the database password")
	}
}

func TestRenderSiteEnableCache(t *testing.T) {
	for _, tc := range []struct {
		enable bool
		want   string
	}{
		{true, "on"},
		{false, "off"},
	} {
		in := testSiteRender()
		in.Site.EnableCache = tc.enable
		dir, _ := renderSite(t, in)
		raw, err := os.ReadFile(filepath.Join(dir, compose.SiteComposeFile))
		if err != nil {
			t.Fatal(err)
		}
		var f siteFile
		if err := yaml.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		if got := f.Services["nginx"].Environment["NGINX_FASTCGI_CACHE"]; got != tc.want {
			t.Errorf("EnableCache=%v: NGINX_FASTCGI_CACHE = %q, want %q", tc.enable, got, tc.want)
		}
	}
}

func TestRenderSiteFileModesAndRerender(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("non-root file modes")
	}
	in := testSiteRender()
	dir, _ := renderSite(t, in)
	wpConfig := filepath.Join(dir, compose.SiteWPConfigFile)
	for file, want := range map[string]os.FileMode{
		compose.SiteComposeFile:  0o644,
		compose.SiteWPConfigFile: 0o444,
		".":                      0o700 | os.ModeDir,
	} {
		info, err := os.Stat(filepath.Join(dir, file))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != want {
			t.Errorf("%s mode = %v, want %v", file, info.Mode(), want)
		}
	}
	before, err := os.Stat(wpConfig)
	if err != nil {
		t.Fatal(err)
	}

	// Re-rendering the same input changes nothing, so the driver doesn't
	// recreate containers, and a loosened mode is corrected.
	if err := os.Chmod(wpConfig, 0o666); err != nil {
		t.Fatal(err)
	}
	res, err := compose.RenderSite(dir, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.ComposeChanged || res.WPConfigChanged {
		t.Errorf("unchanged re-render result = %+v", res)
	}
	after, err := os.Stat(wpConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Mode() != 0o444 {
		t.Errorf("unchanged wp-config.php was replaced or kept mode %v", after.Mode())
	}

	// A changed password replaces only wp-config.php.
	in.WPConfig.DB.Password = "rotated"
	if res, err = compose.RenderSite(dir, in); err != nil {
		t.Fatal(err)
	}
	if res.ComposeChanged || !res.WPConfigChanged {
		t.Errorf("password change result = %+v", res)
	}
	if got, _ := os.ReadFile(wpConfig); !bytes.Contains(got, []byte("'rotated'")) {
		t.Error("wp-config.php not updated")
	}

	// A resource change replaces only compose.yaml.
	in.Site.Resources.MemoryLimitMB = 1024
	if res, err = compose.RenderSite(dir, in); err != nil {
		t.Fatal(err)
	}
	if !res.ComposeChanged || res.WPConfigChanged {
		t.Errorf("resource change result = %+v", res)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("site dir has %d entries, want 2 (temporary files left behind?)", len(entries))
	}
}

func TestRenderSiteDBHost(t *testing.T) {
	in := testSiteRender()
	dir, _ := renderSite(t, in)
	got, err := os.ReadFile(filepath.Join(dir, compose.SiteWPConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte("define( 'DB_HOST', 'pf-mariadb:3306' );")) {
		t.Error("default DB host not used")
	}
	in.WPConfig.DBHost = "db:3306"
	dir, _ = renderSite(t, in)
	if got, _ := os.ReadFile(filepath.Join(dir, compose.SiteWPConfigFile)); !bytes.Contains(got, []byte("'db:3306'")) {
		t.Error("DB host override not used")
	}
}

func TestRenderSiteInvalid(t *testing.T) {
	tests := map[string]func(*compose.SiteRender){
		"no site":          func(in *compose.SiteRender) { in.Site = nil },
		"invalid config":   func(in *compose.SiteRender) { in.Site.PreviewDomain = "" },
		"uid out of range": func(in *compose.SiteRender) { in.Site.UID = 1000 },
		"uppercase id":     func(in *compose.SiteRender) { in.Site.ID = "Site1" },
		"id with slash":    func(in *compose.SiteRender) { in.Site.ID = "a/b" },
		"no images":        func(in *compose.SiteRender) { in.Images = compose.SiteImages{PHP: in.Images.PHP} },
		"tiny memory":      func(in *compose.SiteRender) { in.Site.Resources.MemoryLimitMB = 128 },
		"bad cpu":          func(in *compose.SiteRender) { in.Site.Resources.CPU = "lots" },
		"wp-config":        func(in *compose.SiteRender) { in.WPConfig.Salts.NonceSalt = "" },
		"bad constant": func(in *compose.SiteRender) {
			in.WPConfig.Constants = []wpconfig.Constant{{Name: "DB_HOST", Value: "x"}}
		},
		"zero login rate": func(in *compose.SiteRender) { in.Security.LoginRateLimit.RequestsPerMinute = 0 },
		"rule injection": func(in *compose.SiteRender) {
			in.Site.PreviewDomain = "acme.example.com`) || Host(`other.example.com"
		},
		"uppercase domain": func(in *compose.SiteRender) { in.Site.PreviewDomain = "Acme.example.com" },
		"bare domain":      func(in *compose.SiteRender) { in.Site.PreviewDomain = "localhost" },
		"bad platform":     func(in *compose.SiteRender) { in.PlatformProject = "a`b" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			in := testSiteRender()
			mutate(&in)
			dir := filepath.Join(t.TempDir(), "site")
			_, err := compose.RenderSite(dir, in)
			if !errors.Is(err, compose.ErrInvalidSite) {
				t.Fatalf("err = %v, want ErrInvalidSite", err)
			}
			if strings.Contains(err.Error(), "ecret") {
				t.Errorf("error leaks the password: %v", err)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Error("files were written for invalid input")
			}
		})
	}
	if _, err := compose.RenderSite("relative/dir", testSiteRender()); !errors.Is(err, compose.ErrInvalidSite) {
		t.Errorf("relative dir: err = %v", err)
	}
}

// TestRenderSiteNginxSecurity checks that the security set's nginx half
// reaches the site-nginx environment (images/README.md), and php's
// environment stays free of it.
func TestRenderSiteNginxSecurity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.WordPressSecurityConfig)
		want   map[string]string
	}{
		{"defaults", func(*config.WordPressSecurityConfig) {}, map[string]string{
			"NGINX_LOGIN_RATE_LIMIT": "on", "NGINX_LOGIN_RATE": "10r/m", "NGINX_LOGIN_BURST": "10", "NGINX_SECURITY_HEADERS": "on",
		}},
		{"custom rate", func(c *config.WordPressSecurityConfig) {
			c.LoginRateLimit = config.LoginRateLimitConfig{Enabled: true, RequestsPerMinute: 30, Burst: 0}
		}, map[string]string{
			"NGINX_LOGIN_RATE_LIMIT": "on", "NGINX_LOGIN_RATE": "30r/m", "NGINX_LOGIN_BURST": "0", "NGINX_SECURITY_HEADERS": "on",
		}},
		{"off", func(c *config.WordPressSecurityConfig) {
			c.LoginRateLimit.Enabled, c.Headers = false, false
		}, map[string]string{"NGINX_LOGIN_RATE_LIMIT": "off", "NGINX_SECURITY_HEADERS": "off"}},
		{"zero value", func(c *config.WordPressSecurityConfig) { *c = config.WordPressSecurityConfig{} },
			map[string]string{"NGINX_LOGIN_RATE_LIMIT": "off", "NGINX_SECURITY_HEADERS": "off"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := testSiteRender()
			tc.mutate(&in.Security)
			dir, _ := renderSite(t, in)
			raw, err := os.ReadFile(filepath.Join(dir, compose.SiteComposeFile))
			if err != nil {
				t.Fatal(err)
			}
			var f siteFile
			if err := yaml.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			got := f.Services["nginx"].Environment
			delete(got, "NGINX_FASTCGI_CACHE") // TestRenderSiteEnableCache covers it
			if !maps.Equal(got, tc.want) {
				t.Errorf("nginx environment = %v, want %v", got, tc.want)
			}
			for k := range f.Services["php"].Environment {
				if strings.HasPrefix(k, "NGINX_") {
					t.Errorf("php environment has %s", k)
				}
			}
		})
	}
}

// TestRenderSiteTwoFactor checks that only required two-factor turns on the
// must-use plugin module that enforces it; optional two-factor is just the
// plugin, which package sites installs (#43).
func TestRenderSiteTwoFactor(t *testing.T) {
	const constant = "define( 'PRESSFLEET_REQUIRE_TWO_FACTOR', true );"
	for mode, want := range map[string]bool{
		config.TwoFactorOff:               false,
		config.TwoFactorOptional:          false,
		config.TwoFactorRequiredForAdmins: true,
	} {
		t.Run(mode, func(t *testing.T) {
			in := testSiteRender()
			in.Security.TwoFactor = mode
			dir, _ := renderSite(t, in)
			raw, err := os.ReadFile(filepath.Join(dir, compose.SiteWPConfigFile))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(string(raw), constant); got != want {
				t.Errorf("wp-config.php defines PRESSFLEET_REQUIRE_TWO_FACTOR = %v, want %v", got, want)
			}
		})
	}
}

func TestSiteProjectName(t *testing.T) {
	if got := compose.SiteProjectName(testSiteID); got != "pf-"+testSiteID {
		t.Errorf("SiteProjectName = %q", got)
	}
}

// TestRenderSiteAsRoot covers the control plane in its container, where it
// runs as root and gives wp-config.php to the site's UID.
func TestRenderSiteAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	in := testSiteRender()
	dir, _ := renderSite(t, in)
	for _, rerender := range []bool{false, true} {
		if rerender {
			// An unchanged re-render repairs ownership in place.
			if err := os.Lchown(filepath.Join(dir, compose.SiteWPConfigFile), 0, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := compose.RenderSite(dir, in); err != nil {
				t.Fatal(err)
			}
		}
		info, err := os.Stat(filepath.Join(dir, compose.SiteWPConfigFile))
		if err != nil {
			t.Fatal(err)
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			t.Skip("no ownership information")
		}
		if info.Mode() != 0o400 || int(st.Uid) != in.Site.UID || int(st.Gid) != in.Site.UID {
			t.Errorf("wp-config.php mode %v owner %d:%d, want 0400 %d:%d", info.Mode(), st.Uid, st.Gid, in.Site.UID, in.Site.UID)
		}
	}
}
