// Package objectcachetest provides an in-memory [objectcache.Manager] for
// unit tests.
package objectcachetest

import (
	"context"
	"slices"
	"sync"

	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
)

// Call records one method invocation on the fake.
type Call struct {
	Op     string // "CreateUser" or "DeleteUser"
	SiteID string
}

// Fake is an in-memory Manager. It is safe for concurrent use.
type Fake struct {
	// Err, if set, is consulted at the start of every call; a non-nil result
	// is returned without changing state. Use it to inject failures.
	Err func(op, siteID string) error

	mu    sync.Mutex
	users map[string]bool
	calls []Call
}

var _ objectcache.Manager = (*Fake)(nil)

// New returns an empty Fake.
func New() *Fake {
	return &Fake{users: make(map[string]bool)}
}

// SecretRef returns the reference CreateUser hands out for siteID.
func SecretRef(siteID string) string {
	return "objectcachetest/" + siteID
}

// HasUser reports whether the site has a user.
func (f *Fake) HasUser(siteID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.users[siteID]
}

// Users returns the sites that have a user, sorted.
func (f *Fake) Users() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.users {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Calls returns every call made so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *Fake) begin(op, siteID string) error {
	f.calls = append(f.calls, Call{Op: op, SiteID: siteID})
	if f.Err != nil {
		return f.Err(op, siteID)
	}
	return nil
}

// CreateUser implements objectcache.Manager.
func (f *Fake) CreateUser(_ context.Context, siteID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("CreateUser", siteID); err != nil {
		return "", err
	}
	f.users[siteID] = true
	return SecretRef(siteID), nil
}

// DeleteUser implements objectcache.Manager.
func (f *Fake) DeleteUser(_ context.Context, siteID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("DeleteUser", siteID); err != nil {
		return err
	}
	delete(f.users, siteID)
	return nil
}
