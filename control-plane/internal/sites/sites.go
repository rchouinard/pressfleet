// Package sites implements the site workflows: provisioning (design doc
// §5.1), the one-time retrieval of the initial WordPress admin credentials,
// settings changes and start/stop (§5.5), and deletion. The API validates
// and records a request in one transaction and queues a job; the job's
// steps create, change, or remove the site's runtime resources through
// provisioner.Provisioner and sitedb.Manager.
//
// # Provisioning
//
// [Service.Create] validates the request, then in one transaction allocates
// the site's UID, picks its slug and preview domain, stores its WordPress
// salts and a generated admin password as secrets, inserts the site with
// status provisioning, creates its WP-Cron schedule (package sitecron, which
// skips sites that aren't running), and enqueues a [CreateJobType] job. If
// anything fails, nothing was created.
//
// The job's steps are:
//
//  1. prepare: check that the site still wants provisioning. Its Undo runs
//     last in a rollback and records the failure (see below).
//  2. create_database: sitedb.Manager.CreateDatabase. Undo drops it.
//  3. start_stack: provisioner.CreateSite renders and starts the stack.
//     Undo deletes it, with its volumes and rendered files.
//  4. install_wordpress: `wp core install` with the preview URL, unless
//     `wp core is-installed` says an earlier attempt already did.
//  5. finish: set the status to running.
//
// Every step is idempotent: the database manager and the provisioner
// converge on retries, and the install checks first.
//
// # Failed provisioning
//
// When the job fails for good, the rollback removes the stack and the
// database, deletes the site's salts and admin password secrets and its
// cron schedule, and sets the site's status to failed with the error in its
// status message. The site row stays, rather than disappearing, so the user
// sees what happened where they started it; DELETE removes it, and that job
// finds nothing else left to remove.
//
// # Deletion
//
// [Service.Delete] sets the status to deleting and enqueues a
// [DeleteJobType] job, whose steps remove the stack with its network,
// volumes, and rendered files (provisioner.DeleteSite), the database, its
// user and credentials (sitedb.Manager.DropDatabase), the salts and admin
// password secrets, and finally the site row with its schedules. Each step
// succeeds when there is nothing left to remove, so a retried or repeated
// deletion converges. Deletion can't be rolled back: a job that gives up
// marks the site failed, and deleting it again queues a new job.
//
// # Settings changes
//
// [Service.Update] changes the PHP version and resource limits. It
// validates the merged settings like creation does (a PHP version only when
// it changes, so a site on an end-of-life branch can still get new limits),
// then in one transaction saves them as the site's desired state, sets the
// status to updating, and enqueues an [UpdateJobType] job whose payload
// records the previous settings. The job's steps are:
//
//  1. prepare: check the site isn't being deleted. Its Undo runs last in a
//     rollback (see below).
//  2. apply: provisioner.UpdateSite with the saved settings, which
//     recreates php on the new image; a stopped site stays stopped. Undo
//     applies the previous settings again.
//  3. finish: set the status from the live phase, running or stopped.
//
// A failed update is reverted, not left half applied: the rollback puts the
// previous settings back on the containers and in the site record, and sets
// the status from what the containers are then doing, with the error in the
// status message ("update failed and was reverted: ..."). Only if the
// containers can't be reverted either is the site marked failed; its record
// then holds the previous settings, which a new PATCH can retry from.
//
// # Start and stop
//
// [Service.Start] and [Service.Stop] enqueue [StartJobType] and
// [StopJobType] jobs, which call provisioner.StartSite or StopSite and set
// the status to running or stopped (the WP-Cron job skips stopped sites).
// The status doesn't change while the job waits; a job that gives up marks
// the site failed.
//
// Updating, starting, and stopping are refused with ErrBusy while a job is
// provisioning, updating, or deleting the site, and with ErrNotProvisioned
// for a site whose provisioning failed, which has nothing left to change.
//
// # Startup reconciliation
//
// Install-wide settings such as the WordPress security set (§6.1) reach a
// site whenever the driver renders it. So that a changed setting also
// reaches sites nobody touches, [Service.EnqueueReconcile] queues a
// [ReconcileJobType] job per site when the control plane starts; its steps
// re-render running and stopped sites with provisioner.UpdateSite, which
// recreates only what changed. See reconcile.go for how other features hook
// in. A reconciliation that gives up leaves the site's status alone unless
// its containers are no longer running or stopped, and records the error in
// the status message.
//
// # Live status
//
// [Service.Runtime] asks the provisioner for the site's live phase, within
// a short timeout. If the container runtime can't be reached it reports
// RuntimeUnknown instead of failing, and the recorded status stands.
//
// All site jobs share the lock key "site:<id>", so a deletion requested
// while provisioning runs waits for it. Provisioning checks the status
// before its expensive steps and gives up (rolling back) once it sees the
// site is being deleted, and it never overwrites the deleting status.
//
// # Slugs
//
// A slug is derived from the site name ([Slugify]) and made unique with a
// numeric suffix (acme, acme-2, acme-3, ...). Slugs are never reused while a
// site holds them, including failed sites, and the unique constraints on
// slug and preview domain settle concurrent creations: the loser retries
// with the next free slug.
//
// # Admin credentials
//
// The admin password is generated at creation and stored encrypted.
// [Service.TakeAdminCredentials] returns it once, once WordPress is
// installed, and deletes it in the same transaction; the conditional update
// in store.Sites.TakeAdminPassword makes concurrent callers see it at most
// once.
package sites

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitecron"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// Job types.
const (
	CreateJobType = "site.create"
	DeleteJobType = "site.delete"
	UpdateJobType = "site.update"
	StartJobType  = "site.start"
	StopJobType   = "site.stop"
)

