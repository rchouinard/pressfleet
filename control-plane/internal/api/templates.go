package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
)

// Site template updates (#95, design doc §4.7, §5.5): the rollout that
// re-renders sites after an upgrade changed the site templates, alongside
// the core (core.go) and image (images.go) rollouts.

// GetTemplateRollout returns the templates rollout's state.
func (h *Handlers) GetTemplateRollout(ctx context.Context, _ gen.GetTemplateRolloutRequestObject) (gen.GetTemplateRolloutResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Platform()); err != nil {
		return nil, err
	}
	if h.sites == nil {
		return nil, errNoSites
	}
	ro, err := h.sites.Rollout(ctx, sites.TemplateRolloutName)
	if err != nil {
		return nil, coreError(err)
	}
	out, err := h.templateRolloutResponse(ctx, ro)
	if err != nil {
		return nil, coreError(err)
	}
	return gen.GetTemplateRollout200JSONResponse(out), nil
}

// ResumeTemplateRollout lifts a halt of the templates rollout.
func (h *Handlers) ResumeTemplateRollout(ctx context.Context, _ gen.ResumeTemplateRolloutRequestObject) (gen.ResumeTemplateRolloutResponseObject, error) {
	// Resuming a rollout is the same decision for templates as for core
	// releases and images.
	if _, err := h.authorize(ctx, authz.ApproveCoreUpdate, authz.Platform()); err != nil {
		return nil, err
	}
	if h.sites == nil {
		return nil, errNoSites
	}
	ro, err := h.sites.ResumeRollout(ctx, sites.TemplateRolloutName)
	if err != nil {
		return nil, coreError(err)
	}
	out, err := h.templateRolloutResponse(ctx, ro)
	if err != nil {
		return nil, coreError(err)
	}
	return gen.ResumeTemplateRollout200JSONResponse(out), nil
}

func (h *Handlers) templateRolloutResponse(ctx context.Context, ro sites.RolloutStatus) (gen.TemplateRollout, error) {
	templates, pending, err := h.sites.TemplateProgress(ctx)
	if err != nil {
		return gen.TemplateRollout{}, err
	}
	out := gen.TemplateRollout{
		Halted:              ro.Halted(),
		ConsecutiveFailures: ro.ConsecutiveFailures,
		MaxFailures:         ro.MaxFailures,
		Window:              updateWindowResponse(ro.Window),
		Templates:           templates,
		PendingSites:        pending,
		Failures:            []gen.TemplateUpdateFailure{},
	}
	if ro.Halted() {
		out.HaltedAt, out.HaltReason = timePtr(ro.HaltedAt), &ro.HaltReason
	}
	for _, f := range ro.Failures {
		out.Failures = append(out.Failures, gen.TemplateUpdateFailure{SiteID: f.SiteID, Target: f.Target, Message: f.Message, FailedAt: f.FailedAt.UTC()})
	}
	return out, nil
}
