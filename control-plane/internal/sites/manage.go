package sites

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Managing a site after creation (#28): listing, live status, settings
// changes, and start/stop. See the package documentation for the job
// lifecycle and the failure policy.

// DefaultStatusTimeout bounds how long [Service.Runtime] waits for the
// provisioner, so a slow or unreachable container runtime delays a site's
// detail page by a moment rather than failing it.
const DefaultStatusTimeout = 3 * time.Second

// List returns a page of sites and their aliases, keyed by site ID, from
// the state database alone: listing never asks the container runtime.
func (s *Service) List(ctx context.Context, f store.SiteFilter) (store.SitePage, map[string][]string, error) {
	sites := store.NewSites(s.db)
	page, err := sites.List(ctx, f)
	if err != nil {
		return store.SitePage{}, nil, err
	}
	ids := make([]string, len(page.Sites))
	for i, site := range page.Sites {
		ids[i] = site.ID
	}
	aliases, err := sites.AliasesForSites(ctx, ids)
	if err != nil {
		return store.SitePage{}, nil, err
	}
	return page, aliases, nil
}

// RuntimePhase is a site's live runtime phase as the API reports it: a
// provisioner.SitePhase, or one of the two phases for when there is none.
type RuntimePhase string

// Runtime phases beyond those of provisioner.SitePhase.
const (
	// RuntimeAbsent means the site has no runtime resources, e.g. before
	// provisioning has started them or after it failed and removed them.
	RuntimeAbsent RuntimePhase = "absent"
	// RuntimeUnknown means the container runtime couldn't be asked; the
	// site's recorded status is all there is.
	RuntimeUnknown RuntimePhase = "unknown"
)

// Runtime is the live state of a site's containers.
type Runtime struct {
	Phase   RuntimePhase
	Message string
}

// Runtime asks the provisioner for the site's live state, within the
// status timeout. It never fails: when the runtime can't be reached it
// reports RuntimeUnknown and logs why, so callers fall back to the recorded
// status.
func (s *Service) Runtime(ctx context.Context, siteID string) Runtime {
	ctx, cancel := context.WithTimeout(ctx, s.statusTimeout)
	defer cancel()
	st, err := s.prov.GetSite(ctx, siteID)
	switch {
	case err == nil:
		return Runtime{Phase: RuntimePhase(st.Phase), Message: st.Message}
	case errors.Is(err, provisioner.ErrSiteNotFound):
		return Runtime{Phase: RuntimeAbsent}
	case errors.Is(err, context.DeadlineExceeded):
		s.log.WarnContext(ctx, "reading live site status timed out", "site_id", siteID, "timeout", s.statusTimeout)
		return Runtime{Phase: RuntimeUnknown, Message: "The container runtime didn't answer in time; the status is the last one recorded."}
	default:
		s.log.WarnContext(ctx, "reading live site status failed", "site_id", siteID, "error", err)
		return Runtime{Phase: RuntimeUnknown, Message: "The container runtime couldn't be reached; the status is the last one recorded."}
	}
}

// SiteUpdate is a request to change a site's settings. Nil fields keep
// their current value.
type SiteUpdate struct {
	PHPVersion          *string
	CPU                 *string
	MemoryReservationMB *int
	MemoryLimitMB       *int
	// PreviewProtection turns preview password protection on or off, and
	// PreviewUsername changes its username (see preview.go).
	PreviewProtection *bool
	PreviewUsername   *string
	// CreatedByUserID is the user starting the job.
	CreatedByUserID string
}

// updatePayload is the payload of a site.update job. The previous settings
// are recorded when the request is accepted, because the site row holds the
// new ones from then on; rolling back restores them.
type updatePayload struct {
	Previous store.SiteSettings `json:"previous"`
}

// Update validates the requested settings, saves them as the site's
// desired state with status updating, and queues a [UpdateJobType] job that
// applies them. It returns the site as saved and the job. If the update
// turns preview protection on for the first time, it generates the preview
// password and returns the credentials too, the only time they are
// returned; otherwise they are nil. Validation problems are a
// *ValidationError; a site that a job is working on, or that never finished
// provisioning, is ErrBusy or ErrNotProvisioned.
func (s *Service) Update(ctx context.Context, siteID string, req SiteUpdate) (store.Site, store.Job, *PreviewCredentials, error) {
	var (
		site    store.Site
		job     store.Job
		preview *PreviewCredentials
	)
	err := s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		current, err := s.manageable(ctx, sites, siteID)
		if err != nil {
			return err
		}
		next, err := s.validateUpdate(current, req)
		if err != nil {
			return err
		}
		if _, err := sites.SetSettings(ctx, siteID, next); err != nil {
			return err
		}
		if next.PreviewProtection && current.PreviewPasswordSecretRef == "" {
			password, err := s.replacePreviewPassword(ctx, q, current)
			if err != nil {
				return err
			}
			preview = &PreviewCredentials{Username: next.PreviewUsername, Password: password}
		}
		if _, err := sites.SetStatus(ctx, siteID, store.SiteUpdating, ""); err != nil {
			return err
		}
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            UpdateJobType,
			TargetType:      "site",
			TargetID:        siteID,
			Payload:         updatePayload{Previous: current.Settings()},
			LockKey:         LockKey(siteID),
			CreatedByUserID: req.CreatedByUserID,
		})
		if err != nil {
			return err
		}
		site, err = sites.Get(ctx, siteID)
		return err
	})
	if err != nil {
		return store.Site{}, store.Job{}, nil, err
	}
	s.log.InfoContext(ctx, "site update queued", "site_id", siteID, "job_id", job.ID, "php_version", site.PHPVersion,
		"preview_protection", site.PreviewProtection)
	return site, job, preview, nil
}

