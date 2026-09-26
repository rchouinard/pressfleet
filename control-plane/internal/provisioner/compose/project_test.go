package compose_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// recorder is a fake ExecFunc that records the arguments it was called with
// and what it read from stdin.
type recorder struct {
	args  [][]string
	stdin []string
	out   []byte
	err   error
}

func (r *recorder) exec(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	r.args = append(r.args, args)
	in := ""
	if stdin != nil {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return nil, err
		}
		in = string(b)
	}
	r.stdin = append(r.stdin, in)
	return r.out, r.err
}

func project(r *recorder) compose.Project {
	return compose.Project{Name: "pf-site1", Dir: "/data/sites/site1", Docker: r.exec}
}

var prefix = []string{"compose", "--project-name", "pf-site1", "--project-directory", "/data/sites/site1"}

func TestCommands(t *testing.T) {
	tests := []struct {
		name string
		call func(compose.Project) error
		want []string
	}{
		{"up", func(p compose.Project) error { return p.Up(context.Background(), compose.UpOptions{}) },
			[]string{"up", "--detach", "--remove-orphans"}},
		{"up wait", func(p compose.Project) error {
			return p.Up(context.Background(), compose.UpOptions{Wait: true, WaitTimeout: 90 * time.Second})
		}, []string{"up", "--detach", "--remove-orphans", "--wait", "--wait-timeout", "90"}},
		{"down", func(p compose.Project) error { return p.Down(context.Background(), compose.DownOptions{}) },
			[]string{"down", "--remove-orphans"}},
		{"down volumes", func(p compose.Project) error {
			return p.Down(context.Background(), compose.DownOptions{Volumes: true})
		}, []string{"down", "--remove-orphans", "--volumes"}},
		{"ps", func(p compose.Project) error { _, err := p.Ps(context.Background()); return err },
			[]string{"ps", "--all", "--format", "json"}},
		{"exec", func(p compose.Project) error {
			_, err := p.Exec(context.Background(), "control-plane", "pressfleet", "version")
			return err
		}, []string{"exec", "--no-TTY", "control-plane", "pressfleet", "version"}},
		{"logs", func(p compose.Project) error { _, err := p.Logs(context.Background()); return err },
			[]string{"logs", "--no-color", "--timestamps"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &recorder{}
			if err := tt.call(project(r)); err != nil {
				t.Fatal(err)
			}
			want := append(append([]string{}, prefix...), tt.want...)
			if len(r.args) != 1 || !reflect.DeepEqual(r.args[0], want) {
				t.Errorf("args = %q, want %q", r.args, want)
			}
		})
	}
}

func TestExecInput(t *testing.T) {
	r := &recorder{}
	_, err := project(r).ExecInput(context.Background(), strings.NewReader("s3cret\n"),
		"control-plane", "pressfleet", "user", "create", "--password-stdin")
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]string{}, prefix...), "exec", "--no-TTY", "control-plane", "pressfleet", "user", "create", "--password-stdin")
	if len(r.args) != 1 || !reflect.DeepEqual(r.args[0], want) {
		t.Errorf("args = %q, want %q", r.args, want)
	}
	if r.stdin[0] != "s3cret\n" {
		t.Errorf("stdin = %q", r.stdin[0])
	}
}

func TestPort(t *testing.T) {
	tests := []struct {
		name, out, want string
		wantErr         error
	}{
		{"published", "127.0.0.1:49153\n", "127.0.0.1:49153", nil},
		{"several", "0.0.0.0:49153\n[::]:49153\n", "0.0.0.0:49153", nil},
		{"unpublished", ":0\n", "", compose.ErrNotPublished},
		{"empty", "", "", compose.ErrNotPublished},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &recorder{out: []byte(tt.out)}
			got, err := project(r).Port(context.Background(), "control-plane", 8080)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Errorf("got %q, %v; want %q, %v", got, err, tt.want, tt.wantErr)
			}
			want := append(append([]string{}, prefix...), "port", "control-plane", "8080")
			if !reflect.DeepEqual(r.args[0], want) {
				t.Errorf("args = %q, want %q", r.args[0], want)
			}
		})
	}
	if _, err := project(&recorder{}).Port(context.Background(), "control-plane", 0); !errors.Is(err, compose.ErrInvalidProject) {
		t.Errorf("port 0: err = %v", err)
	}
}

