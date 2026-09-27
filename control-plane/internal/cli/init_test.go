package cli_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor/doctortest"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
)

// fakePlatform records what init asks of the platform Compose project.
type fakePlatform struct {
	mu      sync.Mutex
	name    string
	dir     string
	ups     []compose.UpOptions
	upErr   error
	execs   [][]string
	stdins  []string
	execErr error
}

func (f *fakePlatform) project(name, dir string) cli.PlatformProject {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.name, f.dir = name, dir
	return fakeProject{f}
}

type fakeProject struct{ f *fakePlatform }

func (p fakeProject) Up(_ context.Context, opts compose.UpOptions) error {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.ups = append(p.f.ups, opts)
	return p.f.upErr
}

func (p fakeProject) ExecInput(_ context.Context, stdin io.Reader, service string, command ...string) ([]byte, error) {
	b, err := io.ReadAll(stdin)
	if err != nil {
		return nil, err
	}
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.execs = append(p.f.execs, append([]string{service}, command...))
	p.f.stdins = append(p.f.stdins, string(b))
	if p.f.execErr != nil {
		return nil, p.f.execErr
	}
	return []byte("Created admin user admin@example.com (id usr_1)\n"), nil
}

// initEnv is a temporary install root: a config directory and a data
// directory, with a host whose DNS records point at it.
type initEnv struct {
	t          *testing.T
	configPath string
	dataDir    string
	keyFile    string
	env        map[string]string
	host       *doctortest.Host
	docker     *doctortest.Docker
	platform   *fakePlatform
}

func newInitEnv(t *testing.T) *initEnv {
	t.Helper()
	root := t.TempDir()
	host := doctortest.Healthy()
	ip := []netip.Addr{netip.MustParseAddr("203.0.113.10")}
	host.DNS["fleet.example.com"] = ip
	host.DNS["*.preview.example.com"] = ip
	return &initEnv{
		t:          t,
		configPath: filepath.Join(root, "etc", "pressfleet.yaml"),
		dataDir:    filepath.Join(root, "data"),
		keyFile:    filepath.Join(root, "etc", "master.key"),
		env:        map[string]string{},
		host:       host,
		docker:     doctortest.HealthyDocker(),
		platform:   &fakePlatform{},
	}
}

// nonInteractiveArgs answers every question with flags.
func (e *initEnv) nonInteractiveArgs(extra ...string) []string {
	return append([]string{
		"--non-interactive", "--confirm-key-stored",
		"--dashboard-domain", "fleet.example.com",
		"--preview-base-domain", "preview.example.com",
		"--data-dir", e.dataDir,
		"--admin-email", "admin@example.com",
		"--control-plane-image", "pressfleet-control-plane:test",
	}, extra...)
}

func (e *initEnv) run(stdin string, args ...string) result {
	e.t.Helper()
	var stdout, stderr syncBuffer
	code := cli.Run(context.Background(), append([]string{"init", "--config", e.configPath}, args...), cli.Options{
		Version: "v1.2.3-test",
		Stdin:   strings.NewReader(stdin),
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env: func(k string) (string, bool) {
			v, ok := e.env[k]
			return v, ok
		},
		HostProbe:       e.host,
		DockerProbe:     e.docker,
		PlatformProject: e.platform.project,
	})
	return result{code, stdout.String(), stderr.String()}
}

var (
	printedKey      = regexp.MustCompile(`(?m)^    (pfmk1:\S+)$`)
	printedPassword = regexp.MustCompile(`(?m)^  Admin password:   (\S+)$`)
)

