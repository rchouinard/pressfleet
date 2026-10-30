package sites

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/pluginupdates"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitecron"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/wpconfig"
)

// Creating a staging site from a production site (#202, design doc §5.11).
//
// [Service.CreateStaging] checks the production site (a production site,
// running or stopped) and the request, then in one transaction:
//
//   - locks the production site's row and checks it again, so a deletion
//     of the production site can't slip past its staging site guard (see
//     Delete and store.Sites.Lock);
//   - allocates the staging site like import's allocate step does, with
//     status provisioning: its own UID, salts, and (later, in the job)
//     database and object cache credentials; a slug derived from the
//     production site's (acme-staging, acme-staging-2, ...); the production
//     site's PHP version, core, table prefix, resources (unless
//     overridden), and initial admin username and email, but no admin
//     password, because production's users come with the copy; preview
//     protection on with generated credentials, returned once; outbound
//     mail off; cron and plugin update schedules as for any site, and
//     scheduled backups off;
//   - unless the request names a snapshot, queues a site.backup job of
//     kind sync on the production site, under the production site's lock
//     key;
//   - queues a [CloneJobType] job on the staging site, under its own lock
//     key, after the backup job (package jobs, "Job chains").
//
// The site.clone job provisions the staging site with production's
// content. Its steps are those of provisioning, restore, and import:
//
//  1. prepare: read the snapshot from the awaited backup job's result, or
//     from the payload, and check it is still in the repository. A failed
//     backup fails the job permanently (jobs.AwaitedJob.Err). If the
//     snapshot was taken on a newer core than the staging site's, the
//     site moves to an image with at least that core before its stack
//     starts. Its Undo runs last in a rollback (see below).
//  2. create_database, create_object_cache, start_stack: as provisioning.
//  3. restore: backup.Engine.Restore of the production snapshot into the
//     staging site.
//  4. restart_site and upgrade_database: as a restore.
//  5. rewrite_urls: as a restore, from the hosts the restored home and
//     siteurl name and from the production site's current host, to the
//     staging site's preview domain.
//  6. configure_plugins: the default plugin stack, as provisioning.
//  7. purge_object_cache and purge_page_cache, then finish: record the
//     snapshot and the time on the staging site, and set it running.
//
// # Failure
//
// A clone that fails for good rolls back like provisioning: the stack, the
// object cache user, and the database are removed, the secrets and
// schedules are deleted, and the staging site is marked failed with the
// error. Its row stays, so the user sees what happened on the production
// site's page, where the clone was started; deleting it removes the rest.
// The production site isn't touched: its sync backup is a backup like any
// other.

// CloneJobType creates a staging site's runtime resources and fills it with
// a snapshot of its production site.
const CloneJobType = "site.clone"

// Job data keys of the site.clone job.
const (
	// dataCloneSnapshot is the full ID of the snapshot being cloned, once
	// prepare resolved it.
	dataCloneSnapshot = "clone_snapshot_id"
	// dataCloneBackupID is the ID of the sync backup the job waited for.
	dataCloneBackupID = "clone_backup_id"
)

// ErrSourceFailed means a staging site can't be created or refreshed from a
// fresh backup of a production site that is marked failed: it has nothing
// reliable to copy.
var ErrSourceFailed = errors.New("the production site is marked failed; restore or fix it before copying it to a staging site")

// Limits of a staging site's name and slug.
const (
	// stagingSlugSuffix is appended to the production site's slug.
	stagingSlugSuffix = "-staging"
	// stagingNameSuffix is appended to the production site's name.
	stagingNameSuffix = " (staging)"
	// maxSiteName is the longest site name the API accepts.
	maxSiteName = 100
)

// NewStaging is a request to create a staging site. Optional fields are
// zero when unset.
type NewStaging struct {
	// Name defaults to "<production name> (staging)".
	Name string
	// The resources override the production site's field by field.
	CPU                 *string
	MemoryReservationMB *int
	MemoryLimitMB       *int
	// SnapshotID is an existing snapshot of the production site, or a
	// unique prefix of at least 8 characters, to copy instead of taking a
	// fresh sync backup.
	SnapshotID string
	// CreatedByUserID is the user starting the jobs.
	CreatedByUserID string
}

// StagingCreated is what CreateStaging recorded and queued.
type StagingCreated struct {
	// Site is the staging site, with status provisioning.
	Site store.Site
	// Job is the site.clone job.
	Job store.Job
	// BackupJob is the sync backup of the production site that Job waits
	// for; nil when the request named a snapshot.
	BackupJob *store.Job
	// Preview is the staging site's preview domain login, which is never
	// returned again.
	Preview PreviewCredentials
}

