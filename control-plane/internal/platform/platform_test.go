package platform_test

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
)

var update = flag.Bool("update", false, "rewrite golden files in testdata/")

const testPassword = "s3cret-$root-\"pw\""

func testConfig() platform.Config {
	return platform.Config{
		DataDir: "/var/lib/pressfleet",
		Images: platform.Images{
			ControlPlane: "ghcr.io/rchouinard/pressfleet/control-plane:v1.2.3",
			MariaDB:      "docker.io/library/mariadb:12.3.3@sha256:2f4f46f80e7251990bfdc601ae3f7f2964ddb4dc1868f7deca699fc1ece8c35e",
		},
		MariaDBRootPassword: testPassword,
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

	for path, want := range map[string]os.FileMode{
		platform.ComposeFile:               0o644,
		platform.MariaDBConfigFile:         0o644,
		platform.MariaDBRootPasswordSecret: 0o600,
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

	// The password must only appear in the secret file.
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, platform.MariaDBRootPasswordSecret) {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(content, []byte("s3cret")) {
			t.Errorf("%s contains the root password", path)
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
	if len(entries) != 1 {
		t.Errorf("secrets dir has %d entries, want 1", len(entries))
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
	if len(file.Services) != 2 {
		t.Errorf("services = %d, want 2", len(file.Services))
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
		"multiline pw":      func(c *platform.Config) { c.MariaDBRootPassword = "a\nb" },
		"tiny mariadb":      func(c *platform.Config) { c.MariaDBMemoryMB = 128 },
		"tiny cp":           func(c *platform.Config) { c.ControlPlaneMemoryMB = 16 },
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
	if !pinned.MatchString(images.MariaDB) {
		t.Errorf("mariadb image not pinned by digest: %q", images.MariaDB)
	}
	if _, err := platform.DefaultImages(""); !errors.Is(err, platform.ErrInvalidConfig) {
		t.Errorf("empty version: err = %v", err)
	}
}

func TestProject(t *testing.T) {
	p := platform.Project("/var/lib/pressfleet/platform")
	if p.Name != platform.ProjectName || p.Dir != "/var/lib/pressfleet/platform" {
		t.Errorf("project = %+v", p)
	}
}
