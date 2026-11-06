package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/coreupdate"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// WordPress core updates (#57, design doc §5.5). Holding and approving are
// both authorized as core_update.approve: deciding which core releases a
// site gets, for or against.

// GetSiteCore returns the site's core update status.
func (h *Handlers) GetSiteCore(ctx context.Context, req gen.GetSiteCoreRequestObject) (gen.GetSiteCoreResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	st, err := h.rollouts.CoreStatus(ctx, req.SiteID)
	if err != nil {
		return nil, coreError(err)
	}
	return gen.GetSiteCore200JSONResponse(siteCoreResponse(st)), nil
}

// SetSiteCorePolicy replaces the site's core update policy.
func (h *Handlers) SetSiteCorePolicy(ctx context.Context, req gen.SetSiteCorePolicyRequestObject) (gen.SetSiteCorePolicyResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ApproveCoreUpdate, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	st, err := h.rollouts.SetCorePolicy(ctx, req.SiteID, corePolicyUpdate(req.Body))
	if err != nil {
		return nil, coreError(err)
	}
	return gen.SetSiteCorePolicy200JSONResponse(siteCoreResponse(st)), nil
}

// UpdateSiteCore queues the site's core update now.
func (h *Handlers) UpdateSiteCore(ctx context.Context, req gen.UpdateSiteCoreRequestObject) (gen.UpdateSiteCoreResponseObject, error) {
	p, err := h.authorize(ctx, authz.ApproveCoreUpdate, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	site, job, err := h.rollouts.UpdateCore(ctx, req.SiteID, p.UserID)
	if err != nil {
		return nil, coreError(err)
	}
	_, aliases, err := h.sites.Get(ctx, site.ID)
	if err != nil {
		return nil, siteError(err)
	}
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.UpdateSiteCore202JSONResponse{
		Body:    gen.SiteAccepted{Site: siteResponse(site, aliases), Job: j},
		Headers: gen.UpdateSiteCore202ResponseHeaders{Location: &location},
	}, nil
}

// GetClientGroupCorePolicy returns the group's core update policy.
func (h *Handlers) GetClientGroupCorePolicy(ctx context.Context, req gen.GetClientGroupCorePolicyRequestObject) (gen.GetClientGroupCorePolicyResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewClientGroups, authz.ClientGroup(req.ClientGroupID)); err != nil {
		return nil, err
	}
	p, err := h.rollouts.ClientGroupCorePolicy(ctx, req.ClientGroupID)
	if err != nil {
		return nil, coreError(err)
	}
	return gen.GetClientGroupCorePolicy200JSONResponse(corePolicyResponse(p)), nil
}

// SetClientGroupCorePolicy replaces the group's core update policy.
func (h *Handlers) SetClientGroupCorePolicy(ctx context.Context, req gen.SetClientGroupCorePolicyRequestObject) (gen.SetClientGroupCorePolicyResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ApproveCoreUpdate, authz.ClientGroup(req.ClientGroupID)); err != nil {
		return nil, err
	}
	p, err := h.rollouts.SetClientGroupCorePolicy(ctx, req.ClientGroupID, corePolicyUpdate(req.Body))
	if err != nil {
		return nil, coreError(err)
	}
	return gen.SetClientGroupCorePolicy200JSONResponse(corePolicyResponse(p)), nil
}

// GetCoreRollout returns the core rollout's state.
func (h *Handlers) GetCoreRollout(ctx context.Context, _ gen.GetCoreRolloutRequestObject) (gen.GetCoreRolloutResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Platform()); err != nil {
		return nil, err
	}
	ro, err := h.rollouts.Rollout(ctx, sites.CoreRolloutName)
	if err != nil {
		return nil, coreError(err)
	}
	return gen.GetCoreRollout200JSONResponse(h.coreRolloutResponse(ro)), nil
}

// ResumeCoreRollout lifts a halt of the core rollout.
func (h *Handlers) ResumeCoreRollout(ctx context.Context, _ gen.ResumeCoreRolloutRequestObject) (gen.ResumeCoreRolloutResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ApproveCoreUpdate, authz.Platform()); err != nil {
		return nil, err
	}
	ro, err := h.rollouts.ResumeRollout(ctx, sites.CoreRolloutName)
	if err != nil {
		return nil, coreError(err)
	}
	return gen.ResumeCoreRollout200JSONResponse(h.coreRolloutResponse(ro)), nil
}

// coreError maps core update errors to API errors.
func coreError(err error) error {
	switch {
	case errors.Is(err, sites.ErrClientGroupNotFound), errors.Is(err, sites.ErrUnknownRollout):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, sites.ErrNoCoreUpdate), errors.Is(err, sites.ErrNoBackups):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return siteError(err)
}

