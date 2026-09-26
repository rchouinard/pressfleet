package cli_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
)

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
// at an empty file so no real config is read.
func run(t *testing.T, ctx context.Context, args []string, env map[string]string) result {
	t.Helper()
	emptyConfig := filepath.Join(t.TempDir(), "pressfleet.yaml")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{config.EnvConfigPath: emptyConfig}
	for k, v := range env {
		vars[k] = v
	}
	var stdout, stderr syncBuffer
	code := cli.Run(ctx, args, cli.Options{
		Version: "v1.2.3-test",
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env: func(k string) (string, bool) {
			v, ok := vars[k]
			return v, ok
		},
	})
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
	done := make(chan result, 1)
	go func() {
		done <- run(t, ctx, []string{"serve", "--data-dir", dataDir, "--log-format", "json", "--listen", ":0"}, nil)
	}()

	// serve creates the data directory layout before logging that it started.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dataDir, "sites")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve did not create the data directory; result: %+v", <-done)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	r := <-done
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	for _, want := range []string{`"msg":"pressfleet starting"`, `"listen":":0"`, `"msg":"pressfleet stopped"`} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("log output missing %s:\n%s", want, r.stderr)
		}
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // serve returns immediately
	r := run(t, ctx, []string{"serve", "--data-dir", t.TempDir(), "--log-format", "text"},
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

func TestNotImplemented(t *testing.T) {
	for _, cmd := range []string{"init", "doctor"} {
		r := run(t, context.Background(), []string{cmd, "--data-dir", t.TempDir()}, nil)
		if r.code != 1 || !strings.Contains(r.stderr, "not implemented yet") {
			t.Errorf("%s: exit %d, stderr: %s", cmd, r.code, r.stderr)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	r := run(t, context.Background(), []string{"frobnicate"}, nil)
	if r.code != 1 || !strings.Contains(r.stderr, "unknown command") {
		t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
	}
}
