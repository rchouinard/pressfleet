package cli

import (
	"context"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
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
			keys, err := a.loadKeyring()
			if err != nil {
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
			if err := a.verifyMasterKey(cmd.Context(), db, keys); err != nil {
				return err
			}

			// Components register readiness checks here.
			readiness := &httpserver.Readiness{}
			readiness.Register("state_database", db.Ping)
			authn := auth.NewService(db, auth.Options{Logger: a.log})
			proxies, err := a.cfg.HTTP.TrustedProxyPrefixes()
			if err != nil {
				return err
			}
			handler, err := api.NewHandler(api.Options{
				Logger:         a.log,
				Readiness:      readiness,
				DB:             db,
				Auth:           authn,
				Authenticator:  api.NewAuthenticator(authn),
				TrustedProxies: proxies,
				SecureCookies:  a.cfg.HTTP.SecureCookies == config.SecureCookiesAlways,
			})
			if err != nil {
				return err
			}

			// Job workers and the scheduler run alongside the server and
			// stop with it; the pool gives running steps a grace period.
			// Job handlers are registered on the pool as workflows land.
			pool := jobs.NewPool(db, jobs.Options{
				Concurrency: a.cfg.Jobs.Concurrency,
				Logger:      a.log.With("component", "jobs"),
				Secrets:     secrets.NewStore(db, keys),
			})
			scheduler := jobs.NewScheduler(db, jobs.SchedulerOptions{Logger: a.log.With("component", "scheduler")})
			workersCtx, stopWorkers := context.WithCancel(cmd.Context())
			defer stopWorkers()
			var workers sync.WaitGroup
			workers.Go(func() { _ = pool.Run(workersCtx) })
			workers.Go(func() { _ = scheduler.Run(workersCtx) })

			srv := httpserver.New(httpserver.Options{Addr: a.cfg.Listen, Handler: handler, Logger: a.log})
			srvErr := srv.Run(cmd.Context())
			// Stop the workers too if the server failed on its own, and wait
			// for them so the database isn't closed under running steps.
			stopWorkers()
			workers.Wait()
			if srvErr != nil {
				return srvErr
			}

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
