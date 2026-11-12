package provisioner

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWPCLIRunArgv(t *testing.T) {
	got := WPCLIRunArgv("abc", 29*time.Second+time.Millisecond, "/usr/local/bin/wp", "user", "create", "--user_pass=$(x)", "")
	want := []string{"sh", "-c", wpcliRunScript, "pressfleet-wpcli", "abc", "31", "10",
		"/usr/local/bin/wp", "user", "create", "--user_pass=$(x)", ""}
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q\nwant %q", got, want)
	}
	// A deadline that has passed still gets a second, and the arguments
	// are never part of the script.
	if got := WPCLIRunArgv("abc", -time.Hour, "wp"); got[5] != "1" {
		t.Errorf("seconds = %q, want 1", got[5])
	}
	if strings.Contains(wpcliRunScript, "user_pass") {
		t.Error("the script holds arguments")
	}
}

func TestWPCLIStopArgv(t *testing.T) {
	got := WPCLIStopArgv("abc")
	want := []string{"sh", "-c", wpcliStopScript, "pressfleet-wpcli-stop", "abc", "10"}
	if !slices.Equal(got, want) {
		t.Errorf("argv = %q\nwant %q", got, want)
	}
}

func TestNewWPCLICallID(t *testing.T) {
	a, b := NewWPCLICallID(), NewWPCLICallID()
	if a == b || len(a) != 16 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Errorf("ids %q and %q", a, b)
	}
}

// The scripts run in the php container under dash; these tests run them
// under the host's sh, which is all they need, with the host's setsid.

func needScriptTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"sh", "setsid", "sleep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found: %v", tool, err)
		}
	}
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("no /proc")
	}
}

// scriptRun starts the run script with the given seconds and grace, in
// place of the script's fixed grace period.
func scriptRun(id, secs, grace string, stdin string, argv ...string) *exec.Cmd {
	cmd := exec.Command("sh", append([]string{"-c", wpcliRunScript, "pressfleet-wpcli", id, secs, grace}, argv...)...)
	cmd.Stdin = strings.NewReader(stdin)
	cmd.WaitDelay = 20 * time.Second
	return cmd
}

// hasProcess reports whether a process's command line contains marker.
func hasProcess(t *testing.T, marker string) bool {
	t.Helper()
	files, _ := filepath.Glob("/proc/[0-9]*/cmdline")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		line := string(bytes.ReplaceAll(b, []byte{0}, []byte{' '}))
		// The scripts' own shells carry their arguments, the marker
		// among them.
		if strings.Contains(line, marker) && !strings.Contains(line, "pressfleet-wpcli") {
			return true
		}
	}
	return false
}

func TestWPCLIRunScriptCommand(t *testing.T) {
	needScriptTools(t)
	id := NewWPCLICallID()
	cmd := scriptRun(id, "30", "1", "from stdin\n", "sh", "-c", "cat; echo out; echo err >&2; exit 3")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Fatalf("err = %v, want exit status 3", err)
	}
	if stdout.String() != "from stdin\nout\n" || stderr.String() != "err\n" {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat("/tmp/.pressfleet-wpcli." + id); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("pid file left behind: %v", err)
	}
}

func TestWPCLIRunScriptKillsLeftovers(t *testing.T) {
	needScriptTools(t)
	// A child that ignores SIGTERM and outlives the command, holding
	// its output: the end of the command kills it, or this would hang.
	const marker = "sleep 4301"
	cmd := scriptRun(NewWPCLICallID(), "30", "1", "", "sh", "-c", `sh -c 'trap "" TERM; exec `+marker+`' & sleep 0.3; echo done`)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "done\n" {
		t.Errorf("stdout %q", stdout.String())
	}
	waitGone(t, marker)
}

func TestWPCLIRunScriptDeadline(t *testing.T) {
	needScriptTools(t)
	// WP-CLI ends on SIGTERM, but its child ignores it: the group is
	// killed after the grace period, which timeout(1) would not do.
	const marker = "sleep 4302"
	cmd := scriptRun(NewWPCLICallID(), "1", "1", "", "sh", "-c", `sh -c 'trap "" TERM; exec `+marker+`' & wait`)
	start := time.Now()
	if err := cmd.Run(); err == nil {
		t.Error("a command stopped at its deadline succeeded")
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the command ran %s past a 1 second deadline", took)
	}
	waitGone(t, marker)
}

func TestWPCLIStopScript(t *testing.T) {
	needScriptTools(t)
	const marker = "sleep 4303"
	id := NewWPCLICallID()
	cmd := scriptRun(id, "300", "1", "", "sh", "-c", `sh -c 'trap "" TERM; exec `+marker+`' & wait`)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// The stop command waits for the call's pid file, so it can start at
	// once.
	stop := exec.Command("sh", "-c", wpcliStopScript, "pressfleet-wpcli-stop", id, "1")
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("stop: %v: %s", err, out)
	}
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the command still runs after the stop command")
	}
	waitGone(t, marker)

	// With nothing to stop, it ends rather than wait on a missing file
	// for long.
	start := time.Now()
	stop = exec.Command("sh", "-c", wpcliStopScript, "pressfleet-wpcli-stop", NewWPCLICallID(), "1")
	if err := stop.Run(); err != nil {
		t.Errorf("stop of a call that never ran: %v", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("stop of a call that never ran took %s", took)
	}
}

func waitGone(t *testing.T, marker string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !hasProcess(t, marker) {
			return
		}
	}
	t.Errorf("%q still runs", marker)
}
