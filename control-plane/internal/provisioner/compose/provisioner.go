package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// The Compose provisioner implements the Provisioner part of the driver.
// LogReader, StatsReader, and TaskRunner come with the issues that need them
// (#78, #68, the backup pipeline), and the full provisioner.Driver assertion
// with them; stubbing them now would only hide which parts are missing.
var _ provisioner.Provisioner = (*Provisioner)(nil)

// ErrNotImplemented is returned by Provisioner methods whose feature has not
// been built yet (SetMaintenance, #44; PurgeCache, #41).
var ErrNotImplemented = fmt.Errorf("compose: not implemented yet: %w", errors.ErrUnsupported)

// ErrSharedServiceUnavailable means a platform container that sites depend
// on, such as MariaDB, does not exist, so a site network can't be attached
// to it.
var ErrSharedServiceUnavailable = errors.New("compose: platform service container not found")

// Defaults for ProvisionerConfig.
const (
	DefaultWaitTimeout = 3 * time.Minute
)

// SecretReader reads stored secrets; secrets.Store implements it.
type SecretReader interface {
	Get(ctx context.Context, ref secrets.Ref) (redact.Secret, error)
}

// ImageResolver maps a site's PHP and WordPress versions to its images;
// *catalog.Catalog implements it.
type ImageResolver interface {
	Images(php, wordpress string) (catalog.Images, error)
}

// ProvisionerConfig configures NewProvisioner.
type ProvisionerConfig struct {
	// Layout locates the site directories the projects are rendered into.
	Layout config.Layout
	// Secrets resolves SiteConfig's DBSecretRef and WPSaltsSecretRef.
	Secrets SecretReader
	// Images resolves SiteConfig's PHPVersion and WPCoreVersion.
	Images ImageResolver
	// MariaDBContainer is the platform MariaDB container
	// (platform.MariaDBContainer), connected to each site network under
	// MariaDBAlias.
	MariaDBContainer string
	// Docker runs the docker CLI; nil means DockerExec. Tests inject a fake.
	Docker ExecFunc
	// Logger receives lifecycle events; nil discards them.
	Logger *slog.Logger
	// WaitTimeout bounds how long starting a site waits for its containers
	// to become healthy; zero means DefaultWaitTimeout.
	WaitTimeout time.Duration
	// WPCLITimeout bounds each ExecWPCLI call, in addition to its context;
	// zero means DefaultWPCLITimeout.
	WPCLITimeout time.Duration
	// WPCLIMaxOutput is the most standard output ExecWPCLI keeps, in bytes;
	// zero means DefaultWPCLIMaxOutput.
	WPCLIMaxOutput int
	// SMTP is the platform's outbound email relay, rendered into every
	// site's wp-config.php for the platform mu-plugin (§5.6). Nil means
	// sites send no email.
	SMTP *wpconfig.SMTP
}

// Provisioner is the Compose implementation of provisioner.Provisioner. Each
// site is the Compose project SiteProjectName(id), rendered by RenderSite
// into Layout.SiteDir(id).
//
// SiteConfig carries only references to the site's secrets. The provisioner
// resolves them when it renders wp-config.php, so plaintext never passes
// through job payloads or the Provisioner interface, and a Kubernetes driver
// can resolve the same references into a Secret instead.
type Provisioner struct {
	layout      config.Layout
	secrets     SecretReader
	images      ImageResolver
	shared      []sharedService
	docker      ExecFunc
	log         *slog.Logger
	waitTimeout time.Duration
	smtp        *wpconfig.SMTP

	wpcliDocker    ExecFunc // docker, but with bounded output
	wpcliTimeout   time.Duration
	wpcliMaxOutput int
}

// sharedService is a platform container that joins every site network under
// a fixed alias. Traefik and Redis join this list in M2.
type sharedService struct {
	container string
	alias     string
}

