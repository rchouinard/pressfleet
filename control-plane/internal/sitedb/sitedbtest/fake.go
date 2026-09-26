// Package sitedbtest provides an in-memory [sitedb.Manager] for unit tests.
package sitedbtest

import (
	"context"
	"os"
	"slices"
	"sync"

	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
)

// Call records one method invocation on the fake.
type Call struct {
	Op     string // method name, e.g. "Dump"
	SiteID string
	Path   string // Dump/Import path
}

// Fake is an in-memory Manager. Each site's database contents are an opaque
// byte slice; Dump and Import write and read real files so they compose with
// code that moves dumps around on disk. It is safe for concurrent use.
type Fake struct {
	// Err, if set, is consulted at the start of every call; a non-nil result
	// is returned without changing state. Use it to inject failures.
	Err func(op, siteID string) error

	mu    sync.Mutex
	dbs   map[string][]byte
	calls []Call
}

var _ sitedb.Manager = (*Fake)(nil)

// New returns an empty Fake.
func New() *Fake {
	return &Fake{dbs: make(map[string][]byte)}
}

// SecretRef returns the reference CreateDatabase hands out for siteID.
func SecretRef(siteID string) string {
	return "sitedbtest/" + siteID
}

// Data returns a copy of the site's database contents.
func (f *Fake) Data(siteID string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.dbs[siteID]
	return slices.Clone(d), ok
}

// SetData replaces the site's database contents, creating it if needed.
func (f *Fake) SetData(siteID string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dbs[siteID] = slices.Clone(data)
}

// Calls returns every call made so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *Fake) begin(op, siteID, path string) error {
	f.calls = append(f.calls, Call{Op: op, SiteID: siteID, Path: path})
	if f.Err != nil {
		return f.Err(op, siteID)
	}
	return nil
}

// CreateDatabase implements sitedb.Manager.
func (f *Fake) CreateDatabase(_ context.Context, siteID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("CreateDatabase", siteID, ""); err != nil {
		return "", err
	}
	if _, ok := f.dbs[siteID]; !ok {
		f.dbs[siteID] = []byte{}
	}
	return SecretRef(siteID), nil
}

// DropDatabase implements sitedb.Manager.
func (f *Fake) DropDatabase(_ context.Context, siteID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DropDatabase", siteID, ""); err != nil {
		return err
	}
	delete(f.dbs, siteID)
	return nil
}

// Dump implements sitedb.Manager.
func (f *Fake) Dump(_ context.Context, siteID, destPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Dump", siteID, destPath); err != nil {
		return err
	}
	data, ok := f.dbs[siteID]
	if !ok {
		return sitedb.ErrDatabaseNotFound
	}
	return os.WriteFile(destPath, data, 0o600)
}

// Import implements sitedb.Manager.
func (f *Fake) Import(_ context.Context, siteID, srcPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("Import", siteID, srcPath); err != nil {
		return err
	}
	if _, ok := f.dbs[siteID]; !ok {
		return sitedb.ErrDatabaseNotFound
	}
	data, err := os.ReadFile(srcPath) //nolint:gosec // G304: test fake reads caller-provided paths by design
	if err != nil {
		return err
	}
	f.dbs[siteID] = data
	return nil
}
