package sites

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// The default plugin stack (design doc §5.1 step 7).
//
// # Where the plugins come from
//
// The site-php image bundles each default plugin's release zip, pinned by
// version and SHA-256 in images/shared/plugins.json and verified when the
// image is built, under BundledPluginDir. Provisioning installs a missing
// plugin from there with `wp plugin install <zip>`, so it needs no network
// access and installs exactly what CI built and tested. Once installed, a
// plugin is a regular plugin in wp-content: WordPress updates it like any
// other (§5.6), and the platform never replaces a version the site already
// has, so an update is never undone.
//
// # Converging
//
// configurePlugins runs at provisioning and in every site.reconcile job. One
// `wp plugin list` call says what the site has; it installs what is missing,
// activates what is inactive, and configures what isn't configured, so a
// site that already matches costs that one call. A plugin the site owner
// deleted or deactivated comes back at the next reconciliation: the
// platform depends on these plugins (the object cache drop-in, and Nginx
// Helper's cache purges).
//
// A plugin missing from the site image (an image built before it was
// bundled) is skipped with a warning rather than failing the job, so a site
// isn't marked failed until its images are updated; the next reconciliation
// after that installs it.

// BundledPluginDir is where the site-php image keeps the default plugins'
// release zips, one <slug>.zip each (images/README.md, "Default plugins").
const BundledPluginDir = "/opt/pressfleet/plugins"

// WordPress.org slugs of the default plugins.
const (
	PluginRedisCache = "redis-cache"
)

// redisDropInTitle is the plugin header of the Redis Object Cache plugin's
// object-cache.php drop-in, which WordPress reports as its title.
const redisDropInTitle = "Redis Object Cache Drop-In"

// objectCacheDropIn is the drop-in's name in `wp plugin list`.
const objectCacheDropIn = "object-cache.php"

// wantedPlugins returns the default plugins every site should have, in the
// order they are installed and activated.
func (s *Service) wantedPlugins() []string {
	return []string{PluginRedisCache}
}

// wpPlugin is an entry of `wp plugin list --format=json`.
type wpPlugin struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Title  string `json:"title"`
}

func (p wpPlugin) active() bool { return p.Status == "active" || p.Status == "active-network" }

// sitePlugins is what `wp plugin list` reports: regular plugins by slug,
// and drop-ins by file name. Must-use plugins are left out.
type sitePlugins struct {
	plugins map[string]wpPlugin
	dropins map[string]wpPlugin
}

// listPlugins asks WP-CLI for the site's plugins. The update check is
// skipped: it would contact WordPress.org on every call.
func (s *Service) listPlugins(ctx context.Context, siteID string) (sitePlugins, error) {
	out, err := s.prov.ExecWPCLI(ctx, siteID, []string{
		"plugin", "list", "--format=json", "--fields=name,status,title", "--skip-update-check",
	})
	if err != nil {
		return sitePlugins{}, fmt.Errorf("listing plugins: %w", err)
	}
	var list []wpPlugin
	if strings.TrimSpace(out) != "" {
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			return sitePlugins{}, fmt.Errorf("listing plugins: unexpected output from WP-CLI: %w", err)
		}
	}
	sp := sitePlugins{plugins: map[string]wpPlugin{}, dropins: map[string]wpPlugin{}}
	for _, p := range list {
		switch p.Status {
		case "dropin":
			sp.dropins[p.Name] = p
		case "must-use":
		default:
			sp.plugins[p.Name] = p
		}
	}
	return sp, nil
}

// configurePlugins converges the site's default plugin stack; see the top
// of this file. The site must be running.
func (s *Service) configurePlugins(ctx context.Context, r *jobs.Run, siteID string) error {
	sp, err := s.listPlugins(ctx, siteID)
	if err != nil {
		return err
	}
	var activate []string
	for _, slug := range s.wantedPlugins() {
		p, ok := sp.plugins[slug]
		if !ok {
			installed, err := s.installBundledPlugin(ctx, r, siteID, slug)
			if err != nil {
				return err
			}
			if !installed {
				continue
			}
			p = wpPlugin{Name: slug, Status: "inactive"}
		}
		if !p.active() {
			activate = append(activate, slug)
			p.Status = "active"
		}
		sp.plugins[slug] = p
	}
	if len(activate) > 0 {
		args := append([]string{"plugin", "activate"}, activate...)
		if _, err := s.prov.ExecWPCLI(ctx, siteID, args); err != nil {
			return fmt.Errorf("activating %s: %w", strings.Join(activate, ", "), err)
		}
		r.Logf("Activated %s.", strings.Join(activate, ", "))
	}
	return s.enableObjectCache(ctx, r, siteID, sp)
}

