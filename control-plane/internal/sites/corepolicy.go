package sites

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/coreupdate"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Core update policies and status (#57, design doc §5.5): holds and
// approvals of major releases, per site and per client group. See package
// coreupdate for what they mean.

// ErrClientGroupNotFound means the client group doesn't exist.
var ErrClientGroupNotFound = errors.New("client group not found")

// CorePolicyUpdate is a site's or client group's new core update policy.
// At most one of its fields is set; neither means the site follows its
// client group (or the group, the platform's defaults).
type CorePolicyUpdate struct {
	// Hold keeps the site on its current major branch.
	Hold bool
	// ApprovedBranch approves major releases up to that branch ("7.2");
	// it must be a branch the version manifest has.
	ApprovedBranch string
}

// SiteCore is a site's core version, policies, and plan.
type SiteCore struct {
	Site        store.Site
	Policy      store.CorePolicy
	GroupPolicy store.CorePolicy
	Plan        coreupdate.Plan
	// LastFailure is the site's last failed core update, if the rollout
	// won't retry it; nil otherwise.
	LastFailure *store.RolloutFailure
}

// CoreStatus returns the site's core update status.
func (rt *Rollouts) CoreStatus(ctx context.Context, siteID string) (SiteCore, error) {
	site, err := store.NewSites(rt.s.db).Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return SiteCore{}, fmt.Errorf("site %s: %w", siteID, ErrNotFound)
	}
	if err != nil {
		return SiteCore{}, err
	}
	policies := store.NewCorePolicies(rt.s.db)
	out := SiteCore{Site: site}
	if out.Policy, err = policies.Site(ctx, siteID); err != nil {
		return SiteCore{}, err
	}
	if out.GroupPolicy, err = policies.ClientGroup(ctx, site.ClientGroupID); err != nil {
		return SiteCore{}, err
	}
	if out.Plan, err = rt.corePlan(ctx, rt.s.db, site); err != nil {
		return SiteCore{}, err
	}
	f, found, err := store.NewRollouts(rt.s.db).Failure(ctx, coreRollout, siteID)
	if err != nil {
		return SiteCore{}, err
	}
	if found && f.Target == out.Plan.Target {
		out.LastFailure = &f
	}
	return out, nil
}

// SetCorePolicy replaces the site's core update policy and returns its new
// status. A policy change reaches the site at the rollout's next pass, which
// it queues.
func (rt *Rollouts) SetCorePolicy(ctx context.Context, siteID string, u CorePolicyUpdate) (SiteCore, error) {
	err := rt.s.db.InTx(ctx, func(q store.Querier) error {
		if _, err := store.NewSites(q).Get(ctx, siteID); errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("site %s: %w", siteID, ErrNotFound)
		} else if err != nil {
			return err
		}
		policies := store.NewCorePolicies(q)
		old, err := policies.Site(ctx, siteID)
		if err != nil {
			return err
		}
		p, err := rt.corePolicyFrom(old, u)
		if err != nil {
			return err
		}
		if err := policies.SetSite(ctx, siteID, p); err != nil {
			return err
		}
		return rt.queueRolloutAfterPolicyChange(ctx, q)
	})
	if err != nil {
		return SiteCore{}, err
	}
	rt.s.log.InfoContext(ctx, "site core update policy changed", "site_id", siteID, "hold", u.Hold, "approved_branch", u.ApprovedBranch)
	return rt.CoreStatus(ctx, siteID)
}

// ClientGroupCorePolicy returns the client group's core update policy.
func (rt *Rollouts) ClientGroupCorePolicy(ctx context.Context, groupID string) (store.CorePolicy, error) {
	if _, err := store.NewClientGroups(rt.s.db).Get(ctx, groupID); errors.Is(err, store.ErrNotFound) {
		return store.CorePolicy{}, fmt.Errorf("client group %s: %w", groupID, ErrClientGroupNotFound)
	} else if err != nil {
		return store.CorePolicy{}, err
	}
	return store.NewCorePolicies(rt.s.db).ClientGroup(ctx, groupID)
}

// SetClientGroupCorePolicy replaces the client group's core update policy,
// which its sites without a policy of their own follow.
func (rt *Rollouts) SetClientGroupCorePolicy(ctx context.Context, groupID string, u CorePolicyUpdate) (store.CorePolicy, error) {
	err := rt.s.db.InTx(ctx, func(q store.Querier) error {
		if _, err := store.NewClientGroups(q).Get(ctx, groupID); errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("client group %s: %w", groupID, ErrClientGroupNotFound)
		} else if err != nil {
			return err
		}
		policies := store.NewCorePolicies(q)
		old, err := policies.ClientGroup(ctx, groupID)
		if err != nil {
			return err
		}
		p, err := rt.corePolicyFrom(old, u)
		if err != nil {
			return err
		}
		if err := policies.SetClientGroup(ctx, groupID, p); err != nil {
			return err
		}
		return rt.queueRolloutAfterPolicyChange(ctx, q)
	})
	if err != nil {
		return store.CorePolicy{}, err
	}
	rt.s.log.InfoContext(ctx, "client group core update policy changed", "client_group_id", groupID, "hold", u.Hold, "approved_branch", u.ApprovedBranch)
	return rt.ClientGroupCorePolicy(ctx, groupID)
}

// corePolicyFrom validates an update and returns the policy to store,
// keeping the time an existing hold was placed.
func (rt *Rollouts) corePolicyFrom(old store.CorePolicy, u CorePolicyUpdate) (store.CorePolicy, error) {
	switch {
	case u.Hold && u.ApprovedBranch != "":
		return store.CorePolicy{}, &ValidationError{Fields: []FieldError{{"/approved_branch", "can't approve a major release and hold at once"}}}
	case u.Hold:
		since := old.HoldSince
		if !old.Hold || since.IsZero() {
			since = rt.s.now()
		}
		return store.CorePolicy{Hold: true, HoldSince: since}, nil
	case u.ApprovedBranch != "":
		if !catalog.ValidBranch(u.ApprovedBranch) {
			return store.CorePolicy{}, &ValidationError{Fields: []FieldError{{"/approved_branch", "must be a WordPress major branch like 7.2"}}}
		}
		if _, ok := rt.s.catalog.LatestInBranch(u.ApprovedBranch); !ok {
			var branches []string
			for _, w := range rt.s.catalog.WordPressVersions() {
				branches = append(branches, w.Branch())
			}
			return store.CorePolicy{}, &ValidationError{Fields: []FieldError{{"/approved_branch",
				fmt.Sprintf("has no site images; approve one of %v", branches)}}}
		}
		return store.CorePolicy{ApprovedBranch: u.ApprovedBranch}, nil
	}
	return store.CorePolicy{}, nil
}

// queueRolloutAfterPolicyChange queues a pass of the core rollout, so an
// approval takes effect without waiting for the schedule.
func (rt *Rollouts) queueRolloutAfterPolicyChange(ctx context.Context, q store.Querier) error {
	if !rt.coreUpdates() {
		return nil
	}
	return enqueueRolloutPass(ctx, q, coreRollout)
}

// CoreRolloutName is the name of the WordPress core rollout, for
// Service.Rollout and Service.ResumeRollout.
const CoreRolloutName = coreRollout

// CoreReleases returns the WordPress releases with site images, newest
// first.
func (rt *Rollouts) CoreReleases() []catalog.WordPressVersion {
	return rt.s.catalog.WordPressVersions()
}