// assertInstalled checks the files and platform calls of a successful
// install and returns the key in the key file.
func (e *initEnv) assertInstalled(r result) secrets.MasterKey {
	t := e.t
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", r.code, r.stdout, r.stderr)
	}
	if !strings.Contains(r.stdout, "WARNING: "+cli.MasterKeyWarning+"\n") {
		t.Errorf("stdout lacks the verbatim master key warning:\n%s", r.stdout)
	}

	// The key file is private, parses, and is outside the data directory.
	info, err := os.Stat(e.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %04o, want 0600", perm)
	}
	key, err := loadKey(e.keyFile, e.dataDir)
	if err != nil {
		t.Fatal(err)
	}

	// The config file round-trips through the loader.
	cfg, err := config.Load(e.configPath, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("written config is invalid: %v", err)
	}
	if cfg.DataDir != e.dataDir || cfg.DashboardDomain != "fleet.example.com" ||
		cfg.PreviewBaseDomain != "preview.example.com" || cfg.MasterKeyFile != e.keyFile {
		t.Errorf("config = %+v", cfg)
	}

	// The platform project was rendered and started, and waited on.
	compose, err := os.ReadFile(filepath.Join(e.dataDir, "platform", platform.ComposeFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(compose), `"pressfleet-control-plane:test"`) {
		t.Errorf("compose.yaml doesn't use the --control-plane-image")
	}
	if e.platform.name != platform.ProjectName || e.platform.dir != filepath.Join(e.dataDir, "platform") {
		t.Errorf("project = %s in %s", e.platform.name, e.platform.dir)
	}
	if len(e.platform.ups) != 1 || !e.platform.ups[0].Wait {
		t.Errorf("up calls = %+v, want one with Wait", e.platform.ups)
	}

	// The admin was created inside the control plane container, with the
	// password on stdin.
	want := []string{"control-plane", "pressfleet", "user", "create", "--email", "admin@example.com", "--role", "admin", "--password-stdin"}
	if len(e.platform.execs) != 1 || !slices.Equal(e.platform.execs[0], want) {
		t.Errorf("execs = %q, want %q", e.platform.execs, want)
	}
	return key
}

func TestInitNonInteractive(t *testing.T) {
	e := newInitEnv(t)
	r := e.run("", e.nonInteractiveArgs()...)
	key := e.assertInstalled(r)

	// The key printed once is the key in the file.
	if m := printedKey.FindStringSubmatch(r.stdout); m == nil || m[1] != key.Encode().Reveal() {
		t.Errorf("printed key doesn't match the key file:\n%s", r.stdout)
	}
	// The generated admin password is printed once and is what the
	// container got.
	m := printedPassword.FindStringSubmatch(r.stdout)
	if m == nil || e.platform.stdins[0] != m[1]+"\n" {
		t.Errorf("printed password %q, container stdin %q", m, e.platform.stdins)
	}
	if strings.Contains(r.stderr, key.Encode().Reveal()) || (m != nil && strings.Contains(r.stderr, m[1])) {
		t.Error("a secret was logged")
	}
}

func TestInitAdminPasswordStdin(t *testing.T) {
	e := newInitEnv(t)
	r := e.run("correct horse battery\n", e.nonInteractiveArgs("--admin-password-stdin")...)
	e.assertInstalled(r)
	if e.platform.stdins[0] != "correct horse battery\n" {
		t.Errorf("container stdin = %q", e.platform.stdins[0])
	}
	if strings.Contains(r.stdout, "correct horse battery") || strings.Contains(r.stdout, "Admin password") {
		t.Errorf("supplied password was printed:\n%s", r.stdout)
	}

	// A password that fails the policy is rejected before anything happens.
	e = newInitEnv(t)
	r = e.run("short\n", e.nonInteractiveArgs("--admin-password-stdin")...)
	if r.code != 1 || !strings.Contains(r.stderr, auth.ErrPasswordPolicy.Error()) || fileExists(e.keyFile) {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}
}

func TestInitEnvironment(t *testing.T) {
	e := newInitEnv(t)
	e.env = map[string]string{
		"PRESSFLEET_DASHBOARD_DOMAIN":    "fleet.example.com",
		"PRESSFLEET_PREVIEW_BASE_DOMAIN": "preview.example.com",
		"PRESSFLEET_DATA_DIR":            e.dataDir,
		"PRESSFLEET_PUBLIC_IP":           "203.0.113.10",
	}
	r := e.run("", "--non-interactive", "--confirm-key-stored", "--admin-email", "admin@example.com",
		"--control-plane-image", "pressfleet-control-plane:test")
	e.assertInstalled(r)
	cfg, err := config.Load(e.configPath, func(string) (string, bool) { return "", false })
	if err != nil || cfg.PublicIP != "203.0.113.10" {
		t.Errorf("public_ip = %q, %v", cfg.PublicIP, err)
	}
}

