//go:build !unix

package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// lockSQLite doesn't lock outside Unix. PressFleet only runs on Linux; this
// keeps the package building elsewhere.
func lockSQLite(dbPath string, _ bool) (func() error, error) {
	if dbPath == "" {
		return nil, errors.New("sqlite path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("creating database directory: %w", err)
	}
	return func() error { return nil }, nil
}