// clonePayload is the payload of a site.clone job.
type clonePayload struct {
	// SourceSiteID is the production site.
	SourceSiteID string `json:"source_site_id"`
	// SnapshotID is the snapshot to clone; empty means the one the awaited
	// backup job took.
	SnapshotID string `json:"snapshot_id,omitempty"`
}

// CreateStaging creates a staging site of the production site and queues
// the jobs that fill it with the production site's content. A staging site
// as the source, or an invalid name, resources, or snapshot, is a
// *ValidationError; a production site that a job is working on (including
// its deletion) is ErrBusy, one that was never provisioned
// ErrNotProvisioned, and one marked failed ErrSourceFailed. Nothing changes
// then.
func (s *Service) CreateStaging(ctx context.Context, productionID string, req NewStaging) (StagingCreated, error) {
	if !s.restores() {
		return StagingCreated{}, ErrNoBackups
	}
	if s.previewBase == "" {
		return StagingCreated{}, ErrNoPreviewDomain
	}
	// Fail fast, before asking the repository; the transaction checks
	// again.
	prod, err := store.NewSites(s.db).Get(ctx, productionID)
	if errors.Is(err, store.ErrNotFound) {
		return StagingCreated{}, fmt.Errorf("site %s: %w", productionID, ErrNotFound)
	}
	if err != nil {
		return StagingCreated{}, err
	}
	if err := checkStagingSource(prod); err != nil {
		return StagingCreated{}, err
	}
	site, err := validateStaging(prod, req)
	if err != nil {
		return StagingCreated{}, err
	}
	var snapID string
	if req.SnapshotID != "" {
		snap, err := s.findSnapshot(ctx, prod.ID, req.SnapshotID)
		if errors.Is(err, backup.ErrSnapshotNotFound) {
			return StagingCreated{}, &ValidationError{Fields: []FieldError{{"/snapshot_id", "is not a snapshot of site " + prod.ID + " in its backup repository"}}}
		}
		if err != nil {
			return StagingCreated{}, err
		}
		core, err := s.coreFor(snap.WPCoreVersion, site.WPCoreVersion)
		if err != nil {
			return StagingCreated{}, &ValidationError{Fields: []FieldError{{"/snapshot_id", err.Error()}}}
		}
		if core != "" {
			site.WPCoreVersion = core
		}
		snapID = string(snap.ID)
	}

	var out StagingCreated
	for attempt := 1; ; attempt++ {
		out, err = s.createStaging(ctx, prod.ID, site, snapID, req.CreatedByUserID)
		if errors.Is(err, store.ErrConflict) && attempt < createAttempts {
			continue // a concurrent creation took the slug
		}
		break
	}
	if err != nil {
		return StagingCreated{}, err
	}
	attrs := []any{"site_id", out.Site.ID, "slug", out.Site.Slug, "parent_site_id", prod.ID, "job_id", out.Job.ID}
	if out.BackupJob != nil {
		attrs = append(attrs, "backup_job_id", out.BackupJob.ID)
	} else {
		attrs = append(attrs, "snapshot_id", snapID)
	}
	s.log.InfoContext(ctx, "staging site creation queued", attrs...)
	return out, nil
}

// checkStagingSource checks that a staging site can be created from the
// site, or refreshed from a fresh backup of it: a production site that is
// running or stopped.
func checkStagingSource(prod store.Site) error {
	if prod.IsStaging() {
		return &ValidationError{Fields: []FieldError{{"/site_id", "is a staging site; staging sites are created from their production site"}}}
	}
	switch prod.Status {
	case store.SiteRunning, store.SiteStopped:
		return nil
	case store.SiteProvisioning, store.SiteUpdating, store.SiteDeleting:
		return fmt.Errorf("%w: the site is %s", ErrBusy, prod.Status)
	}
	if prod.WPSaltsSecretRef == "" {
		return ErrNotProvisioned
	}
	return ErrSourceFailed
}

