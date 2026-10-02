// Package platform renders the platform services Compose project: the
// control plane, MariaDB, the object cache (Redis protocol, served by
// Valkey), VictoriaMetrics, and Traefik with its Docker socket proxy (design
// doc §4.1). VictoriaMetrics is reachable on the platform network only and
// keeps its data for Config.MetricsRetentionDays (config.MetricsConfig).
//
// The host CLI renders the project from templates embedded in its binary
// (package templates) into <data_dir>/platform/. Rendered files are never
// edited by hand; re-rendering overwrites them (§4.7).
package platform

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/crypto/bcrypt"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner/compose"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/templates"
)

// ProjectName is the Compose project name of the platform services.
const ProjectName = "pressfleet"

// Memory limits from the §4.1 capacity budget. The control plane shares its
// 256 MiB budget line with Traefik. Traefik uses about 25 MiB idle, but
// about 50 MiB while it starts and while it gets certificates; with 64 MiB
// (a Go memory limit of 48 MiB) it spent its time collecting garbage and
// failed its healthcheck, so it gets 96 MiB, the extra 32 MiB out of the
// headroom line. The socket proxy, a small Go program serving one client,
// uses about 11 MiB and comes on top, out of the headroom line, as does the
// maintenance page service, which serves one static page.
const (
	DefaultMariaDBMemoryMB      = 768
	DefaultControlPlaneMemoryMB = 192
	DefaultTraefikMemoryMB      = 96
	DefaultSocketProxyMemoryMB  = 24
	MaintenanceMemoryMB         = 24
)

// VictoriaMetrics sizing and address. The §4.1 budget line is its container
// limit; it sizes its caches from the cgroup limit itself.
const (
	DefaultVictoriaMetricsMemoryMB = 256
	// VictoriaMetricsService is the Compose service name, which is also its
	// host name on the platform network. The collector writes to, and the
	// dashboard queries, http://victoria-metrics:8428.
	VictoriaMetricsService = "victoria-metrics"
	VictoriaMetricsPort    = 8428
)

// Object cache sizing. The §4.1 budget line is the cache's maxmemory; the
// container limit adds headroom for the server itself, client buffers, and
// allocator fragmentation, which maxmemory doesn't count.
const (
	DefaultRedisMaxMemoryMB = 256
	RedisOverheadMB         = 64
)

// RedisDatabases is the number of logical databases of the object cache.
// Each site gets its own (database 0 stays unused), so it bounds the number
// of sites, far above the ~250 site networks a host's address pool allows
// (§4.1).
const RedisDatabases = 1024

// RedisAdminUser is the object cache ACL user the control plane manages
// sites with; config.Config's redis.admin_user defaults to it.
const RedisAdminUser = "pressfleet"

// LetsEncryptCAServer is the ACME directory Traefik uses unless
// ACMEConfig.CAServer names another.
const LetsEncryptCAServer = config.LetsEncryptDirectory

// DefaultListen is the control plane's listen address inside its container.
const DefaultListen = ":8080"

// The SFTP gateway (design doc §5.8) runs in the control plane, which
// listens on SFTPContainerPort (config.Config's sftp.listen default); the
// platform publishes it on the host's sftp.port, DefaultSFTPPort unless
// configured.
const (
	DefaultSFTPPort   = 2222
	SFTPContainerPort = 2222
)

// ErrInvalidConfig is returned by Render for a Config that can't be rendered.
var ErrInvalidConfig = errors.New("platform: invalid config")

// Images are the image references of the platform services.
type Images struct {
	ControlPlane string
	MariaDB      string
	Redis        string
	Traefik      string
	SocketProxy  string
	// VictoriaMetrics is the metrics storage image.
	VictoriaMetrics string
}

