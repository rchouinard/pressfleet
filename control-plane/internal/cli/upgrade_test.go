package cli_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// fakeUpgradePlatform stands in for the platform's Compose project and its
// running containers during an upgrade. Service hashes come from the
// rendered compose.yaml, as Compose's would.
type fakeUpgradePlatform struct {
	t  *testing.T
	mu sync.Mutex
	// version is what the control plane reports, and ready whether it is.
	version string
	ready   bool
	// mariaDBImage is the image the MariaDB container runs.
	mariaDBImage string
	// upVersion is the version the control plane reports after an Up of
	// the upgraded project (one whose compose.yaml runs the control plane
	// image of v1.2.3-test, the CLI's version), and upErr
	// fails that Up instead.
	upVersion string
	upErr     error
	// initial is the version the control plane reports after an Up of
	// any other project, such as the previous one a rollback starts,
	// unless rollbackVersion is set; rollbackUpErr fails that Up instead.
	initial         string
	rollbackVersion string
	rollbackUpErr   error
	// onUp runs on every Up, e.g. to write a migration backup.
	onUp func()
	// stopErr and runErr fail StopServices and RunService, after onStop
	// and onRun run.
	stopErr, runErr error
	onStop, onRun   func()

	// listDir answers a listing through a project's control plane
	// container; nil lists the directory on the host.
	listDir func(projectDir, dir string) ([]string, error)
	// readFile answers a read through a project's control plane
	// container; nil reads the file on the host.
	readFile func(projectDir, path string) ([]byte, error)

	ups   []compose.UpOptions
	pulls int
	// stops are the services stopped, and runs the commands run in
	// one-off containers.
	stops [][]string
	runs  []fakeRun
	// listed and read are the project directories listings and reads ran
	// in.
	listed []string
	read   []string
}

func (p fakeUpgradeProject) ListDir(_ context.Context, service, dir string) ([]string, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if service != "control-plane" {
		p.f.t.Errorf("listing a directory in service %q", service)
	}
	p.f.listed = append(p.f.listed, p.dir)
	if p.f.listDir != nil {
		return p.f.listDir(p.dir, dir)
	}
	entries, err := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, err
}

func (p fakeUpgradeProject) ReadFile(_ context.Context, service, path string) ([]byte, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if service != "control-plane" {
		p.f.t.Errorf("reading a file in service %q", service)
	}
	p.f.read = append(p.f.read, p.dir)
	if p.f.readFile != nil {
		return p.f.readFile(p.dir, path)
	}
	return os.ReadFile(path)
}

func (f *fakeUpgradePlatform) project(_, dir string, files []string) cli.UpgradeProject {
	file := filepath.Join(dir, platform.ComposeFile)
	if len(files) > 0 {
		file = files[0]
	}
	return fakeUpgradeProject{f: f, dir: dir, file: file}
}

func (f *fakeUpgradePlatform) probe(string) cli.ControlPlaneProbe {
	return fakeUpgradeProject{f: f}
}

type fakeUpgradeProject struct {
	f         *fakeUpgradePlatform
	dir, file string
}

func (p fakeUpgradeProject) services() map[string]any {
	p.f.t.Helper()
	data, err := os.ReadFile(p.file)
	if err != nil {
		p.f.t.Fatal(err)
	}
	var doc struct {
		Services map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		p.f.t.Fatal(err)
	}
	return doc.Services
}

func (p fakeUpgradeProject) ConfigHashes(context.Context) (map[string]string, error) {
	hashes := map[string]string{}
	for name, svc := range p.services() {
		b, err := json.Marshal(svc)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		hashes[name] = hex.EncodeToString(sum[:])
	}
	return hashes, nil
}

func (p fakeUpgradeProject) FileMounts(context.Context) (map[string][]string, error) {
	return map[string][]string{
		"traefik":       {filepath.Join(p.dir, platform.TraefikConfigFile), filepath.Join(p.dir, platform.TraefikDynamicDir)},
		"mariadb":       {filepath.Join(p.dir, platform.MariaDBConfigFile), filepath.Join(p.dir, platform.MariaDBRootPasswordSecret)},
		"redis":         {filepath.Join(p.dir, platform.RedisConfigFile)},
		"control-plane": {filepath.Join(p.dir, platform.TraefikDynamicDir)},
	}, nil
}

