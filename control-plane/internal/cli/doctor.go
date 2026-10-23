package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
)

// Exit codes for `pressfleet doctor`.
const (
	exitRequiredFailed = 1
	exitWarnings       = 2
)

func (a *app) doctorCommand() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that this host meets PressFleet's requirements",
		Long: `Check that this host meets PressFleet's requirements and print a fix for
each problem.

Exit status is 0 when every check passes, 1 when a required check fails,
and 2 when only recommended checks fail or some check warns.`,
		Args:        cobra.NoArgs,
		Annotations: map[string]string{checkSkew: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep := doctor.Run(cmd.Context(), a.doctorOptions(), doctor.ScopeInstall, doctor.ScopeRuntime)
			write := rep.WriteText
			if jsonOut {
				write = rep.WriteJSON
			}
			if err := write(cmd.OutOrStdout()); err != nil {
				return err
			}
			switch rep.Outcome() {
			case doctor.Failed:
				return &exitError{code: exitRequiredFailed}
			case doctor.Warnings:
				return &exitError{code: exitWarnings}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print the report as JSON")
	return cmd
}

func (a *app) doctorOptions() doctor.Options {
	opts := doctor.Options{
		Host:              a.opts.HostProbe,
		Docker:            a.opts.DockerProbe,
		DataDir:           a.cfg.DataDir,
		DashboardDomain:   strings.TrimSuffix(a.cfg.DashboardDomain, "."),
		PreviewBaseDomain: strings.TrimSuffix(a.cfg.PreviewBaseDomain, "."),
		SFTPPort:          a.cfg.SFTP.Port,
		Installed:         platformInstalled(a.cfg.Layout()),
	}
	if opts.Host == nil {
		opts.Host = doctor.System{}
	}
	if opts.Docker == nil {
		opts.Docker = compose.HostInspector{}
	}
	// Validate has already rejected malformed addresses.
	opts.PublicIP, _ = netip.ParseAddr(a.cfg.PublicIP)
	return opts
}

// platformDoctor backs the API's doctor endpoint. The control plane runs in a
// container with no docker CLI, so host-only checks are reported as
// unavailable rather than run (and failed). It has the state database, so
// it runs the checks that need it, through webauthn.
func (a *app) platformDoctor(webauthn doctor.WebAuthnProvider) api.DoctorFunc {
	return func(ctx context.Context) doctor.Report {
		opts := a.doctorOptions()
		opts.SkipHostOnly = true
		opts.WebAuthn = webauthn
		return doctor.Run(ctx, opts, doctor.ScopeInstall, doctor.ScopeRuntime)
	}
}

// preflight runs runtime-scope checks before the control plane starts and
// refuses to start if a required check fails.
func (a *app) preflight(ctx context.Context, skip bool) error {
	if skip {
		a.log.Warn("PREFLIGHT CHECKS SKIPPED: --skip-preflight is for development machines only; " +
			"PressFleet may misbehave on a host that doesn't meet its requirements")
		return nil
	}
	rep := doctor.Run(ctx, a.doctorOptions(), doctor.ScopeRuntime)
	for _, r := range rep.Results {
		attrs := []any{"check", r.ID, "status", r.Status, "severity", r.Severity, "message", r.Message}
		if r.Fix != "" {
			attrs = append(attrs, "fix", r.Fix)
		}
		level := slog.LevelDebug
		switch {
		case r.Status == doctor.StatusFail && r.Severity == doctor.Required:
			level = slog.LevelError
		case r.Status == doctor.StatusFail, r.Status == doctor.StatusWarn:
			level = slog.LevelWarn
		}
		a.log.Log(ctx, level, "preflight check", attrs...)
	}
	if failures := rep.Failures(); len(failures) > 0 {
		names := make([]string, len(failures))
		for i, f := range failures {
			names[i] = f.ID
		}
		return fmt.Errorf("required preflight checks failed: %s (run `pressfleet doctor` for fixes, "+
			"or pass --skip-preflight on a development machine)", strings.Join(names, ", "))
	}
	return nil
}

// platformInstalled reports whether init has rendered the platform project
// into the data directory, which is what makes this host a PressFleet host.
func platformInstalled(l config.Layout) bool {
	_, err := os.Stat(filepath.Join(l.PlatformDir(), platform.ComposeFile))
	return err == nil
}