// manageable returns the site if it can be updated, started, or stopped:
// no job may be working on it (provisioning, updating, deleting), and it
// must have finished provisioning. A site whose provisioning failed has no
// containers or secrets left; it can only be deleted.
func (s *Service) manageable(ctx context.Context, sites store.Sites, siteID string) (store.Site, error) {
	site, err := sites.Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Site{}, fmt.Errorf("site %s: %w", siteID, ErrNotFound)
	}
	if err != nil {
		return store.Site{}, err
	}
	switch site.Status {
	case store.SiteProvisioning, store.SiteUpdating, store.SiteDeleting:
		return store.Site{}, fmt.Errorf("%w: the site is %s", ErrBusy, site.Status)
	}
	if site.WPSaltsSecretRef == "" {
		return store.Site{}, ErrNotProvisioned
	}
	return site, nil
}

// validateUpdate merges req into the site's settings and validates the
// result like creation does. The PHP version is only checked when it
// changes, so a site on a branch past end of life can still get new
// resource limits.
func (s *Service) validateUpdate(site store.Site, req SiteUpdate) (store.SiteSettings, error) {
	next := site.Settings()
	var fields []FieldError
	if req.PHPVersion != nil && *req.PHPVersion != site.PHPVersion {
		now := s.now()
		if err := s.catalog.ValidatePHPVersion(*req.PHPVersion, now); err != nil {
			fields = append(fields, FieldError{"/php_version", phpDetail(s.catalog, err, now)})
		}
		next.PHPVersion = *req.PHPVersion
	}
	if req.CPU != nil {
		next.CPULimit = *req.CPU
	}
	if req.MemoryReservationMB != nil {
		next.MemoryReservationMB = *req.MemoryReservationMB
	}
	if req.MemoryLimitMB != nil {
		next.MemoryLimitMB = *req.MemoryLimitMB
	}
	fields = append(fields, validateResources(provisioner.ResourceLimits{
		CPU:                 next.CPULimit,
		MemoryReservationMB: next.MemoryReservationMB,
		MemoryLimitMB:       next.MemoryLimitMB,
	})...)
	if req.PreviewProtection != nil {
		next.PreviewProtection = *req.PreviewProtection
	}
	if req.PreviewUsername != nil {
		if err := provisioner.ValidateBasicAuthUsername(*req.PreviewUsername); err != nil {
			fields = append(fields, FieldError{"/preview_protection/username", err.Error()})
		}
		next.PreviewUsername = *req.PreviewUsername
	}
	if next.PreviewProtection && next.PreviewUsername == "" {
		// The slug is a valid username, and no secret: it is in the
		// preview domain.
		next.PreviewUsername = site.Slug
	}
	if len(fields) > 0 {
		return store.SiteSettings{}, &ValidationError{Fields: fields}
	}
	return next, nil
}

