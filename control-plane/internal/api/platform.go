package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
)

// GetPlatform reports the install's connection details (#123) and the build
// of the running control plane. Every role that may see sites may read it:
// it only tells them where to connect and what they are talking to.
func (h *Handlers) GetPlatform(ctx context.Context, _ gen.GetPlatformRequestObject) (gen.GetPlatformResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Platform()); err != nil {
		return nil, err
	}
	build := gen.PlatformBuild{
		Version: h.build.Version, GoVersion: h.build.GoVersion, Os: h.build.OS, Arch: h.build.Arch,
		Modified: h.build.Modified,
	}
	if h.build.Commit != "" {
		build.Commit = &h.build.Commit
	}
	out := gen.GetPlatform200JSONResponse{
		Sftp: gen.PlatformSftp{
			Enabled: h.sftp.Enabled,
			Host:    h.sftp.Host,
			Port:    h.sftp.Port,
		},
		Build: build,
	}
	if h.latestRelease != nil {
		if r, ok := h.latestRelease(); ok {
			out.Release = &gen.PlatformRelease{
				LatestVersion: r.LatestVersion, ReleaseURL: r.ReleaseURL, CheckedAt: r.CheckedAt.UTC(),
				UpdateAvailable: r.UpdateAvailable(h.build.Version),
			}
		}
	}
	return out, nil
}

// DoctorFunc runs the preflight checks that can run from the control plane.
// Checks that need the host must come back as doctor.StatusUnavailable.
type DoctorFunc func(ctx context.Context) doctor.Report

// GetPlatformDoctor returns the platform's preflight check results.
func (h *Handlers) GetPlatformDoctor(ctx context.Context, _ gen.GetPlatformDoctorRequestObject) (gen.GetPlatformDoctorResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.Platform()); err != nil {
		return nil, err
	}
	rep := h.doctor(ctx)
	out := gen.DoctorReport{Outcome: gen.DoctorReportOutcome(rep.Outcome().String()), Results: make([]gen.DoctorResult, len(rep.Results))}
	for i, r := range rep.Results {
		out.Results[i] = gen.DoctorResult{
			ID: r.ID, Name: r.Name, Message: r.Message,
			Severity: gen.DoctorResultSeverity(r.Severity), Status: gen.DoctorResultStatus(r.Status),
		}
		if r.Fix != "" {
			fix := r.Fix
			out.Results[i].Fix = &fix
		}
	}
	return gen.GetPlatformDoctor200JSONResponse(out), nil
}
