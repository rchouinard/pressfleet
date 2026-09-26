package secrets

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Where the master key comes from, as named in errors and documentation.
const (
	EnvMasterKey     = "PRESSFLEET_MASTER_KEY"
	EnvMasterKeyFile = "PRESSFLEET_MASTER_KEY_FILE"
)

// ErrKeyFileInDataDir means the key file is inside the data directory. The
// data directory is backed up, and the key must be kept apart from backups
// (design doc §4.1).
var ErrKeyFileInDataDir = errors.New("the master key file must not be inside the data directory")

// maxKeyFileSize bounds how much of a key file is read; an encoded key is
// well under 100 bytes.
const maxKeyFileSize = 4096

// KeySource says where to find the master key.
type KeySource struct {
	// Value is the encoded key itself ($PRESSFLEET_MASTER_KEY). It takes
	// precedence over File.
	Value redact.Secret
	// File is a file holding the encoded key (master_key_file).
	File string
	// DataDir is the data directory, which must not contain File.
	DataDir string
}

// LoadMasterKey reads and parses the master key. Value wins over File if
// both are set. A key file inside the data directory is rejected, and a key
// file other users can access is accepted with a warning.
func LoadMasterKey(src KeySource, log *slog.Logger) (MasterKey, error) {
	if src.Value != "" {
		if src.File != "" {
			log.Warn("master key set in both the environment and a key file; using the environment",
				"env", EnvMasterKey, "key_file", src.File)
		}
		k, err := ParseMasterKey(src.Value)
		if err != nil {
			return MasterKey{}, fmt.Errorf("%s: %w", EnvMasterKey, err)
		}
		return k, nil
	}
	if src.File == "" {
		return MasterKey{}, fmt.Errorf("%w: set %s, or master_key_file (%s) to a file holding the key; see docs/configuration.md",
			ErrNoMasterKey, EnvMasterKey, EnvMasterKeyFile)
	}
	text, err := readKeyFile(src.File, src.DataDir, log)
	if err != nil {
		return MasterKey{}, fmt.Errorf("master key file %s: %w", src.File, err)
	}
	k, err := ParseMasterKey(text)
	if err != nil {
		return MasterKey{}, fmt.Errorf("master key file %s: %w", src.File, err)
	}
	return k, nil
}

func readKeyFile(path, dataDir string, log *slog.Logger) (redact.Secret, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("must be an absolute path")
	}
	inside, err := within(path, dataDir)
	if err != nil {
		return "", err
	}
	if inside {
		return "", fmt.Errorf("%w (%s): keep the key separate from the host's backups", ErrKeyFileInDataDir, dataDir)
	}
	f, err := os.Open(path) //nolint:gosec // G304: the path comes from operator configuration
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		log.Warn("master key file is accessible to other users; restrict it with chmod 600",
			"key_file", path, "mode", fmt.Sprintf("%04o", perm))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize))
	if err != nil {
		return "", err
	}
	return redact.Secret(data), nil
}

// within reports whether path is dataDir or inside it, comparing both the
// paths as written and with symbolic links resolved.
func within(path, dataDir string) (bool, error) {
	if dataDir == "" {
		return false, nil
	}
	if isWithin(filepath.Clean(path), filepath.Clean(dataDir)) {
		return true, nil
	}
	rpath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	rdir, err := filepath.EvalSymlinks(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		// A data directory that doesn't exist yet can't contain the file.
		return false, nil
	} else if err != nil {
		return false, err
	}
	return isWithin(rpath, rdir), nil
}

func isWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && filepath.IsLocal(rel)
}
