package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor/doctortest"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// fakeControlPlane stands in for the platform's control plane container.
// The zero value is a platform without one.
type fakeControlPlane struct {
	mu sync.Mutex
	// container is the control plane container; nil means none.
	container *compose.ContainerDetails
	// findErr fails finding the container, as when Docker is down.
	findErr error
	// info is what `pressfleet admin info` prints, and execErr makes it
	// fail instead.
	info    string
	execErr error

	projects []string
	execs    [][]string
}

// running returns a running control plane whose admin interface reports
// version.
func running(version string) *fakeControlPlane {
	return &fakeControlPlane{
		container: &compose.ContainerDetails{Running: true, Image: "ghcr.io/rchouinard/pressfleet/control-plane:" + version},
		info:      fmt.Sprintf(`{"interface":1,"version":%q,"ready":true}`, version),
	}
}

func (f *fakeControlPlane) probe(project string) cli.ControlPlaneProbe {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.projects = append(f.projects, project)
	return fakeProbe{f}
}

func (f *fakeControlPlane) calls() (projects []string, execs [][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.projects), slices.Clone(f.execs)
}

type fakeProbe struct{ f *fakeControlPlane }

func (p fakeProbe) ServiceContainer(_ context.Context, service string) (compose.ContainerDetails, error) {
	if service != "control-plane" {
		return compose.ContainerDetails{}, fmt.Errorf("unexpected service %q", service)
	}
	switch {
	case p.f.findErr != nil:
		return compose.ContainerDetails{}, p.f.findErr
	case p.f.container == nil:
		return compose.ContainerDetails{}, compose.ErrNoContainer
	}
	return *p.f.container, nil
}

func (p fakeProbe) ExecService(_ context.Context, service string, command ...string) ([]byte, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	p.f.execs = append(p.f.execs, append([]string{service}, command...))
	if p.f.execErr != nil {
		return nil, p.f.execErr
	}
	return []byte(p.f.info + "\n"), nil
}

// runAgainst runs the CLI (version v1.2.3-test) with cp as the installed
// control plane.
func runAgainst(t *testing.T, cp *fakeControlPlane, env map[string]string, args ...string) result {
	t.Helper()
	return runProbed(t, context.Background(), args, env, doctortest.Healthy(), doctortest.HealthyDocker(), &syncBuffer{}, cp)
}

func TestSkewCheckRefusesOutOfPolicy(t *testing.T) {
	tests := []struct {
		name    string
		cp      *fakeControlPlane
		wantErr []string
	}{
		{"control plane two minors ahead", running("v1.4.0"), []string{
			"pressfleet CLI v1.2.3-test can't manage control plane v1.4.0",
			"the CLI's minor version must be within one of the control plane's",
			"install a pressfleet CLI v1.3.x to v1.5.x",
			"can't be downgraded",
		}},
		{"control plane two minors behind", running("v1.0.5"), []string{
			"can't manage control plane v1.0.5",
			"upgrade the control plane to v1.2.3-test with `pressfleet upgrade`, or use a pressfleet CLI v1.0.x to v1.1.x",
		}},
		{"major version differs", running("v2.2.0"), []string{
			"their major versions must match",
			"install a pressfleet CLI v2.1.x to v2.3.x",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runAgainst(t, tt.cp, nil, "doctor")
			if r.code == 0 {
				t.Fatalf("exit 0, want non-zero; stdout:\n%s", r.stdout)
			}
			if r.stdout != "" {
				t.Errorf("doctor ran anyway:\n%s", r.stdout)
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(r.stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, r.stderr)
				}
			}
		})
	}
}

