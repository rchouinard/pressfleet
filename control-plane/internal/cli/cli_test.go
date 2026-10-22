package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository/repositorytest"
	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor/doctortest"
	"github.com/rchouinard/pressfleet/control-plane/internal/releasecheck"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
)

// testKey is the master key run and runWith supply by default; override
// PRESSFLEET_MASTER_KEY in env to use another one ("" means unset).
func testKey(t *testing.T) string { return secretstest.KeyText(t, 1) }

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type result struct {
	code           int
	stdout, stderr string
}

// run invokes the CLI with an isolated environment: PRESSFLEET_CONFIG points
// at an empty file so no real config is read, and preflight checks see a
// healthy fake host instead of the real host and Docker.
func run(t *testing.T, ctx context.Context, args []string, env map[string]string) result {
	t.Helper()
	return runWith(t, ctx, args, env, doctortest.Healthy(), doctortest.HealthyDocker())
}

// runWithStderr is like run but writes stderr to a caller-supplied buffer,
// so a test can watch the output of a long-running command.
func runWithStderr(t *testing.T, ctx context.Context, args []string, env map[string]string, stderr *syncBuffer) result {
	t.Helper()
	return runFull(t, ctx, args, env, doctortest.Healthy(), doctortest.HealthyDocker(), stderr)
}

// runWith is like run but with caller-supplied preflight fakes.
func runWith(t *testing.T, ctx context.Context, args []string, env map[string]string,
	host *doctortest.Host, docker *doctortest.Docker,
) result {
	t.Helper()
	return runFull(t, ctx, args, env, host, docker, &syncBuffer{})
}

func runFull(t *testing.T, ctx context.Context, args []string, env map[string]string,
	host *doctortest.Host, docker *doctortest.Docker, stderr *syncBuffer,
) result {
	t.Helper()
	return runProbed(t, ctx, args, env, host, docker, stderr, &fakeControlPlane{})
}

// runProbed is runFull with a caller-supplied control plane; the others see
// none installed.
func runProbed(t *testing.T, ctx context.Context, args []string, env map[string]string,
	host *doctortest.Host, docker *doctortest.Docker, stderr *syncBuffer, cp *fakeControlPlane,
) result {
	t.Helper()
	return runOptions(t, ctx, args, env, cli.Options{Stderr: stderr, HostProbe: host, DockerProbe: docker, ControlPlane: cp.probe})
}

// offlineReleases is the release source of every test that sets none: unit
// tests never reach GitHub.
var offlineReleases = releasecheck.SourceFunc(func(context.Context) ([]releasecheck.Release, error) {
	return nil, errors.New("no network in unit tests")
})

// runOptions runs the CLI (version v1.2.3-test) with opts, filling in its
// output, environment, and an offline release source, and fakes for
// anything else that would reach the host.
func runOptions(t *testing.T, ctx context.Context, args []string, env map[string]string, opts cli.Options) result {
	t.Helper()
	emptyConfig := filepath.Join(t.TempDir(), "pressfleet.yaml")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{config.EnvConfigPath: emptyConfig, "PRESSFLEET_MASTER_KEY": testKey(t)}
	for k, v := range env {
		vars[k] = v
	}
	var stdout syncBuffer
	stderr, ok := opts.Stderr.(*syncBuffer)
	if !ok {
		stderr = &syncBuffer{}
	}
	opts.Version = "v1.2.3-test"
	if opts.RepositoryTasks == nil {
		opts.RepositoryTasks = repositorytest.NewLocalRunner(t)
	}
	opts.Stdout, opts.Stderr = &stdout, stderr
	opts.Env = func(k string) (string, bool) {
		v, ok := vars[k]
		return v, ok
	}
	if opts.HostProbe == nil {
		opts.HostProbe = doctortest.Healthy()
	}
	if opts.DockerProbe == nil {
		opts.DockerProbe = doctortest.HealthyDocker()
	}
	if opts.ControlPlane == nil {
		opts.ControlPlane = (&fakeControlPlane{}).probe
	}
	if opts.ReleaseSource == nil {
		opts.ReleaseSource = offlineReleases
	}
	code := cli.Run(ctx, args, opts)
	return result{code, stdout.String(), stderr.String()}
}

func TestVersion(t *testing.T) {
	// Version must work even with a broken configuration.
	r := run(t, context.Background(), []string{"version"}, map[string]string{"PRESSFLEET_LOG_LEVEL": "bogus"})
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if !strings.HasPrefix(r.stdout, "pressfleet v1.2.3-test (") {
		t.Errorf("stdout = %q", r.stdout)
	}
}