func TestResources(t *testing.T) {
	outputs := map[string]string{
		"container": "pf-site1-php-1\npf-site1-nginx-1\n",
		"network":   "pf-site1_default\n",
		"volume":    "",
	}
	var calls [][]string
	p := compose.Project{Name: "pf-site1", Dir: "/data/sites/site1", Docker: func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte(outputs[args[0]]), nil
	}}
	res, err := p.Resources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := compose.Resources{
		Containers: []string{"pf-site1-php-1", "pf-site1-nginx-1"},
		Networks:   []string{"pf-site1_default"},
		Volumes:    []string{},
	}
	if !reflect.DeepEqual(res, want) || res.Empty() {
		t.Errorf("got %+v, want %+v", res, want)
	}
	for _, c := range calls {
		if !slices.Contains(c, "label=com.docker.compose.project=pf-site1") {
			t.Errorf("call %q doesn't filter by project label", c)
		}
	}
	if !(compose.Resources{}).Empty() {
		t.Error("zero Resources is not Empty")
	}
}

func TestPsParsesBothFormats(t *testing.T) {
	want := []compose.Container{
		{Name: "pf-site1-php-1", Service: "php", State: "running", Health: "healthy"},
		{Name: "pf-site1-nginx-1", Service: "nginx", State: "exited", ExitCode: 137},
	}
	tests := map[string]string{
		"ndjson": `{"Name":"pf-site1-php-1","Service":"php","State":"running","Health":"healthy","ExitCode":0,"Image":"x"}
{"Name":"pf-site1-nginx-1","Service":"nginx","State":"exited","Health":"","ExitCode":137}
`,
		"array": `[{"Name":"pf-site1-php-1","Service":"php","State":"running","Health":"healthy"},
{"Name":"pf-site1-nginx-1","Service":"nginx","State":"exited","ExitCode":137}]`,
	}
	for name, out := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := project(&recorder{out: []byte(out)}).Ps(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}

	got, err := project(&recorder{out: []byte("\n")}).Ps(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("empty output: got %+v, %v", got, err)
	}
	if _, err := project(&recorder{out: []byte("not json")}).Ps(context.Background()); err == nil {
		t.Error("invalid output: expected an error")
	}
}

func TestInvalidProject(t *testing.T) {
	tests := map[string]compose.Project{
		"empty name":    {Name: "", Dir: "/x"},
		"uppercase":     {Name: "PressFleet", Dir: "/x"},
		"leading dash":  {Name: "-p", Dir: "/x"},
		"relative dir":  {Name: "pressfleet", Dir: "platform"},
		"empty dir":     {Name: "pressfleet"},
		"flag in names": {Name: "a b", Dir: "/x"},
	}
	for name, p := range tests {
		t.Run(name, func(t *testing.T) {
			r := &recorder{}
			p.Docker = r.exec
			if err := p.Up(context.Background(), compose.UpOptions{}); !errors.Is(err, compose.ErrInvalidProject) {
				t.Errorf("err = %v, want ErrInvalidProject", err)
			}
			if len(r.args) != 0 {
				t.Errorf("docker was invoked: %q", r.args)
			}
		})
	}

	if _, err := project(&recorder{}).Exec(context.Background(), "php"); !errors.Is(err, compose.ErrInvalidProject) {
		t.Errorf("exec without command: err = %v", err)
	}
}

func TestCommandErrorIsWrapped(t *testing.T) {
	cmdErr := &compose.CommandError{Args: []string{"compose", "up"}, Stderr: "no such image", Err: &exec.ExitError{}}
	err := project(&recorder{err: cmdErr}).Up(context.Background(), compose.UpOptions{})
	if !errors.Is(err, compose.ErrCommandFailed) {
		t.Errorf("errors.Is(ErrCommandFailed) = false for %v", err)
	}
	var ce *compose.CommandError
	if !errors.As(err, &ce) || ce.Stderr != "no such image" {
		t.Errorf("errors.As(*CommandError) failed for %v", err)
	}
	if !strings.Contains(err.Error(), "pf-site1") || !strings.Contains(err.Error(), "no such image") {
		t.Errorf("error message lacks context: %v", err)
	}
}

func TestDockerExec(t *testing.T) {
	// A stand-in docker binary on PATH, so this runs without Docker.
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = fail ]; then echo \"boom\" >&2; exit 3; fi\n" +
		"if [ \"$1\" = stdin ]; then read -r line; echo \"stdin: $line\"; exit 0; fi\necho \"args: $*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	out, err := compose.DockerExec(context.Background(), nil, "compose", "ps")
	if err != nil || string(out) != "args: compose ps\n" {
		t.Errorf("got %q, %v", out, err)
	}

	out, err = compose.DockerExec(context.Background(), strings.NewReader("hello\n"), "stdin")
	if err != nil || string(out) != "stdin: hello\n" {
		t.Errorf("stdin: got %q, %v", out, err)
	}

	_, err = compose.DockerExec(context.Background(), nil, "fail")
	var ce *compose.CommandError
	if !errors.As(err, &ce) || !errors.Is(err, compose.ErrCommandFailed) {
		t.Fatalf("err = %v, want *CommandError", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || ce.Stderr != "boom" {
		t.Errorf("err = %v, want exit 3 with stderr", err)
	}
}
