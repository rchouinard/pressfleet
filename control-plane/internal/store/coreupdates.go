package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CorePolicy is a site's or client group's WordPress core update policy
// (#57, design doc §5.5; package coreupdate says what it means). The zero
// CorePolicy is none.
type CorePolicy struct {
	// Hold keeps the site on its current major branch; HoldSince is when
	// the hold was placed.
	Hold      bool
	HoldSince time.Time
	// ApprovedBranch is the newest major branch the site may move to, or
	// empty for none.
	ApprovedBranch string
	UpdatedAt      time.Time
}

// IsZero reports whether the policy decides nothing.
func (p CorePolicy) IsZero() bool { return !p.Hold && p.ApprovedBranch == "" }

// CorePolicies reads and writes core update policies of sites and client
// groups.
type CorePolicies struct {
	q Querier
}

// NewCorePolicies returns a CorePolicies that runs queries on q.
func NewCorePolicies(q Querier) CorePolicies {
	return CorePolicies{q: q}
}

// corePolicyTable is where one kind of target keeps its policies.
type corePolicyTable struct{ table, key, what string }

var (
	siteCorePolicies  = corePolicyTable{"site_core_policies", "site_id", "site"}
	groupCorePolicies = corePolicyTable{"client_group_core_policies", "client_group_id", "client group"}
)

// Site returns the site's policy; the zero CorePolicy if it has none. It
// doesn't check that the site exists.
func (r CorePolicies) Site(ctx context.Context, siteID string) (CorePolicy, error) {
	return r.get(ctx, siteCorePolicies, siteID)
}

// SetSite replaces the site's policy; the zero CorePolicy removes it. A
// missing site is ErrReference.
func (r CorePolicies) SetSite(ctx context.Context, siteID string, p CorePolicy) error {
	return r.set(ctx, siteCorePolicies, siteID, p)
}

// Sites returns every site's policy, keyed by site ID. Sites without one
// have no entry.
func (r CorePolicies) Sites(ctx context.Context) (map[string]CorePolicy, error) {
	return r.all(ctx, siteCorePolicies)
}

// ClientGroup returns the client group's policy; the zero CorePolicy if it
// has none.
func (r CorePolicies) ClientGroup(ctx context.Context, clientGroupID string) (CorePolicy, error) {
	return r.get(ctx, groupCorePolicies, clientGroupID)
}

// SetClientGroup replaces the client group's policy; the zero CorePolicy
// removes it. A missing group is ErrReference.
func (r CorePolicies) SetClientGroup(ctx context.Context, clientGroupID string, p CorePolicy) error {
	return r.set(ctx, groupCorePolicies, clientGroupID, p)
}

// ClientGroups returns every client group's policy, keyed by group ID.
func (r CorePolicies) ClientGroups(ctx context.Context) (map[string]CorePolicy, error) {
	return r.all(ctx, groupCorePolicies)
}

const corePolicyColumns = "hold, hold_since, approved_branch, updated_at"

func scanCorePolicy(s scanner, dest ...any) (CorePolicy, error) {
	var (
		p     CorePolicy
		since sql.NullTime
	)
	err := s.Scan(append(dest, &p.Hold, &since, &p.ApprovedBranch, &p.UpdatedAt)...)
	p.HoldSince = since.Time
	return p, err
}

func (r CorePolicies) get(ctx context.Context, t corePolicyTable, id string) (CorePolicy, error) {
	p, err := scanCorePolicy(r.q.QueryRowContext(ctx,
		"SELECT "+corePolicyColumns+" FROM "+t.table+" WHERE "+t.key+" = ?", id))
	if errors.Is(err, ErrNotFound) {
		return CorePolicy{}, nil
	}
	if err != nil {
		return CorePolicy{}, fmt.Errorf("core update policy of %s %s: %w", t.what, id, err)
	}
	return p, nil
}

