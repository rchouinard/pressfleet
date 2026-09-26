package compose_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// hostileArgs are arguments a shell, the docker CLI, or timeout(1) would
// interpret if they were ever parsed rather than passed through as argv.
// WP-CLI parses --name arguments itself, even after "--", and rejects ones
// the command doesn't know, so the options here are ones it accepts.
var hostileArgs = []string{
	"; rm -rf /",
	"$(touch /tmp/pwned)",
	"`touch /tmp/pwned`",
	"line1\nline2",
	`it's "quoted" \ back`,
	"*",
	"$HOME",
	"| cat /etc/passwd",
	"--",
	"--user=0",
	"-T",
	"--url=http://x/$(id)",
	"",
}

// exitError is a fake process exit status.
type exitError int

func (e exitError) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitError) ExitCode() int { return int(e) }

func runningFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	if _, err := f.prov.CreateSite(context.Background(), f.cfg); err != nil {
		t.Fatal(err)
	}
	f.docker.calls = nil
	return f
}

func TestExecWPCLIPassesArgsVerbatim(t *testing.T) {
	f := runningFixture(t)
	f.docker.execOut = []byte("output\n")
	args := append([]string{"eval-file", "/tmp/x.php"}, hostileArgs...)

	out, err := f.prov.ExecWPCLI(context.Background(), testSiteID, args)
	if err != nil {
		t.Fatal(err)
	}
	if out != "output\n" {
		t.Errorf("output = %q", out)
	}
	// The site's UID, a "--" ending the options, and timeout(1) running wp;
	// the arguments follow as separate, unchanged argv elements.
	got := f.docker.execArgs
	prefix := []string{"exec", "--no-TTY", "--user", "20042:20042", "--", "php", "timeout", "--kill-after=10s"}
	if len(got) < len(prefix)+2+len(args) || !slices.Equal(got[:len(prefix)], prefix) {
		t.Fatalf("exec argv = %q", got)
	}
	if got[len(prefix)+1] != "/usr/local/bin/wp" {
		t.Errorf("command = %q, want /usr/local/bin/wp", got[len(prefix)+1])
	}
	if tail := got[len(prefix)+2:]; !slices.Equal(tail, args) {
		t.Errorf("wp argv = %q, want %q", tail, args)
	}
}

func TestExecWPCLITimeout(t *testing.T) {
	f := runningFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := f.prov.ExecWPCLI(ctx, testSiteID, []string{"cron", "event", "run", "--due-now"}); err != nil {
		t.Fatal(err)
	}
	// The in-container limit outlasts the caller's deadline, so the caller
	// sees its own context error rather than timeout(1)'s exit status.
	got := f.docker.execArgs[8]
	secs, err := strconv.Atoi(strings.TrimSuffix(got, "s"))
	if err != nil || secs < 30 || secs > 31 {
		t.Errorf("in-container timeout = %q, want 30s or 31s", got)
	}

	// Without a deadline, the configured WP-CLI timeout applies.
	if _, err := f.prov.ExecWPCLI(context.Background(), testSiteID, []string{"option", "get", "home"}); err != nil {
		t.Fatal(err)
	}
	if got := f.docker.execArgs[8]; got != "601s" {
		t.Errorf("default in-container timeout = %q, want 601s", got)
	}
}

func TestExecWPCLIExitError(t *testing.T) {
	f := runningFixture(t)
	f.docker.execOut = []byte("partial")
	f.docker.execErr = &compose.CommandError{
		Args:   []string{"compose", "exec", "php", "wp", "user", "create", "--user_pass=hunter2"},
		Stderr: "Error: boom",
		Err:    exitError(3),
	}
	_, err := f.prov.ExecWPCLI(context.Background(), testSiteID, []string{"user", "create", "--user_pass=hunter2"})
	var ee *provisioner.ExecError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *provisioner.ExecError", err)
	}
	if ee.ExitCode != 3 || ee.Stderr != "Error: boom" || ee.Stdout != "partial" {
		t.Errorf("ExecError = %+v", ee)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error reveals the arguments: %v", err)
	}
}

func TestExecWPCLICommandFails(t *testing.T) {
	f := runningFixture(t)
	f.docker.execErr = &compose.CommandError{
		Args: []string{"compose", "exec", "php", "wp", "--user_pass=hunter2"},
		Err:  errors.New("exec: \"docker\": executable file not found in $PATH"),
	}
	_, err := f.prov.ExecWPCLI(context.Background(), testSiteID, []string{"--user_pass=hunter2"})
	var ee *provisioner.ExecError
	if !errors.Is(err, compose.ErrCommandFailed) || errors.As(err, &ee) {
		t.Errorf("err = %v, want ErrCommandFailed and no ExecError", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error reveals the arguments: %v", err)
	}
}

func TestExecWPCLIContextEnded(t *testing.T) {
	f := runningFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.docker.execErr = &compose.CommandError{Err: errors.New("signal: killed")}
	cancel()
	// Ps and inspect succeed in the fake even with ctx canceled; the
	// failure is attributed to the context.
	if _, err := f.prov.ExecWPCLI(ctx, testSiteID, []string{"option", "list"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestExecWPCLITruncatesOutput(t *testing.T) {
	f := runningFixture(t)
	p, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout: f.layout, Secrets: memSecrets{}, Images: imageMap{}, MariaDBContainer: testMariaDB,
		Docker: f.docker.exec, WPCLIMaxOutput: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.docker.execOut = []byte("0123456789")
	out, err := p.ExecWPCLI(context.Background(), testSiteID, []string{"export", "--stdout"})
	if !errors.Is(err, compose.ErrOutputTruncated) || out != "0123" {
		t.Errorf("ExecWPCLI = %q, %v; want the first 4 bytes and ErrOutputTruncated", out, err)
	}
	f.docker.execOut = []byte("0123")
	if out, err := p.ExecWPCLI(context.Background(), testSiteID, []string{"option", "get", "x"}); err != nil || out != "0123" {
		t.Errorf("output at the limit: %q, %v", out, err)
	}
}

func TestExecWPCLIRefuses(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*fixture)
		siteID string
		args   []string
		want   error
	}{
		{"missing site", func(f *fixture) { clear(f.docker.containers) }, testSiteID, nil, provisioner.ErrSiteNotFound},
		{"stopped site", func(f *fixture) {
			if err := f.prov.StopSite(context.Background(), testSiteID); err != nil {
				panic(err)
			}
		}, testSiteID, nil, compose.ErrSiteNotRunning},
		{"root user", func(f *fixture) { f.docker.phpUser = "0:0" }, testSiteID, nil, compose.ErrUnexpectedUser},
		{"no user", func(f *fixture) { f.docker.phpUser = "" }, testSiteID, nil, compose.ErrUnexpectedUser},
		{"uid outside the site range", func(f *fixture) { f.docker.phpUser = "1000:1000" }, testSiteID, nil, compose.ErrUnexpectedUser},
		{"gid differs", func(f *fixture) { f.docker.phpUser = "20042:0" }, testSiteID, nil, compose.ErrUnexpectedUser},
		{"invalid site ID", func(*fixture) {}, "../x", nil, compose.ErrInvalidSite},
		{"NUL in argument", func(*fixture) {}, testSiteID, []string{"a\x00b"}, compose.ErrInvalidCommand},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := runningFixture(t)
			tt.setup(f)
			f.docker.execArgs = nil
			if _, err := f.prov.ExecWPCLI(context.Background(), tt.siteID, tt.args); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if f.docker.execArgs != nil {
				t.Errorf("ran exec anyway: %q", f.docker.execArgs)
			}
		})
	}
}
