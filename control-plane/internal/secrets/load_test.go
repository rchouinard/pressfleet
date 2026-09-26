package secrets_test

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
)

func writeKeyFile(t *testing.T, path, text string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // not subject to the umask
		t.Fatal(err)
	}
	return path
}

func TestLoadMasterKey(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	envKey, fileKey := secretstest.Key(t, 1), secretstest.Key(t, 2)
	keyFile := writeKeyFile(t, filepath.Join(root, "keys", "master.key"), fileKey.Encode().Reveal(), 0o600)
	openFile := writeKeyFile(t, filepath.Join(root, "keys", "open.key"), fileKey.Encode().Reveal(), 0o644)
	inDataDir := writeKeyFile(t, filepath.Join(dataDir, "master.key"), fileKey.Encode().Reveal(), 0o600)
	garbled := writeKeyFile(t, filepath.Join(root, "keys", "garbled.key"), "pfmk1:nope", 0o600)
	// A path outside the data directory that resolves into it.
	link := filepath.Join(root, "linked-data")
	if err := os.Symlink(dataDir, link); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		src      secrets.KeySource
		wantID   string
		wantErr  error
		wantMsg  string
		wantWarn string
	}{
		{name: "env", src: secrets.KeySource{Value: envKey.Encode()}, wantID: envKey.ID()},
		{name: "file", src: secrets.KeySource{File: keyFile}, wantID: fileKey.ID()},
		{
			name: "env wins over file", src: secrets.KeySource{Value: envKey.Encode(), File: keyFile},
			wantID: envKey.ID(), wantWarn: "using the environment",
		},
		{name: "missing", src: secrets.KeySource{}, wantErr: secrets.ErrNoMasterKey, wantMsg: "PRESSFLEET_MASTER_KEY"},
		{name: "invalid env", src: secrets.KeySource{Value: "hunter2"}, wantErr: secrets.ErrInvalidMasterKey, wantMsg: "PRESSFLEET_MASTER_KEY: invalid master key"},
		{name: "garbled file", src: secrets.KeySource{File: garbled}, wantErr: secrets.ErrInvalidMasterKey, wantMsg: garbled},
		{name: "missing file", src: secrets.KeySource{File: filepath.Join(root, "absent.key")}, wantErr: os.ErrNotExist},
		{name: "relative file", src: secrets.KeySource{File: "master.key"}, wantMsg: "must be an absolute path"},
		{name: "directory", src: secrets.KeySource{File: root}, wantMsg: "not a regular file"},
		{
			name: "file in data dir", src: secrets.KeySource{File: inDataDir},
			wantErr: secrets.ErrKeyFileInDataDir,
		},
		{
			name: "file in data dir through a symlink", src: secrets.KeySource{File: filepath.Join(link, "master.key")},
			wantErr: secrets.ErrKeyFileInDataDir,
		},
		{
			name: "file readable by others", src: secrets.KeySource{File: openFile},
			wantID: fileKey.ID(), wantWarn: "accessible to other users",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			if tt.src.DataDir == "" {
				tt.src.DataDir = dataDir
			}
			k, err := secrets.LoadMasterKey(tt.src, log)
			switch {
			case tt.wantErr != nil || tt.wantMsg != "":
				if err == nil || (tt.wantErr != nil && !errors.Is(err, tt.wantErr)) || !strings.Contains(err.Error(), tt.wantMsg) {
					t.Fatalf("err = %v, want %v containing %q", err, tt.wantErr, tt.wantMsg)
				}
			case err != nil:
				t.Fatal(err)
			case k.ID() != tt.wantID:
				t.Errorf("loaded key %s, want %s", k.ID(), tt.wantID)
			}
			if !strings.Contains(logs.String(), tt.wantWarn) || (tt.wantWarn == "" && logs.Len() > 0) {
				t.Errorf("logs = %q, want warning %q", logs.String(), tt.wantWarn)
			}
			for _, key := range []secrets.MasterKey{envKey, fileKey} {
				if strings.Contains(logs.String(), key.Encode().Reveal()) || (err != nil && strings.Contains(err.Error(), key.Encode().Reveal())) {
					t.Error("key text leaked into logs or error")
				}
			}
		})
	}
}

func TestLoadMasterKeyDataDirNotCreatedYet(t *testing.T) {
	root := t.TempDir()
	keyFile := writeKeyFile(t, filepath.Join(root, "master.key"), secretstest.KeyText(t, 3), 0o600)
	src := secrets.KeySource{File: keyFile, DataDir: filepath.Join(root, "data")}
	if _, err := secrets.LoadMasterKey(src, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	// The data directory as written contains the file, even though it
	// doesn't exist yet.
	src.DataDir = root
	if _, err := secrets.LoadMasterKey(src, slog.New(slog.DiscardHandler)); !errors.Is(err, secrets.ErrKeyFileInDataDir) {
		t.Errorf("err = %v, want ErrKeyFileInDataDir", err)
	}
	src.Value = redact.Secret(secretstest.KeyText(t, 4))
	if _, err := secrets.LoadMasterKey(src, slog.New(slog.DiscardHandler)); err != nil {
		t.Errorf("the environment wins, so the ignored file isn't checked: %v", err)
	}
}
