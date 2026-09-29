// Package deploy deploys theme and plugin packages to sites (#113, design
// doc §5.8, decision #60): agency developers and CI pipelines upload a zip
// or tar.gz (package uploads) and deploy it into the site's wp-content with
// an API token, without shell access to the site.
//
// # Deploying
//
// The API (POST /sites/{id}/deploy) calls [Service.Deploy] after
// authorizing the caller (authz.DeployCode). It checks the request and the
// site, which must be running, and in one transaction queues a [JobType]
// job with the site's lock key and claims the upload for it, so the job is
// only queued with its package. The job's steps are:
//
//  1. prepare: the site is still running and the upload still the job's.
//     Its Undo runs last in a rollback and says what the rollback left.
//  2. validate_package: [Inspect] the upload, before anything touches the
//     site: a zip or tar.gz of plain files and directories only, with no
//     paths leaving the package (zip slip), no links or device files, and
//     within [MaxEntries] and [MaxUnpackedBytes].
//  3. stage: copy the upload into a transfer directory and unpack it, in a
//     site task running as the site's UID (so the files are the site's),
//     into a hidden staging directory next to the target (sitetask.go).
//     Undo removes the staging directory.
//  4. swap: exchange the staging directory with the target in one atomic
//     rename, keeping the previous version under a hidden name. Undo swaps
//     the previous version back (or removes a first deployment), reloads
//     PHP, and purges the caches.
//  5. reload_code: provisioner.ReloadCode, so PHP's opcode cache doesn't
//     keep running the previous version's files next to the new ones.
//  6. activate: optionally activate the theme or plugin with WP-CLI,
//     recording what was active before. Undo puts that back.
//  7. purge_object_cache and purge_page_cache.
//  8. health_check: the platform's site health check (sites.Service
//     CheckHealth, §5.5): the home page, requested inside the site's
//     runtime as the edge router would (provisioner.Prober, bypassing the
//     page cache, maintenance mode, and preview protection), must answer
//     without a server error, and WordPress must load under WP-CLI. It
//     retries while the site settles, then the job rolls back.
//  9. remove_previous: the deployment is committed; remove the previous
//     version. Failing to remove it only warns.
//  10. finish: the result, and release the upload.
//
// A deployment doesn't change the site's status: the site keeps serving
// the previous version until the swap, and the previous version again if
// the job rolls back. The job's state and log say how it went.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

// JobType deploys a package to a site.
const JobType = "site.deploy"

// Kind is what a package is, which decides where it goes in wp-content.
type Kind string

// Package kinds.
const (
	KindTheme    Kind = "theme"
	KindPlugin   Kind = "plugin"
	KindMUPlugin Kind = "mu-plugin"
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool { return k == KindTheme || k == KindPlugin || k == KindMUPlugin }

// Dir is the kind's directory in wp-content.
func (k Kind) Dir() string {
	switch k {
	case KindTheme:
		return "themes"
	case KindPlugin:
		return "plugins"
	case KindMUPlugin:
		return "mu-plugins"
	}
	return ""
}

// MaxNameLength bounds a target's directory name.
const MaxNameLength = 100

// namePattern is a target's directory name: a WordPress slug, in any case.
// Names starting with a dot are hidden, which is where deployments keep
// their staging and previous versions.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidateName checks a target's directory name.
func ValidateName(name string) error {
	if len(name) > MaxNameLength || !namePattern.MatchString(name) {
		return fmt.Errorf("must be 1 to %d letters, digits, dots, hyphens, and underscores, starting with a letter or digit", MaxNameLength)
	}
	return nil
}

// Sentinel errors.
var (
	// ErrSiteNotFound means the site doesn't exist.
	ErrSiteNotFound = errors.New("site not found")
	// ErrNotDeployable means the site can't take a deployment now: it
	// isn't running (WP-CLI and the health check need it), another job is
	// provisioning, updating, or deleting it, or its provisioning failed.
	ErrNotDeployable = errors.New("the site can't take a deployment")
)

// FieldError is one invalid field of a request, named by a JSON Pointer.
type FieldError struct {
	Field  string
	Detail string
}

// ValidationError reports an invalid deployment request.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	var parts []string
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Detail)
	}
	return "invalid deployment: " + strings.Join(parts, "; ")
}

// Request is a deployment to queue.
type Request struct {
	// UploadID is the complete upload holding the package, made by the
	// same user for this site (purpose site_deploy).
	UploadID string
	Kind     Kind
	// Name is the target's directory in the kind's directory, e.g.
	// themes/<name>.
	Name string
	// Activate activates the theme or plugin once it is in place; not for
	// must-use plugins, which are always active.
	Activate bool
	// UserID is the user starting the job.
	UserID string
}

// payload is a deployment job's payload.
type payload struct {
	UploadID string `json:"upload_id"`
	Kind     Kind   `json:"kind"`
	Name     string `json:"name"`
	Activate bool   `json:"activate,omitempty"`
}

// Task time limits: unpacking, and the other operations.
const (
	unpackTimeout = 30 * time.Minute
	taskTimeout   = 5 * time.Minute
)

// HealthChecker checks that a running site works; sites.Service implements
// it. Its error says why the site is unhealthy.
type HealthChecker interface {
	CheckHealth(ctx context.Context, site store.Site, wantCore string) error
}