// Start queues a [StartJobType] job that starts the site's containers.
func (s *Service) Start(ctx context.Context, siteID, createdByUserID string) (store.Job, error) {
	return s.enqueueSiteJob(ctx, siteID, StartJobType, createdByUserID)
}

// Stop queues a [StopJobType] job that stops the site's containers,
// keeping its data.
func (s *Service) Stop(ctx context.Context, siteID, createdByUserID string) (store.Job, error) {
	return s.enqueueSiteJob(ctx, siteID, StopJobType, createdByUserID)
}

// enqueueSiteJob queues a job that works on a manageable site: start, stop,
// or a cache purge. The status doesn't change until the job has done its
// work: there is no "starting" status, and the jobs are quick. Starting a
// running site, or stopping a stopped one, converges.
func (s *Service) enqueueSiteJob(ctx context.Context, siteID, jobType, createdByUserID string) (store.Job, error) {
	var job store.Job
	err := s.db.InTx(ctx, func(q store.Querier) error {
		if _, err := s.manageable(ctx, store.NewSites(q), siteID); err != nil {
			return err
		}
		var err error
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            jobType,
			TargetType:      "site",
			TargetID:        siteID,
			LockKey:         LockKey(siteID),
			CreatedByUserID: createdByUserID,
		})
		return err
	})
	if err != nil {
		return store.Job{}, err
	}
	s.log.InfoContext(ctx, "site job queued", "site_id", siteID, "job_type", jobType, "job_id", job.ID)
	return job, nil
}

// Step names of the site.update, site.start, and site.stop jobs. Keep them
// stable: a queued job resumes by step name.
const (
	stepPrepareUpdate = "prepare"
	stepApply         = "apply"
	stepStart         = "start"
	stepStop          = "stop"
	stepFinishManage  = "finish"
)

// dataRevertError is the site.update job data key recording that putting
// the previous settings back failed, so the final rollback step marks the
// site failed instead of restoring its status.
const dataRevertError = "revert_error"

func (s *Service) updateHandler() jobs.Handler {
	return jobs.Handler{
		Type: UpdateJobType,
		Steps: []jobs.Step{
			{Name: stepPrepareUpdate, Do: s.prepareUpdate, Undo: s.revertSettings},
			{Name: stepApply, Do: s.applyUpdate, Undo: s.revertApply},
			{Name: stepFinishManage, Do: s.finishUpdate},
		},
	}
}

func (s *Service) startHandler() jobs.Handler {
	return jobs.Handler{
		Type: StartJobType,
		Steps: []jobs.Step{
			{Name: stepStart, Do: s.startSite, Undo: s.failPower},
			{Name: stepFinishManage, Do: s.finishPower(store.SiteRunning)},
		},
	}
}

func (s *Service) stopHandler() jobs.Handler {
	return jobs.Handler{
		Type: StopJobType,
		Steps: []jobs.Step{
			{Name: stepStop, Do: s.stopSite, Undo: s.failPower},
			{Name: stepFinishManage, Do: s.finishPower(store.SiteStopped)},
		},
	}
}

// managedSite loads the site of an update, start, or stop job, and stops
// the job if the site is being deleted or has nothing to manage.
func (s *Service) managedSite(ctx context.Context, r *jobs.Run) (store.Site, error) {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return store.Site{}, err
	}
	if err := abortIfDeleting(site); err != nil {
		return store.Site{}, err
	}
	if site.WPSaltsSecretRef == "" {
		return store.Site{}, jobs.Permanent(ErrNotProvisioned)
	}
	return site, nil
}

