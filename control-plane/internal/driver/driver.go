// Package driver builds the site driver the configuration names (§4.2).
//
// It is the one place, with the host commands that manage the platform's own
// stack, that names a driver's types: everything else depends on the
// [provisioner] interfaces, so a second driver plugs in here. It lives in
// its own package, not in cli, so that the control plane's wiring (serve and
// the commands that run in its container) holds no driver types, and not in
// provisioner, which the drivers import.
package driver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/doctor"
	"github.com/rchouinard/pressfleet/control-plane/internal/platform"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/kubernetes"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// ErrUnknownDriver is returned for a driver name that isn't supported.
var ErrUnknownDriver = errors.New("driver: unknown driver")

// ErrNotKubernetes is returned for a Kubernetes-only operation on another
// driver.
var ErrNotKubernetes = errors.New("driver: the configured driver is not kubernetes")

// Runtime is a driver and the optional capabilities it has. A capability the
// driver lacks is nil, and consumers turn their feature off.
type Runtime struct {
	// Driver orchestrates sites and runs tasks. It may also implement
	// optional interfaces, such as StartupHook and
	// sites.SharedServiceAttacher, which callers find by type assertion.
	Driver provisioner.Driver
	// FPMStatus reads PHP-FPM's status for the metrics collector.
	FPMStatus provisioner.FPMStatusReader
	// TraefikMetrics reads the edge router's metrics.
	TraefikMetrics provisioner.TraefikMetricsReader
	// NodeStats samples the nodes of a cluster for the host metrics. It is
	// nil where the platform runs on one host, whose procfs the host
	// collector reads instead.
	NodeStats provisioner.NodeStatsReader
	// EdgeAddress is where the control plane reaches the edge router's
	// HTTPS port (host:port) when the domains' own addresses won't do, as
	// inside a cluster; the uptime checks connect there. Empty means the
	// checks resolve each domain.
	EdgeAddress string
	// Templates fingerprints how the driver renders sites, for the
	// templates rollout.
	Templates provisioner.TemplateFingerprinter
	// Snapshots undoes a re-render, for the same rollout.
	Snapshots provisioner.RenderSnapshotter
	// HostProbe reads facts about the host's container engine for the
	// doctor.
	HostProbe doctor.DockerProbe
}

// StartupHook is implemented by a driver that has work to do when the
// control plane starts, before it runs tasks: the Compose driver removes the
// secret files of tasks a crash left. Like sites.SharedServiceAttacher, it
// is optional.
type StartupHook interface {
	Startup(ctx context.Context) error
}

// BackgroundWorker is implemented by a driver that has work to do while the
// control plane runs: the Kubernetes driver keeps sites' internet policies
// away from the cluster's nodes as they come and go. RunBackground runs
// until ctx ends. Like StartupHook, it is optional.
type BackgroundWorker interface {
	RunBackground(ctx context.Context) error
}

// SecretReader reads the secrets sites need.
type SecretReader interface {
	Get(ctx context.Context, ref secrets.Ref) (redact.Secret, error)
}

// ImageResolver maps a site's PHP and WordPress versions to its images.
type ImageResolver interface {
	Images(php, wordpress string) (catalog.Images, error)
}

// Options are what the driver needs from the control plane besides the
// configuration.
type Options struct {
	Config  *config.Config
	Secrets SecretReader
	Images  ImageResolver
	// SMTP is the relay sites send email through; nil for none.
	SMTP *wpconfig.SMTP
	// LoginPublicKey is the platform login key's public half, base64.
	LoginPublicKey string
	Logger         *slog.Logger
}

// New builds the driver that opts.Config.Driver names. A driver that
// talks to an orchestrator over the network (Kubernetes) connects to it
// within ctx, and fails if it can't, so the control plane doesn't start
// without its cluster.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	switch opts.Config.Driver {
	case config.SiteDriverCompose:
		return newCompose(opts)
	case config.SiteDriverKubernetes:
		return newKubernetes(ctx, opts)
	default:
		return nil, fmt.Errorf("%w %q", ErrUnknownDriver, opts.Config.Driver)
	}
}

// NewRepositoryTasks builds a driver for the commands that run outside the
// serving control plane (`pressfleet repository`, `state`) and only run
// repository tasks, so it has no sites to resolve.
func NewRepositoryTasks(cfg *config.Config, log *slog.Logger) (provisioner.RepositoryTaskRunner, error) {
	switch cfg.Driver {
	case config.SiteDriverCompose:
		return compose.NewProvisioner(compose.ProvisionerConfig{
			Layout:           cfg.Layout(),
			Secrets:          noSiteSecrets{},
			Images:           noSiteImages{},
			MariaDBContainer: platform.MariaDBContainer(cfg.PlatformProject),
			PlatformProject:  cfg.PlatformProject,
			BackupNetwork:    platform.BackupNetwork(cfg.PlatformProject),
			TaskImage:        cfg.TaskImage,
			TaskMemoryMB:     cfg.Backup.TaskMemoryMB,
			Logger:           log.With("component", "provisioner"),
		})
	case config.SiteDriverKubernetes:
		return kubernetes.New(kubernetesConfig(cfg, log))
	default:
		return nil, fmt.Errorf("%w %q", ErrUnknownDriver, cfg.Driver)
	}
}

// NewHostProbe returns the doctor's probe of the host for the configured
// driver. The host CLI has no secrets or state, so it can't build a Runtime.
func NewHostProbe(cfg *config.Config) (doctor.DockerProbe, error) {
	switch cfg.Driver {
	case config.SiteDriverCompose:
		return compose.HostInspector{}, nil
	case config.SiteDriverKubernetes:
		return noDockerHost{}, nil
	default:
		return nil, fmt.Errorf("%w %q", ErrUnknownDriver, cfg.Driver)
	}
}

