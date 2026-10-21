package platform_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

const (
	testPassword      = "s3cret-$root-\"pw\""
	testRedisPassword = "r3dis-admin-pw"
	testSSHHostKey    = "-----BEGIN OPENSSH PRIVATE KEY-----\nZ2F0ZXdheS1rZXk=\n-----END OPENSSH PRIVATE KEY-----\n"
)

func testConfig() platform.Config {
	return platform.Config{
		DataDir: "/var/lib/pressfleet",
		Images: platform.Images{
			ControlPlane: "ghcr.io/rchouinard/pressfleet/control-plane:v1.2.3",
			MariaDB:      "docker.io/library/mariadb:12.3.3@sha256:2f4f46f80e7251990bfdc601ae3f7f2964ddb4dc1868f7deca699fc1ece8c35e",
			Redis:        "docker.io/valkey/valkey:9.1.2@sha256:ac9c858b86dc81f3482d53a9d854547379a5f41e012500f839ab257c399063e0",
			Traefik:      "docker.io/library/traefik:v3.7.13@sha256:3429c14149401de2ac82fc72ddc6a92642332b90deb3012301ff211b9d2d0f18",
			SocketProxy:  "docker.io/wollomatic/socket-proxy:1.13.1@sha256:14b0afd0939218e02736794447868b7d5e1d5b231207b727a935845236f08507",

			VictoriaMetrics: "docker.io/victoriametrics/victoria-metrics:v1.153.0@sha256:8d4ccd756bdb0d0fc3f1e7d3664ff188ec49cdf0ef0d685d0d557674f49702c8",
		},
		MariaDBRootPassword: testPassword,
		RedisAdminPassword:  testRedisPassword,
		SSHHostKey:          testSSHHostKey,
		MasterKeyFile:       "/etc/pressfleet/master.key",
		PreviewBaseDomain:   "preview.agency.example",
		PublicIP:            "203.0.113.7",
		DashboardDomain:     "fleet.agency.example",
		ACME:                platform.ACMEConfig{Email: "ops@agency.example"},
	}
}