func TestSkewCheckAllows(t *testing.T) {
	notRunning := running("v1.4.0")
	notRunning.container.Running = false
	notRunning.container.Labels = map[string]string{"org.opencontainers.image.version": "v1.2.0"}
	unanswered := running("v1.3.1")
	unanswered.execErr = errors.New("exec failed")
	tests := []struct {
		name     string
		cp       *fakeControlPlane
		wantWarn string
	}{
		{"not installed", &fakeControlPlane{}, ""},
		{"same minor", running("v1.2.0"), ""},
		{"one minor ahead", running("v1.3.9"), ""},
		{"one minor behind", running("v1.1.0"), ""},
		// The label wins over the tag (v1.4.0, which would be refused).
		{"stopped, version from the image label", notRunning, ""},
		{"not answering, version from the image tag", unanswered, ""},
		{"Docker down", &fakeControlPlane{findErr: errors.New("Cannot connect to the Docker daemon")},
			"can't check the control plane's version: finding the control plane: Cannot connect to the Docker daemon"},
		{"development control plane", running("dev"), "development build"},
		{"no version anywhere", &fakeControlPlane{
			container: &compose.ContainerDetails{Image: "pressfleet@sha256:0123"},
		}, "its image has no version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runAgainst(t, tt.cp, nil, "doctor")
			if r.code != 0 {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			if !strings.Contains(r.stdout, "0 failed") {
				t.Errorf("doctor didn't run:\n%s", r.stdout)
			}
			if tt.wantWarn == "" && strings.Contains(r.stderr, "Warning") {
				t.Errorf("unexpected warning: %s", r.stderr)
			}
			if !strings.Contains(r.stderr, tt.wantWarn) {
				t.Errorf("stderr lacks %q: %s", tt.wantWarn, r.stderr)
			}
		})
	}
}

func TestSkewCheckAsksAdminInterface(t *testing.T) {
	cp := running("v1.2.0")
	r := runAgainst(t, cp, map[string]string{"PRESSFLEET_PLATFORM_PROJECT": "fleet"}, "doctor")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	projects, execs := cp.calls()
	if !slices.Equal(projects, []string{"fleet"}) {
		t.Errorf("probed projects %q, want the configured platform_project", projects)
	}
	want := [][]string{{"control-plane", "pressfleet", "admin", "info"}}
	if !slices.EqualFunc(execs, want, slices.Equal) {
		t.Errorf("execs = %q, want %q", execs, want)
	}
}

func TestSkewCheckCommands(t *testing.T) {
	// Each command against a control plane outside the policy: host
	// commands refuse, the exempt ones and those that run inside the
	// control plane container don't check.
	tests := []struct {
		args    []string
		checked bool
	}{
		{[]string{"doctor"}, true},
		{[]string{"metrics-access", "rotate"}, true},
		{[]string{"version"}, false},
		{[]string{"key", "generate"}, false},
		{[]string{"healthcheck"}, false},
		{[]string{"admin", "info"}, false},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			cp := running("v1.4.0")
			r := runAgainst(t, cp, map[string]string{"PRESSFLEET_LISTEN": "127.0.0.1:1"}, tt.args...)
			refused := strings.Contains(r.stderr, "can't manage control plane")
			if refused != tt.checked {
				t.Errorf("refused = %v, want %v; exit %d, stderr: %s", refused, tt.checked, r.code, r.stderr)
			}
			if tt.checked && r.code == 0 {
				t.Errorf("exit 0 after refusing")
			}
		})
	}
}

