package cli

import (
	"context"
	"fmt"
	"net/netip"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/audit"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/restic"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/domains"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/redis"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitecron"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/mariadb"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// reattachTimeout bounds reconnecting the platform services to site
// networks at startup.
const reattachTimeout = 5 * time.Minute

// siteRuntime returns the Compose provisioner and the site database manager.
func (a *app) siteRuntime(cat *catalog.Catalog, secretStore secrets.Store, keys *secrets.Keyring, db *store.DB) (*compose.Provisioner, *mariadb.Manager, error) {
	smtp, err := siteSMTP(a.cfg.SMTP, keys)
	if err != nil {
		return nil, nil, err
	}
	prov, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout:            a.cfg.Layout(),
		Secrets:           secretStore,
		Images:            cat,
		MariaDBContainer:  platform.MariaDBContainer(a.cfg.PlatformProject),
		RedisContainer:    platform.RedisContainer(a.cfg.PlatformProject),
		TraefikContainer:  platform.TraefikContainer(a.cfg.PlatformProject),
		TaskImage:         a.cfg.TaskImage,
		PlatformProject:   a.cfg.PlatformProject,
		TraefikDynamicDir: filepath.Join(a.cfg.Layout().PlatformDir(), platform.TraefikDynamicDir),
		WordPressSecurity: a.cfg.WordPressSecurity,
		Logger:            a.log.With("component", "provisioner"),
		SMTP:              smtp,
	})
	if err != nil {
		return nil, nil, err
	}
	siteDBs, err := mariadb.New(mariadb.Config{
		Addr:         a.cfg.MariaDB.Address,
		User:         a.cfg.MariaDB.AdminUser,
		PasswordFile: a.cfg.MariaDB.AdminPasswordFile,
	}, db, secretStore, a.log.With("component", "sitedb"))
	if err != nil {
		return nil, nil, err
	}
	return prov, siteDBs, nil
}

// dnsChecker returns the check that a site's new domains resolve to this
// host (package domains): to public_ip, which config validation has
// checked, or to the dashboard domain's addresses.
func (a *app) dnsChecker() *domains.Checker {
	publicIP, _ := netip.ParseAddr(a.cfg.PublicIP)
	return domains.NewChecker(domains.Options{PublicIP: publicIP, DashboardDomain: a.cfg.DashboardDomain})
}

