package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/logging"
	"github.com/rchouinard/pressfleet/control-plane/internal/maintenance"
)

// maintenancePageCommand runs the platform's maintenance page service
// (package maintenance). It needs no configuration: the platform runs it
// in the control plane image, with no data directory, secrets, or Docker
// socket.
func (a *app) maintenancePageCommand() *cobra.Command {
	var (
		listen string
		check  bool
	)
	cmd := &cobra.Command{
		Use:   "maintenance-page",
		Short: "Serve the maintenance page Traefik shows for sites in maintenance mode",
		Long: "Serves the maintenance page (503 with Retry-After) on every path. The platform runs it as its " +
			"maintenance service. With --check, asks a running instance for the page instead, for container health checks.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{skipConfig: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if check {
				url, err := healthzURL(listen)
				if err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), healthcheckTimeout)
				defer cancel()
				// Any path serves the page; the /healthz of healthzURL is as
				// good as another.
				return maintenance.Check(ctx, url)
			}
			log, err := logging.New(a.opts.Stderr, "info", "json")
			if err != nil {
				return err
			}
			// main cancels the context on SIGTERM, which Docker sends to stop
			// the container.
			return maintenance.Serve(cmd.Context(), listen, log.With("component", "maintenance"))
		},
	}
	cmd.Flags().StringVar(&listen, "listen", maintenance.DefaultListen, "address to listen on")
	cmd.Flags().BoolVar(&check, "check", false, "check that the service answers on --listen, then exit")
	return cmd
}