func (s *Service) prepareUpdate(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	protection := "off"
	if previewAuth(site) != nil {
		protection = "on for user " + site.PreviewUsername
	}
	r.Logf("Applying PHP %s, resources of %s CPU, %d MiB reserved, %d MiB limit, and preview password protection %s.",
		site.PHPVersion, site.CPULimit, site.MemoryReservationMB, site.MemoryLimitMB, protection)
	return nil
}

// applyUpdate converges the site's containers to its saved settings. The
// provisioner recreates php when its image changes and keeps a stopped site
// stopped.
func (s *Service) applyUpdate(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	st, err := s.converge(ctx, r, site)
	if err != nil {
		return err
	}
	r.Logf("Containers are updated (%s).", st.Phase)
	return nil
}

// converge applies the site's settings with provisioner.UpdateSite. A site
// without containers can't be updated, so that is permanent.
func (s *Service) converge(ctx context.Context, r *jobs.Run, site store.Site) (*provisioner.SiteStatus, error) {
	// Ask first, so a missing stack doesn't make the database and object
	// cache managers recreate a database and a user for nothing.
	if _, err := s.prov.GetSite(ctx, site.ID); err != nil {
		if errors.Is(err, provisioner.ErrSiteNotFound) {
			return nil, jobs.Permanent(fmt.Errorf("the site has no containers: %w", err))
		}
		return nil, err
	}
	dbRef, cacheRef, err := s.secretRefs(ctx, r)
	if err != nil {
		return nil, err
	}
	cfg := SiteConfig(site, dbRef, cacheRef)
	if err := cfg.Validate(); err != nil {
		return nil, jobs.Permanent(err)
	}
	st, err := s.prov.UpdateSite(ctx, cfg)
	if errors.Is(err, provisioner.ErrSiteNotFound) {
		return nil, jobs.Permanent(err)
	}
	return st, err
}

// revertApply puts the previous settings back on the containers. If that
// keeps failing, it gives up on its last attempt without failing the
// rollback, and records why, so revertSettings still runs and marks the
// site failed: a rollback_failed job would leave the site updating, which
// refuses every further change.
func (s *Service) revertApply(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil || site.Status != store.SiteUpdating {
		// Gone or being deleted: nothing worth reverting.
		return nil
	}
	var p updatePayload
	if err := r.Payload(&p); err != nil {
		return jobs.Permanent(err)
	}
	_, err = s.converge(ctx, r, site.WithSettings(p.Previous))
	if err == nil {
		r.Logf("Put the previous settings back (PHP %s).", p.Previous.PHPVersion)
		return nil
	}
	if !jobs.IsPermanent(err) && r.Job().Attempts < r.Job().MaxAttempts {
		return err
	}
	r.Warnf("Couldn't put the previous settings back: %v", err)
	return r.Set(dataRevertError, err.Error())
}

// revertSettings is the last Undo of a failed site.update job. It restores
// the previous settings in the site record and sets the status from what
// the containers are doing, with the error in the status message. If the
// containers couldn't be reverted, the site is marked failed. A site being
// deleted is left alone.
func (s *Service) revertSettings(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	var p updatePayload
	if err := r.Payload(&p); err != nil {
		return jobs.Permanent(err)
	}
	var revertErr string
	if _, err := r.Get(dataRevertError, &revertErr); err != nil {
		return err
	}
	status, msg := store.SiteFailed, statusMessage("update failed", r.Job().Error)
	if revertErr == "" {
		status = s.observedStatus(ctx, siteID)
		msg = statusMessage("update failed and was reverted", r.Job().Error)
	}
	err := s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		site, err := sites.Get(ctx, siteID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && site.Status != store.SiteUpdating) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := sites.SetSettings(ctx, siteID, p.Previous); err != nil {
			return err
		}
		_, err = sites.SetStatus(ctx, siteID, status, msg, store.SiteUpdating)
		return err
	})
	if err != nil {
		return err
	}
	r.Warnf("Restored the previous settings (PHP %s); the site is %s.", p.Previous.PHPVersion, status)
	return nil
}