// Sentinel errors.
var (
	// ErrNotFound means the site doesn't exist.
	ErrNotFound = errors.New("site not found")
	// ErrNoPreviewDomain means preview_base_domain isn't configured, so a
	// new site would have no domain.
	ErrNoPreviewDomain = errors.New("the platform has no preview base domain configured (set preview_base_domain)")
	// ErrNotReady means the admin credentials exist but WordPress isn't
	// installed with them yet.
	ErrNotReady = errors.New("the site is still being provisioned")
	// ErrCredentialsGone means the admin credentials were already retrieved,
	// or provisioning failed and deleted them.
	ErrCredentialsGone = errors.New("the initial admin credentials are no longer available")
	// ErrBusy means a job is provisioning, updating, or deleting the site,
	// so it can't be changed until that finishes.
	ErrBusy = errors.New("another job is working on the site; try again when it has finished")
	// ErrNotProvisioned means the site's provisioning failed, so it has
	// nothing to update, start, or stop; it can only be deleted.
	ErrNotProvisioned = errors.New("the site was never provisioned; delete it and create it again")
)

// FieldError is a problem with one field of a request, named by its JSON
// Pointer, e.g. "/php_version".
type FieldError struct {
	Field  string
	Detail string
}

// ValidationError lists the problems with a request.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	var parts []string
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Detail)
	}
	return "invalid site: " + strings.Join(parts, "; ")
}

// Options configures a Service.
type Options struct {
	DB      *store.DB
	Secrets secrets.Store
	Catalog *catalog.Catalog
	// PreviewBaseDomain is config.Config.PreviewBaseDomain. Empty refuses
	// site creation with ErrNoPreviewDomain.
	PreviewBaseDomain string
	// Provisioner and Databases are used by the job handlers.
	Provisioner provisioner.Provisioner
	Databases   sitedb.Manager
	// Logger defaults to discarding output.
	Logger *slog.Logger
	// Now defaults to time.Now; tests fix it for the catalog's support
	// dates.
	Now func() time.Time
	// StatusTimeout bounds each live status query (Service.Runtime); zero
	// means DefaultStatusTimeout.
	StatusTimeout time.Duration
}

// Service runs the site workflows.
type Service struct {
	db          *store.DB
	secrets     secrets.Store
	catalog     *catalog.Catalog
	previewBase string
	prov        provisioner.Provisioner
	databases   sitedb.Manager
	log         *slog.Logger
	now         func() time.Time

	statusTimeout time.Duration
}

