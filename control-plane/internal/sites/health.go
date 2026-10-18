package sites

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// The site health check, shared by the rollouts and by deployments
// (package deploy, through Service.CheckHealth).

// CheckHealth checks that a running site works (§5.5): an HTTP request to
// its web server gets an answer below 500, and WP-CLI reports core version
// wantCore (unless it is empty) and loads WordPress with its plugins. It
// tries again until the health timeout while the site's containers settle,
// except after a wrong core version, which waiting doesn't fix. Its error
// wraps ErrUnhealthy.
func (s *Service) CheckHealth(ctx context.Context, site store.Site, wantCore string) error {
	deadline := time.Now().Add(s.healthTimeout)
	for {
		err := s.healthOnce(ctx, site, wantCore)
		if err == nil {
			return nil
		}
		if errors.Is(err, errWrongCore) || !time.Now().Before(deadline) || ctx.Err() != nil {
			return fmt.Errorf("%w: %w", ErrUnhealthy, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ErrUnhealthy, ctx.Err())
		case <-time.After(s.healthInterval):
		}
	}
}

func (s *Service) healthOnce(ctx context.Context, site store.Site, wantCore string) error {
	if s.prober != nil {
		// A query string bypasses the page cache, which would otherwise
		// answer for PHP, even with a stale page when PHP fails.
		req := provisioner.ProbeRequest{
			Host: strings.TrimPrefix(URL(site), "https://"),
			Path: "/?pressfleet-health-check=" + strings.ToLower(rand.Text()[:8]),
		}
		res, err := s.prober.ProbeHTTP(ctx, site.ID, req)
		if err != nil {
			return fmt.Errorf("HTTP request: %w", err)
		}
		if res.StatusCode >= 500 {
			return fmt.Errorf("HTTP request: the site answered %d", res.StatusCode)
		}
	}
	if wantCore != "" {
		got, err := s.wpcli(ctx, site.ID, "core", "version")
		if err != nil {
			return err
		}
		if got != wantCore {
			return fmt.Errorf("%w: WP-CLI reports WordPress %s, want %s", errWrongCore, got, wantCore)
		}
	}
	if _, err := s.wpcli(ctx, site.ID, "core", "is-installed"); err != nil {
		return err
	}
	return nil
}

// wpcli runs WP-CLI for a health check, keeping only WP-CLI's error
// message in the error.
func (s *Service) wpcli(ctx context.Context, siteID string, args ...string) (string, error) {
	out, err := s.prov.ExecWPCLI(ctx, siteID, args)
	var ee *provisioner.ExecError
	if errors.As(err, &ee) {
		return "", fmt.Errorf("wp %s: exit status %d: %s", strings.Join(args, " "), ee.ExitCode, trimWPCLIError(ee.Stderr))
	}
	if err != nil {
		return "", fmt.Errorf("wp %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out), nil
}
