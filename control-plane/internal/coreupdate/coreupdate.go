// Package coreupdate decides which WordPress core release a site should run
// (design doc §5.5): the rollout policy, as data in and a plan out. It has
// no side effects; package sites applies its plans with core update jobs.
//
// # Policy
//
// WordPress numbers major releases x.y and their minor and security
// releases x.y.z; a major branch is x.y (catalog.Branch).
//
//   - Minor and security releases roll out automatically: a site moves to
//     the newest release of its own branch that the version manifest
//     lists.
//   - Major releases wait for approval. A site or its client group approves
//     a branch (Policy.ApprovedBranch), and the site may then move to the
//     newest release of any branch up to it. Approving 7.3 lets a site on
//     7.1 move straight to 7.3.
//   - Optionally (Settings.MajorAutoApproveAfter), a major release is
//     approved automatically once its images have been published for that
//     long (catalog.WordPressVersion.Published). An explicit approval or
//     hold overrides it.
//   - A hold (Policy.Hold) keeps a site on its current branch, which still
//     gets that branch's minor and security releases: a hold never leaves
//     a site unpatched, as long as the branch gets releases.
//
// The site's own policy wins over its client group's: a site with a hold
// or an approval of its own ignores the group's, so a group can be held
// while one of its sites is approved, and the reverse. A site with neither
// follows its group.
//
// # Warnings
//
// Holding a site on an older branch is only as safe as the branch's
// security releases, which WordPress backports as a courtesy. The plan
// warns ([Warning]) when a site has been held on an older branch longer
// than Settings.HoldWarningAfter, when its branch is no longer built (it
// left the manifest, so the site gets no more core updates), and when the
// manifest marks the branch insecure (wordpress.org reports its newest
// release as insecure: it no longer gets security fixes).
package coreupdate

import (
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
)

// ErrInvalidPolicy means a policy can't be used as given, e.g. an approved
// branch that isn't a WordPress branch.
var ErrInvalidPolicy = errors.New("invalid core update policy")

// DefaultHoldWarningAfter is how long a site may be held on an older branch
// before its plan warns, unless Settings says otherwise.
const DefaultHoldWarningAfter = 90 * 24 * time.Hour

// Policy is a site's or client group's say in its core updates. The zero
// Policy has none: the site follows its group, and the group the defaults.
type Policy struct {
	// Hold keeps the site on its current major branch, which still gets
	// its minor and security releases. HoldSince is when the hold was
	// placed.
	Hold      bool
	HoldSince time.Time
	// ApprovedBranch is the newest major branch ("7.2") the site may move
	// to; empty approves none. It is ignored while Hold is set.
	ApprovedBranch string
}

// Validate checks the policy's approved branch.
func (p Policy) Validate() error {
	if p.ApprovedBranch != "" && !catalog.ValidBranch(p.ApprovedBranch) {
		return fmt.Errorf("%w: approved branch %q is not a WordPress branch like 7.2", ErrInvalidPolicy, p.ApprovedBranch)
	}
	return nil
}

// set reports whether the policy decides anything.
func (p Policy) set() bool { return p.Hold || p.ApprovedBranch != "" }

// Settings are the install-wide rollout settings.
type Settings struct {
	// MajorAutoApproveAfter approves a major release automatically once
	// its images have been published this long; zero never does.
	MajorAutoApproveAfter time.Duration
	// HoldWarningAfter is how long a site may be held on an older branch
	// before its plan warns; zero means DefaultHoldWarningAfter.
	HoldWarningAfter time.Duration
}

// Source says whose decision a plan follows.
type Source string

// Sources of a hold or approval.
const (
	SourceNone        Source = ""
	SourceSite        Source = "site"
	SourceClientGroup Source = "client_group"
	// SourceAuto is Settings.MajorAutoApproveAfter.
	SourceAuto Source = "auto"
)

// Kind says what kind of release a target is.
type Kind string

// Release kinds.
const (
	// KindMinor is a minor or security release on the site's own branch.
	KindMinor Kind = "minor"
	// KindMajor is a release on a newer branch.
	KindMajor Kind = "major"
)

// PendingReason says why a newer major release isn't a site's target.
type PendingReason string

// Reasons a major release is pending.
const (
	PendingHeld             PendingReason = "held"
	PendingAwaitingApproval PendingReason = "awaiting_approval"
)

// Pending is the newest major release a site may not move to yet.
type Pending struct {
	Version string
	Branch  string
	Reason  PendingReason
	// AutoApproveAt is when the release will be approved automatically
	// (Settings.MajorAutoApproveAfter); zero if it won't be.
	AutoApproveAt time.Time
}

// Warning codes.
const (
	// WarnHeldLong: the site has been held on an older branch for longer
	// than Settings.HoldWarningAfter.
	WarnHeldLong = "held_long"
	// WarnBranchNotBuilt: the site's branch is no longer in the version
	// manifest, so it gets no more core updates.
	WarnBranchNotBuilt = "branch_not_built"
	// WarnBranchInsecure: the site's branch no longer gets security
	// releases.
	WarnBranchInsecure = "branch_insecure"
)

// Warning is something about a site's core an operator should look at.
type Warning struct {
	Code    string
	Message string
}

