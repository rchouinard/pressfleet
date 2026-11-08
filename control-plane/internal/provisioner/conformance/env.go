package conformance

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Env describes the environment a driver runs in and how to build it. The
// driver's test fills it in and passes it to [Run].
type Env struct {
	// Images are the site images by PHP version. It has to hold PHPVersion
	// and AltPHPVersion. The images have to be usable by the driver as they
	// are, so a driver that pulls them can name remote ones; the suite also
	// runs its tasks and sessions with the PHP image, which has a shell and
	// the PHP CLI.
	Images map[string]catalog.Images
	// PHPVersion is the version sites start with, and AltPHPVersion another
	// one, to which the lifecycle test switches a site. WPCoreVersion is
	// the WordPress version both images were built for.
	PHPVersion, AltPHPVersion, WPCoreVersion string
	// Database are credentials that sites can log in with: the MariaDB the
	// driver was set up to point sites at has a database and user like
	// this, reachable from every site. The suite gives each site a table
	// prefix of its own, so all of its sites can share the database.
	Database sitedb.Credentials
	// ObjectCache, if set, are credentials that sites can log in to the
	// Valkey the driver points sites at with; the suite sets KeyPrefix.
	// Without it the object cache test is skipped.
	ObjectCache *objectcache.Credentials
	// New returns the driver for one test, set up to use the given data
	// directory, secrets, and images. It registers whatever has to be torn
	// down with t.Cleanup. The suite deletes the sites it creates before
	// the cleanups run (cleanups run last in, first out), so a cleanup can
	// assume that its test's sites are gone.
	New func(t *testing.T, s Setup) Instance
}

// Setup is what the suite hands the driver.
type Setup struct {
	// Layout is a fresh data directory, with its directories created. It
	// stands for the control plane's data directory: Task.TransferDir and
	// Task.RepositoryDir are directories under it.
	Layout config.Layout
	// Secrets holds the credentials of the suite's sites. The driver
	// resolves SiteConfig's DBSecretRef, WPSaltsSecretRef, and
	// RedisSecretRef in it.
	Secrets *Secrets
	// Images resolves the PHP and WordPress versions of a SiteConfig.
	Images ImageSet
}

// Instance is a driver and the ways of looking at what it runs that the
// driver contract doesn't have. They are how the suite checks what a driver
// did, with a command in a site container rather than with the driver's own
// word for it.
type Instance struct {
	// Driver is the driver under test.
	Driver provisioner.Driver
	// Exec runs a command in a component of a running site, as the
	// component's user, and returns its standard output. A failing command
	// is an error. It bypasses the driver (docker exec, kubectl exec).
	Exec func(ctx context.Context, siteID string, c provisioner.Component, argv ...string) (string, error)
	// Get sends an HTTP GET to the site's web server (the nginx component)
	// from inside the site's runtime, with host as the Host header, as the
	// edge router does: over HTTPS as far as the site can tell, without DNS
	// or certificates.
	Get func(ctx context.Context, siteID, host, path string) (*HTTPResponse, error)
	// Address, if set, returns a host:port at which a peer in the same
	// runtime (a container or Pod elsewhere) would reach the site's web
	// server if nothing stopped it. The isolation checks try it from other
	// sites and from tasks; without it they are skipped.
	Address func(ctx context.Context, siteID string) (string, error)
	// EdgeOnlyIngress says that a site's web server takes connections only
	// from the edge router, so its own tasks can't reach it at Address
	// either, as on Kubernetes, where the site's NetworkPolicy admits only
	// Traefik. Without it, a site's task must reach its own site.
	EdgeOnlyIngress bool
	// OnePod says that a site's components run in one unit that is
	// recreated as a whole, as a Kubernetes pod is: a change to
	// wp-config.php recreates nginx with php. Without it, nginx must keep
	// running.
	OnePod bool
	// Leftovers, if set, lists the runtime objects the driver has for an
	// owner, which is a site ID or a client group ID. With deleted false it
	// lists only objects that must not outlive the task or session that
	// made them (containers, Pods, secrets); with deleted true, after
	// DeleteSite, everything that belongs to the site. A non-empty list
	// fails the test. Without it these checks are skipped.
	Leftovers func(ctx context.Context, owner string, deleted bool) ([]string, error)
	// InMaintenance, if set, reports whether the edge sends the site's
	// visitors to the maintenance page. Without it the suite only checks
	// that the calls succeed.
	InMaintenance func(ctx context.Context, siteID string) (bool, error)
}

// HTTPResponse is what a site's web server answered.
type HTTPResponse struct {
	StatusCode int
	Header     http.Header
	Body       string
}

// ImageSet is a catalog of images by PHP version, for drivers that resolve
// images through an interface like compose.ImageResolver.
type ImageSet map[string]catalog.Images

// Images returns the images of the PHP version, or catalog.ErrInvalidVersion.
func (m ImageSet) Images(php, _ string) (catalog.Images, error) {
	img, ok := m[php]
	if !ok {
		return catalog.Images{}, fmt.Errorf("%w: PHP version %q", catalog.ErrInvalidVersion, php)
	}
	return img, nil
}

// Secrets is an in-memory store of site secrets. Get has the signature
// drivers use to read them.
type Secrets struct {
	mu sync.Mutex
	m  map[secrets.Ref]redact.Secret
}

// Get returns the secret with the reference, or store.ErrNotFound.
func (s *Secrets) Get(_ context.Context, ref secrets.Ref) (redact.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[ref]
	if !ok {
		return "", fmt.Errorf("get secret %s: %w", ref, store.ErrNotFound)
	}
	return v, nil
}

func (s *Secrets) put(ref string, v redact.Secret) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[secrets.Ref]redact.Secret{}
	}
	s.m[secrets.Ref(ref)] = v
}
