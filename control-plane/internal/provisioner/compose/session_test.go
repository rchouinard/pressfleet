package compose_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// sessionAttach fakes the attached `docker start` of a session worker: it
// echoes standard input to standard output, with a prefix, until the input
// ends.
type sessionAttach struct {
	t     *testing.T
	calls [][]string
	block bool
}

func (a *sessionAttach) attach(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	a.calls = append(a.calls, args)
	if !slices.Equal(args[:5], []string{"container", "start", "--attach", "--interactive", "--"}) {
		a.t.Fatalf("unexpected attached docker call: %q", args)
	}
	if a.block {
		<-ctx.Done()
		return &compose.CommandError{Args: args, Err: ctx.Err()}
	}
	b, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	_, _ = stdout.Write(append([]byte("echo:"), b...))
	_, _ = io.WriteString(stderr, "worker log\n")
	return nil
}

func newSessionProvisioner(t *testing.T, image string) (*compose.Provisioner, *taskDocker, *sessionAttach) {
	t.Helper()
	a := &sessionAttach{t: t}
	prov, f, _ := newTaskProvisioner(t, image, func(c *compose.ProvisionerConfig) { c.DockerAttach = a.attach })
	return prov, f, a
}

func TestRunSession(t *testing.T) {
	prov, f, a := newSessionProvisioner(t, testTaskImage)
	var stdout, stderr bytes.Buffer
	err := prov.RunSession(context.Background(), testSiteID, provisioner.Session{
		Command: []string{"pressfleet", "sftp-server", "--root", provisioner.TaskWPContentPath},
		Stdin:   strings.NewReader("sftp bytes\x00\xff"),
		Stdout:  &stdout,
		Stderr:  &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "echo:sftp bytes\x00\xff" || stderr.String() != "worker log\n" {
		t.Errorf("streams: stdout %q, stderr %q", stdout.String(), stderr.String())
	}

	args := strings.Join(f.createArgs, " ")
	for _, want := range []string{
		"--label pressfleet.site-id=" + testSiteID,
		"--label pressfleet.session=true",
		"--interactive",
		"--network none",
		"--user " + testPHPUser,
		"--read-only",
		"--cap-drop ALL",
		"--security-opt no-new-privileges:true",
		"--memory 128m",
		"--pids-limit 64",
		"--mount type=volume,source=" + testSiteVolume + ",target=/site/wp-content",
		"--entrypoint=pressfleet",
		"-- " + testTaskImage + " sftp-server --root /site/wp-content",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("create args %q lack %q", args, want)
		}
	}
	// Only wp-content: no bind mounts, no other volumes, no other network.
	if strings.Count(args, "--mount") != 1 || strings.Contains(args, "readonly") || strings.Count(args, "--network") != 1 {
		t.Errorf("create args %q mount or connect more than wp-content", args)
	}
	name := f.createArgs[slices.Index(f.createArgs, "--name")+1]
	if !strings.HasPrefix(name, "pf-"+testSiteID+"-session-") || len(a.calls) != 1 || a.calls[0][5] != name {
		t.Errorf("worker %q, attach calls %q", name, a.calls)
	}
	if len(f.removed) != 1 || f.removed[0] != name {
		t.Errorf("removed = %v, want the worker", f.removed)
	}
}

func TestRunSessionExitCode(t *testing.T) {
	prov, f, _ := newSessionProvisioner(t, testTaskImage)
	f.exitCode = 3
	err := prov.RunSession(context.Background(), testSiteID, provisioner.Session{
		Command: []string{"false"}, Stdin: strings.NewReader(""), Stdout: io.Discard,
	})
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) || ee.ExitCode != 3 || !strings.Contains(ee.Stderr, "worker log") {
		t.Errorf("RunSession = %v, want an ExecError with exit code 3 and the worker's stderr", err)
	}
	if len(f.removed) != 1 {
		t.Errorf("removed = %v", f.removed)
	}
}

func TestRunSessionCanceled(t *testing.T) {
	prov, f, a := newSessionProvisioner(t, testTaskImage)
	a.block = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- prov.RunSession(ctx, testSiteID, provisioner.Session{
			Command: []string{"sleep"}, Stdin: strings.NewReader(""), Stdout: io.Discard,
		})
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("RunSession = %v, want context.Canceled", err)
	}
	if len(f.removed) != 1 {
		t.Errorf("a canceled worker must be removed: removed = %v", f.removed)
	}
}

func TestRunSessionSiteNotFound(t *testing.T) {
	prov, f, a := newSessionProvisioner(t, testTaskImage)
	f.noSite = true
	err := prov.RunSession(context.Background(), testSiteID, provisioner.Session{
		Command: []string{"x"}, Stdin: strings.NewReader(""), Stdout: io.Discard,
	})
	if !errors.Is(err, provisioner.ErrSiteNotFound) || f.created() || len(a.calls) != 0 {
		t.Errorf("RunSession = %v (created %v, attached %d)", err, f.created(), len(a.calls))
	}
}

func TestRunSessionInvalid(t *testing.T) {
	tests := []struct {
		name  string
		image string
		s     provisioner.Session
	}{
		{"no image", "", provisioner.Session{Command: []string{"x"}, Stdin: strings.NewReader(""), Stdout: io.Discard}},
		{"bad image", "", provisioner.Session{Image: "-rm", Command: []string{"x"}, Stdin: strings.NewReader(""), Stdout: io.Discard}},
		{"no command", testTaskImage, provisioner.Session{Stdin: strings.NewReader(""), Stdout: io.Discard}},
		{"NUL", testTaskImage, provisioner.Session{Command: []string{"x\x00"}, Stdin: strings.NewReader(""), Stdout: io.Discard}},
		{"no stdin", testTaskImage, provisioner.Session{Command: []string{"x"}, Stdout: io.Discard}},
		{"no stdout", testTaskImage, provisioner.Session{Command: []string{"x"}, Stdin: strings.NewReader("")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prov, f, _ := newSessionProvisioner(t, tt.image)
			err := prov.RunSession(context.Background(), testSiteID, tt.s)
			if !errors.Is(err, compose.ErrInvalidSession) || f.created() {
				t.Errorf("RunSession = %v (created %v), want ErrInvalidSession", err, f.created())
			}
		})
	}
	prov, _, _ := newSessionProvisioner(t, testTaskImage)
	if err := prov.RunSession(context.Background(), "../x", provisioner.Session{}); err == nil {
		t.Error("RunSession accepted an invalid site ID")
	}
}
