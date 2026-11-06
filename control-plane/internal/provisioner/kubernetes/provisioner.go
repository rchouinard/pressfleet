package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"regexp"

	"k8s.io/client-go/rest"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
)

// ErrNotImplemented is returned by the driver's operations that later
// changes implement (#216 to #221).
var ErrNotImplemented = errors.New("not implemented by the Kubernetes driver yet")

// ErrInvalidSite is returned for a site ID that can't name a namespace.
var ErrInvalidSite = errors.New("kubernetes: invalid site")

var (
	_ provisioner.Driver                = (*Provisioner)(nil)
	_ provisioner.TemplateFingerprinter = (*Provisioner)(nil)
)

// Config configures the Kubernetes driver.
type Config struct {
	// Layout is the control plane's data directory, where site manifests
	// are rendered.
	Layout config.Layout
	// Kubernetes holds the install's Kubernetes settings.
	Kubernetes config.KubernetesConfig
	Logger     *slog.Logger
}

// Provisioner is the Kubernetes driver.
type Provisioner struct {
	*Client
	layout            config.Layout
	platformNamespace string
	namespacePrefix   string
	log               *slog.Logger
}

// New builds the driver and its client from the kubeconfig file or the
// pod's service account. It doesn't contact the API server; Connect does.
func New(cfg Config) (*Provisioner, error) {
	rc, err := restConfig(cfg.Kubernetes.Kubeconfig)
	if err != nil {
		return nil, err
	}
	return newWithREST(cfg, rc)
}

// newWithREST builds the driver for explicit connection settings.
func newWithREST(cfg Config, rc *rest.Config) (*Provisioner, error) {
	c, err := connect(rc, cfg.logger())
	if err != nil {
		return nil, err
	}
	return newWithClient(cfg, c), nil
}

// newWithClient builds the driver around a client, real or fake.
func newWithClient(cfg Config, c *Client) *Provisioner {
	return &Provisioner{
		Client:            c,
		layout:            cfg.Layout,
		platformNamespace: cfg.Kubernetes.PlatformNamespace,
		namespacePrefix:   cfg.Kubernetes.SiteNamespacePrefix,
		log:               cfg.logger(),
	}
}

func (cfg Config) logger() *slog.Logger {
	if cfg.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return cfg.Logger
}

// Connect checks that the cluster is reachable and accepts the driver's
// credentials, and logs its version. The control plane doesn't start
// without it.
func (p *Provisioner) Connect(ctx context.Context) error {
	info, err := p.Ping(ctx)
	if err != nil {
		return err
	}
	p.log.InfoContext(ctx, "connected to the Kubernetes cluster", "server_version", info.GitVersion, "platform_namespace", p.platformNamespace)
	return nil
}

// TemplateFingerprint implements provisioner.TemplateFingerprinter: a hash
// of templates/k8s and renderVersion.
func (p *Provisioner) TemplateFingerprint() string { return templateFingerprint() }

// namespaceName matches the names namespaces may have (RFC 1123 labels).
var namespaceName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// SiteNamespace returns the namespace of a site, which holds all its
// objects.
func (p *Provisioner) SiteNamespace(siteID string) string { return p.namespacePrefix + siteID }

// validSiteNamespace checks that the site's ID makes a namespace name.
func (p *Provisioner) validSiteNamespace(siteID string) error {
	if ns := p.SiteNamespace(siteID); !namespaceName.MatchString(ns) {
		return fmt.Errorf("%w: site ID %q doesn't make a namespace name: %q", ErrInvalidSite, siteID, ns)
	}
	return nil
}

func notImplemented(op string) error { return fmt.Errorf("%s: %w", op, ErrNotImplemented) }

// CreateSite implements provisioner.Provisioner (#216).
func (p *Provisioner) CreateSite(context.Context, *provisioner.SiteConfig) (*provisioner.SiteStatus, error) {
	return nil, notImplemented("create site")
}

// UpdateSite implements provisioner.Provisioner (#216).
func (p *Provisioner) UpdateSite(context.Context, *provisioner.SiteConfig) (*provisioner.SiteStatus, error) {
	return nil, notImplemented("update site")
}

// GetSite implements provisioner.Provisioner (#216).
func (p *Provisioner) GetSite(context.Context, string) (*provisioner.SiteStatus, error) {
	return nil, notImplemented("get site")
}

// DeleteSite implements provisioner.Provisioner (#216).
func (p *Provisioner) DeleteSite(context.Context, string) error {
	return notImplemented("delete site")
}

// StartSite implements provisioner.Provisioner (#216).
func (p *Provisioner) StartSite(context.Context, string) error {
	return notImplemented("start site")
}

// StopSite implements provisioner.Provisioner (#216).
func (p *Provisioner) StopSite(context.Context, string) error {
	return notImplemented("stop site")
}

// SetMaintenance implements provisioner.Provisioner (#217).
func (p *Provisioner) SetMaintenance(context.Context, string, bool) error {
	return notImplemented("set maintenance mode")
}

// ExecWPCLI implements provisioner.Provisioner (#218).
func (p *Provisioner) ExecWPCLI(context.Context, string, []string) (string, error) {
	return "", notImplemented("run WP-CLI")
}

// PurgeCache implements provisioner.Provisioner (#218).
func (p *Provisioner) PurgeCache(context.Context, string) error {
	return notImplemented("purge the page cache")
}

// ReloadCode implements provisioner.Provisioner (#218).
func (p *Provisioner) ReloadCode(context.Context, string) error {
	return notImplemented("reload code")
}

// Logs implements provisioner.LogReader (#221).
func (p *Provisioner) Logs(context.Context, string, provisioner.LogOptions) iter.Seq2[provisioner.LogEntry, error] {
	return func(yield func(provisioner.LogEntry, error) bool) {
		yield(provisioner.LogEntry{}, notImplemented("read logs"))
	}
}

// Stats implements provisioner.StatsReader (#221).
func (p *Provisioner) Stats(context.Context, string) ([]provisioner.ComponentStats, error) {
	return nil, notImplemented("read stats")
}

// RunTask implements provisioner.TaskRunner (#219).
func (p *Provisioner) RunTask(context.Context, string, provisioner.Task) (*provisioner.TaskResult, error) {
	return nil, notImplemented("run a site task")
}

// RunRepositoryTask implements provisioner.RepositoryTaskRunner (#219).
func (p *Provisioner) RunRepositoryTask(context.Context, string, provisioner.Task) (*provisioner.TaskResult, error) {
	return nil, notImplemented("run a repository task")
}

// RunSession implements provisioner.SessionRunner (#220).
func (p *Provisioner) RunSession(context.Context, string, provisioner.Session) error {
	return notImplemented("run a session")
}

// ProbeHTTP implements provisioner.Prober (#218).
func (p *Provisioner) ProbeHTTP(context.Context, string, provisioner.ProbeRequest) (*provisioner.ProbeResponse, error) {
	return nil, notImplemented("probe a site")
}

// PinImage implements provisioner.ImagePinner (#216).
func (p *Provisioner) PinImage(context.Context, string) (string, error) {
	return "", notImplemented("pin an image")
}

// PullImage implements provisioner.ImagePinner (#216).
func (p *Provisioner) PullImage(context.Context, string) error {
	return notImplemented("pull an image")
}