func TestInitInteractive(t *testing.T) {
	e := newInitEnv(t)
	// The data directory default comes from the environment.
	e.env["PRESSFLEET_DATA_DIR"] = e.dataDir
	answers := strings.Join([]string{
		"https://fleet.example.com", // rejected: not a domain
		"fleet.example.com",
		"*.preview.example.com", // rejected: wildcard
		"preview.example.com",
		"", // no public IP
		"", // default data directory
		"", // default key file, next to the config file
		"",
		"admin@example.com", // required, so the empty line above re-prompts
		"",                  // no SMTP relay: the other SMTP questions are skipped
		"maybe",             // not a confirmation
		"yes",
	}, "\n") + "\n"
	r := e.run(answers, "--control-plane-image", "pressfleet-control-plane:test")
	e.assertInstalled(r)
	for _, want := range []string{
		"Dashboard domain, where the dashboard and API are served (e.g. fleet.agency.example): ",
		`"https://fleet.example.com" is not a domain name`,
		"give the base domain without the wildcard, e.g. preview.example.com",
		"Data directory for sites, state, and rendered projects [" + e.dataDir + "]: ",
		"Master key file, outside the data directory [" + e.keyFile + "]: ",
		"a value is required",
		`Type "yes" or the key ID once the key above is stored off-host`,
		"PASS  Linux on amd64",
	} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
		}
	}
	if !strings.Contains(r.stdout, "Admin password:") {
		t.Error("generated password not printed")
	}
}

func TestInitConfirmWithKeyID(t *testing.T) {
	// The key ID is only known once init prints it, so answer through a pipe.
	e := newInitEnv(t)
	pr, pw := io.Pipe()
	out := &promptWatcher{prompt: `Type "yes" or the key ID`, ch: make(chan string, 1)}
	done := make(chan int)
	go func() {
		done <- cli.Run(context.Background(), []string{"init", "--config", e.configPath,
			"--dashboard-domain", "fleet.example.com", "--preview-base-domain", "preview.example.com",
			"--public-ip", "203.0.113.10", "--data-dir", e.dataDir, "--master-key-file", e.keyFile,
			"--admin-email", "admin@example.com", "--smtp-host", "", "--control-plane-image", "img:test",
		}, cli.Options{
			Version: "v1.2.3-test", Stdin: pr, Stdout: out, Stderr: io.Discard,
			Env:       func(string) (string, bool) { return "", false },
			HostProbe: e.host, DockerProbe: e.docker, PlatformProject: e.platform.project,
		})
	}()
	stdout := <-out.ch
	m := regexp.MustCompile(`key ID ([0-9a-f]{16})`).FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("no key ID in:\n%s", stdout)
	}
	if _, err := io.WriteString(pw, m[1]+"\n"); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	key, err := loadKey(e.keyFile, "")
	if err != nil || key.ID() != m[1] {
		t.Errorf("key file: %v, %v", key.ID(), err)
	}
}

// promptWatcher sends everything written so far on ch once prompt appears.
type promptWatcher struct {
	syncBuffer
	prompt string
	ch     chan string
	once   sync.Once
}

func (w *promptWatcher) Write(p []byte) (int, error) {
	n, err := w.syncBuffer.Write(p)
	if s := w.String(); strings.Contains(s, w.prompt) {
		w.once.Do(func() { w.ch <- s })
	}
	return n, err
}

func TestInitInteractiveAbort(t *testing.T) {
	tests := []struct {
		name, stdin, want string
	}{
		{"declined", "fleet.example.com\npreview.example.com\n\n\n\nadmin@example.com\n\nno\n", "must be stored off-host"},
		{"input ends", "fleet.example.com\n", "standard input closed"},
		{"input ends at confirmation", "fleet.example.com\npreview.example.com\n\n\n\nadmin@example.com\n\n", "standard input closed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newInitEnv(t)
			e.env["PRESSFLEET_DATA_DIR"] = e.dataDir
			r := e.run(tt.stdin)
			if r.code != 1 || !strings.Contains(r.stderr, tt.want) {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			// Nothing was written or started.
			for _, p := range []string{e.keyFile, e.configPath, e.dataDir} {
				if fileExists(p) {
					t.Errorf("%s exists after aborting", p)
				}
			}
			if len(e.platform.ups) != 0 {
				t.Error("platform started")
			}
		})
	}
}

