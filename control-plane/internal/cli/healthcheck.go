package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

// errUnhealthy is returned when the control plane answers but isn't healthy.
var errUnhealthy = errors.New("control plane is unhealthy")

// healthcheckTimeout stays below the Compose healthcheck timeout (5s) so the
// command reports its own error instead of being killed.
const healthcheckTimeout = 3 * time.Second

// healthcheckCommand backs the control plane container's Compose healthcheck.
// It is a subcommand because the runtime image has no curl or wget.
func (a *app) healthcheckCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "healthcheck",
		Short: "Exit 0 if the local control plane reports healthy (for container healthchecks)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url, err := localURL(a.cfg.Listen, "/healthz")
			if err != nil {
				return err
			}
			return checkHealth(cmd.Context(), url)
		},
	}
}

// localURL derives the URL of a control plane endpoint, such as /healthz,
// from the listen address. A wildcard listen address is reached over
// loopback.
func localURL(listen, path string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("parsing listen address %q: %w", listen, err)
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + path, nil
}

func checkHealth(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, healthcheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", errUnhealthy, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%w: GET %s returned %s", errUnhealthy, url, resp.Status)
	}
	return nil
}
