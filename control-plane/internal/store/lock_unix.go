//go:build unix

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockSQLite takes an advisory lock on a file next to the SQLite database:
// shared for normal use, exclusive for replacing the database. It fails with
// ErrInUse instead of waiting. The lock is released by the returned func or
// when the process exits.
//
// SQLite's own locks only last as long as a transaction, so they can't tell
// Restore that a control plane has the database open.
func lockSQLite(dbPath string, exclusive bool) (func() error, error) {
	if dbPath == "" {
		return nil, errors.New("sqlite path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating database directory: %w", err)
	}
	f, err := os.OpenFile(dbPath+".lock", os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: the path comes from operator configuration
	if err != nil {
		return nil, fmt.Errorf("opening database lock file: %w", err)
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrInUse
		}
		return nil, fmt.Errorf("locking database: %w", err)
	}
	return f.Close, nil
}
