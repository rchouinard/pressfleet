package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"

	"k8s.io/client-go/rest"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// ErrInvalidSite is returned for a site the driver can't render: an ID
// that can't name a namespace, or a configuration it can't use.
var ErrInvalidSite = errors.New("kubernetes: invalid site")

var (
	_ provisioner.Driver                = (*Provisioner)(nil)
	_ provisioner.TemplateFingerprinter = (*Provisioner)(nil)
	_ provisioner.RenderSnapshotter     = (*Provisioner)(nil)
	_ provisioner.Prober                = (*Provisioner)(nil)
)

// DefaultWaitTimeout bounds how long the driver waits for a site's pod to
// become ready, or to go away, unless Config.WaitTimeout says otherwise.
// It covers pulling the site's images on a new node and seeding an empty
// wp-content.
const DefaultWaitTimeout = 5 * time.Minute

// SecretReader reads stored secrets; secrets.Store implements it.
type SecretReader interface {
	Get(ctx context.Context, ref secrets.Ref) (redact.Secret, error)
}

// ImageResolver maps a site's PHP and WordPress versions to its images;
// *catalog.Catalog implements it.
type ImageResolver interface {
	Images(php, wordpress string) (catalog.Images, error)
}

// Config configures the Kubernetes driver.
type Config struct {
	// Layout is the control plane's data directory, where site manifests
	// are rendered.
	Layout config.Layout
	// Kubernetes holds the install's Kubernetes settings.
	Kubernetes config.KubernetesConfig
	// Secrets resolves SiteConfig's DBSecretRef, WPSaltsSecretRef, and
	// RedisSecretRef, and Images its PHPVersion and WPCoreVersion. A
	// driver without them (the repository task commands) can't render
	// sites.
	Secrets SecretReader
	Images  ImageResolver
	// MariaDBAddress and RedisAddress are the platform's MariaDB and
	// Valkey as the control plane reaches them (mariadb.address,
	// redis.address): a Service of the platform namespace, or an external
	// address. Sites reach the same servers (platformAddress).
	MariaDBAddress string
	RedisAddress   string
	// WordPressSecurity is the install-wide WordPress security set (§6.1).
	WordPressSecurity config.WordPressSecurityConfig
	// SMTP is the platform's outbound email relay, rendered into every
	// site's wp-config.php; nil means sites send no email.
	SMTP *wpconfig.SMTP
	// LoginPublicKey is the platform's login public key in base64, for
	// dashboard login (§5.9); empty omits it.
	LoginPublicKey string
	// WPCLITimeout bounds each ExecWPCLI call, in addition to its context;
	// zero means DefaultWPCLITimeout. WPCLIMaxOutput is the most standard
	// output it keeps, in bytes; zero means DefaultWPCLIMaxOutput.
	WPCLITimeout   time.Duration
	WPCLIMaxOutput int
	// WaitTimeout bounds waiting for a site's pod; zero means
	// DefaultWaitTimeout.
	WaitTimeout time.Duration
	// Registry is the HTTP client PinImage asks registries with; nil means
	// one with DefaultRegistryTimeout.
	Registry *http.Client
	// Tasks configures site and repository tasks.
	Tasks  TaskConfig
	Logger *slog.Logger
}

// Provisioner is the Kubernetes driver.
type Provisioner struct {
	*Client
	layout            config.Layout
	platformNamespace string
	namespacePrefix   string
	log               *slog.Logger
	waitTimeout       time.Duration

	secrets      SecretReader
	images       ImageResolver
	security     config.WordPressSecurityConfig
	smtp         *wpconfig.SMTP
	loginKey     string
	podCIDRs     []string
	pullSecrets  []string
	storageClass string
	volumeSize   string
	// dbHost and redis are where sites reach MariaDB and Valkey, and
	// egress what their NetworkPolicy allows; setupErr is why they
	// couldn't be worked out, which fails every render.
	dbHost   string
	redis    platformAddress
	egress   egress
	setupErr error

	// exec runs commands in containers; newWithREST connects it.
	exec           executor
	wpcliTimeout   time.Duration
	wpcliMaxOutput int

	registry *http.Client
	// insecureRegistries are asked over plain HTTP (registryURL).
	insecureRegistries []string
	observe            observers
	pinsMu             sync.Mutex
	pins               map[string]string // tag reference -> pinned reference

	// tasks is how tasks run (task.go); attach, if set, replaces
	// Client.attachPod in tests.
	tasks  taskSettings
	attach podAttacher
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
	p := newWithClient(cfg, c)
	p.exec = newRemoteExecutor(c.typed, rc)
	return p, nil
}

