package cli_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
)

func TestKeyGenerate(t *testing.T) {
	// Works without a usable configuration, and so without a state database.
	broken := map[string]string{"PRESSFLEET_DATA_DIR": "relative", "PRESSFLEET_DATABASE_DRIVER": "bogus"}
	var keys []string
	for range 2 {
		r := run(t, context.Background(), []string{"key", "generate"}, broken)
		if r.code != 0 {
			t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
		}
		if _, err := secrets.ParseMasterKey(redact.Secret(r.stdout)); err != nil {
			t.Fatalf("output is not a master key: %v", err)
		}
		if !strings.Contains(r.stderr, "separate from this host and its backups") {
			t.Errorf("stderr = %q, want the master key warning", r.stderr)
		}
		keys = append(keys, r.stdout)
	}
	if keys[0] == keys[1] {
		t.Error("two runs generated the same key")
	}
}

// startAndStop runs serve with a cancelled context, so it returns as soon as
// it has started.
func startAndStop(t *testing.T, dataDir string, env map[string]string) result {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return run(t, ctx, []string{"serve", "--data-dir", dataDir, "--listen", "127.0.0.1:0"}, env)
}

func TestServeRequiresMasterKey(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	r := startAndStop(t, dataDir, map[string]string{"PRESSFLEET_MASTER_KEY": ""})
	if r.code != 1 || !strings.Contains(r.stderr, "master key is not configured") {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Errorf("serve touched the data directory before checking the key: %v", err)
	}

	r = startAndStop(t, dataDir, map[string]string{"PRESSFLEET_MASTER_KEY": "pfmk1:garbled"})
	if r.code != 1 || !strings.Contains(r.stderr, "PRESSFLEET_MASTER_KEY: invalid master key") {
		t.Errorf("garbled key: exit %d, stderr: %s", r.code, r.stderr)
	}
}

func TestServeMasterKeyMismatch(t *testing.T) {
	dataDir := t.TempDir()
	r := startAndStop(t, dataDir, nil)
	if r.code != 0 || !strings.Contains(r.stderr, "master key registered with this installation") {
		t.Fatalf("first start: exit %d, stderr: %s", r.code, r.stderr)
	}
	r = startAndStop(t, dataDir, nil)
	if r.code != 0 || !strings.Contains(r.stderr, "master key verified") {
		t.Fatalf("restart: exit %d, stderr: %s", r.code, r.stderr)
	}
	other := secretstest.KeyText(t, 2)
	r = startAndStop(t, dataDir, map[string]string{"PRESSFLEET_MASTER_KEY": other})
	if r.code != 1 || !strings.Contains(r.stderr, "master key does not match this installation") {
		t.Fatalf("other key: exit %d, stderr: %s", r.code, r.stderr)
	}
	for _, key := range []string{testKey(t), other} {
		if strings.Contains(r.stderr, key) {
			t.Error("stderr contains the master key")
		}
	}
}

func TestServeMasterKeyFile(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	outside := filepath.Join(root, "master.key")
	inside := filepath.Join(dataDir, "master.key")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outside, inside} {
		if err := os.WriteFile(p, []byte(testKey(t)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	r := startAndStop(t, dataDir, map[string]string{"PRESSFLEET_MASTER_KEY": "", "PRESSFLEET_MASTER_KEY_FILE": inside})
	if r.code != 1 || !strings.Contains(r.stderr, "must not be inside the data directory") {
		t.Errorf("key file in data dir: exit %d, stderr: %s", r.code, r.stderr)
	}
	r = startAndStop(t, dataDir, map[string]string{"PRESSFLEET_MASTER_KEY": "", "PRESSFLEET_MASTER_KEY_FILE": outside})
	if r.code != 0 || strings.Contains(r.stderr, testKey(t)) {
		t.Errorf("key file: exit %d, stderr: %s", r.code, r.stderr)
	}
}