func render(t *testing.T, cfg platform.Config) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "platform")
	if err := platform.Render(dir, cfg); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRenderGolden(t *testing.T) {
	dir := render(t, testConfig())
	for _, tc := range []struct{ file, golden string }{
		{platform.ComposeFile, "compose.yaml.golden"},
		{platform.MariaDBConfigFile, "pressfleet.cnf.golden"},
		{platform.RedisConfigFile, "valkey.conf.golden"},
		{platform.TraefikConfigFile, "traefik.yaml.golden"},
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

func TestRenderFileModesAndSecrets(t *testing.T) {
	dir := render(t, testConfig())

	secret, err := os.ReadFile(filepath.Join(dir, platform.MariaDBRootPasswordSecret))
	if err != nil {
		t.Fatal(err)
	}
	if string(secret) != testPassword {
		t.Errorf("secret file = %q", secret)
	}
	secret, err = os.ReadFile(filepath.Join(dir, platform.RedisAdminPasswordSecret))
	if err != nil {
		t.Fatal(err)
	}
	if string(secret) != testRedisPassword {
		t.Errorf("Redis secret file = %q", secret)
	}

	for path, want := range map[string]os.FileMode{
		platform.ComposeFile:               0o644,
		platform.MariaDBConfigFile:         0o644,
		platform.TraefikConfigFile:         0o644,
		platform.MariaDBRootPasswordSecret: 0o600,
		platform.RedisConfigFile:           0o644,
		platform.RedisAdminPasswordSecret:  0o600,
		platform.SSHHostKeySecret:          0o600,
		"secrets":                          0o700 | os.ModeDir,
	} {
		info, err := os.Stat(filepath.Join(dir, path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != want {
			t.Errorf("%s mode = %v, want %v", path, info.Mode(), want)
		}
	}

	// Each password must only appear in its secret file.
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.HasSuffix(path, platform.MariaDBRootPasswordSecret) && bytes.Contains(content, []byte("s3cret")) {
			t.Errorf("%s contains the root password", path)
		}
		if !strings.HasSuffix(path, platform.RedisAdminPasswordSecret) && bytes.Contains(content, []byte(testRedisPassword)) {
			t.Errorf("%s contains the Redis admin password", path)
		}
		if !strings.HasSuffix(path, platform.SSHHostKeySecret) && bytes.Contains(content, []byte("Z2F0ZXdheS1rZXk")) {
			t.Errorf("%s contains the SFTP host key", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRenderReplacesLooseSecretFile(t *testing.T) {
	dir := render(t, testConfig())
	path := filepath.Join(dir, platform.MariaDBRootPasswordSecret)
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.MariaDBRootPassword = "rotated"
	if err := platform.Render(dir, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != 0o600 {
		t.Errorf("mode after re-render = %v", info.Mode())
	}
	if got, _ := os.ReadFile(path); string(got) != "rotated" {
		t.Errorf("secret after re-render = %q", got)
	}
	// No temporary files are left behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("secrets dir has %d entries, want 3", len(entries))
	}
}

// TestRenderedCompose checks the rendered file's structure rather than its
// text, so the properties that matter survive golden updates.
func TestRenderedCompose(t *testing.T) {
	cfg := testConfig()
	cfg.DataDir = "/srv/press$fleet"
	cfg.ProjectName = "pftest"
	cfg.MariaDBMemoryMB = 1024
	dir := render(t, cfg)

	raw, err := os.ReadFile(filepath.Join(dir, platform.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	type mount struct {
		Type, Source, Target string
		ReadOnly             bool `yaml:"read_only"`
	}
	var file struct {
		Name     string
		Services map[string]struct {
			Image       string
			Restart     string
			Environment map[string]string
			Labels      map[string]string
			Ports       []string
			Volumes     []mount
			Networks    []string
			ReadOnly    bool     `yaml:"read_only"`
			CapDrop     []string `yaml:"cap_drop"`
			Logging     struct {
				Options map[string]string
			}
			Deploy struct {
				Resources struct {
					Limits struct{ Memory string }
				}
			}
			Healthcheck struct{ Test []string }
		}
		Secrets  map[string]struct{ File string }
		Networks map[string]struct{ Internal bool }
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.Name != "pftest" {
		t.Errorf("name = %q", file.Name)
	}
	if len(file.Services) != 7 {
		t.Errorf("services = %d, want 7", len(file.Services))
	}
	// Every service is on the platform network except the socket proxy,
	// which only Traefik can reach. The control plane is on the backup
	// network too, where a self-hosted S3 service may be.
	wantNetworks := map[string][]string{
		"control-plane": {"platform", "backup"},
		"mariadb":       {"platform"},
		"redis":         {"platform"},
		"traefik":       {"platform", "socket-proxy"},
		"socket-proxy":  {"socket-proxy"},
		"maintenance":   {"platform"},
		// Platform network only: no published ports, no route.
		"victoria-metrics": {"platform"},
	}
	for _, n := range []string{"socket-proxy", "backup"} {
		if !file.Networks[n].Internal {
			t.Errorf("%s network is not internal", n)
		}
	}
	if got := platform.BackupNetwork(cfg.ProjectName); got != cfg.ProjectName+"_backup" {
		t.Errorf("BackupNetwork = %q", got)
	}
	socketMounts := map[string]bool{} // service -> read-only
	for name, svc := range file.Services {
		if svc.Restart != "unless-stopped" {
			t.Errorf("%s: restart = %q", name, svc.Restart)
		}
		if svc.Logging.Options["max-size"] == "" || svc.Logging.Options["max-file"] == "" {
			t.Errorf("%s: no log rotation", name)
		}
		if svc.Deploy.Resources.Limits.Memory == "" {
			t.Errorf("%s: no memory limit", name)
		}
		if len(svc.Healthcheck.Test) == 0 {
			t.Errorf("%s: no healthcheck", name)
		}
		if !slices.Equal(svc.Networks, wantNetworks[name]) {
			t.Errorf("%s: networks = %v, want %v", name, svc.Networks, wantNetworks[name])
		}
		for _, v := range svc.Volumes {
			if v.Source == "/var/run/docker.sock" {
				socketMounts[name] = v.ReadOnly
			}
		}
		switch {
		case name == "control-plane":
			// Only the SFTP gateway (§5.8); the API goes through Traefik.
			if want := []string{"2222:2222/tcp"}; !slices.Equal(svc.Ports, want) {
				t.Errorf("control-plane publishes ports %v, want %v", svc.Ports, want)
			}
		case len(svc.Ports) > 0 && name != "traefik":
			t.Errorf("%s publishes ports %v; only Traefik may", name, svc.Ports)
		}
	}
	// Only the control plane has read-write access to the Docker socket
	// (§6). Traefik doesn't mount it at all; the socket proxy mounts it
	// read-only (and refuses write requests, see TestRenderedTraefikConfig).
	if want := map[string]bool{"control-plane": false, "socket-proxy": true}; !maps.Equal(socketMounts, want) {
		t.Errorf("docker socket mounts (service: read-only) = %v, want %v", socketMounts, want)
	}
	for _, name := range []string{"traefik", "socket-proxy", "maintenance", "victoria-metrics"} {
		svc := file.Services[name]
		if !svc.ReadOnly || !slices.Equal(svc.CapDrop, []string{"ALL"}) {
			t.Errorf("%s: read_only = %v, cap_drop = %v", name, svc.ReadOnly, svc.CapDrop)
		}
	}
	// The maintenance page service (#44) runs the control plane image with
	// nothing mounted: no data, secrets, or Docker socket.
	if m := file.Services["maintenance"]; m.Image != cfg.Images.ControlPlane || len(m.Volumes) != 0 || len(m.Environment) != 1 {
		t.Errorf("maintenance service = %+v", m)
	}
	// Traefik reads the file provider directory, where the control plane
	// writes maintenance routes, read-only.
	dynamic := false
	for _, v := range file.Services["traefik"].Volumes {
		if v.Source == "./"+platform.TraefikDynamicDir && v.Target == "/etc/traefik/dynamic" && v.ReadOnly {
			dynamic = true
		}
	}
	if !dynamic {
		t.Errorf("traefik doesn't mount %s: %+v", platform.TraefikDynamicDir, file.Services["traefik"].Volumes)
	}
	if fi, err := os.Stat(filepath.Join(dir, platform.TraefikDynamicDir)); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("%s: %v, %v; want mode 0755", platform.TraefikDynamicDir, fi, err)
	}
	tr := file.Services["traefik"]
	if want := []string{"80:80/tcp", "443:443/tcp", "443:443/udp"}; !slices.Equal(tr.Ports, want) {
		t.Errorf("traefik ports = %v, want %v", tr.Ports, want)
	}

	cp := file.Services["control-plane"]
	// '$' is escaped for Compose; YAML itself sees "$$".
	const escaped = "/srv/press$$fleet"
	if cp.Environment["PRESSFLEET_DATA_DIR"] != escaped {
		t.Errorf("PRESSFLEET_DATA_DIR = %q", cp.Environment["PRESSFLEET_DATA_DIR"])
	}
	found := false
	for _, v := range cp.Volumes {
		if v.Source == escaped && v.Target == escaped && v.Type == "bind" {
			found = true
		}
	}
	if !found {
		t.Errorf("data dir not bind-mounted at the identical path: %+v", cp.Volumes)
	}
	if cp.Deploy.Resources.Limits.Memory != "192M" {
		t.Errorf("control plane memory = %q", cp.Deploy.Resources.Limits.Memory)
	}
	db := file.Services["mariadb"]
	if db.Deploy.Resources.Limits.Memory != "1024M" {
		t.Errorf("mariadb memory = %q", db.Deploy.Resources.Limits.Memory)
	}
	if db.Environment["MARIADB_ROOT_PASSWORD_FILE"] == "" {
		t.Error("mariadb root password not supplied through a file")
	}
	if file.Secrets["mariadb_root_password"].File != "./"+platform.MariaDBRootPasswordSecret {
		t.Errorf("secrets = %+v", file.Secrets)
	}
	// The master key file is mounted from where the operator keeps it.
	if file.Secrets["master_key"].File != "/etc/pressfleet/master.key" ||
		cp.Environment["PRESSFLEET_MASTER_KEY_FILE"] != platform.MasterKeySecretPath {
		t.Errorf("master key: secrets = %+v, env = %q", file.Secrets, cp.Environment["PRESSFLEET_MASTER_KEY_FILE"])
	}
	// The control plane learns the platform project name (to find the
	// MariaDB container) and the preview base domain.
	if cp.Environment["PRESSFLEET_PLATFORM_PROJECT"] != "pftest" ||
		cp.Environment["PRESSFLEET_PREVIEW_BASE_DOMAIN"] != "preview.agency.example" {
		t.Errorf("control plane environment = %v", cp.Environment)
	}
	if want := platform.MariaDBContainer("pftest"); want != "pftest-mariadb-1" {
		t.Errorf("MariaDB container = %q", want)
	}
	if want := platform.TraefikContainer("pftest"); want != "pftest-traefik-1" {
		t.Errorf("Traefik container = %q", want)
	}
	// The dashboard domain reaches the control plane through Traefik.
	if cp.Environment["PRESSFLEET_DASHBOARD_DOMAIN"] != "fleet.agency.example" {
		t.Errorf("PRESSFLEET_DASHBOARD_DOMAIN = %q", cp.Environment["PRESSFLEET_DASHBOARD_DOMAIN"])
	}
	for k, want := range map[string]string{
		ingress.PlatformLabel:                                         "pftest",
		"traefik.enable":                                              "true",
		"traefik.docker.network":                                      "pftest_platform",
		"traefik.http.routers.pf-dashboard.rule":                      "Host(`fleet.agency.example`)",
		"traefik.http.routers.pf-dashboard.entrypoints":               ingress.EntryPointHTTPS,
		"traefik.http.routers.pf-dashboard.tls.certresolver":          ingress.CertResolverHTTP,
		"traefik.http.services.pf-dashboard.loadbalancer.server.port": "8080",
	} {
		if got := cp.Labels[k]; got != want {
			t.Errorf("control plane label %s = %q, want %q", k, got, want)
		}
	}

	cnf, err := os.ReadFile(filepath.Join(dir, platform.MariaDBConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^innodb_buffer_pool_size = 512M$`).Match(cnf) {
		t.Errorf("buffer pool not scaled to 1024 MiB:\n%s", cnf)
	}
}

func TestRenderInvalidConfig(t *testing.T) {
	tests := map[string]func(*platform.Config){
		"relative data dir":  func(c *platform.Config) { c.DataDir = "var/lib/pressfleet" },
		"unclean data dir":   func(c *platform.Config) { c.DataDir = "/var/lib/pressfleet/" },
		"no images":          func(c *platform.Config) { c.Images = platform.Images{} },
		"no password":        func(c *platform.Config) { c.MariaDBRootPassword = "" },
		"no redis password":  func(c *platform.Config) { c.RedisAdminPassword = "" },
		"multiline redis":    func(c *platform.Config) { c.RedisAdminPassword = "a\nb" },
		"tiny redis":         func(c *platform.Config) { c.RedisMaxMemoryMB = 16 },
		"no metrics image":   func(c *platform.Config) { c.Images.VictoriaMetrics = "" },
		"tiny metrics":       func(c *platform.Config) { c.VictoriaMetricsMemoryMB = 32 },
		"negative retention": func(c *platform.Config) { c.MetricsRetentionDays = -1 },
		"huge retention":     func(c *platform.Config) { c.MetricsRetentionDays = config.MaxMetricsRetentionDays + 1 },
		"no redis image":     func(c *platform.Config) { c.Images.Redis = "" },
		"multiline pw":       func(c *platform.Config) { c.MariaDBRootPassword = "a\nb" },
		"tiny mariadb":       func(c *platform.Config) { c.MariaDBMemoryMB = 128 },
		"tiny cp":            func(c *platform.Config) { c.ControlPlaneMemoryMB = 16 },
		"no master key":      func(c *platform.Config) { c.MasterKeyFile = "" },
		"relative key file":  func(c *platform.Config) { c.MasterKeyFile = "master.key" },
		"key in data dir":    func(c *platform.Config) { c.MasterKeyFile = "/var/lib/pressfleet/master.key" },
		"relative config":    func(c *platform.Config) { c.ConfigFile = "pressfleet.yaml" },
		"bad preview base":   func(c *platform.Config) { c.PreviewBaseDomain = "*.preview.example" },
		"bad dashboard":      func(c *platform.Config) { c.DashboardDomain = "https://fleet.example" },
		"bad public ip":      func(c *platform.Config) { c.PublicIP = "fleet.example" },
		"bad listen":         func(c *platform.Config) { c.Listen = "8080" },
		"tiny traefik":       func(c *platform.Config) { c.TraefikMemoryMB = 16 },
		"bad acme email":     func(c *platform.Config) { c.ACME.Email = "ops" },
		"http acme server":   func(c *platform.Config) { c.ACME.CAServer = "http://pebble:14000/dir" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			mutate(&cfg)
			dir := filepath.Join(t.TempDir(), "platform")
			err := platform.Render(dir, cfg)
			if !errors.Is(err, platform.ErrInvalidConfig) {
				t.Fatalf("err = %v, want ErrInvalidConfig", err)
			}
			if strings.Contains(err.Error(), "s3cret") {
				t.Errorf("error leaks the password: %v", err)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("files were written for an invalid config")
			}
		})
	}
}

func TestDefaultImages(t *testing.T) {
	images, err := platform.DefaultImages("v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if images.ControlPlane != "ghcr.io/rchouinard/pressfleet/control-plane:v1.2.3" {
		t.Errorf("control plane image = %q", images.ControlPlane)
	}
	// Third-party images must be pinned as tag@sha256:digest (§4.7).
	pinned := regexp.MustCompile(`^[a-z0-9./-]+:[0-9A-Za-z._-]+@sha256:[0-9a-f]{64}$`)
	for name, img := range map[string]string{
		"mariadb": images.MariaDB, "redis": images.Redis, "traefik": images.Traefik, "socket-proxy": images.SocketProxy,
		"victoria-metrics": images.VictoriaMetrics,
	} {
		if !pinned.MatchString(img) {
			t.Errorf("%s image not pinned by digest: %q", name, img)
		}
	}
	if _, err := platform.DefaultImages(""); !errors.Is(err, platform.ErrInvalidConfig) {
		t.Errorf("empty version: err = %v", err)
	}
}

func TestRenderWithoutDomains(t *testing.T) {
	cfg := testConfig()
	cfg.PreviewBaseDomain = ""
	cfg.DashboardDomain = ""
	raw, err := os.ReadFile(filepath.Join(render(t, cfg), platform.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"PRESSFLEET_PREVIEW_BASE_DOMAIN", "PRESSFLEET_DASHBOARD_DOMAIN", "pf-dashboard"} {
		if bytes.Contains(raw, []byte(s)) {
			t.Errorf("%s rendered without domains:\n%s", s, raw)
		}
	}
}

// The SMTP settings reach the control plane's environment intact, and only
// when a relay is configured.
func TestRenderSMTP(t *testing.T) {
	envOf := func(cfg platform.Config) map[string]string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(render(t, cfg), platform.ComposeFile))
		if err != nil {
			t.Fatal(err)
		}
		var f struct {
			Services map[string]struct{ Environment map[string]string }
		}
		if err := yaml.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		return f.Services["control-plane"].Environment
	}
	for name := range envOf(testConfig()) {
		if strings.HasPrefix(name, "PRESSFLEET_SMTP_") {
			t.Errorf("%s rendered without a relay", name)
		}
	}

	for _, smtp := range []config.SMTPConfig{
		{Host: "smtp.example.com", Port: 2525, Username: "relay", Password: "pfenc1:0123:c2VhbGVk",
			Encryption: "tls", FromEmail: "sites@agency.example", FromName: `Agency "$ites"`},
		{Host: "10.0.0.5", Encryption: "none"},
	} {
		cfg := testConfig()
		cfg.SMTP = smtp
		env := envOf(cfg)
		got := config.Default()
		// Compose turns the template's escaped "$$" back into "$".
		lookup := func(k string) (string, bool) { v, ok := env[k]; return strings.ReplaceAll(v, "$$", "$"), ok }
		if err := config.ApplyEnv(&got, lookup); err != nil {
			t.Fatal(err)
		}
		if got.SMTP != smtp {
			t.Errorf("SMTP read back from the environment = %+v, want %+v", got.SMTP, smtp)
		}
	}

	cfg := testConfig()
	cfg.SMTP = config.SMTPConfig{Host: "smtp.example.com", Encryption: "starttls", Username: "relay", Password: "plaintext"}
	if err := platform.Render(t.TempDir(), cfg); !errors.Is(err, platform.ErrInvalidConfig) {
		t.Errorf("plaintext SMTP password: err = %v, want ErrInvalidConfig", err)
	}
}

// The config file reaches the control plane as a secret, and a change to its
// content changes the control plane's definition, so Compose recreates it.
func TestRenderConfigFile(t *testing.T) {
	type service struct {
		Environment map[string]string
		Labels      map[string]string
		Secrets     []string
	}
	type composeFile struct {
		Services map[string]service
		Secrets  map[string]struct{ File string }
	}
	parse := func(cfg platform.Config) composeFile {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(render(t, cfg), platform.ComposeFile))
		if err != nil {
			t.Fatal(err)
		}
		var f composeFile
		if err := yaml.Unmarshal(raw, &f); err != nil {
			t.Fatal(err)
		}
		return f
	}

	none := parse(testConfig()).Services["control-plane"]
	if _, ok := none.Environment["PRESSFLEET_CONFIG"]; ok {
		t.Error("PRESSFLEET_CONFIG rendered without a config file")
	}
	if slices.Contains(none.Secrets, "pressfleet_config") {
		t.Error("config secret rendered without a config file")
	}

	path := filepath.Join(t.TempDir(), "pressfleet.yaml")
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig()
	cfg.ConfigFile = path
	write("audit:\n  retention_days: 30\n")
	f := parse(cfg)
	cp := f.Services["control-plane"]
	if got := cp.Environment["PRESSFLEET_CONFIG"]; got != platform.ConfigSecretPath {
		t.Errorf("PRESSFLEET_CONFIG = %q, want %q", got, platform.ConfigSecretPath)
	}
	if !slices.Contains(cp.Secrets, "pressfleet_config") {
		t.Errorf("control plane secrets = %v, want pressfleet_config", cp.Secrets)
	}
	if got := f.Secrets["pressfleet_config"].File; got != path {
		t.Errorf("config secret file = %q, want %q", got, path)
	}
	if cp.Labels["traefik.enable"] != "true" {
		t.Errorf("dashboard labels lost next to the config hash: %v", cp.Labels)
	}
	first := cp.Labels[platform.ConfigHashLabel]
	if len(first) != 64 {
		t.Fatalf("config hash label = %q, want a SHA-256", first)
	}
	if again := parse(cfg).Services["control-plane"].Labels[platform.ConfigHashLabel]; again != first {
		t.Errorf("config hash changed without a change to the file: %q, then %q", first, again)
	}
	write("audit:\n  retention_days: 60\n")
	if changed := parse(cfg).Services["control-plane"].Labels[platform.ConfigHashLabel]; changed == first {
		t.Error("config hash didn't change with the file's content")
	}

	cfg.DashboardDomain = ""
	if got := parse(cfg).Services["control-plane"].Labels[platform.ConfigHashLabel]; got == "" {
		t.Error("config hash label missing without a dashboard domain")
	}

	cfg.ConfigFile = filepath.Join(t.TempDir(), "missing.yaml")
	if err := platform.Render(t.TempDir(), cfg); err == nil {
		t.Error("rendered with a missing config file")
	}
}

// TestRenderedTraefikConfig checks the properties of Traefik's static
// configuration that the rest of PressFleet relies on.
func TestRenderedTraefikConfig(t *testing.T) {
	tests := map[string]struct {
		acme       platform.ACMEConfig
		wantServer string
	}{
		"lets encrypt": {wantServer: platform.LetsEncryptCAServer},
		"pebble": {
			acme:       platform.ACMEConfig{CAServer: "https://pebble:14000/dir"},
			wantServer: "https://pebble:14000/dir",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			cfg.ProjectName = "pftest"
			cfg.ACME = tc.acme
			tr := readTraefikConfig(t, render(t, cfg))

			web, websecure := tr.EntryPoints[ingress.EntryPointHTTP], tr.EntryPoints[ingress.EntryPointHTTPS]
			if web.Address != ":80" || web.HTTP.Redirections.EntryPoint.To != ingress.EntryPointHTTPS ||
				web.HTTP.Redirections.EntryPoint.Scheme != "https" || !web.HTTP.Redirections.EntryPoint.Permanent {
				t.Errorf("web entry point = %+v", web)
			}
			if websecure.Address != ":443" || !websecure.AsDefault || websecure.HTTP3 == nil || websecure.HTTP.TLS == nil {
				t.Errorf("websecure entry point = %+v", websecure)
			}
			internal := tr.EntryPoints[ingress.EntryPointInternal]
			if internal.Address != fmt.Sprintf("127.0.0.1:%d", ingress.InternalPort) ||
				tr.Ping.EntryPoint != ingress.EntryPointInternal ||
				tr.Metrics.Prometheus.EntryPoint != ingress.EntryPointInternal ||
				!tr.Metrics.Prometheus.AddRoutersLabels {
				t.Errorf("internal entry point %+v, ping %+v, metrics %+v", internal, tr.Ping, tr.Metrics)
			}
			if tr.AccessLog == nil || tr.AccessLog.Fields.Headers.DefaultMode != "drop" {
				t.Errorf("access log = %+v, want enabled without headers", tr.AccessLog)
			}
			d := tr.Providers.Docker
			if d.Endpoint != "tcp://socket-proxy:2375" || d.ExposedByDefault ||
				d.Constraints != "Label(`"+ingress.PlatformLabel+"`, `pftest`)" {
				t.Errorf("docker provider = %+v", d)
			}
			resolver := tr.CertificatesResolvers[ingress.CertResolverHTTP].ACME
			if resolver.CAServer != tc.wantServer || resolver.HTTPChallenge.EntryPoint != ingress.EntryPointHTTP ||
				resolver.Storage == "" {
				t.Errorf("%s resolver = %+v", ingress.CertResolverHTTP, resolver)
			}
		})
	}
}

// traefikConfig is the part of Traefik's static configuration the tests
// check.
type traefikConfig struct {
	EntryPoints map[string]struct {
		Address   string
		AsDefault bool `yaml:"asDefault"`
		HTTP      struct {
			Redirections struct {
				EntryPoint struct {
					To, Scheme string
					Permanent  bool
				} `yaml:"entryPoint"`
			}
			TLS *struct {
				CertResolver string `yaml:"certResolver"`
			}
		}
		HTTP3 *struct{} `yaml:"http3"`
	} `yaml:"entryPoints"`
	Ping struct {
		EntryPoint string `yaml:"entryPoint"`
	}
	Metrics struct {
		Prometheus struct {
			EntryPoint       string `yaml:"entryPoint"`
			AddRoutersLabels bool   `yaml:"addRoutersLabels"`
		}
	}
	AccessLog *struct {
		Fields struct {
			Headers struct {
				DefaultMode string `yaml:"defaultMode"`
			}
		}
	} `yaml:"accessLog"`
	Providers struct {
		Docker struct {
			Endpoint         string
			ExposedByDefault bool `yaml:"exposedByDefault"`
			Constraints      string
		}
		File *struct {
			Directory string
			Watch     bool
		}
	}
	CertificatesResolvers map[string]struct {
		ACME struct {
			Email         string
			CAServer      string `yaml:"caServer"`
			Storage       string
			HTTPChallenge struct {
				EntryPoint string `yaml:"entryPoint"`
			} `yaml:"httpChallenge"`
			DNSChallenge *struct {
				Provider  string
				Resolvers []string
			} `yaml:"dnsChallenge"`
		}
	} `yaml:"certificatesResolvers"`
}

func readTraefikConfig(t *testing.T, dir string) traefikConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, platform.TraefikConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	var tr traefikConfig
	if err := yaml.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	return tr
}

// The control plane's default platform_project matches the project name
// the host CLI installs the platform under.
func TestDefaultPlatformProject(t *testing.T) {
	if got := config.Default().PlatformProject; got != platform.ProjectName {
		t.Errorf("config default platform_project = %q, want %q", got, platform.ProjectName)
	}
}

func TestProject(t *testing.T) {
	p := platform.Project("/var/lib/pressfleet/platform")
	if p.Name != platform.ProjectName || p.Dir != "/var/lib/pressfleet/platform" {
		t.Errorf("project = %+v", p)
	}
}

// TestRenderedRedis checks the object cache's properties that matter for
// isolation and the memory budget (§4.1, §6).
func TestRenderedRedis(t *testing.T) {
	cfg := testConfig()
	cfg.RedisMaxMemoryMB = 128
	dir := render(t, cfg)

	raw, err := os.ReadFile(filepath.Join(dir, platform.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Services map[string]struct {
			Image    string
			User     string
			ReadOnly bool     `yaml:"read_only"`
			CapDrop  []string `yaml:"cap_drop"`
			Secrets  []string
			Deploy   struct {
				Resources struct {
					Limits struct{ Memory string }
				}
			}
		}
		Secrets map[string]struct{ File string }
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	redis, ok := file.Services["redis"]
	if !ok {
		t.Fatal("no redis service")
	}
	if redis.Image != cfg.Images.Redis || redis.User != "999:999" || !redis.ReadOnly ||
		len(redis.CapDrop) != 1 || redis.CapDrop[0] != "ALL" {
		t.Errorf("redis service = %+v", redis)
	}
	// maxmemory plus the fixed overhead.
	if redis.Deploy.Resources.Limits.Memory != "192M" {
		t.Errorf("redis memory limit = %q", redis.Deploy.Resources.Limits.Memory)
	}
	// Only the control plane gets the admin password.
	if len(redis.Secrets) != 0 {
		t.Errorf("redis secrets = %v", redis.Secrets)
	}
	if !slices.Contains(file.Services["control-plane"].Secrets, "redis_admin_password") ||
		file.Secrets["redis_admin_password"].File != "./"+platform.RedisAdminPasswordSecret {
		t.Errorf("control plane secrets = %v, secrets = %+v", file.Services["control-plane"].Secrets, file.Secrets)
	}
	if got := platform.RedisContainer("pftest"); got != "pftest-redis-1" {
		t.Errorf("Redis container = %q", got)
	}

	conf, err := os.ReadFile(filepath.Join(dir, platform.RedisConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(testRedisPassword))
	for _, want := range []string{
		"maxmemory 128mb",
		"maxmemory-policy allkeys-lru",
		`save ""`,
		"appendonly no",
		"databases " + strconv.Itoa(platform.RedisDatabases),
		"user default off resetpass resetkeys resetchannels -@all",
		"user " + platform.RedisAdminUser + " on #" + hex.EncodeToString(sum[:]) + " ~* &* alldbs +@all",
	} {
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(want) + `$`).Match(conf) {
			t.Errorf("valkey.conf lacks %q:\n%s", want, conf)
		}
	}
}

// The control plane's default object cache settings match the admin user
// and secret the platform renders.
func TestDefaultRedisSettings(t *testing.T) {
	r := config.Default().Redis
	if r.AdminUser != platform.RedisAdminUser {
		t.Errorf("config default redis.admin_user = %q, want %q", r.AdminUser, platform.RedisAdminUser)
	}
	if r.AdminPasswordFile != "/run/secrets/redis_admin_password" || r.Address != "redis:6379" {
		t.Errorf("config default redis = %+v", r)
	}
}

// renderedVictoriaMetrics returns the victoria-metrics service of a platform
// project rendered from cfg, and the project's top-level volumes.
func renderedVictoriaMetrics(t *testing.T, cfg platform.Config) (svc struct {
	Image       string
	Command     []string
	Ports       []string
	Labels      map[string]string
	Volumes     []struct{ Type, Source, Target string }
	Networks    []string
	StopGrace   string `yaml:"stop_grace_period"`
	Healthcheck struct{ Test []string }
	Deploy      struct {
		Resources struct {
			Limits struct{ Memory string }
		}
	}
}, volumes map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(render(t, cfg), platform.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Services map[string]yaml.Node
		Volumes  map[string]any
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	node, ok := file.Services[platform.VictoriaMetricsService]
	if !ok {
		t.Fatalf("no %s service", platform.VictoriaMetricsService)
	}
	if err := node.Decode(&svc); err != nil {
		t.Fatal(err)
	}
	return svc, file.Volumes
}

func TestRenderedVictoriaMetrics(t *testing.T) {
	tests := []struct {
		name          string
		days, memory  int
		wantRetention string
		wantMemory    string
	}{
		{name: "defaults", wantRetention: "-retentionPeriod=90d", wantMemory: "256M"},
		{name: "custom", days: 400, memory: 512, wantRetention: "-retentionPeriod=400d", wantMemory: "512M"},
		{name: "one day", days: 1, wantRetention: "-retentionPeriod=1d", wantMemory: "256M"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.MetricsRetentionDays = tc.days
			cfg.VictoriaMetricsMemoryMB = tc.memory
			svc, volumes := renderedVictoriaMetrics(t, cfg)

			if svc.Image != cfg.Images.VictoriaMetrics {
				t.Errorf("image = %q", svc.Image)
			}
			if !slices.Contains(svc.Command, tc.wantRetention) {
				t.Errorf("command = %v, want %s", svc.Command, tc.wantRetention)
			}
			if !slices.Contains(svc.Command, "-storageDataPath=/victoria-metrics-data") ||
				!slices.Contains(svc.Command, "-httpListenAddr=:8428") {
				t.Errorf("command = %v", svc.Command)
			}
			if svc.Deploy.Resources.Limits.Memory != tc.wantMemory {
				t.Errorf("memory = %q, want %s", svc.Deploy.Resources.Limits.Memory, tc.wantMemory)
			}
			// Data survives container replacement and image upgrades.
			if len(svc.Volumes) != 1 || svc.Volumes[0].Type != "volume" ||
				svc.Volumes[0].Source != "victoria-metrics-data" || svc.Volumes[0].Target != "/victoria-metrics-data" {
				t.Errorf("volumes = %+v", svc.Volumes)
			}
			if _, ok := volumes["victoria-metrics-data"]; !ok {
				t.Errorf("volume victoria-metrics-data not declared: %v", volumes)
			}
			// Not publicly exposed: no ports and no Traefik route.
			if len(svc.Ports) != 0 {
				t.Errorf("publishes ports %v", svc.Ports)
			}
			for k := range svc.Labels {
				if strings.HasPrefix(k, "traefik.") {
					t.Errorf("has Traefik label %s", k)
				}
			}
			if !slices.Equal(svc.Networks, []string{"platform"}) {
				t.Errorf("networks = %v", svc.Networks)
			}
			if svc.StopGrace == "" || len(svc.Healthcheck.Test) == 0 {
				t.Errorf("stop grace %q, healthcheck %v", svc.StopGrace, svc.Healthcheck.Test)
			}
		})
	}
}

func TestDefaultVictoriaMetricsSettings(t *testing.T) {
	if config.DefaultMetricsRetentionDays != 90 {
		t.Errorf("default retention = %d days, want 90 (decision #65)", config.DefaultMetricsRetentionDays)
	}
	if platform.DefaultVictoriaMetricsMemoryMB != 256 {
		t.Errorf("default memory = %d MiB, want the 256 MiB of the §4.1 budget", platform.DefaultVictoriaMetricsMemoryMB)
	}
	images, err := platform.DefaultImages("v1.2.3")
	if err != nil || images.VictoriaMetrics == "" {
		t.Errorf("DefaultImages: %q, %v", images.VictoriaMetrics, err)
	}
}