func (p fakeUpgradeProject) Pull(context.Context, compose.PullOptions) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.pulls++
	return nil
}

// fakeRun is a command run in a one-off container of a project.
type fakeRun struct {
	dir     string
	command []string
}

func (p fakeUpgradeProject) Up(ctx context.Context, opts compose.UpOptions) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	p.f.ups = append(p.f.ups, opts)
	if p.f.onUp != nil {
		p.f.onUp()
	}
	if err := ctx.Err(); err != nil {
		// Interrupted: Compose is killed.
		p.f.ready = false
		return err
	}
	upgraded := strings.Contains(readFile(p.f.t, p.file), "control-plane:v1.2.3-test")
	err, version := p.f.upErr, p.f.upVersion
	if !upgraded {
		err, version = p.f.rollbackUpErr, p.f.initial
		if p.f.rollbackVersion != "" {
			version = p.f.rollbackVersion
		}
	}
	if err != nil {
		p.f.ready = false
		return err
	}
	p.f.version, p.f.ready = version, true
	return nil
}

func (p fakeUpgradeProject) StopServices(ctx context.Context, services ...string) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	p.f.stops = append(p.f.stops, services)
	if p.f.onStop != nil {
		p.f.onStop()
	}
	if p.f.stopErr != nil {
		return p.f.stopErr
	}
	p.f.ready = false
	return nil
}

func (p fakeUpgradeProject) RunService(ctx context.Context, service string, command ...string) ([]byte, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if service != "control-plane" {
		p.f.t.Errorf("running a command in service %q", service)
	}
	p.f.runs = append(p.f.runs, fakeRun{dir: p.dir, command: command})
	if p.f.onRun != nil {
		p.f.onRun()
	}
	if p.f.runErr != nil {
		return nil, p.f.runErr
	}
	return []byte("Restored\n"), nil
}

func (p fakeUpgradeProject) ImageDigests(_ context.Context, image string) ([]string, error) {
	return []string{"ghcr.io/example/" + image + "@sha256:0123"}, nil
}

func (p fakeUpgradeProject) ServiceContainer(_ context.Context, service string) (compose.ContainerDetails, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	switch service {
	case "control-plane":
		return compose.ContainerDetails{Running: true, Image: "cp:" + p.f.version, ImageID: "sha256:cp"}, nil
	case "mariadb":
		return compose.ContainerDetails{Running: true, Image: p.f.mariaDBImage, ImageID: "sha256:db"}, nil
	}
	return compose.ContainerDetails{}, compose.ErrNoContainer
}

func (p fakeUpgradeProject) ExecService(context.Context, string, ...string) ([]byte, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	return fmt.Appendf(nil, `{"interface":1,"version":%q,"ready":%t}`+"\n", p.f.version, p.f.ready), nil
}

// upgradeEnv is an installed platform (rendered by init) running version.
type upgradeEnv struct {
	*initEnv
	platform *fakeUpgradePlatform
}