func (r CorePolicies) set(ctx context.Context, t corePolicyTable, id string, p CorePolicy) error {
	what := fmt.Sprintf("set core update policy of %s %s", t.what, id)
	if p.IsZero() {
		if _, err := r.q.ExecContext(ctx, "DELETE FROM "+t.table+" WHERE "+t.key+" = ?", id); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}
	var since any
	if p.Hold {
		since = nullTime(p.HoldSince)
	}
	_, err := r.q.ExecContext(ctx, "INSERT INTO "+t.table+" ("+t.key+", "+corePolicyColumns+") VALUES (?, ?, ?, ?, ?) "+
		"ON CONFLICT ("+t.key+") DO UPDATE SET hold = excluded.hold, hold_since = excluded.hold_since, "+
		"approved_branch = excluded.approved_branch, updated_at = excluded.updated_at",
		id, p.Hold, since, p.ApprovedBranch, now())
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

func (r CorePolicies) all(ctx context.Context, t corePolicyTable) (map[string]CorePolicy, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT "+t.key+", "+corePolicyColumns+" FROM "+t.table)
	if err != nil {
		return nil, fmt.Errorf("list %s core update policies: %w", t.what, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]CorePolicy{}
	for rows.Next() {
		var id string
		p, err := scanCorePolicy(rows, &id)
		if err != nil {
			return nil, fmt.Errorf("list %s core update policies: %w", t.what, err)
		}
		out[id] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list %s core update policies: %w", t.what, err)
	}
	return out, nil
}

// Rollout is the state of a rollout of new images to every site, one site
// at a time: "core" for WordPress core releases (#57).
type Rollout struct {
	Name string
	// Fingerprint identifies what the rollout rolls out, such as the
	// releases in the version manifest; when it changes, the rollout starts
	// over (Rollouts.Sync).
	Fingerprint string
	// ConsecutiveFailures counts the site updates that failed since the
	// last one that succeeded.
	ConsecutiveFailures int
	// HaltedAt is when the rollout stopped after too many failures, with
	// HaltReason; zero while it runs.
	HaltedAt   time.Time
	HaltReason string
	UpdatedAt  time.Time
}

// Halted reports whether the rollout is halted.
func (r Rollout) Halted() bool { return !r.HaltedAt.IsZero() }

// RolloutFailure is a site's last failed update in a rollout.
type RolloutFailure struct {
	SiteID   string
	Target   string
	Message  string
	FailedAt time.Time
}

// Rollouts reads and writes rollout state.
type Rollouts struct {
	q Querier
}

// NewRollouts returns a Rollouts that runs queries on q.
func NewRollouts(q Querier) Rollouts {
	return Rollouts{q: q}
}

// Get returns the rollout's state; a rollout that never ran has only its
// name.
func (r Rollouts) Get(ctx context.Context, name string) (Rollout, error) {
	var (
		ro     = Rollout{Name: name}
		halted sql.NullTime
	)
	err := r.q.QueryRowContext(ctx,
		"SELECT fingerprint, consecutive_failures, halted_at, halt_reason, updated_at FROM rollouts WHERE name = ?", name,
	).Scan(&ro.Fingerprint, &ro.ConsecutiveFailures, &halted, &ro.HaltReason, &ro.UpdatedAt)
	if errors.Is(err, ErrNotFound) {
		return Rollout{Name: name}, nil
	}
	if err != nil {
		return Rollout{}, fmt.Errorf("get rollout %s: %w", name, err)
	}
	ro.HaltedAt = halted.Time
	return ro, nil
}

// ensure creates the rollout's row if it has none.
func (r Rollouts) ensure(ctx context.Context, name string) error {
	_, err := r.q.ExecContext(ctx, "INSERT INTO rollouts (name, updated_at) VALUES (?, ?) ON CONFLICT (name) DO NOTHING", name, now())
	return err
}

// Sync records what the rollout rolls out. If the fingerprint changed, the
// rollout starts over: its failure count is reset and a halt lifted, since
// the new releases may fix what failed. It returns the rollout's state.
func (r Rollouts) Sync(ctx context.Context, name, fingerprint string) (Rollout, error) {
	if err := r.ensure(ctx, name); err != nil {
		return Rollout{}, fmt.Errorf("sync rollout %s: %w", name, err)
	}
	if _, err := r.q.ExecContext(ctx, "UPDATE rollouts SET fingerprint = ?, consecutive_failures = 0, halted_at = NULL, "+
		"halt_reason = '', updated_at = ? WHERE name = ? AND fingerprint <> ?", fingerprint, now(), name, fingerprint); err != nil {
		return Rollout{}, fmt.Errorf("sync rollout %s: %w", name, err)
	}
	return r.Get(ctx, name)
}

// RecordSuccess records that the rollout updated the site: the failure
// count starts over, and the site's failure record is removed.
func (r Rollouts) RecordSuccess(ctx context.Context, name, siteID string) error {
	if err := r.ensure(ctx, name); err != nil {
		return fmt.Errorf("record rollout %s success: %w", name, err)
	}
	if _, err := r.q.ExecContext(ctx, "UPDATE rollouts SET consecutive_failures = 0, updated_at = ? WHERE name = ?", now(), name); err != nil {
		return fmt.Errorf("record rollout %s success: %w", name, err)
	}
	return r.ClearFailure(ctx, name, siteID)
}

// RecordFailure records that updating the site to target failed. With
// counted, the failure counts towards halting the rollout, which halts
// once maxFailures updates in a row have failed (never, if maxFailures is
// zero). Otherwise (an update started by hand) only the site's failure
// record is kept. It returns the rollout's state. A missing site is
// ErrReference.
func (r Rollouts) RecordFailure(ctx context.Context, name string, f RolloutFailure, counted bool, maxFailures int) (Rollout, error) {
	what := "record rollout " + name + " failure"
	if err := r.ensure(ctx, name); err != nil {
		return Rollout{}, fmt.Errorf("%s: %w", what, err)
	}
	if f.FailedAt.IsZero() {
		f.FailedAt = now()
	}
	if _, err := r.q.ExecContext(ctx, "INSERT INTO rollout_failures (rollout, site_id, target, message, failed_at) VALUES (?, ?, ?, ?, ?) "+
		"ON CONFLICT (rollout, site_id) DO UPDATE SET target = excluded.target, message = excluded.message, failed_at = excluded.failed_at",
		name, f.SiteID, f.Target, f.Message, f.FailedAt); err != nil {
		return Rollout{}, fmt.Errorf("%s: %w", what, err)
	}
	if counted {
		if _, err := r.q.ExecContext(ctx, "UPDATE rollouts SET consecutive_failures = consecutive_failures + 1, updated_at = ? WHERE name = ?",
			now(), name); err != nil {
			return Rollout{}, fmt.Errorf("%s: %w", what, err)
		}
		if maxFailures > 0 {
			reason := fmt.Sprintf("%d site updates in a row failed", maxFailures)
			if _, err := r.q.ExecContext(ctx, "UPDATE rollouts SET halted_at = ?, halt_reason = ?, updated_at = ? "+
				"WHERE name = ? AND halted_at IS NULL AND consecutive_failures >= ?",
				now(), reason, now(), name, maxFailures); err != nil {
				return Rollout{}, fmt.Errorf("%s: %w", what, err)
			}
		}
	}
	return r.Get(ctx, name)
}

// Resume lifts a halt and resets the failure count. Sites whose update
// failed are still not retried for the same target.
func (r Rollouts) Resume(ctx context.Context, name string) (Rollout, error) {
	if err := r.ensure(ctx, name); err != nil {
		return Rollout{}, fmt.Errorf("resume rollout %s: %w", name, err)
	}
	if _, err := r.q.ExecContext(ctx, "UPDATE rollouts SET halted_at = NULL, halt_reason = '', consecutive_failures = 0, updated_at = ? WHERE name = ?",
		now(), name); err != nil {
		return Rollout{}, fmt.Errorf("resume rollout %s: %w", name, err)
	}
	return r.Get(ctx, name)
}

// Failure returns the site's failure record in the rollout, and whether it
// has one.
func (r Rollouts) Failure(ctx context.Context, name, siteID string) (RolloutFailure, bool, error) {
	f := RolloutFailure{SiteID: siteID}
	err := r.q.QueryRowContext(ctx, "SELECT target, message, failed_at FROM rollout_failures WHERE rollout = ? AND site_id = ?",
		name, siteID).Scan(&f.Target, &f.Message, &f.FailedAt)
	if errors.Is(err, ErrNotFound) {
		return RolloutFailure{}, false, nil
	}
	if err != nil {
		return RolloutFailure{}, false, fmt.Errorf("rollout %s failure of site %s: %w", name, siteID, err)
	}
	return f, true, nil
}

// Failures returns the rollout's failure records, keyed by site ID.
func (r Rollouts) Failures(ctx context.Context, name string) (map[string]RolloutFailure, error) {
	rows, err := r.q.QueryContext(ctx, "SELECT site_id, target, message, failed_at FROM rollout_failures WHERE rollout = ?", name)
	if err != nil {
		return nil, fmt.Errorf("rollout %s failures: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]RolloutFailure{}
	for rows.Next() {
		var f RolloutFailure
		if err := rows.Scan(&f.SiteID, &f.Target, &f.Message, &f.FailedAt); err != nil {
			return nil, fmt.Errorf("rollout %s failures: %w", name, err)
		}
		out[f.SiteID] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rollout %s failures: %w", name, err)
	}
	return out, nil
}

// ClearFailure removes the site's failure record, so the rollout tries it
// again.
func (r Rollouts) ClearFailure(ctx context.Context, name, siteID string) error {
	if _, err := r.q.ExecContext(ctx, "DELETE FROM rollout_failures WHERE rollout = ? AND site_id = ?", name, siteID); err != nil {
		return fmt.Errorf("clear rollout %s failure of site %s: %w", name, siteID, err)
	}
	return nil
}
