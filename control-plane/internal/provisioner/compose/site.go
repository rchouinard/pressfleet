package compose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
	"github.com/rchouinard/pressfleet/templates"
)

// Network aliases of the platform services on each site network. The driver
// connects each platform container to the site's network under its alias, so
// sites reach shared services by a fixed name without joining the platform
// network or seeing each other (§4.1 Networking).
const (
	MariaDBAlias = "pf-mariadb"
	TraefikAlias = "pf-traefik"
	RedisAlias   = "pf-redis"
)

// DefaultDBHost is the DB_HOST sites use unless SiteRender overrides it.
const DefaultDBHost = MariaDBAlias + ":3306"

// RedisPort is the object cache's port under RedisAlias.
const RedisPort = 6379

// Files RenderSite writes into a site's directory.
const (
	SiteComposeFile  = "compose.yaml"
	SiteWPConfigFile = "wp-config.php"
)

// Minimum site resources (the provisioner package's platform minimums).
// Below them the split leaves php too little memory to run WordPress, or the
// containers too little CPU to start in time.
const (
	MinSiteMemoryLimitMB       = provisioner.MinMemoryLimitMB
	MinSiteMemoryReservationMB = provisioner.MinMemoryReservationMB
	MinSiteCPUMillis           = provisioner.MinCPUMillis
)

// ErrInvalidSite is returned by RenderSite for input it can't render.
var ErrInvalidSite = errors.New("compose: invalid site")

