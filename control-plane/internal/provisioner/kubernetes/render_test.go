package kubernetes

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

const testSiteID = "01hzy3k5q7m9c2v4b6n8r0t1wx"

func testSite() *provisioner.SiteConfig {
	return &provisioner.SiteConfig{
		ID:               testSiteID,
		ClientGroupID:    "01hzy3k0000000000000000000",
		PreviewDomain:    "blog.preview.example.com",
		PHPVersion:       "8.4",
		WPCoreVersion:    "6.9.1",
		DBSecretRef:      "secret/db/site1",
		Resources:        provisioner.DefaultResourceLimits(),
		UID:              provisioner.MinSiteUID,
		WPSaltsSecretRef: "secret/salts/site1",
		RedisSecretRef:   "secret/redis/site1",
		EnableCache:      true,
	}
}

// errNoSecret is fakeSecrets' answer for a reference it doesn't have.
var errNoSecret = errors.New("no such secret")

// fakeSecrets is a secret store in memory.
type fakeSecrets map[secrets.Ref]redact.Secret

func (f fakeSecrets) Get(_ context.Context, ref secrets.Ref) (redact.Secret, error) {
	s, ok := f[ref]
	if !ok {
		return "", fmt.Errorf("secret %s: %w", ref, errNoSecret)
	}
	return s, nil
}

// testSecrets holds testSite's secrets, with fixed salts so that the
// rendered wp-config.php doesn't change between runs.
func testSecrets(t *testing.T) fakeSecrets {
	t.Helper()
	db, err := sitedb.MarshalCredentials(sitedb.Credentials{Database: "site_db", Username: "site_user", Password: "db-password"})
	if err != nil {
		t.Fatal(err)
	}
	salts := wpconfig.Salts{
		AuthKey: "auth-key", SecureAuthKey: "secure-auth-key", LoggedInKey: "logged-in-key", NonceKey: "nonce-key",
		AuthSalt: "auth-salt", SecureAuthSalt: "secure-auth-salt", LoggedInSalt: "logged-in-salt", NonceSalt: "nonce-salt",
	}
	saltsSecret, err := wpconfig.MarshalSalts(salts)
	if err != nil {
		t.Fatal(err)
	}
	redis, err := objectcache.MarshalCredentials(objectcache.Credentials{Username: "site1", Password: "cache-password", Database: 3, KeyPrefix: "site1:"})
	if err != nil {
		t.Fatal(err)
	}
	return fakeSecrets{"secret/db/site1": db, "secret/salts/site1": saltsSecret, "secret/redis/site1": redis}
}

// testImages resolves every version to the same images.
type testImages struct{}

func (testImages) Images(php, wp string) (catalog.Images, error) {
	return catalog.Images{
		PHP:   "ghcr.io/rchouinard/pressfleet/site-php:php" + php + "-wp" + wp,
		Nginx: "ghcr.io/rchouinard/pressfleet/site-nginx:wp" + wp,
	}, nil
}

// testConfig is the driver's configuration for tests: an install whose
// platform services are in the cluster, with an SMTP relay and an image
// pull secret.
func testConfig(t *testing.T) Config {
	t.Helper()
	k := config.DefaultKubernetes()
	k.PodCIDRs = []string{"10.244.0.0/16", "fd00:10:244::/56"}
	k.ImagePullSecrets = []string{"ghcr"}
	k.StorageClasses.Sites = "fast"
	return Config{
		Layout:            config.Layout{Root: t.TempDir()},
		Kubernetes:        k,
		Secrets:           testSecrets(t),
		Images:            testImages{},
		MariaDBAddress:    "mariadb:3306",
		RedisAddress:      "valkey.pressfleet.svc.cluster.local:6379",
		WordPressSecurity: config.DefaultWordPressSecurity(),
		SMTP:              &wpconfig.SMTP{Host: "smtp.example.com", Port: 587, Encryption: "tls"},
		LoginPublicKey:    "bG9naW4tcHVibGljLWtleQ",
		WaitTimeout:       5 * time.Second,
	}
}

// testProvisioner is the driver on fake clientsets, with its data directory
// in a temporary directory.
func testProvisioner(t *testing.T) *Provisioner {
	t.Helper()
	c, _ := fakeClient(t)
	return newWithClient(testConfig(t), c)
}

// golden compares got with testdata/<name>, or rewrites it with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from the golden file (go test -update to accept):\n%s", name, got)
	}
}

// manifestFiles are the files RenderSite writes.
var manifestFiles = []string{
	"deployment.yaml", "namespace.yaml", "network-policies.yaml", "rbac.yaml",
	"routes.yaml", "service.yaml", "volume.yaml", "wp-config.yaml",
}