// installBundledPlugin installs slug from the zip the site image bundles.
// It reports false, after a warning, if the image has no such zip.
func (s *Service) installBundledPlugin(ctx context.Context, r *jobs.Run, siteID, slug string) (bool, error) {
	zip := BundledPluginDir + "/" + slug + ".zip"
	// slug is one of the platform's constants, never user input, so it is
	// safe inside the PHP string.
	out, err := s.prov.ExecWPCLI(ctx, siteID, []string{"eval", "echo is_readable( '" + zip + "' ) ? 'yes' : 'no';"})
	if err != nil {
		return false, fmt.Errorf("looking for the bundled %s plugin: %w", slug, err)
	}
	if strings.TrimSpace(out) != "yes" {
		r.Warnf("The site's image doesn't bundle the %s plugin; it will be installed once the site runs on an image that does.", slug)
		return false, nil
	}
	if _, err := s.prov.ExecWPCLI(ctx, siteID, []string{"plugin", "install", zip}); err != nil {
		return false, fmt.Errorf("installing the %s plugin: %w", slug, err)
	}
	r.Logf("Installed the %s plugin from the site image.", slug)
	return true, nil
}

// enableObjectCache installs the Redis Object Cache plugin's drop-in
// (wp-content/object-cache.php), which is what makes WordPress use the
// object cache, unless the site has one. A drop-in of another plugin is
// left in place: replacing it would break whatever the site owner set up.
func (s *Service) enableObjectCache(ctx context.Context, r *jobs.Run, siteID string, sp sitePlugins) error {
	if p, ok := sp.plugins[PluginRedisCache]; !ok || !p.active() {
		return nil
	}
	dropin, ok := sp.dropins[objectCacheDropIn]
	switch {
	case !ok:
		// It checks the connection first, as the site's ACL user.
		if _, err := s.prov.ExecWPCLI(ctx, siteID, []string{"redis", "enable"}); err != nil {
			return fmt.Errorf("enabling the object cache: %w", err)
		}
		r.Logf("Enabled the object cache.")
	case dropin.Title != redisDropInTitle:
		r.Warnf("The site has another object cache drop-in (%q); leaving it in place, so the platform's object cache isn't used.", dropin.Title)
	}
	return nil
}

// Step names of the plugin steps.
const (
	stepConfigurePlugins = "configure_plugins" // site.create
	stepReconcilePlugins = "plugins"           // site.reconcile
)

// configureCreatedSitePlugins is the configure_plugins step of site.create.
func (s *Service) configureCreatedSitePlugins(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	if err := abortIfDeleting(site); err != nil {
		return err
	}
	return s.configurePlugins(ctx, r, site.ID)
}

// reconcilePlugins is the plugins step of site.reconcile. WP-CLI needs a
// running site, so a stopped site is left for a later reconciliation (the
// next control plane start after it runs again).
func (s *Service) reconcilePlugins(ctx context.Context, r *jobs.Run) error {
	site, ok, err := s.reconcileSite(ctx, r)
	if err != nil || !ok {
		return err
	}
	if site.Status != store.SiteRunning {
		r.Logf("The site is %s; its plugins are checked at a reconciliation while it runs.", site.Status)
		return nil
	}
	err = s.configurePlugins(ctx, r, site.ID)
	if errors.Is(err, provisioner.ErrSiteNotRunning) || errors.Is(err, provisioner.ErrSiteNotFound) {
		r.Warnf("The site isn't running; its plugins are checked at a reconciliation while it runs.")
		return nil
	}
	if err != nil {
		return err
	}
	r.Logf("The site's default plugins are in place.")
	return nil
}