func TestInitValidation(t *testing.T) {
	tests := []struct {
		name string
		args func(e *initEnv) []string
		want string
	}{
		{"missing required flag", func(e *initEnv) []string {
			return []string{"--non-interactive", "--confirm-key-stored", "--dashboard-domain", "fleet.example.com",
				"--data-dir", e.dataDir, "--admin-email", "admin@example.com"}
		}, "--preview-base-domain: a value is required"},
		{"no key confirmation", func(e *initEnv) []string {
			return slices.DeleteFunc(e.nonInteractiveArgs(), func(s string) bool { return s == "--confirm-key-stored" })
		}, "--confirm-key-stored"},
		{"password stdin needs non-interactive", func(*initEnv) []string {
			return []string{"--admin-password-stdin"}
		}, "--admin-password-stdin needs --non-interactive"},
		{"key file inside data dir", func(e *initEnv) []string {
			return e.nonInteractiveArgs("--master-key-file", filepath.Join(e.dataDir, "master.key"))
		}, secrets.ErrKeyFileInDataDir.Error()},
		{"key file is the data dir", func(e *initEnv) []string {
			return e.nonInteractiveArgs("--master-key-file", e.dataDir)
		}, secrets.ErrKeyFileInDataDir.Error()},
		{"relative key file", func(e *initEnv) []string {
			return e.nonInteractiveArgs("--master-key-file", "master.key")
		}, "must be a clean absolute path"},
		{"bad domain", func(e *initEnv) []string {
			return e.nonInteractiveArgs("--dashboard-domain", "fleet")
		}, `--dashboard-domain: "fleet" is not a domain name`},
		{"bad IP", func(e *initEnv) []string { return e.nonInteractiveArgs("--public-ip", "nope") }, "is not an IP address"},
		{"bad email", func(e *initEnv) []string {
			return e.nonInteractiveArgs("--admin-email", "Admin <a@example.com>")
		}, "is not an email address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newInitEnv(t)
			r := e.run("", tt.args(e)...)
			if r.code != 1 || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("exit %d, want stderr containing %q, got: %s", r.code, tt.want, r.stderr)
			}
			if fileExists(e.keyFile) || fileExists(e.configPath) || len(e.platform.ups) != 0 {
				t.Error("init did something despite invalid input")
			}
		})
	}
}

func TestInitRefusesExistingInstall(t *testing.T) {
	tests := []struct {
		name  string
		setup func(e *initEnv)
		want  string
	}{
		{"config file", func(e *initEnv) { writeFile(t, e.configPath, "data_dir: /srv\n") }, "config file"},
		{"key file", func(e *initEnv) { writeFile(t, e.keyFile, "precious\n") }, "master key file"},
		{"state database", func(e *initEnv) { writeFile(t, filepath.Join(e.dataDir, "state", "pressfleet.db"), "") }, "state database"},
		{"platform project", func(e *initEnv) {
			writeFile(t, filepath.Join(e.dataDir, "platform", platform.ComposeFile), "")
		}, "platform project"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newInitEnv(t)
			tt.setup(e)
			r := e.run("", e.nonInteractiveArgs()...)
			if r.code != 1 || !strings.Contains(r.stderr, "already installed") ||
				!strings.Contains(r.stderr, tt.want) || !strings.Contains(r.stderr, "--force") {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			if len(e.platform.ups) != 0 {
				t.Error("platform started")
			}
			if b, err := os.ReadFile(e.keyFile); tt.name == "key file" && (err != nil || string(b) != "precious\n") {
				t.Errorf("key file changed: %q, %v", b, err)
			}
		})
	}
}

