package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/adminlogin"
	"github.com/rchouinard/pressfleet/control-plane/internal/alertchannels"
	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/audit"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/offboard"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repomaint"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/restic"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/statebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/dbsession"
	"github.com/rchouinard/pressfleet/control-plane/internal/deploy"
	"github.com/rchouinard/pressfleet/control-plane/internal/driver"
	"github.com/rchouinard/pressfleet/control-plane/internal/export"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/redis"
	"github.com/rchouinard/pressfleet/control-plane/internal/pluginupdates"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/releasecheck"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sftpgw"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitecron"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/mariadb"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
	"github.com/rchouinard/pressfleet/ui"
)

func (a *app) serveCommand() *cobra.Command {
	var skipPreflight bool
	cmd := &cobra.Command{
		Use:         "serve",
		Short:       "Run the control plane (API, workers, dashboard)",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{logConfigWarnings: "true"},
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
			s, err := a.buildServer(cmd.Context())
			if err != nil {
				return err
			}
			defer s.close()
			if err := s.startup(cmd.Context()); err != nil {
				return err
			}
			if err := s.run(cmd.Context()); err != nil {
				return err
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

// server is the control plane, built and ready to start.
type server struct {
	a   *app
	log *slog.Logger

	// Opened resources, closed by close in reverse order.
	db      *store.DB
	siteDBs *mariadb.Manager

	handler      http.Handler
	rt           *driver.Runtime
	caches       *redis.Manager
	secretStore  secrets.Store
	repos        *repository.Manager
	siteSvc      *sites.Service
	dbSessions   *dbsession.Service
	channels     *alertchannels.Service
	alertEngine  *alerts.Engine
	backupWindow sitebackup.Window
	pool         *jobs.Pool
	scheduler    *jobs.Scheduler
	gateway      *sftpgw.Server
	gatewayLn    net.Listener
	// releases is the release check; nil when it is off.
	releases *releasecheck.Checker
}

// buildServer opens the state database and constructs every component and
// job handler, touching nothing outside the control plane: neither the
// provisioner nor the MariaDB manager reaches Docker or MariaDB until a
// job uses them, so serve starts (and passes /readyz) while either is
// down. On error it closes what it opened.
func (a *app) buildServer(ctx context.Context) (_ *server, err error) {
	keys, err := a.loadKeyring()
	if err != nil {
		return nil, err
	}
	layout := a.cfg.Layout()
	if err := layout.Ensure(); err != nil {
		return nil, err
	}
	a.log.Info("pressfleet starting",
		"version", a.opts.Version, "listen", a.cfg.Listen, "data_dir", layout.Root)
	for _, w := range a.cfg.Warnings {
		a.log.Warn("deprecated configuration", "warning", w)
	}

	// s, not the result, is what the deferred close sees: returning nil
	// with an error must still close what s holds.
	s := &server{a: a, log: a.log}
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	if s.db, err = a.openStateDB(ctx); err != nil {
		return nil, err
	}
	db := s.db
	if err := a.verifyMasterKey(ctx, db, keys); err != nil {
		return nil, err
	}

	// Components register readiness checks here.
	readiness := &httpserver.Readiness{}
	readiness.Register("state_database", db.Ping)
	authn := auth.NewService(db, a.authOptions(keys))
	proxies, err := a.cfg.HTTP.TrustedProxyPrefixes()
	if err != nil {
		return nil, err
	}
	s.secretStore = secrets.NewStore(db, keys)
	// Before serving, so Traefik, which waits for them, starts as soon as
	// possible.
	if err := a.syncDNSCredentials(ctx, keys); err != nil {
		return nil, err
	}
	cat, err := a.catalog()
	if err != nil {
		return nil, err
	}
	if s.rt, s.siteDBs, err = a.siteRuntime(ctx, cat, s.secretStore, keys, db); err != nil {
		return nil, err
	}
	// Like the SMTP password, backup S3 credentials that don't decrypt
	// stop the control plane now rather than at the first backup. restic
	// runs in the driver's repository tasks, never in this process.
	if s.repos, err = a.backupRepositories(db, s.secretStore, keys, s.rt.Driver); err != nil {
		return nil, err
	}
	// Before any task runs: a crash can leave the secret files of a task
	// that was running.
	if h, ok := s.rt.Driver.(driver.StartupHook); ok {
		if err := h.Startup(ctx); err != nil {
			a.log.Warn("the driver's startup work failed", "error", err)
		}
	}
	if s.caches, err = a.objectCaches(s.secretStore, db); err != nil {
		return nil, err
	}
	prov, siteDBs, caches := s.rt.Driver, s.siteDBs, s.caches
	s.backupWindow = a.backupWindow()
	engine, err := restic.New(restic.Options{
		DB:                   db,
		Repositories:         s.repos,
		Databases:            siteDBs,
		Tasks:                prov,
		Layout:               layout,
		Retention:            a.backupRetention(),
		CheckReadDataPercent: a.cfg.Backup.Check.ReadDataPercent,
		Logger:               a.log.With("component", "backup"),
	})
	if err != nil {
		return nil, err
	}
	backups, err := sitebackup.New(sitebackup.Options{
		DB: db, Engine: engine, Window: s.backupWindow, Logger: a.log.With("component", "backup"),
	})
	if err != nil {
		return nil, err
	}
	repoMaint, err := repomaint.New(repomaint.Options{
		DB: db, Engine: engine, Timing: a.maintenanceTiming(), Logger: a.log.With("component", "backup"),
	})
	if err != nil {
		return nil, err
	}
	stateBackups, err := statebackup.New(statebackup.Options{
		DB: db, Repository: s.repos, Retention: a.stateBackupRetention(), TmpDir: layout.TmpDir(),
		Version: a.opts.Version, Logger: a.log.With("component", "state-backup"),
	})
	if err != nil {
		return nil, err
	}
	offboarding, err := offboard.New(offboard.Options{
		DB: db, Repositories: s.repos, Logger: a.log.With("component", "backup"),
	})
	if err != nil {
		return nil, err
	}
	exports, err := export.New(export.Options{
		DB: db, Databases: siteDBs, DatabaseSizes: siteDBs, Tasks: prov, Snapshots: engine,
		Dir: layout.ExportsDir(), TTL: a.cfg.DatabaseExport.TTL(), SiteTTL: a.cfg.SiteExport.TTL(),
		Logger: a.log.With("component", "export"),
	})
	if err != nil {
		return nil, err
	}
	uploadSvc, err := uploads.New(uploads.Options{
		DB: db, Dir: layout.UploadsDir(), MaxSize: a.cfg.Uploads.MaxSizeBytes(), Expiry: a.cfg.Uploads.Expiry(),
		Logger: a.log.With("component", "uploads"),
	})
	if err != nil {
		return nil, err
	}
	s.siteSvc, err = sites.New(sites.Options{
		DB:                db,
		Secrets:           s.secretStore,
		Catalog:           cat,
		PreviewBaseDomain: a.cfg.PreviewBaseDomain,
		Provisioner:       prov,
		Databases:         siteDBs,
		ObjectCaches:      caches,
		Backups:           backups,
		BackupEngine:      engine,
		TwoFactor:         a.cfg.WordPressSecurity.TwoFactor,
		DashboardDomain:   a.cfg.DashboardDomain,
		DNSChecker:        a.dnsChecker(),
		BackupWindow:      s.backupWindow,
		CoreUpdates:       a.coreUpdates(),
		Uploads:           uploadSvc,
		Tasks:             prov,
		TmpDir:            layout.TmpDir(),
		ImageUpdates:      a.imageUpdates(),
		Prober:            prov,
		ImagePinner:       prov,
		Templates:         templateDriver(s.rt),
		RolloutHold:       rolloutHold(layout.UpgradeDir(), a.log.With("component", "rollouts")),
		Logger:            a.log.With("component", "sites"),
	})
	if err != nil {
		return nil, err
	}
	for _, off := range s.siteSvc.Disabled() {
		a.log.Info("site workflow disabled", "workflow", off)
	}
	if a.cfg.PreviewBaseDomain == "" {
		a.log.Warn("preview_base_domain is not set; site creation is refused until it is")
	}
	deploys, err := deploy.New(deploy.Options{
		DB: db, Uploads: uploadSvc, Provisioner: prov, Tasks: prov, ObjectCaches: caches, Health: s.siteSvc, Layout: layout,
		Logger: a.log.With("component", "deploy"),
	})
	if err != nil {
		return nil, err
	}
	hostKey, err := a.sftpHostKey()
	if err != nil {
		return nil, err
	}
	if s.dbSessions, err = a.databaseSessions(db, siteDBs, hostKey); err != nil {
		return nil, err
	}
	metricsSvc, err := a.metricsQueries()
	if err != nil {
		return nil, err
	}
	if s.channels, err = a.alertChannels(db, s.secretStore, keys); err != nil {
		return nil, err
	}
	// Ensure is idempotent: it returns the key siteRuntime made.
	loginSigner, err := adminlogin.Ensure(context.WithoutCancel(ctx), db, s.secretStore)
	if err != nil {
		return nil, fmt.Errorf("platform login key: %w", err)
	}
	if s.releases, err = a.releaseChecker(); err != nil {
		return nil, err
	}
	var latestRelease api.LatestReleaseFunc
	if s.releases != nil {
		latestRelease = s.releases.Latest
	}
	// The pool tells job event streams when a job changes.
	jobChanges := jobs.NewChanges()
	handler, err := api.NewHandler(api.Options{
		AdminLogin:       adminlogin.NewService(db, prov, loginSigner),
		AlertChannels:    s.channels,
		Logger:           a.log,
		Readiness:        readiness,
		DB:               db,
		Auth:             authn,
		Authenticator:    api.NewAuthenticator(authn),
		Sites:            s.siteSvc,
		Backups:          backups,
		Uploads:          uploadSvc,
		Exports:          exports,
		Offboarding:      offboarding,
		DatabaseSessions: s.dbSessions,
		Deployments:      deploys,
		Logs:             prov,
		JobChanges:       jobChanges,
		Doctor:           a.platformDoctor(authn, siteReservations{db}),
		Metrics:          metricsSvc,
		TrustedProxies:   proxies,
		Build:            a.build(),
		LatestRelease:    latestRelease,
		Orchestrator:     a.cfg.Driver,
		SFTP:             api.SFTPInfo{Enabled: a.cfg.SFTP.Enabled(), Host: a.cfg.DashboardDomain, Port: a.cfg.SFTP.Port},
		SecureCookies:    a.cfg.HTTP.SecureCookies == config.SecureCookiesAlways,
	})
	if err != nil {
		return nil, err
	}
	dashboard, err := httpserver.NewUI(ui.Files())
	if err != nil {
		return nil, err
	}
	if !ui.Embedded() {
		a.log.Warn("this binary was built without the dashboard; serving a placeholder page (build with Node.js available to embed it)")
	}
	s.handler = httpserver.WithUI(handler, dashboard)

	if s.gateway, err = a.sftpGateway(db, authn, prov, hostKey, s.dbSessions.Forwarding(authn)); err != nil {
		return nil, err
	}
	if s.gateway != nil {
		if s.gatewayLn, err = net.Listen("tcp", a.cfg.SFTP.Listen); err != nil {
			return nil, fmt.Errorf("SFTP gateway: %w", err)
		}
	}

	// Job workers and the scheduler run alongside the server and stop with
	// it; the pool gives running steps a grace period.
	s.pool = jobs.NewPool(db, jobs.Options{
		Concurrency: a.cfg.Jobs.Concurrency,
		Logger:      a.log.With("component", "jobs"),
		Secrets:     s.secretStore,
		Changes:     jobChanges,
	})
	handlers := slices.Concat(
		[]jobs.Handler{audit.CleanupHandler(db, a.cfg.Audit.Retention(), nil)},
		s.siteSvc.Handlers(),
		[]jobs.Handler{backups.Handler(), uploadSvc.CleanupHandler(), offboarding.Handler(), stateBackups.Handler()},
		repoMaint.Handlers(), exports.Handlers(), []jobs.Handler{deploys.Handler()}, s.dbSessions.Handlers(),
		[]jobs.Handler{pluginupdates.Handler(db, prov)},
	)
	for _, h := range handlers {
		if err := s.pool.Register(h); err != nil {
			return nil, err
		}
	}
	if err := sitecron.Register(s.pool, db, prov); err != nil {
		return nil, err
	}
	if s.alertEngine, err = a.alertEngine(db, s.channels); err != nil {
		return nil, err
	}
	s.scheduler = jobs.NewScheduler(db, jobs.SchedulerOptions{Logger: a.log.With("component", "scheduler")})
	return s, nil
}

// close releases what buildServer opened, in reverse order. It runs after
// run has stopped every worker, so nothing uses them any more.
func (s *server) close() {
	if s.gatewayLn != nil {
		_ = s.gatewayLn.Close()
	}
	if s.siteDBs != nil {
		_ = s.siteDBs.Close()
	}
	if s.db != nil {
		_ = s.db.Close()
	}
}

// startup brings the state database in line with this binary and its
// configuration before serving.
//
// The rule: a step whose failure would silently stop the control plane's
// own housekeeping (its cleanup and sweep schedules, the default alert
// rules) is fatal, because nothing would notice or redo it later. A step
// that reconciles sites or other per-target state is best effort: it is
// logged, the API starts anyway, and the jobs it queues or the next start
// try again. A canceled context means serve is already stopping, and no
// step fails then.
func (s *server) startup(ctx context.Context) error {
	db, siteSvc, a := s.db, s.siteSvc, s.a
	stopping := func() bool { return ctx.Err() != nil }
	bestEffort := []struct {
		what string
		run  func() error
	}{
		// Existing sites pick up changed install-wide settings (the
		// WordPress security set, §6.1) through one reconcile job per site.
		{"queuing site reconciliation", func() error { _, err := siteSvc.EnqueueReconcile(ctx); return err }},
		// Sites created before scheduled backups existed get a backup
		// schedule, and a changed backup window moves every schedule.
		{"reconciling backup schedules", func() error { return sitebackup.ReconcileSchedules(ctx, db, s.backupWindow) }},
		// Likewise every site's plugin and theme update checks.
		{"reconciling update check schedules", func() error { return pluginupdates.ReconcileSchedules(ctx, db) }},
		// Likewise every backup repository's prune and check schedules.
		{"reconciling backup repository maintenance schedules", func() error {
			return repomaint.ReconcileSchedules(ctx, db, a.maintenanceTiming())
		}},
		// The core rollout runs on a schedule, and once now, so releases
		// new to the version manifest start rolling out.
		{"scheduling the rollouts", func() error { return siteSvc.Rollouts().EnsureRolloutSchedule(ctx) }},
		{"queuing the rollouts", func() error { return siteSvc.Rollouts().EnqueueRollout(ctx) }},
	}
	for _, step := range bestEffort {
		if err := step.run(); err != nil && !stopping() {
			s.log.Warn("startup reconciliation failed; serving anyway", "step", step.what, "error", err)
		}
	}
	fatal := []func() error{
		func() error { return audit.EnsureCleanupSchedule(ctx, db) },
		func() error { return uploads.EnsureCleanupSchedule(ctx, db) },
		func() error { return export.EnsureCleanupSchedule(ctx, db) },
		func() error { return dbsession.EnsureSweepSchedule(ctx, db) },
		// The state database's disaster recovery backups (#96).
		func() error { return statebackup.EnsureSchedule(ctx, db, a.cfg.Backup.State.Interval()) },
		// The default alert rules exist whether or not evaluation is on,
		// so the API lists them. Seeding never overwrites a user's edits.
		func() error {
			_, err := alerts.SeedDefaults(ctx, db, s.log.With("component", "alerts"))
			return err
		},
	}
	for _, step := range fatal {
		if err := step(); err != nil && !stopping() {
			return err
		}
	}
	return nil
}

// run starts the background workers and serves HTTP until ctx is canceled
// or the server fails, then stops the workers and waits for them, so the
// database isn't closed under running job steps.
func (s *server) run(ctx context.Context) error {
	workersCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()
	w := &workerGroup{ctx: workersCtx, log: s.log}

	w.Go("job pool", s.pool.Run)
	w.Go("scheduler", s.scheduler.Run)
	if bw, ok := s.rt.Driver.(driver.BackgroundWorker); ok {
		w.Go("driver", bw.RunBackground)
	}
	// The collectors write to VictoriaMetrics, which may still be starting
	// or down: write failures are logged and counted, and the next run
	// tries again.
	if s.a.cfg.Metrics.Enabled {
		collector, err := s.a.metricsService(s.db, s.rt, s.secretStore, s.repos)
		if err != nil {
			return err
		}
		// The managers hold the admin access to both servers already.
		for _, c := range []metrics.Collector{
			metrics.MariaDBCollector{Source: s.siteDBs},
			metrics.RedisCollector{Source: s.caches},
		} {
			if err := collector.Register(c); err != nil {
				return err
			}
		}
		w.Go("metrics collector", collector.Run)
	}
	if s.alertEngine != nil {
		w.Go("alert engine", s.alertEngine.Run)
	}
	// Delivers the alert events the channels accepted, and retries the ones
	// that failed, whether or not rules are being evaluated.
	w.Go("alert delivery", s.channels.Run)
	// Best effort: a platform upgrade may have recreated MariaDB or the
	// object cache, disconnecting them from every site network.
	w.Once(func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, reattachTimeout)
		defer cancel()
		if a, ok := s.rt.Driver.(sites.SharedServiceAttacher); ok {
			s.siteSvc.ReattachSharedServices(ctx, a)
		}
	})
	// The object cache keeps site users only in memory: restore them now
	// and whenever it restarts. (Sites created before it existed get theirs
	// from their site.reconcile job.)
	w.Go("object cache watcher", func(ctx context.Context) error { s.caches.Watch(ctx); return nil })
	// Startup reconciliation of database sessions: expire what expired
	// while the control plane was down and drop temporary users left
	// behind. Best effort: MariaDB may still be starting, and the sweep job
	// retries every minute.
	w.Once(func(ctx context.Context) {
		if _, err := s.dbSessions.Sweep(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("reconciling database sessions failed; the sweep job retries", "error", err)
		}
	})
	// Checks at startup and once a day; a failure only costs a debug log.
	if s.releases != nil {
		w.Go("release check", s.releases.Run)
	}
	// The SFTP gateway stops with the workers, ending its sessions, before
	// the database closes.
	if s.gateway != nil {
		w.Go("SFTP gateway", func(ctx context.Context) error { return s.gateway.Serve(ctx, s.gatewayLn) })
	}

	srv := httpserver.New(httpserver.Options{Addr: s.a.cfg.Listen, Handler: s.handler, Logger: s.log})
	srvErr := srv.Run(ctx)
	// Stop the workers too if the server failed on its own.
	stopWorkers()
	w.Wait()
	return srvErr
}

// workerGroup runs the control plane's background workers. A worker is
// meant to run until ctx is canceled; one that stops earlier, with or
// without an error, is logged as an error, because the control plane then
// keeps serving without it (the API stays up, so the problem can be seen
// and the process restarted).
type workerGroup struct {
	ctx context.Context
	log *slog.Logger
	wg  sync.WaitGroup
}

// Go runs a long-lived worker.
func (w *workerGroup) Go(name string, run func(context.Context) error) {
	w.wg.Go(func() {
		err := run(w.ctx)
		switch {
		case w.ctx.Err() != nil:
			if err != nil && !errors.Is(err, context.Canceled) {
				w.log.Warn("background worker stopped with an error during shutdown", "worker", name, "error", err)
			}
		case err != nil:
			w.log.Error("background worker failed; the control plane keeps running without it", "worker", name, "error", err)
		default:
			w.log.Error("background worker stopped early; the control plane keeps running without it", "worker", name)
		}
	})
}

// Once runs a task that is expected to finish on its own.
func (w *workerGroup) Once(run func(context.Context)) {
	w.wg.Go(func() { run(w.ctx) })
}

// Wait waits for every worker and task to return.
func (w *workerGroup) Wait() { w.wg.Wait() }

// templateDriver returns what the templates rollout needs of the driver, or
// nil when the driver has no templates to roll out (a nil interface, not a
// struct of nils, which sites.Options would take as on).
func templateDriver(rt *driver.Runtime) sites.TemplateDriver {
	if rt.Templates == nil || rt.Snapshots == nil {
		return nil
	}
	return struct {
		provisioner.TemplateFingerprinter
		provisioner.RenderSnapshotter
	}{rt.Templates, rt.Snapshots}
}