// NewProvisioner returns a Compose provisioner.
func NewProvisioner(cfg ProvisionerConfig) (*Provisioner, error) {
	var errs []error
	if cfg.Layout.Root == "" || !filepath.IsAbs(cfg.Layout.Root) {
		errs = append(errs, fmt.Errorf("data directory %q must be absolute", cfg.Layout.Root))
	}
	if cfg.Secrets == nil {
		errs = append(errs, errors.New("a secret reader is required"))
	}
	if cfg.Images == nil {
		errs = append(errs, errors.New("an image resolver is required"))
	}
	if cfg.MariaDBContainer == "" {
		errs = append(errs, errors.New("the MariaDB container name is required"))
	}
	if cfg.WaitTimeout < 0 || cfg.WPCLITimeout < 0 {
		errs = append(errs, errors.New("timeouts must not be negative"))
	}
	if cfg.WPCLIMaxOutput < 0 {
		errs = append(errs, errors.New("WP-CLI output limit must not be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("compose provisioner: %w", err)
	}
	p := &Provisioner{
		layout:         cfg.Layout,
		secrets:        cfg.Secrets,
		images:         cfg.Images,
		shared:         []sharedService{{container: cfg.MariaDBContainer, alias: MariaDBAlias}},
		docker:         cfg.Docker,
		log:            cfg.Logger,
		waitTimeout:    cfg.WaitTimeout,
		smtp:           cfg.SMTP,
		wpcliDocker:    cfg.Docker,
		wpcliTimeout:   cfg.WPCLITimeout,
		wpcliMaxOutput: cfg.WPCLIMaxOutput,
	}
	if p.wpcliTimeout == 0 {
		p.wpcliTimeout = DefaultWPCLITimeout
	}
	if p.wpcliMaxOutput == 0 {
		p.wpcliMaxOutput = DefaultWPCLIMaxOutput
	}
	if p.docker == nil {
		p.docker = DockerExec
		p.wpcliDocker = DockerExecLimited(p.wpcliMaxOutput, wpcliMaxStderr)
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	if p.waitTimeout == 0 {
		p.waitTimeout = DefaultWaitTimeout
	}
	return p, nil
}

// CreateSite renders the site's project, brings it up, waits for it to be
// healthy, and connects the platform services to its network. For an
// existing site it converges it to cfg and starts it.
func (p *Provisioner) CreateSite(ctx context.Context, cfg *provisioner.SiteConfig) (*provisioner.SiteStatus, error) {
	if err := p.converge(ctx, cfg, true); err != nil {
		return nil, fmt.Errorf("create site: %w", err)
	}
	p.log.InfoContext(ctx, "site created", "site_id", cfg.ID)
	return p.GetSite(ctx, cfg.ID)
}

// UpdateSite re-renders an existing site and recreates the containers whose
// image or configuration changed. A stopped site stays stopped: its
// containers are recreated without starting, and pick up the change when the
// site is started.
func (p *Provisioner) UpdateSite(ctx context.Context, cfg *provisioner.SiteConfig) (*provisioner.SiteStatus, error) {
	if cfg == nil {
		return nil, fmt.Errorf("update site: %w: site config is required", ErrInvalidSite)
	}
	if err := validSiteID(cfg.ID); err != nil {
		return nil, fmt.Errorf("update site: %w", err)
	}
	containers, err := p.project(cfg.ID).Ps(ctx)
	if err != nil {
		return nil, fmt.Errorf("update site %s: %w", cfg.ID, err)
	}
	if len(containers) == 0 {
		return nil, fmt.Errorf("update site %s: %w", cfg.ID, provisioner.ErrSiteNotFound)
	}
	phase, _ := sitePhase(containers)
	if err := p.converge(ctx, cfg, phase != provisioner.SiteStopped); err != nil {
		return nil, fmt.Errorf("update site: %w", err)
	}
	p.log.InfoContext(ctx, "site updated", "site_id", cfg.ID)
	return p.GetSite(ctx, cfg.ID)
}

// converge renders the site and applies it. With start, the containers are
// started and waited for; without, they are only created.
func (p *Provisioner) converge(ctx context.Context, cfg *provisioner.SiteConfig, start bool) error {
	if cfg == nil {
		return fmt.Errorf("%w: site config is required", ErrInvalidSite)
	}
	if err := validSiteID(cfg.ID); err != nil {
		return err
	}
	in, err := p.siteRender(ctx, cfg)
	if err != nil {
		return fmt.Errorf("site %s: %w", cfg.ID, err)
	}
	dir, err := p.layout.SiteDir(cfg.ID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	// Always render before up: wp-config.php is bind-mounted with
	// create_host_path: false, so it must exist first.
	res, err := RenderSite(dir, in)
	if err != nil {
		return fmt.Errorf("site %s: %w", cfg.ID, err)
	}

	proj := p.project(cfg.ID)
	stale, err := p.phpStale(ctx, proj, dir, res.WPConfigChanged)
	if err != nil {
		return fmt.Errorf("site %s: %w", cfg.ID, err)
	}
	if stale {
		// Recreate php alone, so nginx keeps serving.
		if err := proj.Up(ctx, UpOptions{NoStart: !start, ForceRecreate: true, Services: []string{string(provisioner.ComponentPHP)}}); err != nil {
			return err
		}
	}
	if err := proj.Up(ctx, UpOptions{NoStart: !start, Wait: start, WaitTimeout: p.waitTimeout}); err != nil {
		return err
	}
	// The network exists now. php's health check doesn't need the database,
	// so connecting after the wait is fine.
	if err := p.attachShared(ctx, cfg.ID); err != nil {
		return fmt.Errorf("site %s: %w", cfg.ID, err)
	}
	return nil
}

// siteRender resolves what RenderSite needs beyond the SiteConfig itself.
func (p *Provisioner) siteRender(ctx context.Context, cfg *provisioner.SiteConfig) (SiteRender, error) {
	if err := cfg.Validate(); err != nil {
		return SiteRender{}, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	images, err := p.images.Images(cfg.PHPVersion, cfg.WPCoreVersion)
	if err != nil {
		return SiteRender{}, fmt.Errorf("%w: %w", ErrInvalidSite, err)
	}
	dbSecret, err := p.secrets.Get(ctx, secrets.Ref(cfg.DBSecretRef))
	if err != nil {
		return SiteRender{}, fmt.Errorf("reading database credentials: %w", err)
	}
	creds, err := sitedb.UnmarshalCredentials(dbSecret)
	if err != nil {
		return SiteRender{}, fmt.Errorf("reading database credentials: %w", err)
	}
	saltsSecret, err := p.secrets.Get(ctx, secrets.Ref(cfg.WPSaltsSecretRef))
	if err != nil {
		return SiteRender{}, fmt.Errorf("reading WordPress salts: %w", err)
	}
	salts, err := wpconfig.UnmarshalSalts(saltsSecret)
	if err != nil {
		return SiteRender{}, fmt.Errorf("reading WordPress salts: %w", err)
	}
	return SiteRender{
		Site:     cfg,
		Images:   SiteImages{PHP: images.PHP, Nginx: images.Nginx},
		WPConfig: wpconfig.Params{DB: creds, Salts: salts, SMTP: p.smtp},
	}, nil
}

// phpStale reports whether the running php container holds an outdated
// wp-config.php. The file is bind-mounted on its own and replaced by rename,
// so a running container keeps the old inode until it is recreated
// (RenderSite). Besides the render that just happened (changed), the file's
// modification time is compared with php's start: that catches a change
// rendered by an earlier attempt that failed before recreating php, so a
// retried job step still converges. A container that isn't running mounts
// the current file when it starts.
func (p *Provisioner) phpStale(ctx context.Context, proj Project, dir string, changed bool) (bool, error) {
	containers, err := proj.Ps(ctx)
	if err != nil {
		return false, err
	}
	php, ok := findService(containers, provisioner.ComponentPHP)
	if !ok {
		return false, nil // up creates it
	}
	if changed {
		return true, nil
	}
	info, found, err := p.inspectContainer(ctx, php.Name)
	if err != nil || !found || !info.State.Running {
		return false, err
	}
	fi, err := os.Stat(filepath.Join(dir, SiteWPConfigFile))
	if err != nil {
		return false, fmt.Errorf("checking %s: %w", SiteWPConfigFile, err)
	}
	return fi.ModTime().After(info.State.StartedAt), nil
}

// GetSite reports the site's phase from the state of its containers.
func (p *Provisioner) GetSite(ctx context.Context, siteID string) (*provisioner.SiteStatus, error) {
	if err := validSiteID(siteID); err != nil {
		return nil, fmt.Errorf("get site: %w", err)
	}
	containers, err := p.project(siteID).Ps(ctx)
	if err != nil {
		return nil, fmt.Errorf("get site %s: %w", siteID, err)
	}
	if len(containers) == 0 {
		return nil, fmt.Errorf("get site %s: %w", siteID, provisioner.ErrSiteNotFound)
	}
	phase, msg := sitePhase(containers)
	// URL stays empty until routing exists (Traefik, M2).
	return &provisioner.SiteStatus{ID: siteID, Phase: phase, Message: msg}, nil
}

// StartSite starts a stopped site and waits for it to be healthy. It
// reconnects the platform services first, in case one was recreated (e.g.
// by a platform upgrade) and lost its site networks.
func (p *Provisioner) StartSite(ctx context.Context, siteID string) error {
	proj, err := p.existing(ctx, siteID)
	if err != nil {
		return fmt.Errorf("start site: %w", err)
	}
	if err := p.attachShared(ctx, siteID); err != nil {
		return fmt.Errorf("start site %s: %w", siteID, err)
	}
	if err := proj.Start(ctx, StartOptions{Wait: true, WaitTimeout: p.waitTimeout}); err != nil {
		return fmt.Errorf("start site: %w", err)
	}
	p.log.InfoContext(ctx, "site started", "site_id", siteID)
	return nil
}

// StopSite stops the site's containers, keeping them, their network, and
// their volumes. The restart policy (unless-stopped) keeps a stopped site
// stopped across Docker restarts.
func (p *Provisioner) StopSite(ctx context.Context, siteID string) error {
	proj, err := p.existing(ctx, siteID)
	if err != nil {
		return fmt.Errorf("stop site: %w", err)
	}
	if err := proj.Stop(ctx); err != nil {
		return fmt.Errorf("stop site: %w", err)
	}
	p.log.InfoContext(ctx, "site stopped", "site_id", siteID)
	return nil
}

// AttachSharedServices connects the platform services (MariaDB) to an
// existing site's network. Docker drops a container's connections to other
// projects' networks when it recreates the container, so after the platform
// MariaDB is recreated (e.g. by an upgrade) running sites can't reach their
// database until this runs; the control plane calls it for every site at
// startup. It returns provisioner.ErrSiteNotFound if the site has no
// containers.
func (p *Provisioner) AttachSharedServices(ctx context.Context, siteID string) error {
	if _, err := p.existing(ctx, siteID); err != nil {
		return fmt.Errorf("attach platform services: %w", err)
	}
	if err := p.attachShared(ctx, siteID); err != nil {
		return fmt.Errorf("attach platform services to site %s: %w", siteID, err)
	}
	return nil
}

// DeleteSite removes the site's containers, network, and volumes, then its
// rendered directory. Every step tolerates what an earlier attempt already
// removed, so deleting a missing site succeeds.
func (p *Provisioner) DeleteSite(ctx context.Context, siteID string) error {
	if err := validSiteID(siteID); err != nil {
		return fmt.Errorf("delete site: %w", err)
	}
	dir, err := p.layout.SiteDir(siteID)
	if err != nil {
		return fmt.Errorf("delete site: %w: %w", ErrInvalidSite, err)
	}
	// Docker refuses to remove a network that containers are still
	// connected to, and the platform containers are not part of the project.
	if err := p.detachShared(ctx, siteID); err != nil {
		return fmt.Errorf("delete site %s: %w", siteID, err)
	}
	// down finds the project's objects by label, so it works even if the
	// rendered directory is already gone.
	if err := p.project(siteID).Down(ctx, DownOptions{Volumes: true}); err != nil {
		return fmt.Errorf("delete site: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("delete site %s: %w", siteID, err)
	}
	p.log.InfoContext(ctx, "site deleted", "site_id", siteID)
	return nil
}

// SetMaintenance is not implemented yet (#44).
func (p *Provisioner) SetMaintenance(context.Context, string, bool) error {
	return fmt.Errorf("set maintenance: %w", ErrNotImplemented)
}

// PurgeCache is not implemented yet (#41).
func (p *Provisioner) PurgeCache(context.Context, string) error {
	return fmt.Errorf("purge cache: %w", ErrNotImplemented)
}

// project returns the site's Compose project. Its directory may not exist;
// ps, stop, and down work from labels alone.
func (p *Provisioner) project(siteID string) Project {
	return Project{Name: SiteProjectName(siteID), Dir: filepath.Join(p.layout.SitesDir(), siteID), Docker: p.docker}
}

// existing returns the project of a site that has containers, or
// provisioner.ErrSiteNotFound.
func (p *Provisioner) existing(ctx context.Context, siteID string) (Project, error) {
	if err := validSiteID(siteID); err != nil {
		return Project{}, err
	}
	proj := p.project(siteID)
	containers, err := proj.Ps(ctx)
	if err != nil {
		return Project{}, err
	}
	if len(containers) == 0 {
		return Project{}, fmt.Errorf("site %s: %w", siteID, provisioner.ErrSiteNotFound)
	}
	return proj, nil
}

func validSiteID(id string) error {
	if !siteID.MatchString(id) {
		return fmt.Errorf("%w: site ID %q must be up to 60 lowercase letters, digits, and hyphens", ErrInvalidSite, id)
	}
	return nil
}

func findService(containers []Container, c provisioner.Component) (Container, bool) {
	for _, ctr := range containers {
		if ctr.Service == string(c) {
			return ctr, true
		}
	}
	return Container{}, false
}

// attachShared connects each platform service container to the site network
// under its alias. A container that is already connected is left alone.
func (p *Provisioner) attachShared(ctx context.Context, siteID string) error {
	network := SiteProjectName(siteID)
	for _, s := range p.shared {
		info, found, err := p.inspectContainer(ctx, s.container)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: %s", ErrSharedServiceUnavailable, s.container)
		}
		if _, ok := info.NetworkSettings.Networks[network]; ok {
			continue
		}
		_, err = p.docker(ctx, nil, "network", "connect", "--alias", s.alias, network, s.container)
		if err != nil && !stderrContains(err, "already exists") {
			return fmt.Errorf("connecting %s to %s: %w", s.container, network, err)
		}
		p.log.DebugContext(ctx, "platform service connected to site network",
			"site_id", siteID, "container", s.container, "alias", s.alias)
	}
	return nil
}

// detachShared disconnects the platform service containers from the site
// network. Missing containers, networks, and connections are fine.
func (p *Provisioner) detachShared(ctx context.Context, siteID string) error {
	network := SiteProjectName(siteID)
	for _, s := range p.shared {
		info, found, err := p.inspectContainer(ctx, s.container)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if _, ok := info.NetworkSettings.Networks[network]; !ok {
			continue
		}
		_, err = p.docker(ctx, nil, "network", "disconnect", network, s.container)
		if err != nil && !stderrContains(err, "is not connected", "not found", "no such network") {
			return fmt.Errorf("disconnecting %s from %s: %w", s.container, network, err)
		}
	}
	return nil
}

// containerInfo is the part of `docker container inspect` the provisioner
// uses.
type containerInfo struct {
	Config struct {
		User string `json:"User"`
	} `json:"Config"`
	State struct {
		Running   bool      `json:"Running"`
		StartedAt time.Time `json:"StartedAt"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
}

// inspectContainer returns found == false if the container doesn't exist.
func (p *Provisioner) inspectContainer(ctx context.Context, name string) (containerInfo, bool, error) {
	out, err := p.docker(ctx, nil, "container", "inspect", "--", name)
	if err != nil {
		if stderrContains(err, "no such container") {
			return containerInfo{}, false, nil
		}
		return containerInfo{}, false, fmt.Errorf("inspecting container %s: %w", name, err)
	}
	var infos []containerInfo
	if err := json.Unmarshal(out, &infos); err != nil {
		return containerInfo{}, false, fmt.Errorf("parsing inspect output for container %s: %w", name, err)
	}
	if len(infos) != 1 {
		return containerInfo{}, false, fmt.Errorf("inspecting container %s: got %d results", name, len(infos))
	}
	return infos[0], true, nil
}

// stderrContains reports whether err is a *CommandError whose stderr
// contains one of the substrings, ignoring case. The docker CLI reports
// "not found" conditions only as text.
func stderrContains(err error, substrings ...string) bool {
	var ce *CommandError
	if !errors.As(err, &ce) {
		return false
	}
	stderr := strings.ToLower(ce.Stderr)
	for _, s := range substrings {
		if strings.Contains(stderr, strings.ToLower(s)) {
			return true
		}
	}
	return false
}

// sitePhase maps the state of a site's containers to a phase and, unless the
// site is running, a message saying why.
//
//   - running: both components running, and healthy or without a health check
//   - pending: both running, but one's health is still "starting"
//   - stopped: neither component is running (exited, or created by an
//     update of a stopped site)
//   - failed: a component is missing, unhealthy, restarting, paused, dead,
//     or stopped while the other runs. With restart: unless-stopped, a
//     crashing container shows as restarting rather than exited.
func sitePhase(containers []Container) (provisioner.SitePhase, string) {
	var (
		failed, starting []string
		stopped, total   int
	)
	for _, comp := range []provisioner.Component{provisioner.ComponentPHP, provisioner.ComponentNginx} {
		total++
		c, ok := findService(containers, comp)
		if !ok {
			failed = append(failed, fmt.Sprintf("%s container is missing", comp))
			continue
		}
		switch c.State {
		case "running":
			switch c.Health {
			case "", "healthy":
			case "starting":
				starting = append(starting, fmt.Sprintf("%s is starting", comp))
			default:
				failed = append(failed, fmt.Sprintf("%s is %s", comp, c.Health))
			}
		case "exited", "created":
			stopped++
		case "restarting":
			failed = append(failed, fmt.Sprintf("%s is restarting (last exit code %d)", comp, c.ExitCode))
		default: // paused, removing, dead
			failed = append(failed, fmt.Sprintf("%s is %s", comp, c.State))
		}
	}
	switch {
	case len(failed) > 0:
		return provisioner.SiteFailed, strings.Join(failed, "; ")
	case stopped == total:
		return provisioner.SiteStopped, "site is stopped"
	case stopped > 0:
		var msgs []string
		for _, comp := range []provisioner.Component{provisioner.ComponentPHP, provisioner.ComponentNginx} {
			if c, _ := findService(containers, comp); c.State != "running" {
				msgs = append(msgs, fmt.Sprintf("%s is %s (exit code %d)", comp, c.State, c.ExitCode))
			}
		}
		return provisioner.SiteFailed, strings.Join(msgs, "; ")
	case len(starting) > 0:
		return provisioner.SitePending, strings.Join(starting, "; ")
	default:
		return provisioner.SiteRunning, ""
	}
}
