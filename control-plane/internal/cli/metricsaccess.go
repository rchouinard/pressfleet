package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
)

// errNoMetricsDomain means metrics.public_domain is not set.
var errNoMetricsDomain = errors.New("metrics.public_domain is not set, so VictoriaMetrics is not exposed; " +
	"set it and run pressfleet init --force to expose it")

func (a *app) metricsAccessCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "metrics-access",
		Short:       "Manage the credential for the public metrics API (run on the host)",
		Annotations: map[string]string{checkSkew: "true"},
	}
	cmd.AddCommand(a.metricsAccessRotateCommand())
	return cmd
}

func (a *app) metricsAccessRotateCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate",
		Short: "Replace the password Grafana uses to query metrics",
		Long: `Generate a new password for the public metrics API (metrics.public_domain)
and print it once. The old password stops working as soon as Traefik reloads
the route, within a few seconds; no container is restarted.

Only a bcrypt hash of the password is kept, so it can't be shown again. Run
this on the host: it writes into the platform project, which the host CLI owns
(design doc §4.7), and never touches the state database.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			domain := a.cfg.Metrics.PublicDomain
			if domain == "" {
				return errNoMetricsDomain
			}
			dir := a.cfg.Layout().PlatformDir()
			if _, err := os.Stat(filepath.Join(dir, platform.ComposeFile)); err != nil {
				return fmt.Errorf("no platform project in %s (run pressfleet init first): %w", dir, err)
			}
			password, err := platform.RotateMetricsAccess(dir, domain)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, err = fmt.Fprintf(out, "URL:      https://%s/\nUser:     %s\nPassword: %s\n\nStore the password now; it can't be shown again.\n",
				domain, platform.MetricsAccessUsername, password.Reveal())
			return err
		},
	}
}
