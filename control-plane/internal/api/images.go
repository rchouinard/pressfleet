package api

import (
	"context"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
)

// Platform image updates (#58, design doc §5.5): the rollout of new builds
// of the site images, alongside the core rollout (core.go).

// GetImageRollout returns the image rollout's state.
func (h *Handlers) GetImageRollout(ctx context.Context, _ gen.GetImageRolloutRequestObject) (gen.GetImageRolloutResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Platform()); err != nil {
		return nil, err
	}
	ro, err := h.sites.Rollout(ctx, sites.ImageRolloutName)
	if err != nil {
		return nil, coreError(err)
	}
	return gen.GetImageRollout200JSONResponse(h.imageRolloutResponse(ro)), nil
}

// ResumeImageRollout lifts a halt of the image rollout.
func (h *Handlers) ResumeImageRollout(ctx context.Context, _ gen.ResumeImageRolloutRequestObject) (gen.ResumeImageRolloutResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ApproveCoreUpdate, authz.Platform()); err != nil {
		return nil, err
	}
	ro, err := h.sites.ResumeRollout(ctx, sites.ImageRolloutName)
	if err != nil {
		return nil, coreError(err)
	}
	return gen.ResumeImageRollout200JSONResponse(h.imageRolloutResponse(ro)), nil
}

func (h *Handlers) imageRolloutResponse(ro sites.RolloutStatus) gen.ImageRollout {
	out := gen.ImageRollout{
		Halted:              ro.Halted(),
		ConsecutiveFailures: ro.ConsecutiveFailures,
		MaxFailures:         ro.MaxFailures,
		Window:              updateWindowResponse(ro.Window),
		Images:              []gen.ImageBuild{},
		Failures:            []gen.ImageUpdateFailure{},
	}
	if ro.Halted() {
		out.HaltedAt, out.HaltReason = timePtr(ro.HaltedAt), &ro.HaltReason
	}
	for _, b := range h.sites.ImageBuilds() {
		ib := gen.ImageBuild{Image: b.Image, Build: b.Build, CheckedAt: b.CheckedAt.UTC()}
		if !b.PulledAt.IsZero() {
			ib.PulledAt = timePtr(b.PulledAt)
		}
		out.Images = append(out.Images, ib)
	}
	for _, f := range ro.Failures {
		out.Failures = append(out.Failures, gen.ImageUpdateFailure{SiteID: f.SiteID, Target: f.Target, Message: f.Message, FailedAt: f.FailedAt.UTC()})
	}
	return out
}

// ListPHPVersions lists the PHP versions of the version manifest with their
// support status and end-of-life dates, so clients don't keep a copy (#122).
func (h *Handlers) ListPHPVersions(ctx context.Context, _ gen.ListPHPVersionsRequestObject) (gen.ListPHPVersionsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Platform()); err != nil {
		return nil, err
	}
	versions, def, now := h.sites.PHPCatalog()
	out := gen.ListPHPVersions200JSONResponse{Items: make([]gen.PHPVersionInfo, len(versions))}
	for i, v := range versions {
		out.Items[i] = gen.PHPVersionInfo{
			Version:             v.Version,
			Status:              gen.PHPVersionInfoStatus(v.Status(now)),
			ActiveSupportEnds:   openapi_types.Date{Time: v.ActiveSupportEnds},
			SecuritySupportEnds: openapi_types.Date{Time: v.SecuritySupportEnds},
		}
	}
	if def != "" {
		out.Default = &def
	}
	return out, nil
}