func TestRenderSite(t *testing.T) {
	p := testProvisioner(t)
	dir, err := p.RenderSite(t.Context(), testSite())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(p.layout.Root, "sites", testSiteID, SiteManifestDir); dir != want {
		t.Errorf("rendered into %s, want %s", dir, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", e.Name(), fi.Mode().Perm())
		}
		content, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		golden(t, e.Name(), content)
	}
	if !slices.Equal(names, manifestFiles) {
		t.Errorf("rendered %q, want %q", names, manifestFiles)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("manifest directory: %v, %v; want mode 0700", fi, err)
	}
	golden(t, "wp-config.php", renderedWPConfig(t, dir))

	// What it renders applies.
	if err := p.ApplyDir(t.Context(), dir); err != nil {
		t.Errorf("applying the rendered manifests: %v", err)
	}
}

// renderedWPConfig decodes wp-config.php from the rendered Secret.
func renderedWPConfig(t *testing.T, dir string) []byte {
	t.Helper()
	obj := renderedObject(t, dir, "Secret", "wp-config")
	enc, _, _ := unstructured.NestedString(obj.Object, "data", "wp-config.php")
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// renderedObject returns one object of the manifests in dir.
func renderedObject(t *testing.T, dir, kind, name string) *unstructured.Unstructured {
	t.Helper()
	objs, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("no %s %s in the manifests", kind, name)
	return nil
}

// Every object is labeled for pruning, in the site's namespace, and every
// pod setting the restricted Pod Security standard asks for is there.
func TestRenderSiteInvariants(t *testing.T) {
	p := testProvisioner(t)
	dir, err := p.RenderSite(t.Context(), testSite())
	if err != nil {
		t.Fatal(err)
	}
	objs, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ns := p.SiteNamespace(testSiteID)
	for _, o := range objs {
		if err := checkRendered(o, ns); err != nil {
			t.Error(err)
		}
	}
	d := renderedObject(t, dir, "Deployment", SiteDeployment)
	if _, found, _ := unstructured.NestedFieldNoCopy(d.Object, "spec", "replicas"); found {
		t.Error("the Deployment sets replicas, which StartSite and StopSite own")
	}
	pod, _, _ := unstructured.NestedMap(d.Object, "spec", "template", "spec")
	for _, f := range []struct {
		path []string
		want any
	}{
		{[]string{"automountServiceAccountToken"}, false},
		{[]string{"securityContext", "runAsNonRoot"}, true},
		{[]string{"securityContext", "runAsUser"}, int64(provisioner.MinSiteUID)},
		{[]string{"securityContext", "runAsGroup"}, int64(provisioner.MinSiteUID)},
		{[]string{"securityContext", "fsGroup"}, int64(provisioner.MinSiteUID)},
		{[]string{"securityContext", "fsGroupChangePolicy"}, "OnRootMismatch"},
		{[]string{"securityContext", "seccompProfile", "type"}, "RuntimeDefault"},
	} {
		if got, _, _ := unstructured.NestedFieldNoCopy(pod, f.path...); fmt.Sprint(got) != fmt.Sprint(f.want) {
			t.Errorf("pod %s = %v, want %v", strings.Join(f.path, "."), got, f.want)
		}
	}
	containers, _, _ := unstructured.NestedSlice(pod, "containers")
	if len(containers) != 2 {
		t.Fatalf("%d containers, want php and nginx", len(containers))
	}
	for _, c := range containers {
		c := c.(map[string]any)
		name := c["name"]
		if v, _, _ := unstructured.NestedBool(c, "securityContext", "readOnlyRootFilesystem"); !v {
			t.Errorf("%s: root filesystem isn't read-only", name)
		}
		if v, found, _ := unstructured.NestedBool(c, "securityContext", "allowPrivilegeEscalation"); !found || v {
			t.Errorf("%s: privilege escalation isn't off", name)
		}
		if drop, _, _ := unstructured.NestedStringSlice(c, "securityContext", "capabilities", "drop"); !slices.Equal(drop, []string{"ALL"}) {
			t.Errorf("%s: drops capabilities %q, want ALL", name, drop)
		}
		for _, probe := range []string{"startupProbe", "readinessProbe"} {
			if cmd, _, _ := unstructured.NestedStringSlice(c, probe, "exec", "command"); !slices.Equal(cmd, []string{"/usr/local/bin/pressfleet-healthcheck"}) {
				t.Errorf("%s: %s runs %q", name, probe, cmd)
			}
		}
	}
}

func TestRenderSiteEnvironment(t *testing.T) {
	p := testProvisioner(t)
	cfg := testSite()
	dir, err := p.RenderSite(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	production := renderedWPConfig(t, dir)
	checksum := func() string {
		d := renderedObject(t, dir, "Deployment", SiteDeployment)
		v, _, _ := unstructured.NestedString(d.Object, "spec", "template", "metadata", "annotations", "pressfleet.io/wp-config-checksum")
		return v
	}
	before := checksum()

	cfg.Environment, cfg.DisableMail = provisioner.EnvironmentStaging, true
	if _, err := p.RenderSite(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	staging := string(renderedWPConfig(t, dir))
	for _, want := range []string{"define( 'WP_ENVIRONMENT_TYPE', 'staging' );", "define( 'PRESSFLEET_DISABLE_MAIL', true );"} {
		if !strings.Contains(staging, want) || strings.Contains(string(production), want) {
			t.Errorf("%s is in the staging wp-config.php only: staging %v, production %v",
				want, strings.Contains(staging, want), strings.Contains(string(production), want))
		}
	}
	// The pod rolls to see it: wp-config.php is mounted as one file.
	if after := checksum(); after == before || len(after) != 64 {
		t.Errorf("wp-config checksum %q, then %q: want a new SHA-256", before, after)
	}
}

func TestRenderSiteIsIdempotent(t *testing.T) {
	p := testProvisioner(t)
	dir, err := p.RenderSite(t.Context(), testSite())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "namespace.yaml")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	// A manifest whose template is gone, and a file that isn't one.
	stale, notes := filepath.Join(dir, "removed.yaml"), filepath.Join(dir, "notes.txt")
	for _, f := range []string{stale, notes} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.RenderSite(t.Context(), testSite()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(file); err != nil || !fi.ModTime().Equal(old) {
		t.Errorf("the unchanged manifest was rewritten: %v, %v", fi, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stale manifest is still there: %v", err)
	}
	if _, err := os.Stat(notes); err != nil {
		t.Errorf("a file that isn't a manifest was removed: %v", err)
	}
}

func TestRenderSiteRejects(t *testing.T) {
	long := testSite()
	long.ID = strings.Repeat("a", 61) // pf- + 61 is past the 63 of a namespace
	invalid := testSite()
	invalid.PHPVersion = ""
	tooSmall := testSite()
	tooSmall.Resources.MemoryLimitMB = 128
	tooSmall.Resources.MemoryReservationMB = 64
	missingSecret := testSite()
	missingSecret.DBSecretRef = "secret/db/missing"
	for name, tt := range map[string]struct {
		cfg    *provisioner.SiteConfig
		mutate func(*Config)
		want   error
	}{
		"site ID too long for a namespace": {cfg: long, want: ErrInvalidSite},
		"invalid config":                   {cfg: invalid, want: ErrInvalidSite},
		"no config":                        {cfg: nil, want: ErrInvalidSite},
		"resources below the minimum":      {cfg: tooSmall, want: ErrInvalidSite},
		"missing secret":                   {cfg: missingSecret, want: errNoSecret},
		"no pod CIDRs":                     {cfg: testSite(), mutate: func(c *Config) { c.Kubernetes.PodCIDRs = nil }, want: ErrInvalidSite},
		"no MariaDB":                       {cfg: testSite(), mutate: func(c *Config) { c.MariaDBAddress = "" }, want: ErrInvalidSite},
		"bad MariaDB":                      {cfg: testSite(), mutate: func(c *Config) { c.MariaDBAddress = "mariadb" }, want: ErrInvalidSite},
		"object cache without Valkey":      {cfg: testSite(), mutate: func(c *Config) { c.RedisAddress = "" }, want: ErrInvalidSite},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			c, _ := fakeClient(t)
			p := newWithClient(cfg, c)
			if _, err := p.RenderSite(t.Context(), tt.cfg); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if _, err := os.Stat(p.layout.SitesDir()); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a rejected site left files: %v", err)
			}
		})
	}
}

func TestTemplateFingerprint(t *testing.T) {
	p := testProvisioner(t)
	fp := p.TemplateFingerprint()
	if !strings.HasPrefix(fp, "r2-") || len(fp) != len("r2-")+16 {
		t.Errorf("TemplateFingerprint = %q, want r2-<16 hex digits>", fp)
	}
	if again := testProvisioner(t).TemplateFingerprint(); again != fp {
		t.Errorf("fingerprint changed: %q, then %q", fp, again)
	}
}

func TestResources(t *testing.T) {
	// The defaults: nginx 0.25 CPU, 16/64 MiB and php 0.75 CPU, 112/448
	// MiB (provisioner.SplitResources).
	split, err := provisioner.SplitResources(provisioner.DefaultResourceLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		in   provisioner.ContainerResources
		want containerResources
	}{
		{split.PHP, containerResources{CPURequest: "75m", CPULimit: "750m", MemoryRequest: "112Mi", MemoryLimit: "448Mi"}},
		{split.Nginx, containerResources{CPURequest: "25m", CPULimit: "250m", MemoryRequest: "16Mi", MemoryLimit: "64Mi"}},
		{provisioner.ContainerResources{CPUs: "0.05", MemoryReservationMB: 8, MemoryLimitMB: 32}, containerResources{CPURequest: "10m", CPULimit: "50m", MemoryRequest: "8Mi", MemoryLimit: "32Mi"}},
	} {
		got, err := resources(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("resources(%+v) = %+v, %v; want %+v", tt.in, got, err, tt.want)
		}
	}
}