func newUpgradeEnv(t *testing.T, version string) *upgradeEnv {
	t.Helper()
	cli.FastUpgradeReadyChecks(t)
	e := newInitEnv(t)
	if r := e.run("", e.nonInteractiveArgs()...); r.code != 0 {
		t.Fatalf("init: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	images, err := platform.DefaultImages("v1.2.3-test")
	if err != nil {
		t.Fatal(err)
	}
	return &upgradeEnv{initEnv: e, platform: &fakeUpgradePlatform{
		t: t, version: version, initial: version, ready: true, mariaDBImage: images.MariaDB, upVersion: "v1.2.3-test",
	}}
}

func (e *upgradeEnv) upgrade(stdin string, args ...string) result {
	e.t.Helper()
	return e.upgradeCtx(context.Background(), "v1.2.3-test", stdin, args...)
}

// upgradeCtx runs upgrade on ctx, which an interrupt would cancel, with a
// CLI of version.
func (e *upgradeEnv) upgradeCtx(ctx context.Context, version, stdin string, args ...string) result {
	e.t.Helper()
	var stdout, stderr syncBuffer
	code := cli.Run(ctx, append([]string{"upgrade", "--config", e.configPath}, args...), cli.Options{
		Version: version,
		Stdin:   strings.NewReader(stdin),
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env: func(k string) (string, bool) {
			v, ok := e.env[k]
			return v, ok
		},
		HostProbe:      e.host,
		DockerProbe:    e.docker,
		ControlPlane:   e.platform.probe,
		UpgradeProject: e.platform.project,
	})
	return result{code, stdout.String(), stderr.String()}
}

func (e *upgradeEnv) path(rel ...string) string {
	return filepath.Join(append([]string{e.dataDir}, rel...)...)
}

func (e *upgradeEnv) record() map[string]any {
	e.t.Helper()
	data, err := os.ReadFile(e.path("upgrade", "upgrade.json"))
	if err != nil {
		e.t.Fatal(err)
	}
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil {
		e.t.Fatal(err)
	}
	return rec
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpgradeRecreatesOnlyChangedServices(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	before := readFile(t, e.path("platform", "compose.yaml"))

	r := e.upgrade("")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	for _, want := range []string{"from v1.2.2 to v1.2.3-test", "Services to recreate: control-plane, maintenance.", "upgraded to v1.2.3-test and healthy"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("output lacks %q:\n%s", want, r.stdout)
		}
	}
	// Only the control plane image changed (init ran with a development
	// image; the maintenance page runs it too),
	// which needs neither a forced recreate nor a confirmation.
	if len(e.platform.ups) != 1 || e.platform.ups[0].ForceRecreate || !e.platform.ups[0].Wait {
		t.Errorf("ups = %+v, want one waiting up", e.platform.ups)
	}
	if e.platform.pulls != 1 {
		t.Errorf("pulled %d times, want 1", e.platform.pulls)
	}
	if got := readFile(t, e.path("upgrade", "previous-platform", "compose.yaml")); got != before {
		t.Error("the saved platform project isn't the one from before the upgrade")
	}
	if got := readFile(t, e.path("platform", "compose.yaml")); !strings.Contains(got, "control-plane:v1.2.3-test") {
		t.Errorf("the platform project wasn't upgraded:\n%s", got)
	}
	rec := e.record()
	if rec["status"] != "succeeded" || rec["from"] != "v1.2.2" || rec["to"] != "v1.2.3-test" {
		t.Errorf("record = %v", rec)
	}
	if _, err := os.Stat(e.path("upgrade", "next-platform")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("staging directory left behind: %v", err)
	}

	// Running it again finds nothing to do.
	r = e.upgrade("")
	if r.code != 0 || !strings.Contains(r.stdout, "nothing to upgrade") {
		t.Errorf("second run: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if len(e.platform.ups) != 1 {
		t.Errorf("second run ran up: %+v", e.platform.ups)
	}
}

// A setting the platform project doesn't pass as an environment variable
// reaches the control plane through the mounted config file: changing it and
// running upgrade again, at the same version, recreates the control plane.
func TestUpgradeAppliesChangedConfigFile(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	if r := e.upgrade(""); r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	compose := readFile(t, e.path("platform", "compose.yaml"))
	for _, want := range []string{"PRESSFLEET_CONFIG: " + `"` + platform.ConfigSecretPath + `"`, "pressfleet_config", platform.ConfigHashLabel} {
		if !strings.Contains(compose, want) {
			t.Errorf("compose.yaml lacks %q", want)
		}
	}

	f, err := os.OpenFile(e.configPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("audit:\n  retention_days: 30\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	r := e.upgrade("")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "Services to recreate: control-plane.") {
		t.Errorf("output doesn't recreate only the control plane:\n%s", r.stdout)
	}
	if len(e.platform.ups) != 2 {
		t.Errorf("ups = %+v, want a second up", e.platform.ups)
	}
}

// An invalid setting in the config file or the environment is refused
// before anything changes, with an error naming the key.
func TestUpgradeRefusesInvalidConfig(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		env        map[string]string
		want       string
	}{
		{name: "file", file: "two_factor:\n  required: sometimes\n", want: "two_factor.required"},
		{name: "file duration", file: "two_factor:\n  reauthentication_max_age: 2h\n", want: "two_factor.reauthentication_max_age"},
		{name: "environment", env: map[string]string{"PRESSFLEET_TWO_FACTOR_REAUTHENTICATION": "maybe"}, want: "two_factor.reauthentication"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newUpgradeEnv(t, "v1.2.2")
			before := readFile(t, e.path("platform", "compose.yaml"))
			if tc.file != "" {
				f, err := os.OpenFile(e.configPath, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.WriteString(tc.file); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			}
			maps.Copy(e.env, tc.env)
			r := e.upgrade("", "--yes")
			if r.code == 0 || !strings.Contains(r.stderr, "invalid configuration") || !strings.Contains(r.stderr, tc.want) {
				t.Fatalf("exit %d, want an error about %s\n%s\n%s", r.code, tc.want, r.stdout, r.stderr)
			}
			if len(e.platform.ups) != 0 || e.platform.pulls != 0 {
				t.Error("upgrade with an invalid config changed the platform")
			}
			if got := readFile(t, e.path("platform", "compose.yaml")); got != before {
				t.Error("upgrade with an invalid config rendered the platform project")
			}
			if _, err := os.Stat(e.path("upgrade")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("upgrade with an invalid config wrote %s: %v", e.path("upgrade"), err)
			}
		})
	}
}

func TestUpgradeConfirmsInterruptingSites(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.3-test")
	// A changed Traefik configuration file changes no service hash, but
	// Traefik must be recreated to read it.
	traefik := e.path("platform", platform.TraefikConfigFile)
	if err := os.WriteFile(traefik, []byte("# from an older release\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, stdin := range []string{"", "n\n"} {
		r := e.upgrade(stdin)
		if r.code == 0 || !strings.Contains(r.stdout, "Recreating traefik interrupts every site") {
			t.Fatalf("stdin %q: exit %d\n%s\n%s", stdin, r.code, r.stdout, r.stderr)
		}
		if len(e.platform.ups) != 0 || e.platform.pulls != 0 {
			t.Fatalf("stdin %q: changed the platform without confirmation", stdin)
		}
	}
	if _, err := os.Stat(e.path("upgrade", "previous-platform")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a canceled upgrade saved the platform project: %v", err)
	}

	r := e.upgrade("y\n")
	if r.code != 0 {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	want := compose.UpOptions{Services: []string{"traefik"}, ForceRecreate: true, Wait: true, WaitTimeout: 10 * time.Minute}
	if len(e.platform.ups) != 2 || !slices.Equal(e.platform.ups[0].Services, want.Services) || !e.platform.ups[0].ForceRecreate {
		t.Errorf("ups = %+v, want a forced recreate of traefik and then everything", e.platform.ups)
	}
	if strings.Contains(readFile(t, traefik), "older release") {
		t.Error("the Traefik configuration wasn't rendered again")
	}
}

func TestUpgradeYesSkipsConfirmation(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	e.env["PRESSFLEET_DATABASE_DRIVER"] = "postgres"
	e.env["PRESSFLEET_DATABASE_POSTGRES_DSN"] = "postgres://pressfleet@db/pressfleet"
	if r := e.upgrade(""); r.code == 0 || !strings.Contains(r.stdout, "PostgreSQL") {
		t.Fatalf("PostgreSQL upgrade without confirmation: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if r := e.upgrade("", "--yes"); r.code != 0 || !strings.Contains(r.stdout, "Confirmed with --yes") {
		t.Fatalf("exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
}

func TestUpgradeRefuses(t *testing.T) {
	tests := []struct {
		name    string
		version string
		mariaDB string
		want    string
	}{
		{name: "downgrade", version: "v1.3.0", want: "can't be downgraded"},
		{name: "too old", version: "v0.0.9", want: "upgrades control planes from v0.1.0 or later"},
		{name: "MariaDB series", version: "v1.2.2", mariaDB: "docker.io/library/mariadb:11.4.2@sha256:00", want: "from the 11.4 to the"},
		{name: "MariaDB series unknown", version: "v1.2.2", mariaDB: "docker.io/library/mariadb@sha256:00", want: "can't tell the MariaDB release series"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newUpgradeEnv(t, tt.version)
			if tt.mariaDB != "" {
				e.platform.mariaDBImage = tt.mariaDB
			}
			r := e.upgrade("", "--yes")
			if r.code == 0 || !strings.Contains(r.stderr, tt.want) {
				t.Fatalf("exit %d, want error %q\n%s\n%s", r.code, tt.want, r.stdout, r.stderr)
			}
			if len(e.platform.ups) != 0 || e.platform.pulls != 0 {
				t.Error("a refused upgrade changed the platform")
			}
		})
	}
}

func TestUpgradeFailurePrintsRecoverySteps(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	original := readFile(t, e.path("platform", "compose.yaml"))
	backups := e.path("state", "backups")
	// A backup from an earlier upgrade, which the recovery must not use.
	if err := os.MkdirAll(backups, 0o700); err != nil {
		t.Fatal(err)
	}
	old := "pressfleet-20200101T000000.000Z-schema0001.db"
	if err := os.WriteFile(filepath.Join(backups, old), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The new control plane migrates, backing the database up, and then
	// fails its healthcheck.
	var migrated string
	e.platform.onUp = func() {
		if migrated != "" {
			return
		}
		migrated = fmt.Sprintf("pressfleet-%s-schema0027.db", time.Now().UTC().Format("20060102T150405.000Z"))
		if err := os.WriteFile(filepath.Join(backups, migrated), nil, 0o600); err != nil {
			t.Error(err)
		}
	}
	e.platform.upErr = errors.New("container pressfleet-control-plane-1 is unhealthy")

	r := e.upgrade("", "--no-rollback")
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	prev := e.path("upgrade", "previous-platform")
	for _, want := range []string{
		"Error: the upgrade to v1.2.3-test failed: starting the upgraded platform",
		"To return to v1.2.2",
		"--project-directory " + e.path("platform") + " stop control-plane",
		"--project-directory " + prev + " run --rm --no-deps control-plane state restore " + filepath.Join(backups, migrated),
		"with the previous control plane image (ghcr.io/example/sha256:cp@sha256:0123)",
		"cp -a " + prev + "/. " + e.path("platform") + "/",
		"up --detach --wait --remove-orphans",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("recovery steps lack %q:\n%s", want, r.stderr)
		}
	}
	if strings.Contains(r.stderr, old) {
		t.Errorf("recovery steps use a backup from before the upgrade:\n%s", r.stderr)
	}
	if rec := e.record(); rec["status"] != "failed" {
		t.Errorf("record = %v", rec)
	}

	// A retry keeps the project saved by the failed attempt, which the
	// recovery steps still point at.
	e.platform.upErr = nil
	r = e.upgrade("")
	if r.code != 0 || !strings.Contains(r.stdout, "didn't finish. Retrying") {
		t.Fatalf("retry: exit %d\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	if len(e.platform.ups) != 2 {
		t.Errorf("the retry didn't start the services again: ups = %+v", e.platform.ups)
	}
	if got := readFile(t, filepath.Join(prev, "compose.yaml")); got != original {
		t.Error("the retry replaced the platform project saved before the first attempt")
	}
	if rec := e.record(); rec["status"] != "succeeded" || rec["from"] != "v1.2.2" {
		t.Errorf("record = %v", rec)
	}
}

func TestUpgradeFailureWithoutMigration(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	e.platform.upErr = errors.New("unhealthy")
	r := e.upgrade("", "--no-rollback")
	if r.code != 1 || !strings.Contains(r.stderr, "The state database needs no restore") || strings.Contains(r.stderr, "state restore") {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
}

func TestUpgradeWrongVersionFails(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	e.platform.upVersion = "v1.2.2" // the control plane kept its image
	r := e.upgrade("")
	if r.code != 2 || !strings.Contains(r.stderr, "the control plane runs v1.2.2, not v1.2.3-test") {
		t.Fatalf("exit %d\n%s", r.code, r.stderr)
	}
}

func TestMariaDBSeries(t *testing.T) {
	for in, want := range map[string]string{
		"docker.io/library/mariadb:12.3.3@sha256:2f4f": "12.3",
		"mariadb:11.4":                  "11.4",
		"mariadb:11.4.2-noble":          "11.4",
		"registry:5000/mariadb:10.11.9": "10.11",
		"registry:5000/mariadb":         "",
		"mariadb:latest":                "",
		"mariadb@sha256:2f4f":           "",
	} {
		if got := cli.MariaDBSeries(in); got != want {
			t.Errorf("MariaDBSeries(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMariaDBSeriesPinned keeps a MariaDB series change from shipping by
// accident: the upgrade refuses one, and MARIADB_AUTO_UPGRADE would
// otherwise apply it silently on a new install's next start. Changing this
// constant needs a supported major upgrade path first (design doc §4.7).
func TestMariaDBSeriesPinned(t *testing.T) {
	const pinned = "12.3"
	images, err := platform.DefaultImages("v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := cli.MariaDBSeries(images.MariaDB); got != pinned {
		t.Errorf("images.yaml moves MariaDB to the %s series; releases are pinned to %s", got, pinned)
	}
}

func TestUpgradeGuideHasRecoverySteps(t *testing.T) {
	steps, err := cli.GuideRecoverySteps()
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile("../../../docs/upgrading.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(guide), steps) {
		t.Errorf("docs/upgrading.md must show the recovery steps exactly as upgrade prints them:\n\n%s", steps)
	}
}

func TestUpgradeGuideHasUncheckedRecoveryStep(t *testing.T) {
	step, err := cli.GuideRecoveryStepUnchecked()
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile("../../../docs/upgrading.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(guide), step) {
		t.Errorf("docs/upgrading.md must show step 2 of the recovery steps for a host that can't look for the backup exactly as upgrade prints it:\n\n%s", step)
	}
}

// failedUpgrade runs an upgrade whose new control plane migrates, writing a
// backup in the state backups directory, and then fails.
func (e *upgradeEnv) failedUpgrade() (backup string, r result) {
	e.t.Helper()
	backups := e.path("state", "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		e.t.Fatal(err)
	}
	backup = fmt.Sprintf("pressfleet-%s-schema0029.db", time.Now().UTC().Format("20060102T150405.000Z"))
	e.platform.onUp = func() {
		if err := os.WriteFile(filepath.Join(backups, backup), nil, 0o600); err != nil {
			e.t.Error(err)
		}
	}
	e.platform.upErr = errors.New("container pressfleet-control-plane-1 is unhealthy")
	return backup, e.upgrade("", "--no-rollback")
}

// failedUpgradeWithMarker is failedUpgrade with a control plane that records
// its migration in a marker. A second backup, older than the marker's but
// also from after the upgrade started, would win by file name alone.
func (e *upgradeEnv) failedUpgradeWithMarker() (backup string, r result) {
	e.t.Helper()
	backups := e.path("state", "backups")
	if err := os.MkdirAll(backups, 0o700); err != nil {
		e.t.Fatal(err)
	}
	now := time.Now().UTC()
	decoy := fmt.Sprintf("pressfleet-%s-schema0028.db", now.Add(-10*time.Second).Format("20060102T150405.000Z"))
	backup = fmt.Sprintf("pressfleet-%s-schema0029.db", now.Format("20060102T150405.000Z"))
	marker, err := json.Marshal(store.MigrationMarker{
		Status: store.MigrationSucceeded, StartedAt: now, FinishedAt: now, Version: "v1.2.3", Dialect: store.SQLite,
		FromSchema: 29, ToSchema: 31, ReachedSchema: 31, Backup: filepath.Join(backups, backup),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.platform.onUp = func() {
		for name, data := range map[string][]byte{
			decoy: nil, backup: nil,
			fmt.Sprintf("migration-%s-schema0029.json", now.Format("20060102T150405.000Z")): marker,
		} {
			if err := os.WriteFile(filepath.Join(backups, name), data, 0o600); err != nil {
				e.t.Error(err)
			}
		}
	}
	e.platform.upErr = errors.New("container pressfleet-control-plane-1 is unhealthy")
	return backup, e.upgrade("", "--no-rollback")
}

// The recovery steps restore the backup the migration marker names, read
// through a container of the previous control plane when the host user
// can't read the backups directory.
func TestUpgradeRecoveryReadsMigrationMarker(t *testing.T) {
	for _, readable := range []bool{true, false} {
		t.Run(fmt.Sprintf("readable=%t", readable), func(t *testing.T) {
			e := newUpgradeEnv(t, "v1.2.2")
			if !readable {
				cli.HostListDir(t, func(dir string) ([]string, error) {
					return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrPermission}
				})
			}
			backup, r := e.failedUpgradeWithMarker()
			if r.code != 1 {
				t.Fatalf("exit %d, want 1\n%s\n%s", r.code, r.stdout, r.stderr)
			}
			want := "control-plane state restore " + e.path("state", "backups", backup) + "\n"
			if !strings.Contains(r.stderr, want) {
				t.Errorf("recovery steps lack %q:\n%s", want, r.stderr)
			}
			wantRead := []string(nil)
			if !readable {
				wantRead = []string{e.path("upgrade", "previous-platform")}
			}
			if !slices.Equal(e.platform.read, wantRead) {
				t.Errorf("reads through Docker ran in %v, want %v", e.platform.read, wantRead)
			}
		})
	}
}

// The state backups directory belongs to root, so an operator who isn't root
// can't read it: the recovery steps must still find the backup, through a
// container of the previous control plane.
func TestUpgradeRecoveryListsUnreadableBackupsThroughDocker(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	cli.HostListDir(t, func(dir string) ([]string, error) {
		return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrPermission}
	})
	backup, r := e.failedUpgrade()
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	prev := e.path("upgrade", "previous-platform")
	want := "--project-directory " + prev + " run --rm --no-deps control-plane state restore " + e.path("state", "backups", backup)
	if !strings.Contains(r.stderr, want) {
		t.Errorf("recovery steps lack %q:\n%s", want, r.stderr)
	}
	if strings.Contains(r.stderr, "needs no restore") || strings.Contains(r.stderr, "couldn't check") {
		t.Errorf("recovery steps doubt a backup found through Docker:\n%s", r.stderr)
	}
	// The previous release's project lists first.
	if len(e.platform.listed) != 1 || e.platform.listed[0] != prev {
		t.Errorf("listings ran in %v, want only %s", e.platform.listed, prev)
	}
}

func TestUpgradeRecoveryWhenBackupsCannotBeListed(t *testing.T) {
	e := newUpgradeEnv(t, "v1.2.2")
	cli.HostListDir(t, func(dir string) ([]string, error) {
		return nil, &fs.PathError{Op: "open", Path: dir, Err: fs.ErrPermission}
	})
	e.platform.listDir = func(string, string) ([]string, error) { return nil, errors.New("docker: no such image") }
	_, r := e.failedUpgrade()
	if r.code != 1 {
		t.Fatalf("exit %d, want 1\n%s\n%s", r.code, r.stdout, r.stderr)
	}
	prev := e.path("upgrade", "previous-platform")
	for _, want := range []string{
		"couldn't check whether the control plane migrated the state database",
		"--project-directory " + prev + " run --rm --no-deps --no-TTY --entrypoint ls control-plane -1 " + e.path("state", "backups"),
		"state restore " + e.path("state", "backups") + "/<file name from the listing>",
		"database schema is newer than this pressfleet binary supports",
		"looking for the pre-migration backup",
	} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("recovery steps lack %q:\n%s", want, r.stderr)
		}
	}
	if strings.Contains(r.stderr, "needs no restore") {
		t.Errorf("recovery steps claim no restore is needed without knowing:\n%s", r.stderr)
	}
	// Both the previous and the current project were tried.
	if len(e.platform.listed) != 2 {
		t.Errorf("listings ran in %v, want two projects", e.platform.listed)
	}
}
