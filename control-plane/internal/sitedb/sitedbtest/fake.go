// Package sitedbtest provides an in-memory [sitedb.Manager] for unit tests.
package sitedbtest

import (
	"context"
	"crypto/rand"
	"maps"
	"os"
	"slices"
	"sync"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
)

// Call records one method invocation on the fake.
type Call struct {
	Op     string // method name, e.g. "Dump"
	SiteID string
	Path   string // Dump/Import path; the session ID for temporary users
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
	temps map[string]TemporaryUser // by session ID
	calls []Call
}

var _ sitedb.Manager = (*Fake)(nil)

// New returns an empty Fake.
func New() *Fake {
	return &Fake{dbs: make(map[string][]byte), temps: make(map[string]TemporaryUser)}
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

// TemporaryUser is a temporary user on the fake.
type TemporaryUser struct {
	SiteID   string
	Access   sitedb.Access
	Password redact.Secret
}

// TemporaryUsers returns the temporary users that exist, by session ID.
func (f *Fake) TemporaryUsers() map[string]TemporaryUser {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.temps)
}

// AddTemporaryUser makes a temporary user exist, as one left behind by a
// crash would.
func (f *Fake) AddTemporaryUser(sessionID string, u TemporaryUser) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.temps[sessionID] = u
}

// CreateTemporaryUser implements sitedb.Manager. The user is named
// "pft_<session ID>".
func (f *Fake) CreateTemporaryUser(_ context.Context, siteID, sessionID string, access sitedb.Access) (sitedb.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("CreateTemporaryUser", siteID, sessionID); err != nil {
		return sitedb.Credentials{}, err
	}
	if _, ok := f.dbs[siteID]; !ok {
		return sitedb.Credentials{}, sitedb.ErrDatabaseNotFound
	}
	pw := redact.Secret(rand.Text())
	f.temps[sessionID] = TemporaryUser{SiteID: siteID, Access: access, Password: pw}
	return sitedb.Credentials{Database: "pf_" + siteID, Username: "pft_" + sessionID, Password: pw}, nil
}

// DropTemporaryUser implements sitedb.Manager. The call's Path is the
// session ID.
func (f *Fake) DropTemporaryUser(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DropTemporaryUser", "", sessionID); err != nil {
		return err
	}
	delete(f.temps, sessionID)
	return nil
}

// ListTemporaryUsers implements sitedb.Manager.
func (f *Fake) ListTemporaryUsers(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("ListTemporaryUsers", "", ""); err != nil {
		return nil, err
	}
	return slices.Sorted(maps.Keys(f.temps)), nil
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
