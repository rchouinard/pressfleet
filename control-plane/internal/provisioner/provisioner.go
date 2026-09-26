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
)

// ErrSiteNotFound is returned when an operation targets a site the driver
// has no runtime resources for.
var ErrSiteNotFound = errors.New("provisioner: site not found")

// Default per-site resource settings (§4.1 Capacity Target).
const (
	DefaultCPU                 = "1.0"
	DefaultMemoryReservationMB = 128
	DefaultMemoryLimitMB       = 512
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
	// RedisSecretRef references the site's Redis ACL credentials. Empty until
	// the shared Redis service exists (M2).
	RedisSecretRef string
}

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

// ExecError is returned by [Provisioner.ExecWPCLI] and [TaskRunner.RunTask]
// when the command ran but exited non-zero.
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
	// SetMaintenance enables or disables the platform maintenance page.
	SetMaintenance(ctx context.Context, siteID string, enabled bool) error
	// ExecWPCLI runs WP-CLI in the site's PHP container as the site's user
	// and returns its stdout. args are passed as argv, never through a shell.
	// A non-zero exit yields an *ExecError.
	ExecWPCLI(ctx context.Context, siteID string, args []string) (string, error)
	// PurgeCache purges the site's page cache and object cache.
	PurgeCache(ctx context.Context, siteID string) error
}