func TestInitChecksSkew(t *testing.T) {
	e := newInitEnv(t)
	e.controlPlane = running("v0.9.0")
	r := e.run("", e.nonInteractiveArgs()...)
	if r.code == 0 || !strings.Contains(r.stderr, "can't manage control plane v0.9.0") {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if len(e.platform.ups) != 0 {
		t.Error("init started the platform after refusing")
	}
}

func TestVersionShowsControlPlane(t *testing.T) {
	stopped := &fakeControlPlane{container: &compose.ContainerDetails{Image: "ghcr.io/rchouinard/pressfleet/control-plane:v1.2.1"}}
	notReady := running("v1.2.3-test")
	notReady.info = `{"interface":1,"version":"v1.2.3-test","ready":false,"ready_error":"down"}`
	tests := []struct {
		name string
		cp   *fakeControlPlane
		want []string
	}{
		{"same version", running("v1.2.3-test"), []string{"control plane: v1.2.3-test (admin interface 1)\n"}},
		{"not ready", notReady, []string{"control plane: v1.2.3-test (admin interface 1), not ready\n"}},
		{"not installed", &fakeControlPlane{}, []string{"control plane: not installed\n"}},
		{"stopped", stopped, []string{"control plane: v1.2.1 (from its image; not answering: the control plane container isn't running)"}},
		{"Docker down", &fakeControlPlane{findErr: errors.New("no daemon")}, []string{"control plane: unknown (finding the control plane: no daemon)"}},
		// Exempt: it reports the skew and still succeeds.
		{"out of policy", running("v1.4.0"), []string{
			"control plane: v1.4.0 (admin interface 1)\n",
			"pressfleet CLI v1.2.3-test can't manage control plane v1.4.0",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runAgainst(t, tt.cp, nil, "version")
			if r.code != 0 {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			if !strings.HasPrefix(r.stdout, "pressfleet v1.2.3-test (") {
				t.Errorf("stdout doesn't start with the CLI version:\n%s", r.stdout)
			}
			for _, want := range tt.want {
				if !strings.Contains(r.stdout, want) {
					t.Errorf("stdout lacks %q:\n%s", want, r.stdout)
				}
			}
		})
	}
}

func TestVersionClient(t *testing.T) {
	cp := running("v1.4.0")
	r := runAgainst(t, cp, nil, "version", "--client")
	if r.code != 0 || strings.Count(r.stdout, "\n") != 1 || strings.Contains(r.stdout, "control plane") {
		t.Fatalf("exit %d, stdout:\n%s", r.code, r.stdout)
	}
	if projects, _ := cp.calls(); len(projects) != 0 {
		t.Error("--client probed the control plane")
	}
}

func TestAdminInfo(t *testing.T) {
	// Nothing listens on port 1, so the control plane isn't ready.
	r := run(t, context.Background(), []string{"admin", "info"}, map[string]string{"PRESSFLEET_LISTEN": "127.0.0.1:1"})
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	var info struct {
		Interface  int    `json:"interface"`
		Version    string `json:"version"`
		Ready      bool   `json:"ready"`
		ReadyError string `json:"ready_error"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &info); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, r.stdout)
	}
	if info.Interface != 1 || info.Version != "v1.2.3-test" || info.Ready || !strings.Contains(info.ReadyError, "unhealthy") {
		t.Errorf("info = %+v", info)
	}
}

// TestAdminChanges runs `admin changes` against a state database of each
// dialect. Package changes tests what it lists; this tests the command.
func TestAdminChanges(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			cfg := storetest.Config(t, d)
			db := storetest.OpenUnmigrated(t, cfg)
			if _, err := db.Migrate(ctx, filepath.Join(t.TempDir(), "backups")); err != nil {
				t.Fatal(err)
			}
			old := store.Job{Type: "site.create", TargetType: store.JobTargetSite, TargetID: id.New()}
			if err := store.NewJobs(db).Enqueue(ctx, &old); err != nil {
				t.Fatal(err)
			}
			time.Sleep(2 * time.Millisecond)
			since := time.Now().UTC()
			created := store.Job{Type: "site.create", TargetType: store.JobTargetSite, TargetID: id.New()}
			if err := store.NewJobs(db).Enqueue(ctx, &created); err != nil {
				t.Fatal(err)
			}
			ev := store.AuditEvent{Action: "user.create", TargetType: "user", TargetID: id.New(), Result: store.AuditSuccess}
			if err := store.NewAuditEvents(db).Insert(ctx, &ev); err != nil {
				t.Fatal(err)
			}

			env := map[string]string{"PRESSFLEET_DATABASE_DRIVER": string(cfg.Driver)}
			if cfg.Driver == store.Postgres {
				env["PRESSFLEET_DATABASE_POSTGRES_DSN"] = cfg.PostgresDSN.Reveal()
			} else {
				env["PRESSFLEET_DATABASE_SQLITE_PATH"] = cfg.SQLitePath
			}
			r := run(t, ctx, []string{"admin", "changes", "--since", since.Format(time.RFC3339Nano)}, env)
			if r.code != 0 {
				t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
			}
			var got struct {
				Blocking []struct {
					Kind  string `json:"kind"`
					JobID string `json:"job_id"`
				} `json:"blocking"`
				Converged []json.RawMessage `json:"converged"`
				Lost      []struct {
					Kind     string `json:"kind"`
					TargetID string `json:"target_id"`
				} `json:"lost"`
			}
			if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, r.stdout)
			}
			if len(got.Blocking) != 1 || got.Blocking[0].JobID != created.ID || got.Converged == nil || len(got.Converged) != 0 ||
				len(got.Lost) != 1 || got.Lost[0].TargetID != ev.TargetID {
				t.Errorf("changes = %s", r.stdout)
			}
		})
	}

	for _, args := range [][]string{{"--since", "yesterday"}, {}} {
		r := run(t, ctx, append([]string{"admin", "changes"}, args...), nil)
		if r.code == 0 || !strings.Contains(r.stderr, "since") {
			t.Errorf("admin changes %v: exit %d, stderr: %s", args, r.code, r.stderr)
		}
	}
}