// siteID restricts site IDs to what works as part of a Compose project and
// Docker network name. Site IDs are lowercase ULIDs.
var siteID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,59}$`)

// SiteProjectName is the Compose project name of a site, which is also the
// name of its network.
func SiteProjectName(siteID string) string { return "pf-" + siteID }

// SiteImages are the site's image references, resolved from the image catalog
// by the caller for the site's PHP and WordPress versions.
type SiteImages struct {
	PHP   string
	Nginx string
}

// SiteRender is everything RenderSite needs for one site.
type SiteRender struct {
	// Site is the site's configuration. Its ID, ClientGroupID, UID, and
	// Resources are used here; RenderSite validates all of it.
	Site *provisioner.SiteConfig
	// Images are the container images for Site.PHPVersion and
	// Site.WPCoreVersion.
	Images SiteImages
	// WPConfig is passed to wpconfig.Render. An empty DBHost means
	// DefaultDBHost; its Security is replaced by the one derived from
	// Security below.
	WPConfig wpconfig.Params
	// Security is the install-wide WordPress security set (§6.1), the
	// same for every site. The zero value turns every feature off.
	Security config.WordPressSecurityConfig
	// PlatformProject is the platform's Compose project name
	// (config.Config.PlatformProject), which the platform's Traefik only
	// routes containers labeled with. Empty renders no routing labels, so
	// the site is unreachable from outside; only tests do that.
	PlatformProject string
}

// RenderResult says which files RenderSite changed, so the driver knows
// what to recreate.
type RenderResult struct {
	// ComposeChanged means compose.yaml changed. `docker compose up`
	// recreates the containers whose configuration changed by itself.
	ComposeChanged bool
	// WPConfigChanged means wp-config.php changed. Compose can't see this
	// (the bind mount's definition is the same), so the driver must
	// recreate php, e.g. with `up --force-recreate php`.
	WPConfigChanged bool
}

// RenderSite writes the site's Compose project (compose.yaml), its
// wp-config.php, its maintenance routes (SiteMaintenanceFile), and, for a
// live site, its custom domain routes (SiteDomainsFile) into dir,
// normally config.Layout.SiteDir. Re-rendering is safe: a file whose
// content is unchanged is left alone.
//
// # File modes and ownership
//
// compose.yaml holds no secrets and gets mode 0644. wp-config.php holds the
// database password and salts, and the php container reads it as the site's
// UID. When the control plane runs as root (in its container), the file is
// owned by the site's UID and GID with mode 0400, readable by that site
// alone. Otherwise (development and tests) the file can't be given away, so
// it gets mode 0444; other host users still can't read it, because the data
// directory tree above it is 0700 (config.Layout.Ensure). The bind mount
// doesn't need the directories to be searchable: Docker resolves the path as
// root when it creates the container.
//
// # Replacing wp-config.php
//
// Both files are replaced by writing a temporary file and renaming it over
// the old one, so a reader never sees a partial file. For wp-config.php that
// matters: PHP could otherwise load, and OPcache keep, a half-written file.
// The cost is that the running php container keeps the old inode, because
// the file is bind-mounted on its own (images/README.md), so a changed
// wp-config.php only takes effect when php is recreated. RenderResult
// reports that, and unchanged content is not rewritten so an idempotent
// re-render never forces a recreate.
func RenderSite(dir string, in SiteRender) (RenderResult, error) {
	var res RenderResult
	data, err := siteData(dir, in)
	if err != nil {
		return res, err
	}
	params := in.WPConfig
	if params.DBHost == "" {
		params.DBHost = DefaultDBHost
	}
	params.Security = wpSecurity(in.Security)
	wpConfig, err := wpconfig.Render(params)
	if err != nil {
		return res, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}

	tmpl, err := template.New("").Option("missingkey=error").Funcs(template.FuncMap{"q": quoteYAML}).
		ParseFS(templates.Compose(), "site.yaml.tmpl")
	if err != nil {
		return res, fmt.Errorf("parsing site template: %w", err)
	}
	var composeFile bytes.Buffer
	if err := tmpl.ExecuteTemplate(&composeFile, "site.yaml.tmpl", data); err != nil {
		return res, fmt.Errorf("rendering %s: %w", SiteComposeFile, err)
	}

	routes, err := maintenanceRoutes(in.Site)
	if err != nil {
		return res, fmt.Errorf("rendering %s: %w", SiteMaintenanceFile, err)
	}
	domains, err := domainRoutes(in.Site, in.Security.Headers)
	if err != nil {
		return res, fmt.Errorf("rendering %s: %w", SiteDomainsFile, err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, fmt.Errorf("creating %s: %w", dir, err)
	}
	// wp-config.php first: compose.yaml refers to it, and Docker would
	// create a directory in its place if it were missing.
	owner, mode := -1, os.FileMode(0o444)
	if os.Geteuid() == 0 {
		owner, mode = in.Site.UID, 0o400
	}
	if res.WPConfigChanged, err = replaceFile(filepath.Join(dir, SiteWPConfigFile), wpConfig, mode, owner); err != nil {
		return res, err
	}
	if res.ComposeChanged, err = replaceFile(filepath.Join(dir, SiteComposeFile), composeFile.Bytes(), 0o644, -1); err != nil {
		return res, err
	}
	// Kept up to date for SetMaintenance, whether or not the site is in
	// maintenance mode (maintenance.go).
	if _, err := replaceFile(filepath.Join(dir, SiteMaintenanceFile), routes, 0o644, -1); err != nil {
		return res, err
	}
	// Only a live site has custom domain routes (domains.go).
	domainsPath := filepath.Join(dir, SiteDomainsFile)
	if domains == nil {
		if err := os.Remove(domainsPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, err
		}
	} else if _, err := replaceFile(domainsPath, domains, 0o644, -1); err != nil {
		return res, err
	}
	return res, nil
}

// siteTemplateData is what site.yaml.tmpl sees. It has no secrets.
type siteTemplateData struct {
	Project        string
	Network        string
	SiteID         string
	ClientGroupID  string
	UID            int
	Images         SiteImages
	PHP            containerResources
	Nginx          containerResources
	PHPMemoryLimit string
	PHPMaxChildren int
	EnableCache    bool
	WPConfigFile   string
	NginxSecurity  nginxSecurity
	// RoutingLabels are nginx's Traefik labels (routing.go).
	RoutingLabels []label
}

// nginxSecurity is the nginx half of the WordPress security set (§6.1), as
// the site-nginx image's environment variables (images/README.md).
type nginxSecurity struct {
	LoginRateLimit  bool
	LoginRate       string // NGINX_LOGIN_RATE, e.g. "10r/m"
	LoginBurst      int
	SecurityHeaders bool
}

func nginxSecuritySettings(s config.WordPressSecurityConfig) (nginxSecurity, error) {
	n := nginxSecurity{LoginRateLimit: s.LoginRateLimit.Enabled, SecurityHeaders: s.Headers}
	if !n.LoginRateLimit {
		return n, nil
	}
	if s.LoginRateLimit.RequestsPerMinute < 1 || s.LoginRateLimit.Burst < 0 {
		return n, errors.New("security: the login rate limit needs a positive rate and a burst of at least 0")
	}
	n.LoginRate = strconv.Itoa(s.LoginRateLimit.RequestsPerMinute) + "r/m"
	n.LoginBurst = s.LoginRateLimit.Burst
	return n, nil
}

func siteData(dir string, in SiteRender) (siteTemplateData, error) {
	var errs []error
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		errs = append(errs, fmt.Errorf("site directory must be a clean absolute path, got %q", dir))
	}
	if in.Site == nil {
		return siteTemplateData{}, fmt.Errorf("%w: site config is required", ErrInvalidSite)
	}
	if err := in.Site.Validate(); err != nil {
		errs = append(errs, err)
	}
	if !siteID.MatchString(in.Site.ID) {
		errs = append(errs, fmt.Errorf("site ID %q must be up to 60 lowercase letters, digits, and hyphens", in.Site.ID))
	}
	if in.Images.PHP == "" || in.Images.Nginx == "" {
		errs = append(errs, errors.New("php and nginx images are required"))
	}
	split, err := splitResources(in.Site.Resources)
	if err != nil {
		errs = append(errs, err)
	}
	nginx, err := nginxSecuritySettings(in.Security)
	if err != nil {
		errs = append(errs, err)
	}
	if err := validateRouting(in.Site); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return siteTemplateData{}, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	routing, err := routingLabels(in.PlatformProject, in.Site)
	if err != nil {
		return siteTemplateData{}, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	return siteTemplateData{
		RoutingLabels:  routing,
		Project:        SiteProjectName(in.Site.ID),
		Network:        SiteProjectName(in.Site.ID),
		SiteID:         in.Site.ID,
		ClientGroupID:  in.Site.ClientGroupID,
		UID:            in.Site.UID,
		Images:         in.Images,
		PHP:            split.PHP,
		Nginx:          split.Nginx,
		PHPMemoryLimit: strconv.Itoa(split.PHPMemoryLimitMB) + "M",
		PHPMaxChildren: split.PHPMaxChildren,
		EnableCache:    in.Site.EnableCache,
		WPConfigFile:   SiteWPConfigFile,
		NginxSecurity:  nginx,
	}, nil
}

// wpSecurity is the part of the security set that wp-config.php carries:
// DISALLOW_FILE_EDIT and the must-use plugin modules' constants.
func wpSecurity(s config.WordPressSecurityConfig) wpconfig.Security {
	sec := wpconfig.Security{
		DisallowFileEdit:            s.DisableFileEdit,
		BlockUserEnumeration:        s.BlockUserEnumeration,
		DisableApplicationPasswords: s.DisableApplicationPasswords,
		// Optional two-factor needs nothing here: the control plane just
		// installs the Two Factor plugin (package sites).
		RequireTwoFactor: s.TwoFactor == config.TwoFactorRequiredForAdmins,
	}
	if s.PasswordPolicy.Enabled {
		sec.PasswordMinLength = s.PasswordPolicy.MinLength
	}
	if s.PasswordHashing == config.PasswordHashingArgon2id {
		sec.PasswordHashing = wpconfig.HashArgon2id
	}
	return sec
}

// containerResources are one container's share of the site's resources.
type containerResources struct {
	CPUs                string // Compose "cpus", e.g. "0.75"
	MemoryLimitMB       int
	MemoryReservationMB int
}

// resourceSplit is how a site's resources are divided, plus the PHP settings
// derived from php's share.
type resourceSplit struct {
	Nginx, PHP       containerResources
	PHPMemoryLimitMB int // PHP_MEMORY_LIMIT
	PHPMaxChildren   int // PHP_FPM_MAX_CHILDREN
}

// Constants of the resource split formulas (see splitResources).
const (
	// phpOverheadMB is php's memory apart from its workers: the FPM
	// master, and OPcache's shared memory (PHP_OPCACHE_MEMORY_CONSUMPTION,
	// 64 MiB by default), which workers share rather than each hold.
	phpOverheadMB = 64
	// maxPHPMemoryLimitMB is PHP_MEMORY_LIMIT when php has room for it; it
	// is the image default and what wp-admin asks for (WP_MAX_MEMORY_LIMIT).
	maxPHPMemoryLimitMB = 256
	minPHPMaxChildren   = 2
	maxPHPMaxChildren   = 32
)

// splitResources divides a site's resources between nginx and php. nginx
// (one worker, static files, FastCGI proxying) needs little and its needs
// barely grow with the site, so it gets a small, bounded share and php the
// rest:
//
//	nginx memory limit       = clamp(limit/8, 32, 128) MiB
//	php memory limit         = limit - nginx memory limit
//	nginx memory reservation = max(clamp(reservation/8, 8, 32), reservation - php memory limit) MiB
//	php memory reservation   = reservation - nginx memory reservation
//	nginx CPU                = clamp(floor(cpu/4, 0.01), 0.1, 0.5)
//	php CPU                  = cpu - nginx CPU
//
// nginx's CPU is a whole number of hundredths because Docker's systemd cgroup
// driver (the default on most hosts) can only express a CPU limit in
// multiples of 10 ms per second and rounds every container's limit up to the
// next one. A share like 0.187 would run as 0.19, and 0.563 as 0.57: a site
// set to 0.75 CPUs would get 0.76. With nginx on a multiple of 0.01, php's
// share is one too whenever the site's total is, so the site gets exactly
// what was set. A total that is not a multiple of 0.01 (0.333) still gets
// rounded up by up to 0.01, in php's share alone.
//
// The second term of the nginx reservation only matters when the reservation
// is close to the limit; it keeps php's reservation within php's limit. The
// shares always add up to the site's totals exactly. For the defaults (1 CPU,
// 128 MiB reserved, 512 MiB limit) nginx gets 0.25 CPU, 16/64 MiB and php
// 0.75 CPU, 112/448 MiB.
//
// PHP settings follow from php's memory limit P:
//
//	PHP_MEMORY_LIMIT     = min(256, floor32(P - 64)) MiB
//	PHP_FPM_MAX_CHILDREN = clamp((P - 64) / max(PHP_MEMORY_LIMIT/4, 32), 2, 32)
//
// 64 MiB is the FPM master and OPcache. One request may use the whole
// memory_limit, but a typical WordPress worker uses about a quarter of it,
// so max_children sizes workers at a quarter (at least 32 MiB) instead of
// assuming every worker peaks at once. If they all do, the kernel's OOM
// killer ends a worker, which FPM replaces; the site stays up. Idle sites
// hold no workers (pm = ondemand). The default site gets PHP_MEMORY_LIMIT
// 256M and 6 children.
func splitResources(r provisioner.ResourceLimits) (resourceSplit, error) {
	var errs []error
	cpu, err := parseCPUMillis(r.CPU)
	if err != nil {
		errs = append(errs, err)
	} else if cpu < MinSiteCPUMillis {
		errs = append(errs, fmt.Errorf("resources: CPU must be at least %s", formatCPUMillis(MinSiteCPUMillis)))
	}
	if r.MemoryLimitMB < MinSiteMemoryLimitMB {
		errs = append(errs, fmt.Errorf("resources: memory limit must be at least %d MiB", MinSiteMemoryLimitMB))
	}
	if r.MemoryReservationMB < MinSiteMemoryReservationMB {
		errs = append(errs, fmt.Errorf("resources: memory reservation must be at least %d MiB", MinSiteMemoryReservationMB))
	}
	if r.MemoryReservationMB > r.MemoryLimitMB {
		errs = append(errs, errors.New("resources: memory reservation must not exceed the limit"))
	}
	if err := errors.Join(errs...); err != nil {
		return resourceSplit{}, err
	}

	var s resourceSplit
	s.Nginx.MemoryLimitMB = clamp(r.MemoryLimitMB/8, 32, 128)
	s.PHP.MemoryLimitMB = r.MemoryLimitMB - s.Nginx.MemoryLimitMB
	s.Nginx.MemoryReservationMB = max(clamp(r.MemoryReservationMB/8, 8, 32), r.MemoryReservationMB-s.PHP.MemoryLimitMB)
	s.PHP.MemoryReservationMB = r.MemoryReservationMB - s.Nginx.MemoryReservationMB

	nginxCPU := clamp(cpu/4/cpuQuotaStepMillis*cpuQuotaStepMillis, 100, 500)
	s.Nginx.CPUs = formatCPUMillis(nginxCPU)
	s.PHP.CPUs = formatCPUMillis(cpu - nginxCPU)

	workerBudget := s.PHP.MemoryLimitMB - phpOverheadMB
	s.PHPMemoryLimitMB = min(maxPHPMemoryLimitMB, workerBudget/32*32)
	perWorker := max(s.PHPMemoryLimitMB/4, 32)
	s.PHPMaxChildren = clamp(workerBudget/perWorker, minPHPMaxChildren, maxPHPMaxChildren)
	return s, nil
}

// cpuQuotaStepMillis is the granularity, in thousandths of a CPU, of the CPU
// limits that Docker's systemd cgroup driver applies exactly (10 ms of CPU
// time per second).
const cpuQuotaStepMillis = 10

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

// parseCPUMillis parses a decimal CPU count ("1.0", "0.5", "2") into
// thousandths of a CPU.
func parseCPUMillis(s string) (int, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > 1024 {
		return 0, fmt.Errorf("resources: CPU %q must be a positive decimal number of CPUs", s)
	}
	return int(math.Round(f * 1000)), nil
}

// formatCPUMillis renders thousandths of a CPU as a decimal ("0.75", "1").
func formatCPUMillis(m int) string {
	s := fmt.Sprintf("%d.%03d", m/1000, m%1000)
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".")
}

// quoteYAML renders v as a double-quoted YAML scalar. JSON string syntax is
// valid YAML; '$' is doubled so Compose doesn't treat it as interpolation.
func quoteYAML(v any) (string, error) {
	b, err := json.Marshal(fmt.Sprint(v))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(string(b), "$", "$$"), nil
}

// replaceFile makes path hold content with the given mode and, if owner is
// not -1, owned by owner:owner. Unchanged content is not rewritten (only the
// mode and owner are corrected), and the result reports whether the content
// changed. Changed content goes to a temporary file that is renamed over
// path, so readers see the old or the new file, never a mix, and the file
// never briefly has looser permissions than mode.
func replaceFile(path string, content []byte, mode os.FileMode, owner int) (changed bool, err error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) { //nolint:gosec // G304: path is inside the site directory the caller chose
		if err := os.Chmod(path, mode); err != nil {
			return false, fmt.Errorf("writing %s: %w", path, err)
		}
		if owner != -1 {
			if err := os.Lchown(path, owner, owner); err != nil {
				return false, fmt.Errorf("writing %s: %w", path, err)
			}
		}
		return false, nil
	}

	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // no-op after a successful rename

	// CreateTemp uses 0600, so the content is never exposed more widely
	// than the final mode; Chmod after writing so a 0400/0444 file can
	// still be written through the open descriptor.
	if owner != -1 {
		if err := f.Chown(owner, owner); err != nil {
			_ = f.Close()
			return false, fmt.Errorf("writing %s: %w", path, err)
		}
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	return true, nil
}
