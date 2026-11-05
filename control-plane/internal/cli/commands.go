package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/rchouinard/pressfleet/control-plane/internal/adminlogin"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/buildinfo"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/dbsession"
	"github.com/rchouinard/pressfleet/control-plane/internal/domains"
	"github.com/rchouinard/pressfleet/control-plane/internal/driver"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache/redis"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sftpgw"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/mariadb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// reattachTimeout bounds reconnecting the platform services to site
// networks at startup.
const reattachTimeout = 5 * time.Minute

// siteRuntime returns the configured site driver and the site database manager.
func (a *app) siteRuntime(ctx context.Context, cat *catalog.Catalog, secretStore secrets.Store, keys *secrets.Keyring, db *store.DB) (*driver.Runtime, *mariadb.Manager, error) {
	smtp, err := siteSMTP(a.cfg.SMTP, keys)
	if err != nil {
		return nil, nil, err
	}
	// Generated on first start. Every site's wp-config.php gets the public
	// half, so the reconcile jobs queued at startup add it to existing sites.
	// Not cancelable: a quick write, and startup shouldn't stop half way.
	loginSigner, err := adminlogin.Ensure(context.WithoutCancel(ctx), db, secretStore)
	if err != nil {
		return nil, nil, fmt.Errorf("platform login key: %w", err)
	}
	rt, err := driver.New(driver.Options{
		Config:         &a.cfg,
		Secrets:        secretStore,
		Images:         cat,
		SMTP:           smtp,
		LoginPublicKey: loginSigner.PublicKeyBase64(),
		Logger:         a.log,
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
	return rt, siteDBs, nil
}

// sftpHostKey returns the SFTP gateway's host key, or nil when the gateway
// is off: sftp.listen is empty, or the host key file doesn't exist, as when
// the control plane runs outside a platform that pressfleet init rendered.
func (a *app) sftpHostKey() (ssh.Signer, error) {
	if !a.cfg.SFTP.Enabled() {
		a.log.Info("the SFTP gateway is off (sftp.listen is empty)")
		return nil, nil
	}
	hostKey, err := sftpgw.LoadHostKey(a.cfg.SFTP.HostKeyFile)
	if errors.Is(err, fs.ErrNotExist) {
		a.log.Warn("the SFTP gateway is off: its host key file doesn't exist; re-run pressfleet init to create it",
			"host_key_file", a.cfg.SFTP.HostKeyFile)
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return hostKey, nil
}

// databaseSessions returns the temporary database session service
// (§5.8). Its tunnels go through the SFTP gateway, so sessions are refused
// when the gateway is off (a nil hostKey).
func (a *app) databaseSessions(db *store.DB, siteDBs *mariadb.Manager, hostKey ssh.Signer) (*dbsession.Service, error) {
	tunnel := dbsession.Tunnel{SSHPort: a.cfg.SFTP.Port, DatabaseAddr: a.cfg.MariaDB.Address}
	if hostKey != nil {
		tunnel.SSHHost = a.cfg.DashboardDomain
	}
	return dbsession.New(dbsession.Options{
		DB: db, Users: siteDBs, Tunnel: tunnel, Logger: a.log.With("component", "database_session"),
	})
}

// metricsService returns the metrics collector service (§7), writing to
// the configured VictoriaMetrics, with the built-in collectors registered.
func (a *app) metricsService(db *store.DB, rt *driver.Runtime, secretStore secrets.Store, repos *repository.Manager) (*metrics.Service, error) {
	log := a.log.With("component", "metrics")
	client, err := metrics.NewClient(metrics.Options{URL: a.cfg.Metrics.URL, Logger: log})
	if err != nil {
		return nil, err
	}
	svc, err := metrics.NewService(metrics.ServiceOptions{
		Writer: client, Targets: metrics.NewSiteTargets(db),
		DefaultInterval: a.cfg.Metrics.Interval(), Logger: log,
	})
	if err != nil {
		return nil, err
	}
	if err := svc.Register(metrics.InventoryCollector{}); err != nil {
		return nil, err
	}
	if rt.TraefikMetrics != nil {
		if err := svc.Register(metrics.TraefikCollector{Reader: rt.TraefikMetrics}); err != nil {
			return nil, err
		}
	}
	if err := svc.Register(metrics.ContainerCollector{Stats: rt.Driver}); err != nil {
		return nil, err
	}
	uptime, err := metrics.NewUptimeCollector(metrics.UptimeOptions{
		Aliases: store.NewSites(db), Secrets: secretStore, Logger: log.With("collector", "uptime"),
	})
	if err != nil {
		return nil, err
	}
	if err := svc.Register(uptime); err != nil {
		return nil, err
	}
	collectors := []metrics.Collector{&metrics.PageCacheCollector{Logs: rt.Driver}}
	if rt.FPMStatus != nil {
		collectors = append(collectors, metrics.PHPFPMCollector{Status: rt.FPMStatus})
	}
	for _, c := range append(collectors, a.hostStorageCollectors(rt.Driver, repos)...) {
		if err := svc.Register(c); err != nil {
			return nil, err
		}
	}
	if err := svc.Register(metrics.BackupJobsCollector{DB: db}); err != nil {
		return nil, err
	}
	if err := svc.Register(metrics.UpdatesCollector{DB: db}); err != nil {
		return nil, err
	}
	return svc, nil
}

// sftpGatewayreturns the SFTP gateway (§5.8), or nil when it is off (a nil
// hostKey). Port forwarding goes to the database of a site on which the
// user has an open database session, and nowhere else.
func (a *app) sftpGateway(db *store.DB, authn *auth.Service, sessions provisioner.SessionRunner, hostKey ssh.Signer,
	forwarding sftpgw.ForwardingPolicy) (*sftpgw.Server, error) {
	if hostKey == nil {
		return nil, nil
	}
	return sftpgw.New(sftpgw.Options{
		DB:               db,
		Auth:             authn,
		Sessions:         sessions,
		HostKey:          hostKey,
		IdleTimeout:      a.cfg.SFTP.IdleTimeout(),
		MaxSessionLength: a.cfg.SFTP.MaxSessionLength(),
		Forwarding:       forwarding,
		Logger:           a.log.With("component", "sftp"),
	})
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

func (a *app) versionCommand() *cobra.Command {
	var client bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the versions of this CLI and the installed control plane",
		Long: `Print the version of this CLI and of the installed control plane, and
whether the two are within the supported version skew (design doc §4.7).

With --client, only the CLI's version is printed, which is all that makes
sense inside the control plane container.`,
		Args: cobra.NoArgs,
		// Works with a broken configuration, so that an operator can still
		// find out which versions are in play.
		Annotations: map[string]string{skipConfig: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintln(out, a.build()); err != nil || client {
				return err
			}
			// Best effort: the configuration only names the platform
			// project; without it the defaults apply.
			if cfg, err := config.Load(a.configPath, a.opts.Env); err == nil {
				a.cfg = cfg
			}
			return a.writeControlPlaneVersion(cmd.Context(), out)
		},
	}
	cmd.Flags().BoolVar(&client, "client", false, "print only this CLI's version")
	return cmd
}

// build describes the running binary.
func (a *app) build() buildinfo.Info {
	return buildinfo.Read(a.opts.Version, a.opts.Commit)
}