func TestServeStartsAndStops(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr syncBuffer
	done := make(chan result, 1)
	go func() {
		done <- runWithStderr(t, ctx, []string{"serve", "--data-dir", dataDir, "--log-format", "json", "--listen", "127.0.0.1:0"}, nil, &stderr)
	}()

	// Wait for the server to log the address it bound.
	addrRE := regexp.MustCompile(`"msg":"http server listening","addr":"([^"]+)"`)
	var addr string
	for deadline := time.Now().Add(5 * time.Second); ; {
		if m := addrRE.FindStringSubmatch(stderr.String()); m != nil {
			addr = m[1]
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server did not start; result: %+v", <-done)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "sites")); err != nil {
		t.Errorf("data directory layout not created: %v", err)
	}

	for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusOK, "/api/v1/nope": http.StatusNotFound, "/nope": http.StatusOK} {
		resp, err := http.Get("http://" + addr + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
		if want == http.StatusNotFound && resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Errorf("GET %s Content-Type = %q", path, resp.Header.Get("Content-Type"))
		}
		// Anything that isn't the API is the dashboard (its router handles
		// /nope); an unknown API path is never answered with the dashboard.
		if path == "/nope" && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("GET %s Content-Type = %q, want the dashboard's index.html", path, resp.Header.Get("Content-Type"))
		}
	}
	cancel()

	r := <-done
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "state", "pressfleet.db")); err != nil {
		t.Errorf("state database not created: %v", err)
	}
	for _, want := range []string{`"msg":"pressfleet starting"`, `"listen":"127.0.0.1:0"`, `"msg":"state database ready"`, `"msg":"http server stopped"`, `"msg":"job workers started"`, `"msg":"job workers stopped"`, `"msg":"scheduler started"`, `"msg":"scheduler stopped"`, `"msg":"pressfleet stopped"`} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("log output missing %s:\n%s", want, r.stderr)
		}
	}
}

func TestServeFailsWhenListenFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	r := run(t, context.Background(), []string{"serve", "--data-dir", t.TempDir(), "--listen", ln.Addr().String()}, nil)
	if r.code != 1 || !strings.Contains(r.stderr, "listening on") {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // serve returns immediately
	r := run(t, ctx, []string{"serve", "--data-dir", t.TempDir(), "--log-format", "text", "--listen", "127.0.0.1:0"},
		map[string]string{"PRESSFLEET_LOG_FORMAT": "json", "PRESSFLEET_DATA_DIR": "relative/is/invalid"})
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if strings.Contains(r.stderr, `"msg"`) || !strings.Contains(r.stderr, "msg=") {
		t.Errorf("expected text log format, got:\n%s", r.stderr)
	}
}

func TestInvalidConfig(t *testing.T) {
	r := run(t, context.Background(), []string{"serve"}, map[string]string{"PRESSFLEET_DATA_DIR": "relative"})
	if r.code != 1 || !strings.Contains(r.stderr, "data_dir must be an absolute path") {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}
}

func TestDoctorExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		breakIt  func(*doctortest.Host, *doctortest.Docker)
		wantCode int
		wantOut  []string
	}{
		{"healthy", func(*doctortest.Host, *doctortest.Docker) {}, 0,
			[]string{"PASS  Linux on amd64", "0 failed, 0 warned"}},
		{"required failure", func(_ *doctortest.Host, d *doctortest.Docker) { d.Info.DefaultAddressPools = nil }, 1,
			[]string{"FAIL  Docker address pools", `fix: Add this to /etc/docker/daemon.json`, `"default-address-pools"`}},
		{"recommended warning", func(h *doctortest.Host, _ *doctortest.Docker) { h.Mem = 4 << 30 }, 2,
			[]string{"WARN  Host memory*", "fix: Use a host with at least 8 GiB"}},
		{"failure beats warning", func(h *doctortest.Host, _ *doctortest.Docker) {
			h.Mem = 4 << 30
			h.Ports = map[int]bool{443: true}
		}, 1, []string{"FAIL  Ports 80 and 443 free", "WARN  Host memory*"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, docker := doctortest.Healthy(), doctortest.HealthyDocker()
			tt.breakIt(host, docker)
			r := runWith(t, context.Background(), []string{"doctor", "--data-dir", t.TempDir()}, nil, host, docker)
			if r.code != tt.wantCode {
				t.Errorf("exit %d, want %d; stdout:\n%s\nstderr: %s", r.code, tt.wantCode, r.stdout, r.stderr)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(r.stdout, want) {
					t.Errorf("stdout missing %q:\n%s", want, r.stdout)
				}
			}
			if r.stderr != "" {
				t.Errorf("stderr = %q, want empty", r.stderr)
			}
		})
	}
}

