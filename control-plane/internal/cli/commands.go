package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"

	"github.com/spf13/cobra"
)

// envSkipPreflight is the environment equivalent of serve's --skip-preflight
// flag, for control plane containers on development machines and in CI.
const envSkipPreflight = "PRESSFLEET_SKIP_PREFLIGHT"

func (a *app) serveCommand() *cobra.Command {
	var skipPreflight bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the control plane (API, workers, dashboard)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if v, ok := a.opts.Env(envSkipPreflight); ok && !cmd.Flags().Changed("skip-preflight") {
				skip, err := strconv.ParseBool(v)
				if err != nil {
					return fmt.Errorf("%s: %w", envSkipPreflight, err)
				}
				skipPreflight = skip
			}
			if err := a.preflight(cmd.Context(), skipPreflight); err != nil {
				return err
			}
			layout := a.cfg.Layout()
			if err := layout.Ensure(); err != nil {
				return err
			}
			a.log.Info("pressfleet starting",
				"version", a.opts.Version, "listen", a.cfg.Listen, "data_dir", layout.Root)

			db, err := a.openStateDB(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = db.Close() }()

			// The API server (#12) and job workers (#14) will run here.
			<-cmd.Context().Done()

			a.log.Info("pressfleet stopped")
			return nil
		},
	}
	cmd.Flags().String("listen", "", "HTTP listen address (overrides config)")
	cmd.Flags().BoolVar(&skipPreflight, "skip-preflight", false,
		"start even if required host checks fail (development only; also $"+envSkipPreflight+")")
	return cmd
}

func (a *app) initCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Install and configure PressFleet on this host",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return notImplemented(8)
		},
	}
}

func notImplemented(issue int) error {
	return fmt.Errorf("not implemented yet (see https://github.com/rchouinard/pressfleet/issues/%d)", issue)
}

func (a *app) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:         "version",
		Short:       "Print version information",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{skipConfig: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), versionString(a.opts.Version))
			return err
		},
	}
}

// versionString formats the version with VCS and toolchain details from the
// embedded build info.
func versionString(version string) string {
	details := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		var rev, modified string
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
		if rev != "" {
			details = "commit " + rev[:min(len(rev), 12)]
			if modified == "true" {
				details += "-dirty"
			}
			details += ", "
		}
	}
	return fmt.Sprintf("pressfleet %s (%s%s, %s/%s)", version, details, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