// Options configure a Service.
type Options struct {
	// DB is the state database. Required.
	DB *store.DB
	// Uploads holds the packages. Required.
	Uploads *uploads.Service
	// Provisioner runs WP-CLI, purges the page cache, and reloads PHP.
	// Required.
	Provisioner provisioner.Provisioner
	// Tasks runs the site tasks that unpack and swap packages. Required.
	Tasks provisioner.TaskRunner
	// ObjectCaches flushes the object cache. Required.
	ObjectCaches objectcache.Manager
	// Layout locates the transfer directories (Layout.TmpDir). Required.
	Layout config.Layout
	// Health checks the deployed site: sites.Service.CheckHealth, which
	// retries while the site settles. Required.
	Health HealthChecker
	// Logger defaults to discarding output.
	Logger *slog.Logger
}

// Service queues deployments and runs their jobs.
type Service struct {
	db      *store.DB
	uploads *uploads.Service
	prov    provisioner.Provisioner
	tasks   provisioner.TaskRunner
	caches  objectcache.Manager
	layout  config.Layout
	health  HealthChecker
	log     *slog.Logger
}

// New returns a Service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.Uploads == nil || opts.Provisioner == nil || opts.Tasks == nil ||
		opts.ObjectCaches == nil || opts.Health == nil || opts.Layout.Root == "" {
		return nil, errors.New("deploy: DB, Uploads, Provisioner, Tasks, ObjectCaches, Health, and Layout are required")
	}
	s := &Service{
		db: opts.DB, uploads: opts.Uploads, prov: opts.Provisioner, tasks: opts.Tasks, caches: opts.ObjectCaches,
		layout: opts.Layout, health: opts.Health, log: opts.Logger,
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// LockKey is the lock key of deployment jobs: the site's, shared with every
// job that changes the site (sites.LockKey).
func LockKey(siteID string) string { return "site:" + siteID }

// Deploy checks req and queues a [JobType] job that deploys the uploaded
// package to the site, claiming the upload for it. An invalid request, or
// an upload that doesn't exist, isn't the caller's, or was made for
// something else, is a *ValidationError; a missing site ErrSiteNotFound; a
// site that isn't running, or that another job is working on,
// ErrNotDeployable. An upload that isn't complete or is in use by another
// job is uploads.ErrNotComplete or uploads.ErrInUse. Nothing is queued on
// error.
func (s *Service) Deploy(ctx context.Context, siteID string, req Request) (store.Job, error) {
	var fields []FieldError
	if !req.Kind.Valid() {
		fields = append(fields, FieldError{"/kind", "must be theme, plugin, or mu-plugin"})
	}
	if err := ValidateName(req.Name); err != nil {
		fields = append(fields, FieldError{"/name", err.Error()})
	}
	if req.Activate && req.Kind == KindMUPlugin {
		fields = append(fields, FieldError{"/activate", "must-use plugins are always active; leave activate off"})
	}
	if !id.Valid(req.UploadID) {
		fields = append(fields, FieldError{"/upload_id", "is not an upload ID"})
	}
	if len(fields) > 0 {
		return store.Job{}, &ValidationError{Fields: fields}
	}
	var job store.Job
	err := s.db.InTx(ctx, func(q store.Querier) error {
		if _, err := deployableSite(ctx, q, siteID); err != nil {
			return err
		}
		var err error
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            JobType,
			TargetType:      "site",
			TargetID:        siteID,
			Payload:         payload{UploadID: req.UploadID, Kind: req.Kind, Name: req.Name, Activate: req.Activate},
			LockKey:         LockKey(siteID),
			CreatedByUserID: req.UserID,
		})
		if err != nil {
			return fmt.Errorf("queue deployment to site %s: %w", siteID, err)
		}
		_, err = s.uploads.Claim(ctx, q, req.UploadID, uploads.Use{
			Purpose: store.UploadSiteDeploy, SiteID: siteID, UserID: req.UserID,
		}, job.ID)
		switch {
		case errors.Is(err, uploads.ErrNotFound):
			return &ValidationError{Fields: []FieldError{{"/upload_id", "no upload you started has this ID"}}}
		case errors.Is(err, uploads.ErrWrongUpload):
			return &ValidationError{Fields: []FieldError{{"/upload_id", err.Error()}}}
		}
		return err
	})
	if err != nil {
		return store.Job{}, err
	}
	s.log.InfoContext(ctx, "deployment queued", "site_id", siteID, "job_id", job.ID, "kind", req.Kind,
		"name", req.Name, "upload_id", req.UploadID)
	return job, nil
}

// deployableSite returns the site if it can take a deployment.
func deployableSite(ctx context.Context, q store.Querier, siteID string) (store.Site, error) {
	site, err := store.NewSites(q).Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Site{}, fmt.Errorf("site %s: %w", siteID, ErrSiteNotFound)
	}
	if err != nil {
		return store.Site{}, err
	}
	if site.WPSaltsSecretRef == "" {
		return store.Site{}, fmt.Errorf("%w: it was never provisioned", ErrNotDeployable)
	}
	if site.Status != store.SiteRunning {
		return store.Site{}, fmt.Errorf("%w: it is %s, and must be running", ErrNotDeployable, site.Status)
	}
	return site, nil
}

// siteHost is the host the site is served on: its primary domain once
// live, else its preview domain.
func siteHost(site store.Site) string {
	if site.PrimaryDomain != "" {
		return site.PrimaryDomain
	}
	return site.PreviewDomain
}