func TestDoctorJSON(t *testing.T) {
	host := doctortest.Healthy()
	host.DNS["fleet.example.com"] = []netip.Addr{netip.MustParseAddr("203.0.113.10")}
	r := runWith(t, context.Background(), []string{"doctor", "--json"},
		map[string]string{"PRESSFLEET_DASHBOARD_DOMAIN": "fleet.example.com"}, host, doctortest.HealthyDocker())
	if r.code != 0 {
		t.Fatalf("exit %d, stdout:\n%s\nstderr: %s", r.code, r.stdout, r.stderr)
	}
	var got struct {
		Outcome string `json:"outcome"`
		Results []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, r.stdout)
	}
	status := map[string]string{}
	for _, res := range got.Results {
		status[res.ID] = res.Status
	}
	if got.Outcome != "ok" || status["dns-dashboard"] != "pass" || status["dns-preview"] != "skip" {
		t.Errorf("outcome %q, statuses %v", got.Outcome, status)
	}
}

func TestServeRefusesOnPreflightFailure(t *testing.T) {
	docker := doctortest.HealthyDocker()
	docker.Info.CgroupVersion = "1"
	host := doctortest.Healthy()
	host.Ports = map[int]bool{80: true} // install scope only: must not block serve
	r := runWith(t, context.Background(), []string{"serve", "--data-dir", t.TempDir(), "--log-format", "json"}, nil, host, docker)
	if r.code != 1 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	for _, want := range []string{`"level":"ERROR","msg":"preflight check","check":"cgroup"`, "required preflight checks failed: cgroup (", "--skip-preflight"} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr missing %s:\n%s", want, r.stderr)
		}
	}
	if strings.Contains(r.stderr, "pressfleet starting") || strings.Contains(r.stderr, `"check":"ports"`) {
		t.Errorf("unexpected output:\n%s", r.stderr)
	}
}

func TestServeSkipPreflight(t *testing.T) {
	docker := doctortest.HealthyDocker()
	docker.InfoErr = errors.New("docker must not be called")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // serve returns immediately after starting
	r := runWith(t, ctx, []string{"serve", "--skip-preflight", "--data-dir", t.TempDir(), "--listen", "127.0.0.1:0"}, nil, doctortest.Healthy(), docker)
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if !strings.Contains(r.stderr, "PREFLIGHT CHECKS SKIPPED") || docker.EngineCalls != 0 {
		t.Errorf("engine calls %d, stderr:\n%s", docker.EngineCalls, r.stderr)
	}
}

func TestServeSkipPreflightEnv(t *testing.T) {
	failing := func() *doctortest.Docker {
		d := doctortest.HealthyDocker()
		d.Info.CgroupVersion = "1"
		return d
	}
	tests := []struct {
		name     string
		env      string
		flags    []string
		wantCode int
		wantErr  string
	}{
		{name: "env skips", env: "true", wantCode: 0},
		{name: "flag overrides env", env: "true", flags: []string{"--skip-preflight=false"}, wantCode: 1, wantErr: "required preflight checks failed"},
		{name: "env false runs checks", env: "0", wantCode: 1, wantErr: "required preflight checks failed"},
		{name: "invalid env", env: "maybe", wantCode: 1, wantErr: "PRESSFLEET_SKIP_PREFLIGHT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // serve returns immediately if it starts
			args := append([]string{"serve", "--data-dir", t.TempDir(), "--listen", "127.0.0.1:0"}, tt.flags...)
			r := runWith(t, ctx, args, map[string]string{"PRESSFLEET_SKIP_PREFLIGHT": tt.env}, doctortest.Healthy(), failing())
			if r.code != tt.wantCode || !strings.Contains(r.stderr, tt.wantErr) {
				t.Errorf("exit %d (want %d), stderr:\n%s", r.code, tt.wantCode, r.stderr)
			}
		})
	}
}

func TestUnknownCommand(t *testing.T) {
	r := run(t, context.Background(), []string{"frobnicate"}, nil)
	if r.code != 1 || !strings.Contains(r.stderr, "unknown command") {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}
}