// DefaultImages returns the images pinned for this release: third-party
// images by digest from the embedded images.yaml, and the control plane
// image tagged with version, the host CLI's own release version.
func DefaultImages(version string) (Images, error) {
	data, err := fs.ReadFile(templates.Platform(), "images.yaml")
	if err != nil {
		return Images{}, fmt.Errorf("reading platform images: %w", err)
	}
	var list struct {
		ControlPlane string `yaml:"control-plane"`
		MariaDB      string `yaml:"mariadb"`
		Redis        string `yaml:"redis"`
		Traefik      string `yaml:"traefik"`
		SocketProxy  string `yaml:"socket-proxy"`

		VictoriaMetrics string `yaml:"victoria-metrics"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&list); err != nil {
		return Images{}, fmt.Errorf("parsing platform images: %w", err)
	}
	if version == "" {
		return Images{}, fmt.Errorf("%w: control plane version is required", ErrInvalidConfig)
	}
	return Images{
		ControlPlane: list.ControlPlane + ":" + version,
		MariaDB:      list.MariaDB,
		Redis:        list.Redis,
		Traefik:      list.Traefik,
		SocketProxy:  list.SocketProxy,

		VictoriaMetrics: list.VictoriaMetrics,
	}, nil
}

// Config is everything needed to render the platform project.
type Config struct {
	// ProjectName is the Compose project name; empty means ProjectName.
	// Tests use a unique name so they don't touch a real installation.
	ProjectName string
	// DataDir is the host's absolute data directory. It is mounted into the
	// control plane at the identical path (see compose.yaml.tmpl for why).
	DataDir string
	// Listen is the control plane listen address; empty means DefaultListen.
	Listen string
	// Images are the service images, normally from DefaultImages.
	Images Images
	// Memory limits in MiB; zero means the §4.1 default.
	ControlPlaneMemoryMB int
	MariaDBMemoryMB      int
	TraefikMemoryMB      int
	SocketProxyMemoryMB  int
	// VictoriaMetricsMemoryMB is VictoriaMetrics' memory limit; zero means
	// DefaultVictoriaMetricsMemoryMB.
	VictoriaMetricsMemoryMB int
	// MetricsRetentionDays is how long metrics are kept
	// (config.MetricsConfig.RetentionDays); zero means
	// config.DefaultMetricsRetentionDays.
	MetricsRetentionDays int
	// MariaDBRootPassword is written to secrets/mariadb_root_password.
	MariaDBRootPassword redact.Secret
	// RedisMaxMemoryMB is the object cache's maxmemory; zero means
	// DefaultRedisMaxMemoryMB.
	RedisMaxMemoryMB int
	// RedisAdminPassword is the password of RedisAdminUser, written to
	// secrets/redis_admin_password for the control plane. The object cache
	// server only gets its SHA-256 hash.
	RedisAdminPassword redact.Secret
	// SSHHostKey is the SFTP gateway's private host key (PEM), written to
	// secrets/ssh_host_key for the control plane. The host CLI generates
	// it once and reuses it (SSHHostKeySecret), so clients' known_hosts
	// entries stay valid across reinstalls and upgrades.
	SSHHostKey redact.Secret
	// SFTPPort is the host port the SFTP gateway is published on; zero
	// means DefaultSFTPPort.
	SFTPPort int
	// MasterKeyFile is the host path of the platform master key file. It is
	// given to the control plane as a Compose secret and must be outside
	// DataDir, which is backed up (§4.1). The key itself never passes
	// through the rendered project.
	MasterKeyFile string
	// SkipPreflight starts the control plane even if required host checks
	// fail. Only for development machines and CI; never for real installs.
	SkipPreflight bool
	// PreviewBaseDomain is passed to the control plane, which derives each
	// site's preview domain from it (config.Config.PreviewBaseDomain).
	// Empty leaves it unset, and site creation is refused until it is set.
	PreviewBaseDomain string
	// PublicIP is this host's public address (config.Config.PublicIP),
	// passed to the control plane for the go-live DNS check (package
	// domains). Empty leaves it unset.
	PublicIP string
	// SMTP is passed to the control plane, which renders it into every
	// site's wp-config.php (config.Config.SMTP). Its password is sealed with
	// the master key, so the rendered project never holds it in plaintext.
	// A zero value (no host) leaves outbound email off.
	SMTP config.SMTPConfig
	// Backup is passed to the control plane: where client group backup
	// repositories live (config.Config.Backup). The S3 secret access key is
	// sealed with the master key. A zero value (or the local backend)
	// renders nothing, leaving the control plane's default.
	Backup config.BackupConfig
	// DashboardDomain is where Traefik serves the control plane's dashboard
	// and API (config.Config.DashboardDomain). Empty renders no dashboard
	// route.
	DashboardDomain string
	// MetricsPublicDomain exposes VictoriaMetrics' read-only query API
	// there, behind basic auth (config.MetricsConfig.PublicDomain). Empty
	// renders no route. MetricsAccessHash is the bcrypt hash of its password
	// (NewMetricsAccess); it is required with the domain.
	MetricsPublicDomain string
	MetricsAccessHash   string
	// ACME configures how Traefik obtains certificates.
	ACME ACMEConfig
}

// ACMEConfig configures Traefik's ACME certificate resolvers
// (config.Config.ACME).
type ACMEConfig struct {
	// Email is the ACME account's contact address; empty registers the
	// account without one.
	Email string
	// CAServer is the ACME directory URL; empty means LetsEncryptCAServer.
	// The host CLI resolves acme.staging to Let's Encrypt's staging
	// directory (config.ACMEConfig.Directory).
	CAServer string
	// CACertificates is the host path of a PEM file of extra roots Traefik
	// trusts for the ACME server, e.g. Pebble's test CA. Empty trusts the
	// system roots only.
	CACertificates string
	// DNSProvider is the lego DNS provider code. When set, Traefik gets one
	// wildcard certificate for the preview base domain with DNS-01, using
	// the credentials the control plane decrypts for it (package ingress);
	// otherwise each preview domain gets its own HTTP-01 certificate.
	DNSProvider string
	// DNSCredentials are the provider's credentials sealed with the master
	// key (config.ACMEConfig.DNSCredentials), passed to the control plane.
	DNSCredentials string
	// DNSResolvers are the DNS servers (host:port) that DNS-01 propagation
	// checks use; empty means the container's resolvers.
	DNSResolvers []string
}

// withDefaults fills zero fields and validates the result.
func (c Config) withDefaults() (Config, error) {
	if c.ProjectName == "" {
		c.ProjectName = ProjectName
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.ControlPlaneMemoryMB == 0 {
		c.ControlPlaneMemoryMB = DefaultControlPlaneMemoryMB
	}
	if c.MariaDBMemoryMB == 0 {
		c.MariaDBMemoryMB = DefaultMariaDBMemoryMB
	}
	if c.RedisMaxMemoryMB == 0 {
		c.RedisMaxMemoryMB = DefaultRedisMaxMemoryMB
	}
	if c.TraefikMemoryMB == 0 {
		c.TraefikMemoryMB = DefaultTraefikMemoryMB
	}
	if c.SocketProxyMemoryMB == 0 {
		c.SocketProxyMemoryMB = DefaultSocketProxyMemoryMB
	}
	if c.VictoriaMetricsMemoryMB == 0 {
		c.VictoriaMetricsMemoryMB = DefaultVictoriaMetricsMemoryMB
	}
	if c.MetricsRetentionDays == 0 {
		c.MetricsRetentionDays = config.DefaultMetricsRetentionDays
	}
	if c.ACME.CAServer == "" {
		c.ACME.CAServer = LetsEncryptCAServer
	}
	if c.SFTPPort == 0 {
		c.SFTPPort = DefaultSFTPPort
	}

	var errs []error
	if !filepath.IsAbs(c.DataDir) || filepath.Clean(c.DataDir) != c.DataDir {
		errs = append(errs, fmt.Errorf("data dir must be a clean absolute path, got %q", c.DataDir))
	}
	if c.Images.ControlPlane == "" || c.Images.MariaDB == "" || c.Images.Redis == "" ||
		c.Images.Traefik == "" || c.Images.SocketProxy == "" || c.Images.VictoriaMetrics == "" {
		errs = append(errs, errors.New("control plane, MariaDB, Redis, Traefik, socket proxy, and VictoriaMetrics images are required"))
	}
	if _, err := listenPort(c.Listen); err != nil {
		errs = append(errs, err)
	}
	if c.ControlPlaneMemoryMB < 64 {
		errs = append(errs, fmt.Errorf("control plane memory must be at least 64 MiB, got %d", c.ControlPlaneMemoryMB))
	}
	if c.MariaDBMemoryMB < 512 {
		errs = append(errs, fmt.Errorf("MariaDB memory must be at least 512 MiB, got %d", c.MariaDBMemoryMB))
	}
	if c.RedisMaxMemoryMB < 32 {
		errs = append(errs, fmt.Errorf("the object cache's maxmemory must be at least 32 MiB, got %d", c.RedisMaxMemoryMB))
	}
	if c.TraefikMemoryMB < 32 {
		errs = append(errs, fmt.Errorf("traefik memory must be at least 32 MiB, got %d", c.TraefikMemoryMB))
	}
	if c.SocketProxyMemoryMB < 8 {
		errs = append(errs, fmt.Errorf("socket proxy memory must be at least 8 MiB, got %d", c.SocketProxyMemoryMB))
	}
	if c.VictoriaMetricsMemoryMB < 64 {
		errs = append(errs, fmt.Errorf("VictoriaMetrics memory must be at least 64 MiB, got %d", c.VictoriaMetricsMemoryMB))
	}
	if c.MetricsRetentionDays < 1 || c.MetricsRetentionDays > config.MaxMetricsRetentionDays {
		errs = append(errs, fmt.Errorf("metrics retention must be between 1 and %d days, got %d", config.MaxMetricsRetentionDays, c.MetricsRetentionDays))
	}
	if !filepath.IsAbs(c.MasterKeyFile) || filepath.Clean(c.MasterKeyFile) != c.MasterKeyFile {
		errs = append(errs, fmt.Errorf("master key file must be a clean absolute path, got %q", c.MasterKeyFile))
	} else if rel, err := filepath.Rel(c.DataDir, c.MasterKeyFile); err == nil && filepath.IsLocal(rel) {
		errs = append(errs, fmt.Errorf("master key file %s must not be inside the data directory", c.MasterKeyFile))
	}
	if c.PublicIP != "" {
		if _, err := netip.ParseAddr(c.PublicIP); err != nil {
			errs = append(errs, fmt.Errorf("public IP must be an IP address, got %q", c.PublicIP))
		}
	}
	if c.PreviewBaseDomain != "" && !config.ValidDomain(c.PreviewBaseDomain) {
		errs = append(errs, fmt.Errorf("preview base domain must be a domain name like preview.example.com, got %q", c.PreviewBaseDomain))
	}
	if err := c.SMTP.Validate(); err != nil {
		errs = append(errs, err)
	}
	if c.Backup.Backend != "" {
		if err := c.Backup.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.DashboardDomain != "" && !config.ValidDomain(c.DashboardDomain) {
		errs = append(errs, fmt.Errorf("dashboard domain must be a domain name like fleet.example.com, got %q", c.DashboardDomain))
	}
	if c.MetricsPublicDomain != "" {
		switch {
		case !config.ValidDomain(c.MetricsPublicDomain):
			errs = append(errs, fmt.Errorf("metrics public domain must be a domain name like metrics.example.com, got %q", c.MetricsPublicDomain))
		case c.MetricsPublicDomain == c.DashboardDomain:
			errs = append(errs, errors.New("metrics public domain must differ from the dashboard domain"))
		}
		if _, err := bcrypt.Cost([]byte(c.MetricsAccessHash)); err != nil {
			errs = append(errs, errors.New("a metrics public domain needs a bcrypt password hash (NewMetricsAccess)"))
		}
	}
	acme := config.ACMEConfig{
		Email:          c.ACME.Email,
		CAServer:       c.ACME.CAServer,
		CACertificates: c.ACME.CACertificates,
		DNSProvider:    c.ACME.DNSProvider,
		DNSCredentials: c.ACME.DNSCredentials,
		DNSResolvers:   c.ACME.DNSResolvers,
	}
	if err := acme.Validate(); err != nil {
		errs = append(errs, err)
	}
	if c.ACME.CACertificates != "" && filepath.Clean(c.ACME.CACertificates) != c.ACME.CACertificates {
		errs = append(errs, fmt.Errorf("ACME CA certificates file must be a clean absolute path, got %q", c.ACME.CACertificates))
	}
	if c.ACME.DNSProvider != "" && c.PreviewBaseDomain == "" {
		errs = append(errs, errors.New("a DNS provider needs the preview base domain, which it gets the wildcard certificate for"))
	}
	if c.MariaDBRootPassword.Reveal() == "" {
		errs = append(errs, errors.New("MariaDB root password is required"))
	}
	if strings.ContainsAny(c.MariaDBRootPassword.Reveal(), "\r\n") {
		// The MariaDB entrypoint reads the secret file as a single line.
		errs = append(errs, errors.New("MariaDB root password must not contain line breaks"))
	}
	if c.RedisAdminPassword.Reveal() == "" {
		errs = append(errs, errors.New("the object cache admin password is required"))
	}
	if strings.ContainsAny(c.RedisAdminPassword.Reveal(), "\r\n") {
		// The control plane reads the secret file as a single line.
		errs = append(errs, errors.New("the object cache admin password must not contain line breaks"))
	}
	if !strings.Contains(c.SSHHostKey.Reveal(), "PRIVATE KEY-----") {
		errs = append(errs, errors.New("the SFTP gateway host key is required, as a PEM private key"))
	}
	if c.SFTPPort < 1 || c.SFTPPort > 65535 || c.SFTPPort == 80 || c.SFTPPort == 443 {
		errs = append(errs, fmt.Errorf("the SFTP port must be a free TCP port other than 80 and 443, got %d", c.SFTPPort))
	}
	if err := errors.Join(errs...); err != nil {
		return c, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return c, nil
}

// templateData is what the templates see. It deliberately has no plaintext
// secrets: credentials go only into files under secrets/, or are sealed with
// the master key (SMTP.Password, BackupS3.SecretAccessKey). The object cache admin password's SHA-256
// hash is not a secret: it can't be used to authenticate, and the password
// is 130 random bits, so the hash can't be reversed.
type templateData struct {
	ProjectName             string
	DataDir                 string
	Listen                  string
	Images                  Images
	ControlPlaneMemoryMB    int
	MariaDBMemoryMB         int
	MariaDBBufferPoolMB     int
	RedisMaxMemoryMB        int
	RedisMemoryMB           int
	RedisDatabases          int
	RedisAdminUser          string
	RedisAdminPasswordHash  string
	SFTPPort                int
	SFTPContainerPort       int
	MasterKeyFile           string
	MasterKeySecretPath     string
	SkipPreflight           bool
	PreviewBaseDomain       string
	PublicIP                string
	SMTP                    *config.SMTPConfig     // nil when outbound email is off
	BackupS3                *config.BackupS3Config // nil for local backups
	ListenPort              int
	TraefikMemoryMB         int
	TraefikGoMemLimitMB     int
	SocketProxyMemoryMB     int
	VictoriaMetricsMemoryMB int
	VictoriaMetricsPort     int
	VictoriaMetricsService  string
	MetricsRetentionDays    int
	DashboardDomain         string
	ACME                    ACMEConfig

	// Traefik names shared with site routes (package ingress).
	PlatformLabel      string
	Constraint         string
	TraefikService     string
	EntryPointHTTP     string
	EntryPointHTTPS    string
	EntryPointInternal string
	InternalPort       int
	CertResolverHTTP   string

	// File provider and maintenance page (package ingress).
	DynamicConfigDir        string // in the Traefik container
	DynamicConfigSource     string // TraefikDynamicDir
	MaintenanceService      string
	MaintenanceHost         string
	MaintenancePort         int
	MaintenanceMemoryMB     int
	MaintenanceGoMemLimitMB int

	// DNS-01 (package ingress).
	CertResolverPreview   string
	PreviewWildcardRouter string
	DNSCredentialsDir     string
	DNSCredentialsReady   string
}

// output is one rendered file, relative to the project directory.
type output struct {
	path     string
	template string // empty for secrets
	secret   redact.Secret
}

// MasterKeySecretPath is where the control plane container sees the master
// key file.
const MasterKeySecretPath = "/run/secrets/master_key" //nolint:gosec // G101: a path, not a credential

// Files rendered by Render, relative to the project directory.
const (
	ComposeFile               = "compose.yaml"
	MariaDBConfigFile         = "mariadb/pressfleet.cnf"
	MariaDBRootPasswordSecret = "secrets/mariadb_root_password"
	RedisConfigFile           = "redis/valkey.conf"
	RedisAdminPasswordSecret  = "secrets/redis_admin_password"
	SSHHostKeySecret          = "secrets/ssh_host_key"
	TraefikConfigFile         = "traefik/traefik.yaml"
	// TraefikDynamicDir is the directory of Traefik's file provider. Render
	// writes TraefikDynamicConfigFile into it; the control plane adds and
	// removes the maintenance routes of sites (package ingress), so it
	// must be readable by Traefik, which runs without CAP_DAC_OVERRIDE,
	// and holds nothing secret but the bcrypt hash of the public metrics
	// credential (TraefikMetricsConfigFile), which is no use without the
	// password.
	TraefikDynamicDir        = "traefik/dynamic"
	TraefikDynamicConfigFile = TraefikDynamicDir + "/platform.yaml"
	// Rendered only with a DNS provider.
	TraefikEntrypointFile = "traefik/entrypoint.sh"
)

// legacyTraefikDynamicConfigFile is where the file provider's configuration
// was before it moved into TraefikDynamicDir. Render removes it.
const legacyTraefikDynamicConfigFile = "traefik/dynamic.yaml"

// traefikDynamicConfigDir is where Traefik sees TraefikDynamicDir.
const traefikDynamicConfigDir = "/etc/traefik/dynamic"

// Render writes the platform Compose project into dir, normally
// config.Layout.PlatformDir(). It is safe to run again: every file is
// replaced atomically. Secret files get mode 0600; other files 0644 because
// the MariaDB and object cache servers, which don't run as the owner, must
// be able to read their config files.
func Render(dir string, cfg Config) error {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return err
	}
	port, _ := listenPort(cfg.Listen) // validated by withDefaults
	data := templateData{
		ProjectName:          cfg.ProjectName,
		DataDir:              cfg.DataDir,
		Listen:               cfg.Listen,
		ListenPort:           port,
		Images:               cfg.Images,
		ControlPlaneMemoryMB: cfg.ControlPlaneMemoryMB,
		MariaDBMemoryMB:      cfg.MariaDBMemoryMB,
		MariaDBBufferPoolMB:  bufferPoolMB(cfg.MariaDBMemoryMB),
		RedisMaxMemoryMB:     cfg.RedisMaxMemoryMB,
		RedisMemoryMB:        cfg.RedisMaxMemoryMB + RedisOverheadMB,
		RedisDatabases:       RedisDatabases,
		RedisAdminUser:       RedisAdminUser,
		// ACL passwords are hashed over the exact bytes a client sends.
		RedisAdminPasswordHash:  sha256Hex(cfg.RedisAdminPassword),
		SFTPPort:                cfg.SFTPPort,
		SFTPContainerPort:       SFTPContainerPort,
		MasterKeyFile:           cfg.MasterKeyFile,
		MasterKeySecretPath:     MasterKeySecretPath,
		SkipPreflight:           cfg.SkipPreflight,
		PreviewBaseDomain:       cfg.PreviewBaseDomain,
		PublicIP:                cfg.PublicIP,
		TraefikMemoryMB:         cfg.TraefikMemoryMB,
		TraefikGoMemLimitMB:     cfg.TraefikMemoryMB * 3 / 4,
		SocketProxyMemoryMB:     cfg.SocketProxyMemoryMB,
		VictoriaMetricsMemoryMB: cfg.VictoriaMetricsMemoryMB,
		VictoriaMetricsPort:     VictoriaMetricsPort,
		VictoriaMetricsService:  VictoriaMetricsService,
		MetricsRetentionDays:    cfg.MetricsRetentionDays,
		DashboardDomain:         cfg.DashboardDomain,
		ACME:                    cfg.ACME,
		PlatformLabel:           ingress.PlatformLabel,
		Constraint:              fmt.Sprintf("Label(`%s`, `%s`)", ingress.PlatformLabel, cfg.ProjectName),
		TraefikService:          ingress.TraefikService,
		EntryPointHTTP:          ingress.EntryPointHTTP,
		EntryPointHTTPS:         ingress.EntryPointHTTPS,
		EntryPointInternal:      ingress.EntryPointInternal,
		InternalPort:            ingress.InternalPort,
		CertResolverHTTP:        ingress.CertResolverHTTP,

		DynamicConfigDir:        traefikDynamicConfigDir,
		DynamicConfigSource:     TraefikDynamicDir,
		MaintenanceService:      ingress.MaintenanceService,
		MaintenanceHost:         ingress.MaintenanceHost,
		MaintenancePort:         ingress.MaintenancePort,
		MaintenanceMemoryMB:     MaintenanceMemoryMB,
		MaintenanceGoMemLimitMB: MaintenanceMemoryMB * 3 / 4,

		CertResolverPreview:   ingress.CertResolverPreview,
		PreviewWildcardRouter: ingress.PreviewWildcardRouter,
		DNSCredentialsDir:     ingress.DNSCredentialsDir,
		DNSCredentialsReady:   ingress.DNSCredentialsReady,
	}
	if cfg.SMTP.Enabled() {
		data.SMTP = &cfg.SMTP
	}
	if cfg.Backup.Backend == config.BackupBackendS3 {
		data.BackupS3 = &cfg.Backup.S3
	}
	tmpl, err := parseTemplates()
	if err != nil {
		return err
	}

	outputs := []output{
		{path: ComposeFile, template: "compose.yaml.tmpl"},
		{path: MariaDBConfigFile, template: "mariadb.cnf.tmpl"},
		{path: TraefikConfigFile, template: "traefik.yaml.tmpl"},
		{path: TraefikDynamicConfigFile, template: "traefik-dynamic.yaml.tmpl"},
		{path: MariaDBRootPasswordSecret, secret: cfg.MariaDBRootPassword},
		{path: RedisConfigFile, template: "valkey.conf.tmpl"},
		{path: RedisAdminPasswordSecret, secret: cfg.RedisAdminPassword},
		{path: SSHHostKeySecret, secret: cfg.SSHHostKey},
	}
	dnsFiles := []output{
		{path: TraefikEntrypointFile, template: "traefik-entrypoint.sh.tmpl"},
	}
	if err := os.Remove(filepath.Join(dir, legacyTraefikDynamicConfigFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", legacyTraefikDynamicConfigFile, err)
	}
	if cfg.MetricsPublicDomain != "" {
		if err := writeMetricsAccess(dir, cfg.MetricsPublicDomain, cfg.MetricsAccessHash); err != nil {
			return err
		}
	} else if err := removeMetricsAccess(dir); err != nil {
		return err
	}
	if cfg.ACME.DNSProvider != "" {
		outputs = append(outputs, dnsFiles...)
	} else {
		// Left over from rendering with a DNS provider before.
		for _, o := range dnsFiles {
			if err := os.Remove(filepath.Join(dir, o.path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("removing %s: %w", o.path, err)
			}
		}
	}
	for _, o := range outputs {
		var content []byte
		mode := os.FileMode(0o644)
		if o.template != "" {
			var buf bytes.Buffer
			if err := tmpl.ExecuteTemplate(&buf, o.template, data); err != nil {
				return fmt.Errorf("rendering %s: %w", o.path, err)
			}
			content = buf.Bytes()
		} else {
			content = []byte(o.secret.Reveal())
			mode = 0o600
		}
		if err := writeFile(filepath.Join(dir, o.path), content, mode); err != nil {
			return err
		}
	}
	// writeFile creates directories 0700; Traefik must be able to list this
	// one (see TraefikDynamicDir).
	if err := os.Chmod(filepath.Join(dir, TraefikDynamicDir), 0o755); err != nil { //nolint:gosec // G302: Traefik must list it, and it holds no password
		return fmt.Errorf("making %s readable: %w", TraefikDynamicDir, err)
	}
	return nil
}

// MariaDBContainer is the name of the MariaDB container of the platform
// project called projectName (normally ProjectName). The Compose provisioner
// connects this container to each site network (§4.1 Networking). Compose v2
// names containers <project>-<service>-<index>, and MariaDB has one replica.
func MariaDBContainer(projectName string) string {
	return projectName + "-mariadb-1"
}

// BackupNetwork is the name of the backup network of the platform project
// called projectName: an internal network (no route out of the host) that
// the control plane and every backup and restore task join, so they reach a
// self-hosted S3-compatible service attached to it by the same name
// (design doc §5.3). Compose names it <project>_<network>.
func BackupNetwork(projectName string) string {
	return projectName + "_backup"
}

// RedisContainer is the name of the object cache container of the platform
// project called projectName. Like MariaDB, the Compose provisioner connects
// it to each site network.
func RedisContainer(projectName string) string {
	return projectName + "-redis-1"
}

// TraefikContainer is the name of the Traefik container of the platform
// project called projectName. The Compose provisioner connects it to each
// site network, like MariaDB.
func TraefikContainer(projectName string) string {
	return projectName + "-" + ingress.TraefikService + "-1"
}

// Project returns the Compose project for a rendered platform directory.
func Project(dir string) compose.Project {
	return compose.Project{Name: ProjectName, Dir: dir}
}

// bufferPoolMB gives InnoDB half of MariaDB's memory, rounded down to the
// 128 MiB chunk size that MariaDB would otherwise round to itself.
func bufferPoolMB(memoryMB int) int {
	return max(128, memoryMB/2/128*128)
}

// sha256Hex returns the hex SHA-256 of s, the form of an ACL "#<hash>"
// password rule.
func sha256Hex(s redact.Secret) string {
	sum := sha256.Sum256([]byte(s.Reveal()))
	return hex.EncodeToString(sum[:])
}

// quote renders s as a double-quoted YAML scalar in compose.yaml. JSON
// string syntax is valid YAML; '$' is doubled so Compose doesn't treat it as
// interpolation.
func quote(s string) (string, error) {
	b, err := yamlQuote(s)
	return strings.ReplaceAll(b, "$", "$$"), err
}

// yamlQuote renders s as a double-quoted YAML scalar in files Compose
// doesn't interpolate, such as Traefik's configuration.
func yamlQuote(s string) (string, error) {
	b, err := json.Marshal(s)
	return string(b), err
}

// backquote wraps s in backquotes, the string syntax of Traefik rules.
// Domains are validated, so they never contain one themselves.
func backquote(s string) string {
	return "`" + s + "`"
}

// listenPort returns the TCP port of a listen address like ":8080", which
// Traefik forwards dashboard requests to.
func listenPort(listen string) (int, error) {
	if _, port, err := net.SplitHostPort(listen); err == nil {
		if n, err := strconv.Atoi(port); err == nil && n > 0 && n <= 65535 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("listen address must be host:port or :port, got %q", listen)
}

// writeFile atomically replaces path with content. Writing to a temporary
// file first also means a pre-existing file with looser permissions never
// briefly holds a secret.
func writeFile(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after a successful rename

	// CreateTemp uses 0600; Chmod sets the final mode regardless of umask.
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