// validateStaging checks the request and returns the staging site it
// describes, without an ID, slug, UID, or secrets.
func validateStaging(prod store.Site, req NewStaging) (store.Site, error) {
	var fields []FieldError
	name := strings.TrimSpace(req.Name)
	switch {
	case req.Name == "":
		name = stagingName(prod.Name)
	case name == "":
		fields = append(fields, FieldError{"/name", "must not be blank"})
	case len([]rune(name)) > maxSiteName:
		fields = append(fields, FieldError{"/name", fmt.Sprintf("must be at most %d characters", maxSiteName)})
	}
	res := provisioner.ResourceLimits{CPU: prod.CPULimit, MemoryReservationMB: prod.MemoryReservationMB, MemoryLimitMB: prod.MemoryLimitMB}
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
	if len(fields) > 0 {
		return store.Site{}, &ValidationError{Fields: fields}
	}
	return store.Site{
		ClientGroupID:       prod.ClientGroupID,
		ParentSiteID:        prod.ID,
		Name:                name,
		Status:              store.SiteProvisioning,
		PHPVersion:          prod.PHPVersion,
		WPCoreVersion:       prod.WPCoreVersion,
		EnableCache:         prod.EnableCache,
		CPULimit:            res.CPU,
		MemoryReservationMB: res.MemoryReservationMB,
		MemoryLimitMB:       res.MemoryLimitMB,
		// One-click login (§5.9) picks the initial administrator, who is
		// in the copied database too.
		AdminUsername: prod.AdminUsername,
		AdminEmail:    prod.AdminEmail,
		// The dump is imported verbatim, so wp-config.php must name its
		// prefix.
		TablePrefix:  prod.TablePrefix,
		MailDisabled: true,
	}, nil
}

// stagingName is the default name of a staging site: the production site's
// with a suffix, within the API's name limit.
func stagingName(prodName string) string {
	runes := []rune(strings.TrimSpace(prodName))
	if limit := maxSiteName - len(stagingNameSuffix); len(runes) > limit {
		runes = runes[:limit]
	}
	return strings.TrimSpace(string(runes)) + stagingNameSuffix
}

// stagingSlugBase derives the base of a staging site's slug from its
// production site's: acme becomes acme-staging. The production slug is
// shortened to keep the base within maxSlugBase, like any slug base, so
// uniqueSlug's suffixes keep the slug within a DNS label.
func stagingSlugBase(prodSlug string) string {
	base := prodSlug
	if limit := maxSlugBase - len(stagingSlugSuffix); len(base) > limit {
		base = strings.TrimRight(base[:limit], "-")
	}
	if base == "" {
		base = "site"
	}
	return base + stagingSlugSuffix
}

// createStaging runs the creation transaction once.
func (s *Service) createStaging(ctx context.Context, prodID string, site store.Site, snapID, userID string) (StagingCreated, error) {
	var out StagingCreated
	err := s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		// Delete refuses a production site with staging sites in its own
		// transaction, but only those it can see. Both transactions lock
		// the production site's row first, so whichever runs second sees
		// the other's outcome: the deletion then sees this staging site,
		// or this transaction sees the deleting status and refuses. The
		// foreign key (ON DELETE RESTRICT) is the last line of defense,
		// failing the deletion's last step, after its other steps removed
		// the production site's stack.
		prod, err := sites.Lock(ctx, prodID)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("site %s: %w", prodID, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if err := checkStagingSource(prod); err != nil {
			return err
		}
		uid, err := store.AllocateSiteUID(ctx, q)
		if err != nil {
			return err
		}
		slug, err := uniqueSlug(ctx, sites, stagingSlugBase(prod.Slug), s.previewBase)
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
		password, hash, err := newPreviewPassword()
		if err != nil {
			return err
		}
		previewRef, err := sec.Put(ctx, secrets.KindSitePreviewPassword, password)
		if err != nil {
			return err
		}
		site.UID, site.Slug, site.PreviewDomain = uid, slug, previewDomain(slug, s.previewBase)
		site.WPSaltsSecretRef = string(saltsRef)
		site.PreviewProtection, site.PreviewUsername = true, slug
		site.PreviewPasswordHash, site.PreviewPasswordSecretRef = hash, string(previewRef)
		if err := sites.Create(ctx, &site); errors.Is(err, store.ErrReference) {
			return fmt.Errorf("site %s: %w", prodID, ErrNotFound)
		} else if err != nil {
			return err
		}
		if err := sitecron.EnsureSchedule(ctx, q, site.ID); err != nil {
			return err
		}
		if err := pluginupdates.EnsureSchedule(ctx, q, site.ID); err != nil {
			return err
		}
		// Staging sites have no scheduled backups (§5.11): every refresh
		// and push takes one anyway. The site's backup schedule can turn
		// them on.
		if _, err := sites.SetBackupSchedule(ctx, site.ID, store.SiteBackupSchedule{
			IntervalHours: store.DefaultBackupIntervalHours,
		}); err != nil {
			return err
		}
		spec := jobs.Spec{
			Type:            CloneJobType,
			TargetType:      store.JobTargetSite,
			TargetID:        site.ID,
			Payload:         clonePayload{SourceSiteID: prod.ID, SnapshotID: snapID},
			LockKey:         store.SiteLockKey(site.ID),
			CreatedByUserID: userID,
		}
		if snapID == "" {
			// The backup runs under the production site's lock key, the
			// clone under the staging site's; a job holds one key.
			bj, err := sitebackup.Enqueue(ctx, q, prod.ID, store.BackupSync, userID)
			if err != nil {
				return err
			}
			out.BackupJob, spec.After = &bj, bj.ID
		}
		if out.Job, err = jobs.Enqueue(ctx, q, spec); err != nil {
			return err
		}
		out.Site = site
		out.Preview = PreviewCredentials{Username: slug, Password: password}
		return nil
	})
	if err != nil {
		return StagingCreated{}, err
	}
	return out, nil
}