// objectCaches returns the manager of site users on the shared object
// cache. Like the MariaDB manager, it doesn't connect until it is used.
func (a *app) objectCaches(secretStore secrets.Store, db *store.DB) (*redis.Manager, error) {
	return redis.New(redis.Config{
		Addr:         a.cfg.Redis.Address,
		User:         a.cfg.Redis.AdminUser,
		PasswordFile: a.cfg.Redis.AdminPasswordFile,
		Databases:    platform.RedisDatabases,
	}, db, secretStore, a.log.With("component", "objectcache"))
}

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
			secretStore := secrets.NewStore(db, keys)
			// Like the SMTP password, backup S3 credentials that don't
			// decrypt stop the control plane now rather than at the first
			// backup.
			repos, err := a.backupRepositories(db, secretStore, keys)
			if err != nil {
				return err
			}
			// Before serving, so Traefik, which waits for them, starts as
			// soon as possible.
			if err := a.syncDNSCredentials(cmd.Context(), keys); err != nil {
				return err
			}
			// Neither the provisioner nor the MariaDB manager touches Docker
			// or MariaDB until a job uses them, so serve starts (and passes
			// /readyz) while either is down.
			cat, err := catalog.Load()
			if err != nil {
				return err
			}
			prov, siteDBs, err := a.siteRuntime(cat, secretStore, keys, db)
			if err != nil {
				return err
			}
			defer func() { _ = siteDBs.Close() }()
			caches, err := a.objectCaches(secretStore, db)
			if err != nil {
				return err
			}
			backupWindow := sitebackup.Window{Start: a.cfg.Backup.Schedule.Start(), Length: a.cfg.Backup.Schedule.Window()}
			siteSvc, err := sites.New(sites.Options{
				DB:                db,
				Secrets:           secretStore,
				Catalog:           cat,
				PreviewBaseDomain: a.cfg.PreviewBaseDomain,
				Provisioner:       prov,
				Databases:         siteDBs,
				ObjectCaches:      caches,
				TwoFactor:         a.cfg.WordPressSecurity.TwoFactor,
				DashboardDomain:   a.cfg.DashboardDomain,
				DNSChecker:        a.dnsChecker(),
				BackupWindow:      backupWindow,
				Logger:            a.log.With("component", "sites"),
			})
			if err != nil {
				return err
			}
			if a.cfg.PreviewBaseDomain == "" {
				a.log.Warn("preview_base_domain is not set; site creation is refused until it is")
			}
			engine, err := restic.New(restic.Options{
				DB:           db,
				Repositories: repos,
				Databases:    siteDBs,
				Tasks:        prov,
				Layout:       layout,
				Logger:       a.log.With("component", "backup"),
			})
			if err != nil {
				return err
			}
			backups, err := sitebackup.New(sitebackup.Options{
				DB: db, Engine: engine, Window: backupWindow, Logger: a.log.With("component", "backup"),
			})
			if err != nil {
				return err
			}
			handler, err := api.NewHandler(api.Options{
				Logger:         a.log,
				Readiness:      readiness,
				DB:             db,
				Auth:           authn,
				Authenticator:  api.NewAuthenticator(authn),
				Sites:          siteSvc,
				Backups:        backups,
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
				Secrets:     secretStore,
			})
			if err := pool.Register(audit.CleanupHandler(db, a.cfg.Audit.Retention(), nil)); err != nil {
				return err
			}
			for _, h := range append(siteSvc.Handlers(), backups.Handler()) {
				if err := pool.Register(h); err != nil {
					return err
				}
			}
			if err := sitecron.Register(pool, db, prov); err != nil {
				return err
			}
			// Existing sites pick up changed install-wide settings (the
			// WordPress security set, §6.1) through one reconcile job per
			// site. Queuing them is best effort: a failure must not keep
			// the API from starting.
			if _, err := siteSvc.EnqueueReconcile(cmd.Context()); err != nil && cmd.Context().Err() == nil {
				a.log.Warn("queuing site reconciliation failed", "error", err)
			}
			// Sites created before scheduled backups existed get a backup
			// schedule, and a changed backup window moves every schedule.
			// Best effort, like the reconcile jobs.
			if err := sitebackup.ReconcileSchedules(cmd.Context(), db, backupWindow); err != nil && cmd.Context().Err() == nil {
				a.log.Warn("reconciling backup schedules failed", "error", err)
			}
			// A canceled context means serve is already stopping; the
			// server below returns at once.
			if err := audit.EnsureCleanupSchedule(cmd.Context(), db); err != nil && cmd.Context().Err() == nil {
				return err
			}
			scheduler := jobs.NewScheduler(db, jobs.SchedulerOptions{Logger: a.log.With("component", "scheduler")})
			workersCtx, stopWorkers := context.WithCancel(cmd.Context())
			defer stopWorkers()
			var workers sync.WaitGroup
			workers.Go(func() { _ = pool.Run(workersCtx) })
			workers.Go(func() { _ = scheduler.Run(workersCtx) })
			// Best effort and in the background: a platform upgrade may have
			// recreated MariaDB or the object cache, disconnecting them from
			// every site network.
			workers.Go(func() {
				ctx, cancel := context.WithTimeout(workersCtx, reattachTimeout)
				defer cancel()
				siteSvc.ReattachSharedServices(ctx, prov)
			})
			// The object cache keeps site users only in memory: restore them
			// now and whenever it restarts. (Sites created before it existed
			// get theirs from their site.reconcile job.)
			workers.Go(func() { caches.Watch(workersCtx) })

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
