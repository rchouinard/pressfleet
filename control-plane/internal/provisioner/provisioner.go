// Package provisioner defines the orchestrator-agnostic contract for managing
// site runtime resources: containers, volumes, and routing (design doc §4.4).
//
// All orchestration goes through [Provisioner]. Code outside a driver package
// must never talk to Docker, Compose, or Kubernetes directly (§4.2); this is
// enforced by the archtest package.
package provisioner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// ErrSiteNotFound is returned when an operation targets a site the driver
// has no runtime resources for.
var ErrSiteNotFound = errors.New("provisioner: site not found")

// ErrSiteNotRunning is returned by [Provisioner.ExecWPCLI] when the site
// exists but isn't running, so callers such as the WP-Cron job can tell a
// stopped site from a failed command without importing a driver.
var ErrSiteNotRunning = errors.New("provisioner: site is not running")

// Default per-site resource settings (§4.1 Capacity Target).
const (
	DefaultCPU                 = "1.0"
	DefaultMemoryReservationMB = 128
	DefaultMemoryLimitMB       = 512
)

// Minimum per-site resources. Below them a site's containers get too little
// memory to run WordPress, or too little CPU to start in time. Drivers
// refuse smaller limits, and the API rejects them before creating anything.
const (
	MinCPUMillis           = 250 // thousandths of a CPU
	MinMemoryReservationMB = 32
	MinMemoryLimitMB       = 256
)

// Site UIDs (§6: each site runs as its own non-root UID). Every site gets a
// unique UID from this range, used as both UID and GID of its containers,
// and never reused after the site is deleted (store.AllocateSiteUID). The
// range starts far above the IDs hosts give system accounts (below 1000) and
// their first local users (from 1000), and ends below 65534 (nobody) and the
// subordinate ID ranges user namespaces use (from 100000). Its 40,000 UIDs
// are far more sites than one installation will create, deleted ones
// included.
const (
	MinSiteUID = 20000
	MaxSiteUID = 59999
)

// RepositoryGID is the GID that owns local backup repositories
// (Task.RepositoryDir). The sites of a client group share one repository
// but each runs as its own UID, so site tasks that use a repository also run
// with this supplementary group; on Kubernetes it maps to the Pod's
// supplementalGroups. Repository tasks (RepositoryTaskRunner) run as it,
// as both user and group. It sits just above the site UID range, below 65534
// (nogroup) and the subordinate ID ranges user namespaces use.
//
// A repository is owned by root:RepositoryGID, with setgid directories
// (mode 2770), so everything created in it keeps the group, and a config
// file that is group-readable, which makes restic (0.14 and later) create
// new files group-readable and directories group-writable. Restic never
// modifies a file in place, so group-readable files in group-writable
// directories are enough for any member to back up, forget, and prune.
const RepositoryGID = 60000

// SiteConfig describes the desired runtime state of a site. It is expressed
// as data and must stay free of orchestrator-specific fields (§4.2).
type SiteConfig struct {
	ID            string
	ClientGroupID string
	PreviewDomain string
	PrimaryDomain string // empty until go-live
	Aliases       []string
	PHPVersion    string
	WPCoreVersion string // platform-managed; selects the site image together with PHPVersion
	EnableCache   bool
	Resources     ResourceLimits
	DBSecretRef   string // reference to stored credentials, never the plaintext password
	// UID is the site's UID and GID, in [MinSiteUID, MaxSiteUID]. Every site
	// component runs as it (§5.6, §6); it maps to runAsUser/runAsGroup on
	// Kubernetes.
	UID int
	// WPSaltsSecretRef references the site's WordPress keys and salts, stored
	// in the format of wpconfig.MarshalSalts.
	WPSaltsSecretRef string
	// TablePrefix is the WordPress table prefix of the site's database,
	// which wp-config.php names; empty means WordPress's default, wp_. An
	// imported site keeps its source's prefix (§5.7).
	TablePrefix string
	// RedisSecretRef references the site's object cache credentials, stored
	// in the format of objectcache.MarshalCredentials. The driver renders
	// them into the Redis Object Cache plugin's settings; empty leaves the
	// site without them.
	RedisSecretRef string
	// PreviewAuth, if set, protects the preview domain, and only the
	// preview domain, with HTTP basic auth (§5.1). It carries the password's
	// bcrypt hash rather than a reference to the password: the hash is all
	// the edge router needs, and it isn't secret enough to need resolving
	// by the driver.
	PreviewAuth *BasicAuth
	// Maintenance sends the site's visitors to the platform's maintenance
	// page instead of the site (§5.4). [Provisioner.SetMaintenance] switches
	// it on its own, quickly; this field is the same state as part of the
	// desired configuration, so that CreateSite and UpdateSite (including
	// the startup re-render) keep it. Callers of SetMaintenance must pass
	// the same value here afterwards, or the next update undoes it.
	Maintenance bool
	// PinnedImages pins components to exact builds of the images that
	// PHPVersion and WPCoreVersion select, by references
	// ImagePinner.PinImage returned for them (§5.5). A component without
	// one runs its image's tag as it is.
	PinnedImages map[Component]string
	// Environment is the site's environment type, rendered as
	// WP_ENVIRONMENT_TYPE (§5.11). Empty means production. A change reaches
	// the running site through UpdateSite like the security set (§6.1).
	Environment Environment
	// DisableMail turns off the site's outbound email: the platform
	// must-use plugin suppresses wp_mail(). It is independent of
	// Environment, though staging sites default to it (§5.11).
	DisableMail bool
}

