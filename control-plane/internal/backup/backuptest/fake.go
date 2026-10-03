// Package backuptest provides an in-memory [backup.Engine] for unit tests.
package backuptest

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
)

// Call records one method invocation on the fake.
type Call struct {
	Op            string // method name, e.g. "Restore"
	SiteID        string
	Options       backup.Options // Backup options
	Snapshot      backup.SnapshotID
	TargetSiteID  string
	ClientGroupID string
}

// Fake is an in-memory Engine. Snapshots are recorded per site and carry no
// data; use Calls to assert on restores, prunes, and checks. Like the real
// engine, Backup with a BackupID returns the site's snapshot with that ID if
// there is one. It is safe for concurrent use.
//
// Group-level operations see the snapshots of the sites put in the group
// with SetGroup. Prune applies KeepLast to each site: the newest KeepLast
// snapshots stay, and zero keeps them all.
type Fake struct {
	// Err, if set, is consulted at the start of every call; a non-nil result
	// is returned without changing state. Use it to inject failures. siteID
	// is the client group ID for Prune, Check, and GroupSnapshots, and the
	// site ID for ForgetSite.
	Err func(op, siteID string) error
	// KeepLast is Prune's retention policy.
	KeepLast int

	mu        sync.Mutex
	next      int
	snapshots map[string][]backup.Snapshot
	groups    map[string]string // site ID -> client group ID
	calls     []Call
}

var _ backup.Engine = (*Fake)(nil)

// New returns an empty Fake.
func New() *Fake {
	return &Fake{snapshots: make(map[string][]backup.Snapshot), groups: make(map[string]string)}
}

// SetGroup puts the site's snapshots in the client group's repository.
func (f *Fake) SetGroup(siteID, clientGroupID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups[siteID] = clientGroupID
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
		if c.ClientGroupID != "" {
			id = c.ClientGroupID
		}
		return f.Err(c.Op, id)
	}
	return nil
}

// Backup implements backup.Engine. Snapshots are named snap-1, snap-2, ...,
// and all have the same made-up sizes.
func (f *Fake) Backup(_ context.Context, siteID string, opts backup.Options) (backup.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(Call{Op: "Backup", SiteID: siteID, Options: opts}); err != nil {
		return backup.Snapshot{}, err
	}
	if opts.BackupID != "" {
		for _, s := range f.snapshots[siteID] {
			if s.BackupID == opts.BackupID {
				return s, nil
			}
		}
	}
	f.next++
	s := backup.Snapshot{
		ID:         backup.SnapshotID(fmt.Sprintf("snap-%d", f.next)),
		Time:       time.Now().UTC(),
		SiteID:     siteID,
		Kind:       opts.Kind,
		BackupID:   opts.BackupID,
		Tags:       []string{"site:" + siteID},
		SizeBytes:  1024,
		AddedBytes: 512,
		FileCount:  3,
	}
	f.snapshots[siteID] = append(f.snapshots[siteID], s)
	return s, nil
}

// ListSnapshots implements backup.Engine. A site the fake never backed up
// has no snapshots.
func (f *Fake) ListSnapshots(_ context.Context, siteID string) ([]backup.Snapshot, error) {
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
	if !slices.ContainsFunc(f.snapshots[siteID], func(s backup.Snapshot) bool { return s.ID == snap }) {
		return backup.ErrSnapshotNotFound
	}
	return nil
}

// Prune implements backup.Engine.
func (f *Fake) Prune(_ context.Context, clientGroupID string) (backup.PruneResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(Call{Op: "Prune", ClientGroupID: clientGroupID}); err != nil {
		return backup.PruneResult{}, err
	}
	res := backup.PruneResult{Forgotten: []backup.SnapshotID{}}
	if f.KeepLast <= 0 {
		return res, nil
	}
	for siteID, snaps := range f.snapshots {
		if f.groups[siteID] != clientGroupID || len(snaps) <= f.KeepLast {
			continue
		}
		drop := len(snaps) - f.KeepLast
		for _, s := range snaps[:drop] {
			res.Forgotten = append(res.Forgotten, s.ID)
		}
		f.snapshots[siteID] = slices.Clone(snaps[drop:])
	}
	return res, nil
}

// Check implements backup.Engine.
func (f *Fake) Check(_ context.Context, clientGroupID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.begin(Call{Op: "Check", ClientGroupID: clientGroupID})
}

// GroupSnapshots implements backup.Engine.
func (f *Fake) GroupSnapshots(_ context.Context, clientGroupID string) ([]backup.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(Call{Op: "GroupSnapshots", ClientGroupID: clientGroupID}); err != nil {
		return nil, err
	}
	out := []backup.Snapshot{}
	for siteID, snaps := range f.snapshots {
		if f.groups[siteID] == clientGroupID {
			out = append(out, snaps...)
		}
	}
	slices.SortFunc(out, func(a, b backup.Snapshot) int { return a.Time.Compare(b.Time) })
	return out, nil
}

// ForgetSite implements backup.Engine: it removes all of the site's
// snapshots. A site the fake never backed up has none to remove.
func (f *Fake) ForgetSite(_ context.Context, siteID string) (backup.PruneResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin(Call{Op: "ForgetSite", SiteID: siteID}); err != nil {
		return backup.PruneResult{}, err
	}
	res := backup.PruneResult{Forgotten: []backup.SnapshotID{}}
	for _, s := range f.snapshots[siteID] {
		res.Forgotten = append(res.Forgotten, s.ID)
	}
	delete(f.snapshots, siteID)
	return res, nil
}