// New returns a Service.
func New(opts Options) (*Service, error) {
	var errs []error
	if opts.DB == nil {
		errs = append(errs, errors.New("a state database is required"))
	}
	if opts.Catalog == nil {
		errs = append(errs, errors.New("an image catalog is required"))
	}
	if opts.Provisioner == nil {
		errs = append(errs, errors.New("a provisioner is required"))
	}
	if opts.Databases == nil {
		errs = append(errs, errors.New("a site database manager is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("sites: %w", err)
	}
	s := &Service{
		db:          opts.DB,
		secrets:     opts.Secrets,
		catalog:     opts.Catalog,
		previewBase: strings.TrimSuffix(strings.ToLower(opts.PreviewBaseDomain), "."),
		prov:        opts.Provisioner,
		databases:   opts.Databases,
		log:         opts.Logger,
		now:         opts.Now,

		statusTimeout: opts.StatusTimeout,
	}
	if s.statusTimeout <= 0 {
		s.statusTimeout = DefaultStatusTimeout
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// LockKey is the lock key of every job that changes the site, so they run
// one at a time (package jobs).
func LockKey(siteID string) string { return "site:" + siteID }

// NewSite is a request to create a site. Optional fields are zero when
// unset.
type NewSite struct {
	ClientGroupID string
	Name          string
	AdminEmail    string
	// AdminUsername defaults to a generated name.
	AdminUsername string
	// PHPVersion defaults to catalog.DefaultPHPVersion.
	PHPVersion string
	// Resources override the platform defaults field by field.
	CPU                 *string
	MemoryReservationMB *int
	MemoryLimitMB       *int
	// CreatedByUserID is the user starting the job.
	CreatedByUserID string
}

// createAttempts bounds retries of the create transaction after a
// concurrent creation took the same slug.
const createAttempts = 3

// Create validates req, records the site with status provisioning, and
// queues its provisioning job. Validation problems are a *ValidationError;
// a missing preview base domain is ErrNoPreviewDomain.
func (s *Service) Create(ctx context.Context, req NewSite) (store.Site, store.Job, error) {
	if s.previewBase == "" {
		return store.Site{}, store.Job{}, ErrNoPreviewDomain
	}
	site, err := s.validate(req)
	if err != nil {
		return store.Site{}, store.Job{}, err
	}
	var job store.Job
	for attempt := 1; ; attempt++ {
		site, job, err = s.create(ctx, site, req)
		if errors.Is(err, store.ErrConflict) && attempt < createAttempts {
			continue
		}
		break
	}
	if errors.Is(err, errNoClientGroup) {
		return store.Site{}, store.Job{}, &ValidationError{Fields: []FieldError{{Field: "/client_group_id", Detail: "client group does not exist"}}}
	}
	if err != nil {
		return store.Site{}, store.Job{}, fmt.Errorf("create site: %w", err)
	}
	s.log.InfoContext(ctx, "site provisioning queued", "site_id", site.ID, "slug", site.Slug, "job_id", job.ID)
	return site, job, nil
}

var errNoClientGroup = errors.New("client group does not exist")

// validate checks req and returns the site it describes, without an ID,
// slug, UID, or secrets.
func (s *Service) validate(req NewSite) (store.Site, error) {
	var fields []FieldError
	name := strings.TrimSpace(req.Name)
	if name == "" {
		fields = append(fields, FieldError{"/name", "must not be blank"})
	}
	php := req.PHPVersion
	now := s.now()
	if php == "" {
		var ok bool
		if php, ok = s.catalog.DefaultPHPVersion(now); !ok {
			fields = append(fields, FieldError{"/php_version", "no supported PHP version is available"})
		}
	} else if err := s.catalog.ValidatePHPVersion(php, now); err != nil {
		fields = append(fields, FieldError{"/php_version", phpDetail(s.catalog, err, now)})
	}
	res := provisioner.DefaultResourceLimits()
	if req.CPU != nil {
		res.CPU = *req.CPU
	}
	if req.MemoryReservationMB != nil {
		res.MemoryReservationMB = *req.MemoryReservationMB
	}
	if req.MemoryLimitMB != nil {
		res.MemoryLimitMB = *req.MemoryLimitMB
	}
	fields = append(fields, validateResources(res)...)
	username := req.AdminUsername
	if username == "" {
		username = "admin-" + strings.ToLower(rand.Text()[:6])
	}
	if len(fields) > 0 {
		return store.Site{}, &ValidationError{Fields: fields}
	}
	return store.Site{
		ClientGroupID:       req.ClientGroupID,
		Name:                name,
		Status:              store.SiteProvisioning,
		PHPVersion:          php,
		WPCoreVersion:       s.catalog.LatestWordPress().Version,
		EnableCache:         true,
		CPULimit:            res.CPU,
		MemoryReservationMB: res.MemoryReservationMB,
		MemoryLimitMB:       res.MemoryLimitMB,
		AdminUsername:       username,
		AdminEmail:          req.AdminEmail,
	}, nil
}

// phpDetail explains a rejected PHP version and lists the ones on offer.
func phpDetail(c *catalog.Catalog, err error, now time.Time) string {
	var offered []string
	for _, p := range c.AvailablePHPVersions(now) {
		offered = append(offered, p.Version)
	}
	reason := "is not a supported PHP version"
	if errors.Is(err, catalog.ErrPHPVersionEOL) {
		reason = "is past end of life"
	}
	return reason + "; use one of " + strings.Join(offered, ", ")
}

// validateResources applies the platform minimums (provisioner.Min*).
func validateResources(r provisioner.ResourceLimits) []FieldError {
	var fields []FieldError
	cpu, err := strconv.ParseFloat(r.CPU, 64)
	minCPU := float64(provisioner.MinCPUMillis) / 1000
	switch {
	case err != nil || cpu > 1024:
		fields = append(fields, FieldError{"/resources/cpu", "must be a number of CPUs, e.g. 1.0"})
	case cpu < minCPU:
		fields = append(fields, FieldError{"/resources/cpu", fmt.Sprintf("must be at least %g", minCPU)})
	}
	if r.MemoryLimitMB < provisioner.MinMemoryLimitMB {
		fields = append(fields, FieldError{"/resources/memory_limit_mb", fmt.Sprintf("must be at least %d", provisioner.MinMemoryLimitMB)})
	}
	switch {
	case r.MemoryReservationMB < provisioner.MinMemoryReservationMB:
		fields = append(fields, FieldError{"/resources/memory_reservation_mb", fmt.Sprintf("must be at least %d", provisioner.MinMemoryReservationMB)})
	case r.MemoryReservationMB > r.MemoryLimitMB:
		fields = append(fields, FieldError{"/resources/memory_reservation_mb", "must not exceed memory_limit_mb"})
	}
	return fields
}

// create runs the creation transaction once.
func (s *Service) create(ctx context.Context, site store.Site, req NewSite) (store.Site, store.Job, error) {
	var job store.Job
	err := s.db.InTx(ctx, func(q store.Querier) error {
		if _, err := store.NewClientGroups(q).Get(ctx, site.ClientGroupID); errors.Is(err, store.ErrNotFound) {
			return errNoClientGroup
		} else if err != nil {
			return err
		}
		uid, err := store.AllocateSiteUID(ctx, q)
		if err != nil {
			return err
		}
		sites := store.NewSites(q)
		slug, err := uniqueSlug(ctx, sites, Slugify(site.Name), s.previewBase)
		if err != nil {
			return err
		}
		salts, err := wpconfig.MarshalSalts(wpconfig.GenerateSalts())
		if err != nil {
			return err
		}
		sec := s.secrets.With(q)
		saltsRef, err := sec.Put(ctx, secrets.KindSiteWPSalts, salts)
		if err != nil {
			return err
		}
		passwordRef, err := sec.Put(ctx, secrets.KindSiteWPAdminPassword, newAdminPassword())
		if err != nil {
			return err
		}
		site.UID, site.Slug, site.PreviewDomain = uid, slug, previewDomain(slug, s.previewBase)
		site.WPSaltsSecretRef, site.AdminPasswordSecretRef = string(saltsRef), string(passwordRef)
		if err := sites.Create(ctx, &site); errors.Is(err, store.ErrReference) {
			// The group was deleted after the check above.
			return errNoClientGroup
		} else if err != nil {
			return err
		}
		if err := sitecron.EnsureSchedule(ctx, q, site.ID); err != nil {
			return err
		}
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            CreateJobType,
			TargetType:      "site",
			TargetID:        site.ID,
			LockKey:         LockKey(site.ID),
			CreatedByUserID: req.CreatedByUserID,
		})
		return err
	})
	return site, job, err
}

// newAdminPassword returns 26 random base32 characters (130 bits).
func newAdminPassword() redact.Secret {
	return redact.Secret(rand.Text())
}

// Get returns the site and its alias domains, or ErrNotFound.
func (s *Service) Get(ctx context.Context, siteID string) (store.Site, []string, error) {
	sites := store.NewSites(s.db)
	site, err := sites.Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Site{}, nil, fmt.Errorf("site %s: %w", siteID, ErrNotFound)
	}
	if err != nil {
		return store.Site{}, nil, err
	}
	aliases, err := sites.Aliases(ctx, siteID)
	if err != nil {
		return store.Site{}, nil, err
	}
	return site, aliases, nil
}

// URL returns the site's public URL: its primary domain once live, else its
// preview domain. Both are served over HTTPS.
func URL(site store.Site) string {
	host := site.PreviewDomain
	if site.PrimaryDomain != "" {
		host = site.PrimaryDomain
	}
	return "https://" + host
}

// AdminCredentials are a site's initial WordPress administrator login.
type AdminCredentials struct {
	Username string
	Email    string
	Password redact.Secret
	LoginURL string
}

// TakeAdminCredentials returns the site's initial admin credentials and
// deletes the stored password, so they can be taken once. It returns
// ErrNotFound for a missing site, ErrNotReady while the site is being
// provisioned, and ErrCredentialsGone once they have been taken (or
// provisioning failed).
func (s *Service) TakeAdminCredentials(ctx context.Context, siteID string) (AdminCredentials, error) {
	var creds AdminCredentials
	err := s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		site, err := sites.Get(ctx, siteID)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("site %s: %w", siteID, ErrNotFound)
		}
		if err != nil {
			return err
		}
		ref := site.AdminPasswordSecretRef
		switch {
		case ref == "":
			return ErrCredentialsGone
		case site.Status == store.SiteProvisioning:
			return ErrNotReady
		}
		sec := s.secrets.With(q)
		password, err := sec.Get(ctx, secrets.Ref(ref))
		if err != nil {
			return err
		}
		taken, err := sites.TakeAdminPassword(ctx, siteID, ref)
		if err != nil {
			return err
		}
		if !taken {
			return ErrCredentialsGone
		}
		if err := sec.Delete(ctx, secrets.Ref(ref)); err != nil {
			return err
		}
		creds = AdminCredentials{
			Username: site.AdminUsername,
			Email:    site.AdminEmail,
			Password: password,
			LoginURL: URL(site) + "/wp-login.php",
		}
		return nil
	})
	if err != nil {
		return AdminCredentials{}, err
	}
	s.log.InfoContext(ctx, "site admin credentials retrieved", "site_id", siteID)
	return creds, nil
}