func (s *Service) cloneHandler() jobs.Handler {
	steps := []jobs.Step{
		{Name: stepPrepare, Do: s.prepareClone, Undo: s.failClone},
		{Name: stepCreateDatabase, Do: s.createDatabase, Undo: s.dropDatabase},
		{Name: stepCreateCache, Do: s.createObjectCache, Undo: s.deleteObjectCache},
		{Name: stepStartStack, Do: s.startStack, Undo: s.deleteStack},
		{Name: stepRestore, Do: s.restoreClone},
		{Name: stepRestartSite, Do: s.restartRestoredSite},
		{Name: stepUpgradeDatabase, Do: s.upgradeRestoredDatabase},
		{Name: stepRewriteURLs, Do: s.rewriteClonedURLs},
		{Name: stepConfigurePlugins, Do: s.configureCreatedSitePlugins},
	}
	steps = append(steps, s.cacheSteps()...)
	steps = append(steps, jobs.Step{Name: stepFinish, Do: s.finishClone})
	return jobs.Handler{Type: CloneJobType, Steps: steps}
}

func cloneJobPayload(r *jobs.Run) (clonePayload, error) {
	var p clonePayload
	if err := r.Payload(&p); err != nil {
		return clonePayload{}, err
	}
	if !id.Valid(p.SourceSiteID) {
		return clonePayload{}, jobs.Permanent(errors.New("the clone job's payload names no production site"))
	}
	return p, nil
}

// cloneSnapshot returns the snapshot prepare resolved.
func cloneSnapshot(r *jobs.Run) (string, error) {
	var snap string
	if _, err := r.Get(dataCloneSnapshot, &snap); err != nil {
		return "", err
	}
	if snap == "" {
		return "", jobs.Permanent(errors.New("the clone job has no snapshot"))
	}
	return snap, nil
}

// prepareClone resolves the snapshot to clone, and moves the staging site
// to a core at least as new as the snapshot's.
func (s *Service) prepareClone(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	if site.Status != store.SiteProvisioning {
		return jobs.Permanent(fmt.Errorf("site is %s, not provisioning", site.Status))
	}
	p, err := cloneJobPayload(r)
	if err != nil {
		return err
	}
	snap, backupID, err := s.copySnapshot(ctx, r, p.SourceSiteID, p.SnapshotID)
	if err != nil {
		return err
	}
	if backupID != "" {
		if err := r.Set(dataCloneBackupID, backupID); err != nil {
			return err
		}
	}
	if err := r.Set(dataCloneSnapshot, string(snap.ID)); err != nil {
		return err
	}
	// The sync backup may have run after a core update of the production
	// site; a database schema can't safely run on an older core.
	core, err := s.coreFor(snap.WPCoreVersion, site.WPCoreVersion)
	if err != nil {
		return jobs.Permanent(err)
	}
	if core != "" {
		if _, err := store.NewSites(s.db).SetWPCoreVersion(ctx, site.ID, core); err != nil {
			return err
		}
		r.Logf("The snapshot was taken on WordPress %s, so the staging site gets WordPress %s.", snap.WPCoreVersion, core)
		site.WPCoreVersion = core
	}
	r.Logf("Creating %s from snapshot %s of site %s (PHP %s, WordPress %s).",
		site.PreviewDomain, shortSnapshot(string(snap.ID)), p.SourceSiteID, site.PHPVersion, site.WPCoreVersion)
	return nil
}