func TestInitForceReusesKey(t *testing.T) {
	e := newInitEnv(t)
	first := e.assertInstalled(e.run("", e.nonInteractiveArgs()...))
	pwFile := filepath.Join(e.dataDir, "platform", platform.MariaDBRootPasswordSecret)
	pw, err := os.ReadFile(pwFile)
	if err != nil {
		t.Fatal(err)
	}
	redisPWFile := filepath.Join(e.dataDir, "platform", platform.RedisAdminPasswordSecret)
	redisPW, err := os.ReadFile(redisPWFile)
	if err != nil || len(redisPW) == 0 {
		t.Fatalf("Redis admin password: %q, %v", redisPW, err)
	}

	// Reinstalling with --force keeps the key and the MariaDB password, and
	// leaves the existing admin alone.
	e.host.DNS["fleet2.example.com"] = e.host.DNS["fleet.example.com"]
	e.platform = &fakePlatform{execErr: errors.New("exit status 1: creating user: " + auth.ErrEmailTaken.Error())}
	r := e.run("", e.nonInteractiveArgs("--force", "--dashboard-domain", "fleet2.example.com")...)
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	key, err := loadKey(e.keyFile, "")
	if err != nil || key.ID() != first.ID() {
		t.Errorf("key changed: %s -> %s (%v)", first.ID(), key.ID(), err)
	}
	if strings.Contains(r.stdout, key.Encode().Reveal()) || !strings.Contains(r.stdout, "Reusing the existing master key") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
	if !strings.Contains(r.stdout, "WARNING: "+cli.MasterKeyWarning) {
		t.Error("warning missing on reinstall")
	}
	if pw2, _ := os.ReadFile(pwFile); string(pw2) != string(pw) {
		t.Error("MariaDB root password changed")
	}
	if pw2, _ := os.ReadFile(redisPWFile); string(pw2) != string(redisPW) {
		t.Error("Redis admin password changed")
	}
	if !strings.Contains(r.stdout, "already exists; left unchanged") || strings.Contains(r.stdout, "Admin password:") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
	if b, err := os.ReadFile(e.configPath + ".bak"); err != nil || !strings.Contains(string(b), "fleet.example.com") {
		t.Errorf("config backup: %q, %v", b, err)
	}
	if b, _ := os.ReadFile(e.configPath); !strings.Contains(string(b), "fleet2.example.com") {
		t.Errorf("config not rewritten:\n%s", b)
	}

	// A broken existing key is never replaced.
	writeFile(t, e.keyFile, "garbage\n")
	r = e.run("", e.nonInteractiveArgs("--force")...)
	if r.code != 1 || !strings.Contains(r.stderr, "never replaces a master key") {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if b, _ := os.ReadFile(e.keyFile); string(b) != "garbage\n" {
		t.Error("broken key file was replaced")
	}
}

func TestInitPreflight(t *testing.T) {
	e := newInitEnv(t)
	e.docker.Info.DefaultAddressPools = nil
	r := e.run("", e.nonInteractiveArgs()...)
	if r.code != 1 || !strings.Contains(r.stderr, "required preflight checks failed: address-pools") ||
		!strings.Contains(r.stdout, "FAIL  Docker address pools") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	if fileExists(e.keyFile) || fileExists(e.configPath) {
		t.Error("files written despite failed preflight")
	}

	// Missing DNS records fail preflight too.
	e = newInitEnv(t)
	e.host.DNS = map[string][]netip.Addr{}
	if r := e.run("", e.nonInteractiveArgs()...); r.code != 1 || !strings.Contains(r.stderr, "dns-dashboard") {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}

	// --skip-preflight installs anyway and tells the control plane to skip
	// its own checks.
	e = newInitEnv(t)
	e.docker.Info.DefaultAddressPools = nil
	r = e.run("", e.nonInteractiveArgs("--skip-preflight")...)
	e.assertInstalled(r)
	if !strings.Contains(r.stdout, "PREFLIGHT CHECKS SKIPPED") {
		t.Errorf("stdout:\n%s", r.stdout)
	}
	if b, _ := os.ReadFile(filepath.Join(e.dataDir, "platform", platform.ComposeFile)); !strings.Contains(string(b), "PRESSFLEET_SKIP_PREFLIGHT") {
		t.Error("control plane not told to skip preflight")
	}
}

func TestInitPlatformFailures(t *testing.T) {
	e := newInitEnv(t)
	e.platform.upErr = errors.New("service control-plane is unhealthy")
	r := e.run("", e.nonInteractiveArgs()...)
	if r.code != 1 || !strings.Contains(r.stderr, "starting the platform: service control-plane is unhealthy") {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if len(e.platform.execs) != 0 {
		t.Error("admin created on a platform that didn't start")
	}

	e = newInitEnv(t)
	e.platform.execErr = errors.New("exit status 1: boom")
	if r := e.run("", e.nonInteractiveArgs()...); r.code != 1 || !strings.Contains(r.stderr, "creating the admin user: exit status 1: boom") {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}
}

// TestMasterKeyWarningIsVerbatim checks that init prints the §4.1 warning
// exactly as the design doc words it (minus Markdown emphasis), and that the
// installation guide repeats it, as §4.1 requires.
func TestMasterKeyWarningIsVerbatim(t *testing.T) {
	doc, err := os.ReadFile("../../../docs/pressfleet_architecture_and_implementation_document.md")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "(must appear in setup documentation and the `pressfleet init` output):** "
	var found string
	for line := range strings.SplitSeq(string(doc), "\n") {
		if _, rest, ok := strings.Cut(line, marker); ok {
			found = strings.ReplaceAll(rest, "**", "")
		}
	}
	if found != cli.MasterKeyWarning {
		t.Errorf("warning differs from the design doc:\n got: %q\nwant: %q", cli.MasterKeyWarning, found)
	}

	guide, err := os.ReadFile("../../../docs/installation.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ReplaceAll(string(guide), "**", ""), cli.MasterKeyWarning) {
		t.Error("docs/installation.md lacks the master key warning")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// loadKey reads a key file the way the control plane does.
func loadKey(path, dataDir string) (secrets.MasterKey, error) {
	return secrets.LoadMasterKey(secrets.KeySource{File: path, DataDir: dataDir}, slog.New(slog.DiscardHandler))
}