// Handlers returns the job handlers of the site workflows, to register on
// the job pool.
func (s *Service) Handlers() []jobs.Handler {
	return []jobs.Handler{
		s.createHandler(), s.deleteHandler(), s.updateHandler(), s.startHandler(), s.stopHandler(),
		s.reconcileHandler(),
	}
}

// SharedServiceAttacher reconnects the platform services to a site's
// network; the Compose provisioner implements it.
type SharedServiceAttacher interface {
	AttachSharedServices(ctx context.Context, siteID string) error
}

// ReattachSharedServices reconnects the platform services to the network of
// every site that has runtime resources. The Compose driver needs it after
// a platform service container (MariaDB, the object cache) is recreated,
// which drops its connections to site networks. It is best effort: failures
// are logged, not returned, and sites being deleted are skipped so a delete
// job isn't raced.
func (s *Service) ReattachSharedServices(ctx context.Context, a SharedServiceAttacher) {
	ids, err := store.NewSites(s.db).ListIDs(ctx, store.SiteDeleting)
	if err != nil {
		s.log.WarnContext(ctx, "listing sites to reattach platform services failed", "error", err)
		return
	}
	attached := 0
	for _, id := range ids {
		switch err := a.AttachSharedServices(ctx, id); {
		case err == nil:
			attached++
		case errors.Is(err, provisioner.ErrSiteNotFound):
			s.log.DebugContext(ctx, "site has no runtime resources to reattach", "site_id", id)
		default:
			s.log.WarnContext(ctx, "reattaching platform services to a site network failed", "site_id", id, "error", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
	if len(ids) > 0 {
		s.log.InfoContext(ctx, "platform services reattached to site networks", "sites", attached, "total", len(ids))
	}
}
