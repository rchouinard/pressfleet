package compose_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// scripted is a fake ExecFunc that answers by the docker subcommand.
type scripted struct {
	calls   [][]string
	answers map[string]string // "container ls" -> output
}

func (s *scripted) exec(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	s.calls = append(s.calls, args)
	if len(args) < 2 {
		return nil, errors.New("unexpected command")
	}
	out, ok := s.answers[args[0]+" "+args[1]]
	if !ok {
		return nil, errors.New("unexpected command " + strings.Join(args, " "))
	}
	return []byte(out), nil
}

const inspectPHP = `[{"Id":"abc123","Name":"/pf-site1-php-1","Image":"sha256:def456","Config":{"Image":"registry.example/site-php:php8.4-wp7.1.2"},
"State":{"Running":true},"NetworkSettings":{"Networks":{"pf-site1":{"IPAddress":"172.30.0.3"}}},
"Mounts":[{"Type":"volume","Name":"pf-site1_wp-content","Source":"/var/lib/docker/volumes/pf-site1_wp-content/_data",
"Destination":"/var/www/html/wp-content","RW":true}]}]`

func TestServiceContainerAndExecService(t *testing.T) {
	ctx := context.Background()
	s := &scripted{answers: map[string]string{
		"container ls":      "abc123\n",
		"container inspect": inspectPHP,
		"container exec":    "8.4.1\n",
	}}
	p := compose.Project{Name: "pf-site1", Dir: "/data/sites/site1", Docker: s.exec}

	c, err := p.ServiceContainer(ctx, "php")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "abc123" || c.Name != "pf-site1-php-1" || c.Image != "registry.example/site-php:php8.4-wp7.1.2" || c.ImageID != "sha256:def456" || !c.Running ||
		c.Networks["pf-site1"] != "172.30.0.3" ||
		len(c.Mounts) != 1 || c.Mounts[0].Destination != "/var/www/html/wp-content" || !c.Mounts[0].RW {
		t.Errorf("container = %+v", c)
	}
	// Found by label, without the project directory.
	wantLs := []string{"container", "ls", "--all", "--no-trunc", "--filter", "label=com.docker.compose.project=pf-site1",
		"--filter", "label=com.docker.compose.service=php", "--format", "{{.ID}}"}
	if !slices.Equal(s.calls[0], wantLs) {
		t.Errorf("ls = %q", s.calls[0])
	}
	for _, call := range s.calls {
		if slices.Contains(call, "/data/sites/site1") {
			t.Errorf("command reads the project directory: %q", call)
		}
	}

	out, err := p.ExecService(ctx, "php", "php", "-r", "echo PHP_VERSION;")
	if err != nil || string(out) != "8.4.1\n" {
		t.Fatalf("ExecService = %q, %v", out, err)
	}
	if last := s.calls[len(s.calls)-1]; !slices.Equal(last, []string{"container", "exec", "abc123", "php", "-r", "echo PHP_VERSION;"}) {
		t.Errorf("exec = %q", last)
	}

	s.answers["container ls"] = ""
	if _, err := p.ServiceContainer(ctx, "php"); !errors.Is(err, compose.ErrNoContainer) {
		t.Errorf("no container: err = %v", err)
	}
	s.answers["container ls"] = "a\nb\n"
	if _, err := p.ServiceContainer(ctx, "php"); err == nil {
		t.Error("two containers: no error")
	}
	if _, err := p.ExecService(ctx, "php"); !errors.Is(err, compose.ErrInvalidProject) {
		t.Errorf("exec without a command: err = %v", err)
	}
}

func TestRunContainer(t *testing.T) {
	ctx := context.Background()
	r := &recorder{out: []byte("HTTP/1.1 200 OK\n")}
	out, err := compose.RunContainer(ctx, r.exec, compose.RunOptions{
		Image: "registry.example/site-php:php8.4-wp7.1.2", Network: "pf-site1",
		Entrypoint: "curl", Command: []string{"-sS", "http://nginx:8080/"},
	})
	if err != nil || string(out) != "HTTP/1.1 200 OK\n" {
		t.Fatalf("RunContainer = %q, %v", out, err)
	}
	want := []string{"container", "run", "--rm", "--user", "65534:65534", "--read-only", "--tmpfs", "/tmp",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--pull", "never",
		"--network", "pf-site1", "--entrypoint", "curl", "--", "registry.example/site-php:php8.4-wp7.1.2", "-sS", "http://nginx:8080/"}
	if !slices.Equal(r.args[0], want) {
		t.Errorf("args = %q\nwant %q", r.args[0], want)
	}
	if _, err := compose.RunContainer(ctx, r.exec, compose.RunOptions{}); !errors.Is(err, compose.ErrInvalidProject) {
		t.Errorf("without an image: err = %v", err)
	}

	// Standard input needs --interactive.
	if _, err := compose.RunContainer(ctx, r.exec, compose.RunOptions{
		Image: "mariadb:11.8", Command: []string{"cat"}, Stdin: strings.NewReader("SELECT 1;"),
	}); err != nil {
		t.Fatal(err)
	}
	last := r.args[len(r.args)-1]
	if !slices.Contains(last, "--interactive") || slices.Index(last, "--interactive") > slices.Index(last, "--") ||
		r.stdin[len(r.stdin)-1] != "SELECT 1;" {
		t.Errorf("args = %q, stdin %q", last, r.stdin[len(r.stdin)-1])
	}
}

func TestHealthchecks(t *testing.T) {
	long := strings.Repeat("x", 5000)
	inspect := `{"Name":"/pressfleet-traefik-1","RestartCount":2,"State":{"Status":"running","ExitCode":0,"OOMKilled":false,` +
		`"Health":{"Status":"unhealthy","FailingStreak":3,"Log":[{"Start":"2026-10-31T10:00:00Z","End":"2026-10-31T10:00:05Z","ExitCode":1,"Output":"` + long + `"}]}}}
{"Name":"/pressfleet-redis-1","RestartCount":0,"State":{"Status":"running","ExitCode":0,"OOMKilled":false}}
`
	s := &scripted{answers: map[string]string{"container ls": "a\nb\n", "container inspect": inspect}}
	p := compose.Project{Name: "pressfleet", Dir: "/data/platform", Docker: s.exec}
	got, err := p.Healthchecks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d containers with a healthcheck, want 1: %+v", len(got), got)
	}
	h := got[0]
	if h.Name != "pressfleet-traefik-1" || h.Health != "unhealthy" || h.FailingStreak != 3 || h.RestartCount != 2 || len(h.Probes) != 1 ||
		h.Probes[0].ExitCode != 1 || h.Probes[0].End.Sub(h.Probes[0].Start) != 5*time.Second {
		t.Errorf("health = %+v", h)
	}
	if o := h.Probes[0].Output; len(o) >= len(long) || !strings.HasSuffix(o, "(cut)") {
		t.Errorf("probe output isn't bounded: %d bytes", len(o))
	}
	// Only the state is asked for, so the environment is never read.
	if format := strings.Join(s.calls[1], " "); strings.Contains(format, ".Config") {
		t.Errorf("inspect reads the configuration: %q", format)
	}
}
