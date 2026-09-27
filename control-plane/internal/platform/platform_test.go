package platform_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

const (
	testPassword      = "s3cret-$root-\"pw\""
	testRedisPassword = "r3dis-admin-pw"
)

func testConfig() platform.Config {
	return platform.Config{
		DataDir: "/var/lib/pressfleet",
		Images: platform.Images{
			ControlPlane: "ghcr.io/rchouinard/pressfleet/control-plane:v1.2.3",
			MariaDB:      "docker.io/library/mariadb:12.3.3@sha256:2f4f46f80e7251990bfdc601ae3f7f2964ddb4dc1868f7deca699fc1ece8c35e",
			Redis:        "docker.io/valkey/valkey:9.1.2@sha256:ac9c858b86dc81f3482d53a9d854547379a5f41e012500f839ab257c399063e0",
		},
		MariaDBRootPassword: testPassword,
		RedisAdminPassword:  testRedisPassword,
		MasterKeyFile:       "/etc/pressfleet/master.key",
		PreviewBaseDomain:   "preview.agency.example",
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
		platform.MariaDBRootPasswordSecret: 0o600,
		platform.RedisConfigFile:           0o644,
		platform.RedisAdminPasswordSecret:  0o600,
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
	if len(entries) != 2 {
		t.Errorf("secrets dir has %d entries, want 2", len(entries))
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
			Volumes     []mount
			Networks    []string
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
		Secrets map[string]struct{ File string }
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if file.Name != "pftest" {
		t.Errorf("name = %q", file.Name)
	}
	if len(file.Services) != 3 {
		t.Errorf("services = %d, want 3", len(file.Services))
	}
	socketUsers := 0
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
		if len(svc.Networks) != 1 || svc.Networks[0] != "platform" {
			t.Errorf("%s: networks = %v", name, svc.Networks)
		}
		for _, v := range svc.Volumes {
			if v.Source == "/var/run/docker.sock" {
				socketUsers++
				if name != "control-plane" || v.ReadOnly {
					t.Errorf("%s: unexpected docker socket mount %+v", name, v)
				}
			}
		}
	}
	if socketUsers != 1 {
		t.Errorf("docker socket mounted %d times, want 1", socketUsers)
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
		"relative data dir": func(c *platform.Config) { c.DataDir = "var/lib/pressfleet" },
		"unclean data dir":  func(c *platform.Config) { c.DataDir = "/var/lib/pressfleet/" },
		"no images":         func(c *platform.Config) { c.Images = platform.Images{} },
		"no password":       func(c *platform.Config) { c.MariaDBRootPassword = "" },
		"no redis password": func(c *platform.Config) { c.RedisAdminPassword = "" },
		"multiline redis":   func(c *platform.Config) { c.RedisAdminPassword = "a\nb" },
		"tiny redis":        func(c *platform.Config) { c.RedisMaxMemoryMB = 16 },
		"no redis image":    func(c *platform.Config) { c.Images.Redis = "" },
		"multiline pw":      func(c *platform.Config) { c.MariaDBRootPassword = "a\nb" },
		"tiny mariadb":      func(c *platform.Config) { c.MariaDBMemoryMB = 128 },
		"tiny cp":           func(c *platform.Config) { c.ControlPlaneMemoryMB = 16 },
		"no master key":     func(c *platform.Config) { c.MasterKeyFile = "" },
		"relative key file": func(c *platform.Config) { c.MasterKeyFile = "master.key" },
		"key in data dir":   func(c *platform.Config) { c.MasterKeyFile = "/var/lib/pressfleet/master.key" },
		"bad preview base":  func(c *platform.Config) { c.PreviewBaseDomain = "*.preview.example" },
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
	for name, img := range map[string]string{"mariadb": images.MariaDB, "redis": images.Redis} {
		if !pinned.MatchString(img) {
			t.Errorf("%s image not pinned by digest: %q", name, img)
		}
	}
	if _, err := platform.DefaultImages(""); !errors.Is(err, platform.ErrInvalidConfig) {
		t.Errorf("empty version: err = %v", err)
	}
}

func TestRenderWithoutPreviewBaseDomain(t *testing.T) {
	cfg := testConfig()
	cfg.PreviewBaseDomain = ""
	raw, err := os.ReadFile(filepath.Join(render(t, cfg), platform.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("PRESSFLEET_PREVIEW_BASE_DOMAIN")) {
		t.Errorf("empty preview base domain rendered:\n%s", raw)
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