// NewClusterProbe returns the doctor's probe of the Kubernetes cluster, or
// ErrNotKubernetes for another driver. It connects lazily: an unreachable
// API server shows in the doctor's first cluster check, not here.
func NewClusterProbe(cfg *config.Config, log *slog.Logger) (doctor.ClusterProbe, error) {
	if cfg.Driver != config.SiteDriverKubernetes {
		return nil, ErrNotKubernetes
	}
	probe, err := kubernetes.NewClusterProbe(kubernetes.ProbeConfig{
		Kubernetes: cfg.Kubernetes,
		Image:      cfg.TaskImage,
		Logger:     log.With("component", "doctor"),
	})
	if err != nil {
		// Not the typed nil pointer, which would make a non-nil interface.
		return nil, err
	}
	return probe, nil
}

// composeDriver is the Compose driver with its startup work.
type composeDriver struct {
	*compose.Provisioner
	layout config.Layout
}

// Startup removes the secret files that a crash left from tasks that were
// running.
func (c composeDriver) Startup(context.Context) error {
	return compose.RemoveStaleTaskSecrets(c.layout)
}

func newCompose(opts Options) (*Runtime, error) {
	cfg := opts.Config
	prov, err := compose.NewProvisioner(compose.ProvisionerConfig{
		Layout:            cfg.Layout(),
		Secrets:           opts.Secrets,
		Images:            opts.Images,
		MariaDBContainer:  platform.MariaDBContainer(cfg.PlatformProject),
		RedisContainer:    platform.RedisContainer(cfg.PlatformProject),
		TraefikContainer:  platform.TraefikContainer(cfg.PlatformProject),
		TaskImage:         cfg.TaskImage,
		TaskMemoryMB:      cfg.Backup.TaskMemoryMB,
		PlatformProject:   cfg.PlatformProject,
		BackupNetwork:     platform.BackupNetwork(cfg.PlatformProject),
		TraefikDynamicDir: filepath.Join(cfg.Layout().PlatformDir(), platform.TraefikDynamicDir),
		WordPressSecurity: cfg.WordPressSecurity,
		Logger:            opts.Logger.With("component", "provisioner"),
		SMTP:              opts.SMTP,
		LoginPublicKey:    opts.LoginPublicKey,
	})
	if err != nil {
		return nil, err
	}
	// The wrapper adds the startup hook; the embedded Provisioner keeps its
	// other optional methods (AttachSharedServices and the like).
	return &Runtime{
		Driver:         composeDriver{Provisioner: prov, layout: cfg.Layout()},
		FPMStatus:      prov,
		TraefikMetrics: prov,
		Templates:      prov,
		Snapshots:      prov,
		HostProbe:      compose.HostInspector{},
	}, nil
}

var (
	_ StartupHook      = (*kubernetes.Provisioner)(nil)
	_ BackgroundWorker = (*kubernetes.Provisioner)(nil)
)

// connectTimeout bounds connecting to the Kubernetes API server at startup.
const connectTimeout = 30 * time.Second

func kubernetesConfig(cfg *config.Config, log *slog.Logger) kubernetes.Config {
	return kubernetes.Config{
		Layout:            cfg.Layout(),
		Kubernetes:        cfg.Kubernetes,
		MariaDBAddress:    cfg.MariaDB.Address,
		RedisAddress:      cfg.Redis.Address,
		WordPressSecurity: cfg.WordPressSecurity,
		Tasks: kubernetes.TaskConfig{
			Image:         cfg.TaskImage,
			MemoryLimitMB: cfg.Backup.TaskMemoryMB,
			S3Endpoint:    cfg.Backup.S3.Endpoint,
		},
		Logger: log.With("component", "provisioner"),
	}
}

func newKubernetes(ctx context.Context, opts Options) (*Runtime, error) {
	kc := kubernetesConfig(opts.Config, opts.Logger)
	kc.Secrets, kc.Images = opts.Secrets, opts.Images
	kc.SMTP, kc.LoginPublicKey = opts.SMTP, opts.LoginPublicKey
	prov, err := kubernetes.New(kc)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := prov.Connect(ctx); err != nil {
		return nil, err
	}
	k := opts.Config.Kubernetes
	return &Runtime{
		Driver:         prov,
		FPMStatus:      prov,
		TraefikMetrics: prov,
		NodeStats:      prov,
		EdgeAddress:    net.JoinHostPort(k.TraefikService+"."+k.PlatformNamespace+".svc", "443"),
		Templates:      prov,
		Snapshots:      prov,
		HostProbe:      noDockerHost{},
	}, nil
}

// errNoDockerHost is what the doctor's Docker checks report on Kubernetes,
// which has no Docker host; the doctor skips them there and runs the
// cluster checks (doctor.ClusterProbe) instead.
var errNoDockerHost = errors.New("the Kubernetes driver runs no Docker host")

// noDockerHost is the doctor's Docker probe on Kubernetes.
type noDockerHost struct{}

func (noDockerHost) Engine(context.Context) (compose.EngineInfo, error) {
	return compose.EngineInfo{}, errNoDockerHost
}

func (noDockerHost) ComposeVersion(context.Context) (string, error) { return "", errNoDockerHost }

// errNoSites is what the site parts of a repository-only driver return.
var errNoSites = errors.New("this driver only runs repository tasks")

type noSiteSecrets struct{}

func (noSiteSecrets) Get(context.Context, secrets.Ref) (redact.Secret, error) { return "", errNoSites }

type noSiteImages struct{}

func (noSiteImages) Images(string, string) (catalog.Images, error) {
	return catalog.Images{}, errNoSites
}
