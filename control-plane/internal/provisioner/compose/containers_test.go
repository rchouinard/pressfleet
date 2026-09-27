package compose_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

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

const inspectPHP = `[{"Id":"abc123","Name":"/pf-site1-php-1","Config":{"Image":"registry.example/site-php:php8.4-wp7.1.2"},
"State":{"Running":true},"NetworkSettings":{"Networks":{"pf-site1":{"IPAddress":"172.30.0.3"}}}}]`

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
	if c.ID != "abc123" || c.Name != "pf-site1-php-1" || c.Image != "registry.example/site-php:php8.4-wp7.1.2" || !c.Running ||
		c.Networks["pf-site1"] != "172.30.0.3" {
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
}