func corePolicyUpdate(b *gen.CorePolicyUpdate) sites.CorePolicyUpdate {
	in := deref(b)
	return sites.CorePolicyUpdate{Hold: deref(in.Hold), ApprovedBranch: deref(in.ApprovedBranch)}
}

func corePolicyResponse(p store.CorePolicy) gen.CorePolicy {
	out := gen.CorePolicy{Hold: p.Hold}
	if p.Hold && !p.HoldSince.IsZero() {
		out.HoldSince = timePtr(p.HoldSince)
	}
	if p.ApprovedBranch != "" {
		out.ApprovedBranch = &p.ApprovedBranch
	}
	return out
}

func timePtr(t time.Time) *time.Time {
	t = t.UTC()
	return &t
}

func siteCoreResponse(st sites.SiteCore) gen.SiteCore {
	plan := st.Plan
	out := gen.SiteCore{
		Version:           plan.Current,
		Branch:            plan.Branch,
		Policy:            corePolicyResponse(st.Policy),
		ClientGroupPolicy: corePolicyResponse(st.GroupPolicy),
		Held:              plan.Held,
		DecidedBy:         gen.SiteCoreDecidedByDefault,
		Warnings:          []gen.CoreWarning{},
	}
	switch plan.Source {
	case coreupdate.SourceSite:
		out.DecidedBy = gen.SiteCoreDecidedBySite
	case coreupdate.SourceClientGroup:
		out.DecidedBy = gen.SiteCoreDecidedByClientGroup
	case coreupdate.SourceAuto:
		out.DecidedBy = gen.SiteCoreDecidedByAuto
	}
	if plan.ApprovedBranch != "" {
		out.ApprovedBranch = &plan.ApprovedBranch
	}
	if plan.Target != "" {
		out.Target = &gen.CoreTarget{Version: plan.Target, Kind: gen.CoreTargetKind(plan.TargetKind)}
	}
	if p := plan.Pending; p != nil {
		out.Pending = &gen.CorePending{Version: p.Version, Branch: p.Branch, Reason: gen.CorePendingReason(p.Reason)}
		if !p.AutoApproveAt.IsZero() {
			out.Pending.AutoApproveAt = timePtr(p.AutoApproveAt)
		}
	}
	for _, w := range plan.Warnings {
		out.Warnings = append(out.Warnings, gen.CoreWarning{Code: gen.CoreWarningCode(w.Code), Message: w.Message})
	}
	if f := st.LastFailure; f != nil {
		lf := coreFailureResponse(*f)
		out.LastFailure = &lf
	}
	return out
}

func coreFailureResponse(f store.RolloutFailure) gen.CoreUpdateFailure {
	return gen.CoreUpdateFailure{SiteID: f.SiteID, Version: f.Target, Message: f.Message, FailedAt: f.FailedAt.UTC()}
}

func (h *Handlers) coreRolloutResponse(ro sites.RolloutStatus) gen.CoreRollout {
	out := gen.CoreRollout{
		Held:                ro.Held,
		Halted:              ro.Halted(),
		ConsecutiveFailures: ro.ConsecutiveFailures,
		MaxFailures:         ro.MaxFailures,
		Releases:            []gen.CoreRelease{},
		Failures:            []gen.CoreUpdateFailure{},
	}
	if ro.Held {
		out.HoldReason = &ro.HoldReason
	}
	if ro.Halted() {
		out.HaltedAt, out.HaltReason = timePtr(ro.HaltedAt), &ro.HaltReason
	}
	out.Window = updateWindowResponse(ro.Window)
	for _, w := range h.rollouts.CoreReleases() {
		r := gen.CoreRelease{Version: w.Version, Branch: w.Branch(), Insecure: w.Insecure}
		if !w.Published.IsZero() {
			r.Published = &openapi_types.Date{Time: w.Published}
		}
		out.Releases = append(out.Releases, r)
	}
	for _, f := range ro.Failures {
		out.Failures = append(out.Failures, coreFailureResponse(f))
	}
	return out
}

// updateWindowResponse returns the maintenance window, or nil if there is
// none.
func updateWindowResponse(w sites.UpdateWindow) *gen.UpdateWindow {
	if w.Length <= 0 || w.Length >= 24*time.Hour {
		return nil
	}
	return &gen.UpdateWindow{
		Start: fmt.Sprintf("%02d:%02d", int(w.Start.Hours()), int(w.Start.Minutes())%60),
		Hours: int(w.Length.Hours()),
	}
}
