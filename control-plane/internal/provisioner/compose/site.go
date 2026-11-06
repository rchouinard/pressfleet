package compose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	params.Security = provisioner.WordPressSecurity(in.Security)
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
	PHP            provisioner.ContainerResources
	Nginx          provisioner.ContainerResources
	PHPMemoryLimit string
	PHPMaxChildren int
	EnableCache    bool
	WPConfigFile   string
	NginxSecurity  provisioner.NginxSecurity
	// RoutingLabels are nginx's Traefik labels (routing.go).
	RoutingLabels []label
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
	split, err := provisioner.SplitResources(in.Site.Resources)
	if err != nil {
		errs = append(errs, err)
	}
	nginx, err := provisioner.NginxSecuritySettings(in.Security)
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
