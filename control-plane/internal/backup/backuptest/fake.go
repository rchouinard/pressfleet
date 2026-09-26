// Package backuptest provides an in-memory [backup.Engine] for unit tests.
package backuptest

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
)

// Call records one method invocation on the fake.
type Call struct {
	Op            string // method name, e.g. "Restore"
	SiteID        string
	Snapshot      backup.SnapshotID
	TargetSiteID  string
	ClientGroupID string
}

// Fake is an in-memory Engine. Snapshots are recorded per site and carry no
// data; use Calls to assert on restores and prunes. It is safe for concurrent
// use.
type Fake struct {
	// Err, if set, is consulted at the start of every call; a non-nil result
	// is returned without changing state. Use it to inject failures. siteID
	// is the client group ID for Prune.
	Err func(op, siteID string) error

	mu        sync.Mutex
	next      int
	snapshots map[string][]backup.SnapshotID
	calls     []Call
}

var _ backup.Engine = (*Fake)(nil)

// New returns an empty Fake.
func New() *Fake {
	return &Fake{snapshots: make(map[string][]backup.SnapshotID)}
}

// Calls returns every call made so far, in order.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *Fake) begin(c Call) error {
	f.calls = append(f.calls, c)
	if f.Err != nil {
		id := c.SiteID
		if c.Op == "Prune" {
			id = c.ClientGroupID
		}
		return f.Err(c.Op, id)
	}
	return nil
}

// Backup implements backup.Engine.
func (f *Fake) Backup(_ context.Context, siteID string) (backup.SnapshotID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(Call{Op: "Backup", SiteID: siteID}); err != nil {
		return "", err
	}
	f.next++
	id := backup.SnapshotID(fmt.Sprintf("snap-%d", f.next))
	f.snapshots[siteID] = append(f.snapshots[siteID], id)
	return id, nil
}

// ListSnapshots implements backup.Engine.
func (f *Fake) ListSnapshots(_ context.Context, siteID string) ([]backup.SnapshotID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(Call{Op: "ListSnapshots", SiteID: siteID}); err != nil {
		return nil, err
	}
	return slices.Clone(f.snapshots[siteID]), nil
}

// Restore implements backup.Engine.
func (f *Fake) Restore(_ context.Context, siteID string, snap backup.SnapshotID, targetSiteID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(Call{Op: "Restore", SiteID: siteID, Snapshot: snap, TargetSiteID: targetSiteID}); err != nil {
		return err
	}
	if !slices.Contains(f.snapshots[siteID], snap) {
		return backup.ErrSnapshotNotFound
	}
	return nil
}

// Prune implements backup.Engine.
func (f *Fake) Prune(_ context.Context, clientGroupID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.begin(Call{Op: "Prune", ClientGroupID: clientGroupID})
}