// copySnapshot returns the snapshot a job that copies a site works from: the
// one named, or the one the backup job it waited for took, with that backup's
// ID. It fails permanently if the awaited backup failed, or if the snapshot
// isn't in the repository: retention may have removed one that was named, and
// a fresh one must be there too before the job changes anything.
func (s *Service) copySnapshot(ctx context.Context, r *jobs.Run, sourceID, snapID string) (backup.Snapshot, string, error) {
	var backupID string
	if snapID == "" {
		aw, err := r.Awaited(ctx)
		if err != nil {
			return backup.Snapshot{}, "", err
		}
		if err := aw.Err(); err != nil {
			return backup.Snapshot{}, "", err
		}
		if aw.Type == sitebackup.JobType {
			if _, err := aw.GetResult("snapshot_id", &snapID); err != nil {
				return backup.Snapshot{}, "", jobs.Permanent(err)
			}
			if _, err := aw.GetResult("backup_id", &backupID); err != nil {
				return backup.Snapshot{}, "", jobs.Permanent(err)
			}
		}
		if snapID == "" {
			return backup.Snapshot{}, "", jobs.Permanent(fmt.Errorf("the %s job %s took no snapshot", aw.Type, aw.ID))
		}
	}
	snap, err := s.findSnapshot(ctx, sourceID, snapID)
	if err != nil {
		return backup.Snapshot{}, "", permanentRestoreError(err)
	}
	return snap, backupID, nil
}

// failClone is the last Undo of a failed site.clone job, as
// failProvisioning is of site.create.
func (s *Service) failClone(ctx context.Context, r *jobs.Run) error {
	return s.failNewSite(ctx, r, "creating the staging site failed")
}

// restoreClone restores the production snapshot into the staging site.
func (s *Service) restoreClone(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := cloneJobPayload(r)
	if err != nil {
		return err
	}
	snap, err := cloneSnapshot(r)
	if err != nil {
		return err
	}
	if err := s.engine.Restore(ctx, p.SourceSiteID, backup.SnapshotID(snap), site.ID, backup.FullRestore()); err != nil {
		return permanentRestoreError(err)
	}
	r.Logf("Restored the production site's wp-content and database (snapshot %s).", shortSnapshot(snap))
	return nil
}

// rewriteClonedURLs moves the copied database's URLs to the staging site's
// preview URL: those of home and siteurl, as a restore does, and the
// production site's current URL, in case the snapshot predates a go-live.
func (s *Service) rewriteClonedURLs(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := cloneJobPayload(r)
	if err != nil {
		return err
	}
	also, err := s.sourceSiteHost(ctx, restoreSource{SiteID: p.SourceSiteID})
	if err != nil {
		return err
	}
	return s.rewriteRestoredHosts(ctx, r, site, also...)
}

// finishClone records the snapshot on the staging site, marks it running,
// and puts the clone in the job's result. A site whose status changed while
// the job ran (a deletion was requested) keeps its new status.
func (s *Service) finishClone(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := cloneJobPayload(r)
	if err != nil {
		return err
	}
	snap, err := cloneSnapshot(r)
	if err != nil {
		return err
	}
	sites := store.NewSites(s.db)
	if _, err := sites.SetStagingRefreshed(ctx, site.ID, snap, s.now()); err != nil {
		return err
	}
	changed, err := sites.SetStatus(ctx, site.ID, store.SiteRunning, "", store.SiteProvisioning)
	if err != nil {
		return err
	}
	result := map[string]any{"site_id": site.ID, "source_site_id": p.SourceSiteID, "snapshot_id": snap}
	var backupID string
	if _, err := r.Get(dataCloneBackupID, &backupID); err != nil {
		return err
	}
	if backupID != "" {
		result["backup_id"] = backupID
	}
	// The preview password was returned once when the staging site was
	// requested; the result refers to the stored secret rather than
	// storing another copy of it.
	if site.PreviewPasswordSecretRef != "" {
		result["preview_username"] = site.PreviewUsername
		result["preview_password"] = jobs.SecretResult{SecretRef: secrets.Ref(site.PreviewPasswordSecretRef)}
	}
	for k, v := range result {
		if err := r.SetResult(k, v); err != nil {
			return err
		}
	}
	if !changed {
		r.Warnf("The site is %s now; leaving its status alone.", site.Status)
		return nil
	}
	r.Logf("The staging site is ready at %s.", URL(site))
	return nil
}