// Environment is a site's environment type.
type Environment string

// Site environments. The empty Environment is production.
const (
	EnvironmentProduction Environment = "production"
	EnvironmentStaging    Environment = "staging"
)

// Validate reports whether e is empty or a known environment.
func (e Environment) Validate() error {
	switch e {
	case "", EnvironmentProduction, EnvironmentStaging:
		return nil
	}
	return fmt.Errorf("environment must be %q or %q", EnvironmentProduction, EnvironmentStaging)
}

// pinnedImage matches the references ImagePinner.PinImage returns: a
// repository with a digest, or an image ID.
var pinnedImage = regexp.MustCompile(`^([a-z0-9][a-z0-9._:/-]*@)?sha256:[0-9a-f]{64}$`)

// BasicAuth is one HTTP basic auth credential.
type BasicAuth struct {
	Username string
	// PasswordHash is a bcrypt hash ($2a$, $2b$, or $2y$) of the password.
	PasswordHash string
}

// Validate checks that the credential can be used: a username of printable
// ASCII without spaces or colons (the basic auth separator), and a bcrypt
// hash.
func (a *BasicAuth) Validate() error {
	var errs []error
	if err := ValidateBasicAuthUsername(a.Username); err != nil {
		errs = append(errs, fmt.Errorf("preview auth: %w", err))
	}
	if !bcryptHash.MatchString(a.PasswordHash) {
		errs = append(errs, errors.New("preview auth: password hash must be a bcrypt hash"))
	}
	return errors.Join(errs...)
}

// ValidateBasicAuthUsername checks a basic auth username: 1 to
// MaxBasicAuthUsername characters of printable ASCII other than spaces and
// colons (the separator of the credentials).
func ValidateBasicAuthUsername(u string) error {
	if u == "" || len(u) > MaxBasicAuthUsername {
		return fmt.Errorf("username must be 1 to %d characters", MaxBasicAuthUsername)
	}
	for _, r := range u {
		if r <= ' ' || r > '~' || r == ':' {
			return errors.New("username may only contain printable ASCII other than spaces and colons")
		}
	}
	return nil
}

// MaxBasicAuthUsername is the longest basic auth username accepted.
const MaxBasicAuthUsername = 64

// bcryptHash matches the modular crypt format of bcrypt: version, two-digit
// cost, and 53 characters of salt and hash.
var bcryptHash = regexp.MustCompile(`^\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}$`)