// Plan is what a site's core should do next.
type Plan struct {
	// Current is the version the site runs, and Branch its branch.
	Current string
	Branch  string
	// Target is the release to move to now, or empty if the site is up to
	// date as far as its policy allows; TargetKind says whether it is a
	// minor or a major release.
	Target     string
	TargetKind Kind
	// Held reports an effective hold, since HeldSince. ApprovedBranch is
	// the effective approval (empty if none). Source is whose decision
	// they are.
	Held           bool
	HeldSince      time.Time
	ApprovedBranch string
	Source         Source
	// Pending is the newest major release the site may not move to yet;
	// nil if there is none.
	Pending  *Pending
	Warnings []Warning
}

// Decide returns the plan of a site running current with its own and its
// client group's policies, against the releases in the catalog, at now.
// A malformed current version is catalog.ErrInvalidVersion; a policy with
// a malformed approved branch is ErrInvalidPolicy.
func Decide(cat *catalog.Catalog, current string, site, group Policy, s Settings, now time.Time) (Plan, error) {
	branch, err := catalog.Branch(current)
	if err != nil {
		return Plan{}, err
	}
	for _, p := range []Policy{site, group} {
		if err := p.Validate(); err != nil {
			return Plan{}, err
		}
	}
	if s.HoldWarningAfter <= 0 {
		s.HoldWarningAfter = DefaultHoldWarningAfter
	}
	plan := Plan{Current: current, Branch: branch}

	// The deciding policy: the site's own, else its group's.
	switch {
	case site.set():
		plan.Source = SourceSite
		plan.Held, plan.HeldSince, plan.ApprovedBranch = site.Hold, site.HoldSince, site.ApprovedBranch
	case group.set():
		plan.Source = SourceClientGroup
		plan.Held, plan.HeldSince, plan.ApprovedBranch = group.Hold, group.HoldSince, group.ApprovedBranch
	}
	if plan.Held {
		plan.ApprovedBranch = ""
	}

	// Newer branches' releases, newest first.
	var newer []catalog.WordPressVersion
	for _, w := range cat.WordPressVersions() {
		if compareBranches(w.Branch(), branch) > 0 {
			newer = append(newer, w)
		}
	}
	if !plan.Held && plan.ApprovedBranch == "" && s.MajorAutoApproveAfter > 0 {
		for _, w := range newer {
			if !w.Published.IsZero() && !now.Before(w.Published.Add(s.MajorAutoApproveAfter)) {
				plan.ApprovedBranch, plan.Source = w.Branch(), SourceAuto
				break
			}
		}
	}

	own, built := cat.LatestInBranch(branch)
	if built && compareVersions(own.Version, current) > 0 {
		plan.Target, plan.TargetKind = own.Version, KindMinor
	}
	if plan.ApprovedBranch != "" {
		for _, w := range newer {
			if compareBranches(w.Branch(), plan.ApprovedBranch) <= 0 {
				plan.Target, plan.TargetKind = w.Version, KindMajor
				break
			}
		}
	}

	if len(newer) > 0 && (plan.TargetKind != KindMajor || compareBranches(newer[0].Branch(), branchOf(plan.Target)) > 0) {
		p := &Pending{Version: newer[0].Version, Branch: newer[0].Branch(), Reason: PendingAwaitingApproval}
		switch {
		case plan.Held:
			p.Reason = PendingHeld
		case s.MajorAutoApproveAfter > 0 && !newer[0].Published.IsZero() && plan.Source != SourceSite && plan.Source != SourceClientGroup:
			p.AutoApproveAt = newer[0].Published.Add(s.MajorAutoApproveAfter)
		}
		plan.Pending = p
	}

	// Warnings concern the branch the site stays on.
	if plan.TargetKind != KindMajor {
		switch {
		case !built && len(newer) > 0:
			plan.Warnings = append(plan.Warnings, Warning{WarnBranchNotBuilt, fmt.Sprintf(
				"WordPress %s is no longer built, so the site gets no more core updates until it moves to a newer major release.", branch)})
		case own.Insecure:
			plan.Warnings = append(plan.Warnings, Warning{WarnBranchInsecure, fmt.Sprintf(
				"WordPress %s no longer gets security releases (wordpress.org reports %s as insecure); move the site to a newer major release.", branch, own.Version)})
		}
	}
	if plan.Held && len(newer) > 0 && !plan.HeldSince.IsZero() && now.Sub(plan.HeldSince) >= s.HoldWarningAfter {
		plan.Warnings = append(plan.Warnings, Warning{WarnHeldLong, fmt.Sprintf(
			"The site has been held on WordPress %s since %s, while %s is available; older branches get security fixes only as a courtesy.",
			branch, plan.HeldSince.UTC().Format(time.DateOnly), newer[0].Version)})
	}
	return plan, nil
}

// branchOf returns a valid version's branch.
func branchOf(version string) string {
	b, _ := catalog.Branch(version)
	return b
}

// compareBranches and compareVersions order values that are already known
// to be valid.
func compareBranches(a, b string) int {
	n, _ := catalog.CompareBranches(a, b)
	return n
}

func compareVersions(a, b string) int {
	n, _ := catalog.CompareWordPressVersions(a, b)
	return n
}