// observedStatus maps the site's live phase to the status to record once a
// job is done with it. Anything but running or stopped, including a runtime
// that can't be asked, is failed.
func (s *Service) observedStatus(ctx context.Context, siteID string) store.SiteStatus {
	st, err := s.prov.GetSite(ctx, siteID)
	if err != nil {
		return store.SiteFailed
	}
	switch st.Phase {
	case provisioner.SiteRunning:
		return store.SiteRunning
	case provisioner.SiteStopped:
		return store.SiteStopped
	}
	return store.SiteFailed
}

// finishUpdate sets the status from what the containers are doing: running,
// or stopped for a site that was stopped. A site being deleted keeps its
// status.
func (s *Service) finishUpdate(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	st, err := s.prov.GetSite(ctx, siteID)
	if err != nil {
		return err
	}
	var status store.SiteStatus
	switch st.Phase {
	case provisioner.SiteRunning:
		status = store.SiteRunning
	case provisioner.SiteStopped:
		status = store.SiteStopped
	default:
		// UpdateSite waits for a started site to be healthy, so this is
		// the site failing right after; retrying finds out.
		return fmt.Errorf("the site is %s after the update: %s", st.Phase, st.Message)
	}
	changed, err := store.NewSites(s.db).SetStatus(ctx, siteID, status, "", store.SiteUpdating)
	if err != nil {
		return err
	}
	if err := r.SetResult("site_id", siteID); err != nil {
		return err
	}
	if !changed {
		r.Warnf("The site's status changed while it was updated; leaving it alone.")
		return nil
	}
	r.Logf("The site is %s with its new settings.", status)
	return nil
}

func (s *Service) startSite(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	if err := s.prov.StartSite(ctx, site.ID); err != nil {
		if errors.Is(err, provisioner.ErrSiteNotFound) {
			return jobs.Permanent(err)
		}
		return err
	}
	r.Logf("Containers are up.")
	return nil
}

func (s *Service) stopSite(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	if err := s.prov.StopSite(ctx, site.ID); err != nil {
		if errors.Is(err, provisioner.ErrSiteNotFound) {
			return jobs.Permanent(err)
		}
		return err
	}
	r.Logf("Containers are stopped.")
	return nil
}

// powerStatuses are the statuses a start or stop job may replace: the ones
// that mean no other job is working on the site. An update queued behind
// the job owns the status (updating) and sets it itself.
var powerStatuses = []store.SiteStatus{store.SiteRunning, store.SiteStopped, store.SiteFailed}

// failPower records why a start or stop job gave up. The containers may be
// half started or stopped, so the site is marked failed; starting or
// stopping it again retries.
func (s *Service) failPower(ctx context.Context, r *jobs.Run) error {
	prefix := "starting failed"
	if r.Job().Type == StopJobType {
		prefix = "stopping failed"
	}
	msg := statusMessage(prefix, r.Job().Error)
	if _, err := store.NewSites(s.db).SetStatus(ctx, r.Job().TargetID, store.SiteFailed, msg, powerStatuses...); err != nil {
		return err
	}
	r.Warnf("The site is marked failed.")
	return nil
}

func (s *Service) finishPower(status store.SiteStatus) jobs.StepFunc {
	return func(ctx context.Context, r *jobs.Run) error {
		siteID := r.Job().TargetID
		changed, err := store.NewSites(s.db).SetStatus(ctx, siteID, status, "", powerStatuses...)
		if err != nil {
			return err
		}
		if err := r.SetResult("site_id", siteID); err != nil {
			return err
		}
		if !changed {
			r.Warnf("Another job owns the site's status; leaving it alone.")
			return nil
		}
		r.Logf("The site is %s.", status)
		return nil
	}
}