// tablePrefix is what WordPress accepts as a table prefix
// (wpdb::set_prefix).
var tablePrefix = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// Validate reports every problem with the config, or nil if it is usable.
func (c *SiteConfig) Validate() error {
	var errs []error
	required := []struct{ name, value string }{
		{"ID", c.ID},
		{"ClientGroupID", c.ClientGroupID},
		{"PreviewDomain", c.PreviewDomain},
		{"PHPVersion", c.PHPVersion},
		{"WPCoreVersion", c.WPCoreVersion},
		{"DBSecretRef", c.DBSecretRef},
		{"WPSaltsSecretRef", c.WPSaltsSecretRef},
	}
	for _, f := range required {
		if f.value == "" {
			errs = append(errs, fmt.Errorf("%s is required", f.name))
		}
	}
	if c.UID < MinSiteUID || c.UID > MaxSiteUID {
		errs = append(errs, fmt.Errorf("UID %d is outside the site UID range %d-%d", c.UID, MinSiteUID, MaxSiteUID))
	}
	if err := c.Resources.Validate(); err != nil {
		errs = append(errs, err)
	}
	if c.TablePrefix != "" && !tablePrefix.MatchString(c.TablePrefix) {
		errs = append(errs, errors.New("TablePrefix may only contain letters, digits, and underscores"))
	}
	if err := c.Environment.Validate(); err != nil {
		errs = append(errs, err)
	}
	if c.PreviewAuth != nil {
		if err := c.PreviewAuth.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	for comp, ref := range c.PinnedImages {
		if comp != ComponentPHP && comp != ComponentNginx {
			errs = append(errs, fmt.Errorf("pinned image of unknown component %q", comp))
		} else if !pinnedImage.MatchString(ref) {
			errs = append(errs, fmt.Errorf("pinned %s image %q is not a digest or image ID reference", comp, ref))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("invalid site config %q: %w", c.ID, err)
	}
	return nil
}

// ResourceLimits apply to the site as a whole; the driver splits them between
// the site's containers.
type ResourceLimits struct {
	CPU                 string // e.g. "1.0"
	MemoryReservationMB int    // guaranteed baseline; default 128
	MemoryLimitMB       int    // hard limit allowing bursts; default 512
}

// DefaultResourceLimits returns the platform default limits.
func DefaultResourceLimits() ResourceLimits {
	return ResourceLimits{
		CPU:                 DefaultCPU,
		MemoryReservationMB: DefaultMemoryReservationMB,
		MemoryLimitMB:       DefaultMemoryLimitMB,
	}
}

// Validate checks that the limits are internally consistent.
func (r ResourceLimits) Validate() error {
	var errs []error
	if r.CPU == "" {
		errs = append(errs, errors.New("resources: CPU is required"))
	}
	if r.MemoryReservationMB <= 0 {
		errs = append(errs, errors.New("resources: MemoryReservationMB must be positive"))
	}
	if r.MemoryLimitMB < r.MemoryReservationMB {
		errs = append(errs, errors.New("resources: MemoryLimitMB must be at least MemoryReservationMB"))
	}
	return errors.Join(errs...)
}

// SitePhase is the coarse runtime state of a site.
type SitePhase string

// Site phases.
const (
	SitePending SitePhase = "pending"
	SiteRunning SitePhase = "running"
	SiteStopped SitePhase = "stopped"
	SiteFailed  SitePhase = "failed"
)

// SiteStatus is the observed runtime state of a site.
type SiteStatus struct {
	ID      string
	Phase   SitePhase
	URL     string
	Message string
}

// ExecError is returned by [Provisioner.ExecWPCLI], [TaskRunner.RunTask],
// and [SessionRunner.RunSession] when the command ran but exited non-zero.
type ExecError struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

func (e *ExecError) Error() string {
	return fmt.Sprintf("command exited with status %d: %s", e.ExitCode, e.Stderr)
}

// Provisioner manages site runtime resources (containers, volumes, routing).
//
// Jobs retry failed steps (§5), so every mutating method must be idempotent:
// calling it again with the same arguments converges on the same state.
type Provisioner interface {
	// CreateSite creates the site's runtime resources and starts them. If the
	// site already exists it converges it to cfg.
	CreateSite(ctx context.Context, cfg *SiteConfig) (*SiteStatus, error)
	// UpdateSite converges an existing site to cfg, recreating containers
	// when images or configuration change. Returns ErrSiteNotFound if the
	// site does not exist.
	UpdateSite(ctx context.Context, cfg *SiteConfig) (*SiteStatus, error)
	// GetSite returns the observed state, or ErrSiteNotFound.
	GetSite(ctx context.Context, siteID string) (*SiteStatus, error)
	// DeleteSite removes all of the site's containers, networks, and volumes.
	// Deleting a site that does not exist succeeds.
	DeleteSite(ctx context.Context, siteID string) error
	// StartSite starts a stopped site. Returns ErrSiteNotFound if absent.
	StartSite(ctx context.Context, siteID string) error
	// StopSite stops a running site without removing data. Returns
	// ErrSiteNotFound if absent.
	StopSite(ctx context.Context, siteID string) error
	// SetMaintenance enables or disables the platform maintenance page for
	// all of the site's domains (§5.4): visitors get 503 with Retry-After.
	// It takes effect within seconds, touches nothing else of the site, and
	// works whether the site's containers are running, stopped, or being
	// recreated; WP-CLI (ExecWPCLI) keeps working. It is the primitive that
	// restores and rollouts build on. SiteConfig.Maintenance must agree, or
	// the next UpdateSite reverts it. Returns ErrSiteNotFound if the site
	// was never rendered.
	SetMaintenance(ctx context.Context, siteID string, enabled bool) error
	// ExecWPCLI runs WP-CLI in the site's PHP container as the site's user
	// and returns its stdout. args are passed as argv, never through a shell.
	// A non-zero exit yields an *ExecError; a site that exists but isn't
	// running yields ErrSiteNotRunning.
	ExecWPCLI(ctx context.Context, siteID string, args []string) (string, error)
	// PurgeCache empties the site's page cache, which lives in the site's
	// own runtime (the FastCGI cache nginx keeps in the site's cache
	// volume), whether or not the site is running. Returns ErrSiteNotFound
	// if the site does not exist.
	//
	// The object cache isn't the driver's: it is a platform service, which
	// objectcache.Manager.FlushCache empties. Package sites purges both
	// wherever the design calls for a cache purge (§5.2, §5.4, the cache
	// purge API).
	PurgeCache(ctx context.Context, siteID string) error
	// ReloadCode makes the site's running PHP workers pick up the code now
	// in wp-content, gracefully: requests in flight finish, and the site's
	// containers aren't restarted. Without it, PHP's opcode cache keeps
	// running cached copies of changed files for up to a minute
	// (images/site-php), so a deployment would run old and new files side
	// by side. A site that isn't running has nothing cached and succeeds.
	// Returns ErrSiteNotFound if the site does not exist.
	ReloadCode(ctx context.Context, siteID string) error
}