// newWithClient builds the driver around a client, real or fake.
func newWithClient(cfg Config, c *Client) *Provisioner {
	k := cfg.Kubernetes
	p := &Provisioner{
		Client:             c,
		layout:             cfg.Layout,
		platformNamespace:  k.PlatformNamespace,
		namespacePrefix:    k.SiteNamespacePrefix,
		log:                cfg.logger(),
		waitTimeout:        cfg.WaitTimeout,
		secrets:            cfg.Secrets,
		images:             cfg.Images,
		security:           cfg.WordPressSecurity,
		smtp:               cfg.SMTP,
		loginKey:           cfg.LoginPublicKey,
		podCIDRs:           k.PodCIDRs,
		pullSecrets:        k.ImagePullSecrets,
		storageClass:       k.StorageClasses.Sites,
		volumeSize:         k.SiteVolumeSize,
		registry:           cfg.Registry,
		insecureRegistries: k.InsecureRegistries,
		pins:               map[string]string{},
		wpcliTimeout:       cfg.WPCLITimeout,
		wpcliMaxOutput:     cfg.WPCLIMaxOutput,
	}
	if p.waitTimeout <= 0 {
		p.waitTimeout = DefaultWaitTimeout
	}
	if p.wpcliTimeout <= 0 {
		p.wpcliTimeout = DefaultWPCLITimeout
	}
	if p.wpcliMaxOutput <= 0 {
		p.wpcliMaxOutput = DefaultWPCLIMaxOutput
	}
	if p.volumeSize == "" {
		p.volumeSize = config.DefaultSiteVolumeSize
	}
	if p.registry == nil {
		p.registry = &http.Client{Timeout: DefaultRegistryTimeout}
	}
	p.setupErr = p.setupNetwork(cfg)
	p.tasks = newTaskSettings(cfg)
	return p
}

// setupNetwork works out how sites reach the platform's services.
func (p *Provisioner) setupNetwork(cfg Config) error {
	pods, err := cfg.Kubernetes.PodPrefixes()
	if err != nil {
		return err
	}
	var services []platformAddress
	if cfg.MariaDBAddress != "" {
		db, err := resolvePlatformAddress(cfg.MariaDBAddress, p.platformNamespace)
		if err != nil {
			return fmt.Errorf("mariadb.address: %w", err)
		}
		p.dbHost = db.HostPort
		services = append(services, db)
	}
	if cfg.RedisAddress != "" {
		r, err := resolvePlatformAddress(cfg.RedisAddress, p.platformNamespace)
		if err != nil {
			return fmt.Errorf("redis.address: %w", err)
		}
		p.redis = r
		services = append(services, r)
	}
	smtpPort := 0
	if cfg.SMTP != nil && cfg.SMTP.Host != "" {
		smtpPort = cfg.SMTP.Port
	}
	p.egress = siteEgress(services, smtpPort, pods)
	return nil
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

// resolve reads what the site's references point at: its images and the
// secrets wp-config.php is rendered from. Plaintext secrets go no further
// than the rendered Secret.
func (p *Provisioner) resolve(ctx context.Context, cfg *provisioner.SiteConfig) (siteInputs, error) {
	if p.setupErr != nil {
		return siteInputs{}, fmt.Errorf("%w: %w", ErrInvalidSite, p.setupErr)
	}
	if p.secrets == nil || p.images == nil {
		return siteInputs{}, errors.New("this driver has no secrets or image catalog to render sites with")
	}
	if p.dbHost == "" {
		return siteInputs{}, fmt.Errorf("%w: the MariaDB address isn't set", ErrInvalidSite)
	}
	images, err := p.images.Images(cfg.PHPVersion, cfg.WPCoreVersion)
	if err != nil {
		return siteInputs{}, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	in := siteInputs{images: siteImages{PHP: images.PHP, Nginx: images.Nginx}}
	if ref := cfg.PinnedImages[provisioner.ComponentPHP]; ref != "" {
		in.images.PHP = ref
	}
	if ref := cfg.PinnedImages[provisioner.ComponentNginx]; ref != "" {
		in.images.Nginx = ref
	}

	dbSecret, err := p.secrets.Get(ctx, secrets.Ref(cfg.DBSecretRef))
	if err != nil {
		return siteInputs{}, fmt.Errorf("reading database credentials: %w", err)
	}
	creds, err := sitedb.UnmarshalCredentials(dbSecret)
	if err != nil {
		return siteInputs{}, fmt.Errorf("reading database credentials: %w", err)
	}
	saltsSecret, err := p.secrets.Get(ctx, secrets.Ref(cfg.WPSaltsSecretRef))
	if err != nil {
		return siteInputs{}, fmt.Errorf("reading WordPress salts: %w", err)
	}
	salts, err := wpconfig.UnmarshalSalts(saltsSecret)
	if err != nil {
		return siteInputs{}, fmt.Errorf("reading WordPress salts: %w", err)
	}
	params := p.wpConfigParams()
	params.DB, params.Salts, params.TablePrefix = creds, salts, cfg.TablePrefix
	params.Environment, params.DisableMail = string(cfg.Environment), cfg.DisableMail
	if p.loginKey != "" {
		params.SiteID = cfg.ID
	}
	if cfg.RedisSecretRef != "" {
		if p.redis.Host == "" {
			return siteInputs{}, fmt.Errorf("%w: the site has an object cache, but the Valkey address isn't set", ErrInvalidSite)
		}
		secret, err := p.secrets.Get(ctx, secrets.Ref(cfg.RedisSecretRef))
		if err != nil {
			return siteInputs{}, fmt.Errorf("reading object cache credentials: %w", err)
		}
		rc, err := objectcache.UnmarshalCredentials(secret)
		if err != nil {
			return siteInputs{}, fmt.Errorf("reading object cache credentials: %w", err)
		}
		params.Redis = &wpconfig.Redis{
			Host: p.redis.Host, Port: p.redis.Port,
			Username: rc.Username, Password: rc.Password, Database: rc.Database, Prefix: rc.KeyPrefix,
		}
	}
	if in.wpConfig, err = wpconfig.Render(params); err != nil {
		return siteInputs{}, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	return in, nil
}
