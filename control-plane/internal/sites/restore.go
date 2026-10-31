package sites

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Restoring a site from a snapshot (#55, design doc §5.4).
//
// [Service.Restore] checks the request before anything changes: the
// snapshot must be one of the source site's (the target itself unless
// another site is named), the source must be in the target's client group
// (each group has its own repository and key, and the MVP doesn't copy
// snapshots between them), a site image must exist with at least the
// snapshot's WordPress core version, and the target must be running, since
// the job needs WP-CLI. It then sets the target's status to updating and
// queues a [RestoreJobType] job on the target, with the target's lock key.
// The job's steps are:
//
//  1. prepare: check the snapshot is still in the repository. Its Undo
//     runs last in a rollback and records the outcome (see below).
//  2. record_backup and take_snapshot (package backup/sitebackup): a
//     pre-restore backup of the target, so the restore can be undone.
//     They run inside this job rather than as a site.backup job, which
//     would wait for this job's lock key. A retry finds the backup record
//     and snapshot an earlier attempt made, so it never takes a second one.
//  3. enable_maintenance: record the maintenance flag the site had, then
//     turn maintenance mode on, in the site record (so re-renders keep it)
//     and with provisioner.SetMaintenance.
//  4. update_core: if the snapshot was taken on a newer WordPress core
//     than the target runs, move the target to the oldest site image with
//     at least that version (a database schema can't safely run on an
//     older core). The previous version is recorded first.
//  5. restore: backup.Engine.Restore replaces the target's wp-content and
//     database with the snapshot's.
//  6. restart_site: stop and start the site's containers, so PHP-FPM's
//     opcode cache, which checks files for changes only once a minute,
//     doesn't keep running the replaced plugins' code. The site is in
//     maintenance mode, so its visitors don't notice.
//  7. upgrade_database: `wp core update-db`, for a snapshot from an older
//     core than the target runs (a no-op otherwise).
//  8. rewrite_urls: if the restored database's home or siteurl names
//     another host than the target's URL (a restore into another site, or
//     of a snapshot taken before go-live), `wp search-replace` from it to
//     the target's host, as go-live does, then set home and siteurl.
//  9. purge_object_cache and purge_page_cache (cache.go).
//  10. disable_maintenance: the restore is complete; put the maintenance
//      flag back as it was before the restore (off, unless the site was
//      already in maintenance mode).
//  11. finish: set the status from the live phase.
//
// The object cache is flushed before WP-CLI runs on the restored database
// (steps 7 and 8): it still holds the pre-restore site's objects, options
// included.
//
// # Failure and rollback
//
// A failed step is retried with backoff, except for what can't get better:
// a missing snapshot or site, no suitable image, and WP-CLI failing on the
// restored site (WordPress that can't load its restored plugins or
// database won't load on the next attempt either, and the site is down
// meanwhile). Then the job rolls back, newest step first:
//
//   - restore, if it started and the restore isn't complete: restore the
//     pre-restore snapshot into the target (wp-content and database),
//     restart it, and purge its caches. If restoring keeps failing, it
//     gives up on its last attempt without failing the rollback, so the
//     site is marked failed with the pre-restore backup's ID in its status
//     message.
//   - update_core: if the core was moved and the site's data is back as it
//     was (rolled back, or never touched), move it back to the previous
//     version, which the pre-restore database was made for. If the data
//     couldn't be rolled back, the newer core stays: the database may
//     already have the newer schema.
//   - enable_maintenance: if the restore never started, nothing on the
//     site changed, so the previous maintenance flag is put back. Once the
//     restore started, the site stays in maintenance mode (§5.4 step 7):
//     an operator checks it and turns maintenance mode off.
//   - record_backup: a pre-restore backup that didn't finish is marked
//     failed; a finished one is kept, with its snapshot.
//   - prepare: sets the status from the live phase (running) with the
//     outcome in the status message, or failed if the rollback couldn't
//     restore the data or the core.
//
// A job that fails after the restore is complete (turning maintenance
// mode off, or finishing) doesn't roll the restored site back: its status
// message says the restore finished and maintenance mode may still be on.
//
// # The shared pipeline
//
// These steps are the restore pipeline of §5.4, which site.refresh
// (refresh.go) and site.push (push.go) run too: [restoreFlow] holds what differs
// between the job types, and [Service.restoreFlowHandler] builds the steps.
// Everything else comes from the job's [restoreSource]: which snapshot of
// which site, and how much of it. A step outside the scope succeeds without
// doing anything, instead of being left out, so a job's steps don't depend on
// its payload. site.restore always has the whole snapshot in scope, so its
// steps, their names, and what they do are as listed above.

// RestoreJobType restores a snapshot into a site.
const RestoreJobType = "site.restore"

// Step names of the site.restore job, besides prepare, the backup steps,
// the cache purge steps, and finish. Keep them stable: a queued job
// resumes by step name.
const (
	stepEnableMaintenance  = "enable_maintenance"
	stepUpdateCore         = "update_core"
	stepRestore            = "restore"
	stepRestartSite        = "restart_site"
	stepUpgradeDatabase    = "upgrade_database"
	stepDisableMaintenance = "disable_maintenance"
)

// Job data keys of the site.restore job.
const (
	// dataMaintenanceBefore is the maintenance flag before the restore.
	dataMaintenanceBefore = "maintenance_before"
	// dataCorePrevious and dataCoreTarget are the core versions update_core
	// moves the site from and to; absent if it didn't move it.
	dataCorePrevious = "core_previous"
	dataCoreTarget   = "core_target"
	// dataRestoreStarted records that the restore step has started to
	// change the site's data, dataRestoreComplete that the restore is done
	// (after which the job doesn't roll back), and dataRolledBack that the
	// rollback restored the pre-restore snapshot.
	dataRestoreStarted  = "restore_started"
	dataRestoreComplete = "restore_complete"
	dataRolledBack      = "rolled_back"
	// dataRollbackError records why restoring the pre-restore snapshot
	// failed for good.
	dataRollbackError = "rollback_error"
	// dataRestoreSnapshot and dataRestoreSnapshotCore are the snapshot and its
	// WordPress core version, for a job whose prepare step resolves them
	// because the snapshot didn't exist when the job was queued.
	dataRestoreSnapshot     = "restore_snapshot_id"
	dataRestoreSnapshotCore = "restore_snapshot_core"
	// dataRestoreSnapshotTime is the time the snapshot was taken, which is how
	// old the data it brings is (store.Site.StagingDatabaseAsOf). Jobs queued
	// before it existed lack it, which reads as unknown.
	dataRestoreSnapshotTime = "restore_snapshot_time"
)

// snapshotTimeLayout formats a snapshot's time in job data. RFC 3339 with
// nanoseconds round-trips exactly, as restic's times are finer than seconds.
const snapshotTimeLayout = time.RFC3339Nano

// setSnapshotTime records when the snapshot was taken in the job's data under
// key. A snapshot with no time records nothing, which reads as unknown.
func setSnapshotTime(r *jobs.Run, key string, t time.Time) error {
	if t.IsZero() {
		return nil
	}
	return r.Set(key, t.UTC().Format(snapshotTimeLayout))
}

// getSnapshotTime reads a time recorded by setSnapshotTime. Absent, or
// unreadable, is the zero time: a job queued before the time was recorded must
// finish, and an unknown age is honest where a wrong one isn't.
func getSnapshotTime(r *jobs.Run, key string) (time.Time, error) {
	var v string
	if _, err := r.Get(key, &v); err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(snapshotTimeLayout, v)
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

// ErrNoBackups means the service has no backup engine, so it can't
// restore; a wiring bug.
var ErrNoBackups = errors.New("sites: restores need a backup service and engine")

// Restore is a request to restore a snapshot into a site.
type Restore struct {
	// SnapshotID is the snapshot's ID, or a unique prefix of at least 8
	// characters.
	SnapshotID string
	// SourceSiteID is the site the snapshot is of; empty means the target
	// site itself.
	SourceSiteID string
	// CreatedByUserID is the user starting the job.
	CreatedByUserID string
}

// restorePayload is the payload of a job of the restore pipeline.
type restorePayload struct {
	SourceSiteID string `json:"source_site_id"`
	// SnapshotID is empty for a job whose prepare step takes the snapshot
	// from the backup job it waited for.
	SnapshotID string `json:"snapshot_id,omitempty"`
	// SnapshotWPCore is the snapshot's WordPress core version; empty if
	// the snapshot doesn't say, or isn't known yet.
	SnapshotWPCore string `json:"snapshot_wp_core,omitempty"`
	// Scope is how much of the snapshot to restore. A site.restore job has
	// none, and restores all of it, as those queued before scopes existed
	// do.
	Scope *restoreScope `json:"scope,omitempty"`
}

// restoreScope is backup.RestoreOptions in a job's payload.
type restoreScope struct {
	Files    backup.RestoreFiles `json:"files"`
	Database bool                `json:"database"`
}

// scope returns how much of the snapshot the job restores.
func (p restorePayload) scope() backup.RestoreOptions {
	if p.Scope == nil {
		return backup.FullRestore()
	}
	return backup.RestoreOptions{Files: p.Scope.Files, Database: p.Scope.Database}
}

// restoreSource is what a job of the restore pipeline restores.
type restoreSource struct {
	// SiteID is the site the snapshot is of.
	SiteID string
	// SnapshotID is the snapshot's full ID.
	SnapshotID string
	// WPCore is the snapshot's WordPress core version; empty if the snapshot
	// doesn't say.
	WPCore string
	// Scope is how much of the snapshot to restore.
	Scope backup.RestoreOptions
	// SnapshotTime is when the snapshot was taken; zero if the job's prepare
	// step didn't record it.
	SnapshotTime time.Time
}

// restoreFlow is what differs between the job types of the restore pipeline.
type restoreFlow struct {
	// jobType is the job's type.
	jobType string
	// noun names the operation in status messages: "restore" reads "restore
	// failed" and "its pre-restore backup".
	noun string
	// prepare is the first step. It checks that the snapshot exists,
	// resolving it into dataRestoreSnapshot if the payload doesn't name it,
	// and has to fail before it changes anything on the site: its failure
	// rolls back only the status.
	prepare jobs.StepFunc
	// rewriteFrom returns hosts to rewrite to the site's besides those the
	// restored database's home and siteurl name; nil means none.
	rewriteFrom func(ctx context.Context, src restoreSource) ([]string, error)
	// convergePlugins adds a configure_plugins step after the URL rewrite,
	// for a job whose database comes with another site's plugin state.
	convergePlugins bool
	// repairPlugins, if set, is what that step does when the database isn't
	// restored, instead of nothing; a job whose files can take away plugins
	// its site can't do without needs it.
	repairPlugins jobs.StepFunc
	// finish is the last step. It should call finishRestore or finishRestoreAs.
	finish jobs.StepFunc
}

// scopedStep runs do only if the job's scope wants it; otherwise the step
// logs why it has nothing to do.
func scopedStep(wanted func(backup.RestoreOptions) bool, skipped string, do jobs.StepFunc) jobs.StepFunc {
	return func(ctx context.Context, r *jobs.Run) error {
		src, err := jobRestoreSource(r)
		if err != nil {
			return err
		}
		if !wanted(src.Scope) {
			r.Logf("%s", skipped)
			return nil
		}
		return do(ctx, r)
	}
}

// databaseInScope and filesInScope say which steps a scope needs.
//
// The core, the schema upgrade, the URL rewrite, and the plugin stack all
// exist for the database: a core only has to be as new as the schema it
// runs, and only a restored database names another site's URLs and plugin
// state (§5.4). A restore that keeps the database must not touch it, which
// the last three would.
//
// The restart exists for the files: PHP-FPM's opcode cache notices changed
// files only once a minute, so it could keep running replaced code, while a
// restored database reaches PHP on its next query.
//
// The cache purges run for every scope, so they aren't scoped. A cached page
// is built from code and content alike, so it goes stale when either is
// replaced, and the object cache holds both the old database's objects and
// what the old plugins stored in their own format. Neither purge touches the
// database.
func databaseInScope(o backup.RestoreOptions) bool { return o.Database }

func filesInScope(o backup.RestoreOptions) bool { return o.Files != backup.RestoreFilesNone }

func (s *Service) restores() bool { return s.backups != nil && s.engine != nil }

// Restore checks the request and queues a [RestoreJobType] job that
// restores the snapshot into the site, with status updating. It returns
// the site as saved and the job. A snapshot that isn't the source site's,
// a source in another client group, or a snapshot from a WordPress core
// newer than any site image is a *ValidationError; nothing changes. A
// target that isn't running is ErrNotRunning; one a job is working on, or
// that never finished provisioning, ErrBusy or ErrNotProvisioned.
func (s *Service) Restore(ctx context.Context, siteID string, req Restore) (store.Site, store.Job, error) {
	if !s.restores() {
		return store.Site{}, store.Job{}, ErrNoBackups
	}
	source := req.SourceSiteID
	if source == "" {
		source = siteID
	}
	// Fail fast, before asking the repository; the transaction checks
	// again.
	target, err := s.manageable(ctx, store.NewSites(s.db), siteID)
	if err != nil {
		return store.Site{}, store.Job{}, err
	}
	if target.Status != store.SiteRunning {
		return store.Site{}, store.Job{}, fmt.Errorf("%w (it is %s)", ErrNotRunning, target.Status)
	}
	snap, err := s.checkRestore(ctx, target, source, req.SnapshotID)
	if err != nil {
		return store.Site{}, store.Job{}, err
	}

	var (
		site store.Site
		job  store.Job
	)
	err = s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		current, err := s.manageable(ctx, sites, siteID)
		if err != nil {
			return err
		}
		if current.Status != store.SiteRunning {
			return fmt.Errorf("%w (it is %s)", ErrNotRunning, current.Status)
		}
		if _, err := sites.SetStatus(ctx, siteID, store.SiteUpdating, ""); err != nil {
			return err
		}
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:       RestoreJobType,
			TargetType: store.JobTargetSite,
			TargetID:   siteID,
			Payload: restorePayload{
				SourceSiteID: source, SnapshotID: string(snap.ID), SnapshotWPCore: snap.WPCoreVersion,
			},
			LockKey:         store.SiteLockKey(siteID),
			CreatedByUserID: req.CreatedByUserID,
		})
		if err != nil {
			return err
		}
		site, err = sites.Get(ctx, siteID)
		return err
	})
	if err != nil {
		return store.Site{}, store.Job{}, err
	}
	s.log.InfoContext(ctx, "site restore queued", "site_id", siteID, "source_site_id", source,
		"snapshot_id", snap.ID, "job_id", job.ID)
	return site, job, nil
}

// checkRestore validates a restore of the source site's snapshot into the
// target and returns the snapshot.
func (s *Service) checkRestore(ctx context.Context, target store.Site, source, snapID string) (backup.Snapshot, error) {
	invalid := func(field, detail string) error {
		return &ValidationError{Fields: []FieldError{{field, detail}}}
	}
	if !id.Valid(source) {
		return backup.Snapshot{}, invalid("/source_site_id", "is not a site ID")
	}
	group, err := sourceGroup(ctx, s.db, source)
	if errors.Is(err, store.ErrNotFound) {
		return backup.Snapshot{}, invalid("/source_site_id", "no site or backup has this ID")
	}
	if err != nil {
		return backup.Snapshot{}, err
	}
	if group != target.ClientGroupID {
		return backup.Snapshot{}, invalid("/source_site_id", "is in another client group; restores stay within a client group, whose sites share a backup repository")
	}
	snap, err := s.findSnapshot(ctx, source, snapID)
	if errors.Is(err, backup.ErrSnapshotNotFound) {
		return backup.Snapshot{}, invalid("/snapshot_id", "is not a snapshot of site "+source+" in its backup repository")
	}
	if err != nil {
		return backup.Snapshot{}, err
	}
	if _, err := s.coreFor(snap.WPCoreVersion, target.WPCoreVersion); err != nil {
		return backup.Snapshot{}, invalid("/snapshot_id", err.Error())
	}
	return snap, nil
}

// sourceGroup returns the client group of the site whose snapshots a
// restore reads: the site's, or for a deleted site that of its backups.
func sourceGroup(ctx context.Context, q store.Querier, siteID string) (string, error) {
	site, err := store.NewSites(q).Get(ctx, siteID)
	if err == nil {
		return site.ClientGroupID, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	return store.NewBackupRecords(q).SiteClientGroup(ctx, siteID)
}

// findSnapshot returns the site's snapshot with the ID, or a unique prefix
// of it, or backup.ErrSnapshotNotFound.
func (s *Service) findSnapshot(ctx context.Context, siteID, snapID string) (backup.Snapshot, error) {
	if snapID == "" {
		return backup.Snapshot{}, backup.ErrSnapshotNotFound
	}
	snaps, err := s.engine.ListSnapshots(ctx, siteID)
	if errors.Is(err, backup.ErrSiteNotFound) {
		return backup.Snapshot{}, backup.ErrSnapshotNotFound
	}
	if err != nil {
		return backup.Snapshot{}, fmt.Errorf("listing the snapshots of site %s: %w", siteID, err)
	}
	var found []backup.Snapshot
	for _, snap := range snaps {
		if string(snap.ID) == snapID {
			return snap, nil
		}
		if len(snapID) >= minSnapshotPrefix && strings.HasPrefix(string(snap.ID), snapID) {
			found = append(found, snap)
		}
	}
	if len(found) != 1 {
		return backup.Snapshot{}, backup.ErrSnapshotNotFound
	}
	return found[0], nil
}

// minSnapshotPrefix is the shortest snapshot ID prefix a restore accepts:
// the short form restic shows.
const minSnapshotPrefix = 8

// errNoCore means no site image has a core new enough for a snapshot.
var errNoCore = errors.New("no site image has a new enough WordPress core")

// coreFor returns the core version a site on current must move to before
// a snapshot from snapCore is restored into it, or "" if it needn't move.
func (s *Service) coreFor(snapCore, current string) (string, error) {
	if snapCore == "" {
		return "", nil
	}
	newer, err := catalog.CompareWordPressVersions(snapCore, current)
	if err != nil {
		return "", fmt.Errorf("can't compare the snapshot's WordPress version with the site's: %w", err)
	}
	if newer <= 0 {
		return "", nil
	}
	w, ok, err := s.catalog.WordPressAtLeast(snapCore)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: the snapshot was taken on WordPress %s, newer than any site image (the newest is %s)",
			errNoCore, snapCore, s.catalog.LatestWordPress().Version)
	}
	return w.Version, nil
}

func (s *Service) restoreHandler() jobs.Handler {
	return s.restoreFlowHandler(restoreFlow{
		jobType: RestoreJobType,
		noun:    "restore",
		prepare: s.prepareRestore,
		finish:  s.finishRestoreSite,
	})
}

// finishRestoreSite finishes a restore, then, for a staging site, forgets how
// old its database and uploads are: a restore puts back data of any age, and
// the times from before it would understate what a push loses. The restore is
// always the full scope.
func (s *Service) finishRestoreSite(ctx context.Context, r *jobs.Run) error {
	if err := s.finishRestore(ctx, r); err != nil {
		return err
	}
	sites := store.NewSites(s.db)
	site, err := sites.Get(ctx, r.Job().TargetID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !site.IsStaging() {
		return nil
	}
	_, err = sites.SetStagingAsOf(ctx, site.ID, store.AsOfClear(), store.AsOfClear())
	return err
}

// restoreFlowHandler builds a job type of the restore pipeline. Keep each job
// type's step names and their order stable: a queued job resumes by step
// name.
func (s *Service) restoreFlowHandler(f restoreFlow) jobs.Handler {
	steps := []jobs.Step{{Name: stepPrepareUpdate, Do: f.prepare, Undo: func(ctx context.Context, r *jobs.Run) error {
		return s.finishFailedRestore(ctx, r, f.noun)
	}}}
	steps = append(steps, s.backups.Steps(store.BackupPreRestore)...)
	steps = append(steps,
		jobs.Step{Name: stepEnableMaintenance, Do: s.enableRestoreMaintenance, Undo: s.revertRestoreMaintenance},
		jobs.Step{
			Name: stepUpdateCore,
			Do:   scopedStep(databaseInScope, "The database isn't being restored, so the site keeps its WordPress core.", s.updateRestoreCore),
			Undo: s.revertRestoreCore,
		},
		jobs.Step{Name: stepRestore, Do: s.restoreSnapshot, Undo: func(ctx context.Context, r *jobs.Run) error {
			return s.rollbackRestore(ctx, r, f.noun)
		}},
		jobs.Step{
			Name: stepRestartSite,
			Do:   scopedStep(filesInScope, "No files were restored, so the site needs no restart.", s.restartRestoredSite),
		},
		jobs.Step{
			Name: stepUpgradeDatabase,
			Do:   scopedStep(databaseInScope, "The database wasn't restored, so it needs no upgrade.", s.upgradeRestoredDatabase),
		},
		jobs.Step{
			Name: stepRewriteURLs,
			Do: scopedStep(databaseInScope, "The database wasn't restored, so its URLs are the site's already.", func(ctx context.Context, r *jobs.Run) error {
				return s.rewriteRestoredURLs(ctx, r, f.rewriteFrom)
			}),
		},
	)
	if f.convergePlugins {
		converge := scopedStep(databaseInScope, "The database wasn't restored, so the site's plugin state is its own.", s.configureCreatedSitePlugins)
		if f.repairPlugins != nil {
			converge = func(ctx context.Context, r *jobs.Run) error {
				src, err := jobRestoreSource(r)
				if err != nil {
					return err
				}
				if databaseInScope(src.Scope) {
					return s.configureCreatedSitePlugins(ctx, r)
				}
				return f.repairPlugins(ctx, r)
			}
		}
		steps = append(steps, jobs.Step{Name: stepConfigurePlugins, Do: converge})
	}
	steps = append(steps, s.cacheSteps()...)
	steps = append(steps,
		jobs.Step{Name: stepDisableMaintenance, Do: s.disableRestoreMaintenance},
		jobs.Step{Name: stepFinishManage, Do: f.finish},
	)
	return jobs.Handler{Type: f.jobType, Steps: steps}
}

// restoreJobPayload reads the payload of a job of the restore pipeline.
func restoreJobPayload(r *jobs.Run) (restorePayload, error) {
	var p restorePayload
	if err := r.Payload(&p); err != nil {
		return restorePayload{}, err
	}
	if !id.Valid(p.SourceSiteID) {
		return restorePayload{}, jobs.Permanent(errors.New("the job's payload names no source site"))
	}
	return p, nil
}

// jobRestoreSource returns what the job restores: the payload's snapshot, or
// the one its prepare step resolved.
func jobRestoreSource(r *jobs.Run) (restoreSource, error) {
	p, err := restoreJobPayload(r)
	if err != nil {
		return restoreSource{}, err
	}
	src := restoreSource{SiteID: p.SourceSiteID, SnapshotID: p.SnapshotID, WPCore: p.SnapshotWPCore, Scope: p.scope()}
	if _, err := r.Get(dataRestoreSnapshot, &src.SnapshotID); err != nil {
		return restoreSource{}, err
	}
	if _, err := r.Get(dataRestoreSnapshotCore, &src.WPCore); err != nil {
		return restoreSource{}, err
	}
	if src.SnapshotTime, err = getSnapshotTime(r, dataRestoreSnapshotTime); err != nil {
		return restoreSource{}, err
	}
	if src.SnapshotID == "" {
		return restoreSource{}, jobs.Permanent(errors.New("the job has no snapshot to restore"))
	}
	return src, nil
}

// flag reads a boolean from the job's data; absent is false.
func flag(r *jobs.Run, key string) (bool, error) {
	var v bool
	_, err := r.Get(key, &v)
	return v, err
}

// lastAttempt reports whether a failing Undo should give up rather than be
// retried.
func lastAttempt(r *jobs.Run, err error) bool {
	return jobs.IsPermanent(err) || r.Job().Attempts >= r.Job().MaxAttempts
}

// permanentRestoreError marks the restore errors that retrying can't fix.
func permanentRestoreError(err error) error {
	for _, target := range []error{
		backup.ErrSnapshotNotFound, backup.ErrSiteNotFound, backup.ErrCrossGroup, backup.ErrInvalidRestoreOptions,
		provisioner.ErrSiteNotFound, sitedb.ErrDatabaseNotFound, errors.ErrUnsupported,
	} {
		if errors.Is(err, target) {
			return jobs.Permanent(err)
		}
	}
	return err
}

// prepareRestore checks the snapshot is still there: retention may have
// removed it while the job waited.
func (s *Service) prepareRestore(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	src, err := jobRestoreSource(r)
	if err != nil {
		return err
	}
	if _, err := s.findSnapshot(ctx, src.SiteID, src.SnapshotID); err != nil {
		return permanentRestoreError(err)
	}
	from := "its own snapshot"
	if src.SiteID != site.ID {
		from = "a snapshot of site " + src.SiteID
	}
	r.Logf("Restoring %s (%s) into %s, after taking a pre-restore backup.", from, shortSnapshot(src.SnapshotID), site.PreviewDomain)
	return nil
}

func shortSnapshot(snapID string) string {
	if len(snapID) > 8 {
		return snapID[:8]
	}
	return snapID
}

// enableRestoreMaintenance puts the site in maintenance mode, recording
// the flag it had first.
func (s *Service) enableRestoreMaintenance(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	var before bool
	found, err := r.Get(dataMaintenanceBefore, &before)
	if err != nil {
		return err
	}
	if !found {
		if err := r.Set(dataMaintenanceBefore, site.Maintenance); err != nil {
			return err
		}
	}
	if _, err := store.NewSites(s.db).SetMaintenance(ctx, site.ID, true); err != nil {
		return err
	}
	if err := s.prov.SetMaintenance(ctx, site.ID, true); err != nil {
		if errors.Is(err, provisioner.ErrSiteNotFound) {
			return jobs.Permanent(err)
		}
		return err
	}
	r.Logf("The site's domains show the maintenance page.")
	return nil
}

// revertRestoreMaintenance puts the previous maintenance flag back if the
// restore never started; otherwise the site stays in maintenance mode.
func (s *Service) revertRestoreMaintenance(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	started, err := flag(r, dataRestoreStarted)
	if err != nil {
		return err
	}
	complete, err := flag(r, dataRestoreComplete)
	if err != nil {
		return err
	}
	switch {
	case complete:
		return nil
	case started:
		r.Warnf("The site stays in maintenance mode: its data was restored and rolled back. Turn maintenance mode off once it has been checked.")
		return nil
	}
	var before bool
	if found, err := r.Get(dataMaintenanceBefore, &before); err != nil || !found || before {
		return err
	}
	if _, err := store.NewSites(s.db).SetMaintenance(ctx, siteID, false); err != nil {
		return err
	}
	if err := s.prov.SetMaintenance(ctx, siteID, false); err != nil {
		if !lastAttempt(r, err) {
			return err
		}
		r.Warnf("Couldn't take the site off the maintenance page: %v", err)
		return nil
	}
	r.Logf("Nothing was restored; the site's domains serve the site again.")
	return nil
}

// updateRestoreCore moves the site to a core at least as new as the
// snapshot's, if it runs an older one.
func (s *Service) updateRestoreCore(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	var target string
	found, err := r.Get(dataCoreTarget, &target)
	if err != nil {
		return err
	}
	if !found {
		src, err := jobRestoreSource(r)
		if err != nil {
			return err
		}
		target, err = s.coreFor(src.WPCore, site.WPCoreVersion)
		if err != nil {
			return jobs.Permanent(err)
		}
		if target == "" {
			r.Logf("The site's WordPress %s can run the snapshot's database.", site.WPCoreVersion)
			return nil
		}
		// The previous version first, so a retry or rollback knows it.
		if err := r.Set(dataCorePrevious, site.WPCoreVersion); err != nil {
			return err
		}
		if err := r.Set(dataCoreTarget, target); err != nil {
			return err
		}
	}
	var previous string
	if _, err := r.Get(dataCorePrevious, &previous); err != nil {
		return err
	}
	if _, err := store.NewSites(s.db).SetWPCoreVersion(ctx, site.ID, target); err != nil {
		return err
	}
	site.WPCoreVersion = target
	if _, err := s.converge(ctx, r, site); err != nil {
		return err
	}
	r.Logf("Moved the site from WordPress %s to %s, which the snapshot's database needs.", previous, target)
	return nil
}

// revertRestoreCore moves the site back to its previous core once its data
// is back as it was.
func (s *Service) revertRestoreCore(ctx context.Context, r *jobs.Run) error {
	var previous string
	found, err := r.Get(dataCorePrevious, &previous)
	if err != nil || !found {
		return err
	}
	var rollbackErr string
	if _, err := r.Get(dataRollbackError, &rollbackErr); err != nil {
		return err
	}
	if rollbackErr != "" {
		r.Warnf("Keeping the newer WordPress core: the site's database couldn't be rolled back and may need it.")
		return nil
	}
	complete, err := flag(r, dataRestoreComplete)
	if err != nil || complete {
		return err
	}
	site, err := s.jobSite(ctx, r)
	if err != nil || site.Status != store.SiteUpdating {
		return nil
	}
	if _, err := store.NewSites(s.db).SetWPCoreVersion(ctx, site.ID, previous); err != nil {
		return err
	}
	site.WPCoreVersion = previous
	if _, err := s.converge(ctx, r, site); err != nil {
		if !lastAttempt(r, err) {
			return err
		}
		r.Warnf("Couldn't move the site back to WordPress %s: %v", previous, err)
		return r.Set(dataRevertError, err.Error())
	}
	r.Logf("Moved the site back to WordPress %s.", previous)
	return nil
}

// restoreSnapshot restores the snapshot into the site.
func (s *Service) restoreSnapshot(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	src, err := jobRestoreSource(r)
	if err != nil {
		return err
	}
	if err := r.Set(dataRestoreStarted, true); err != nil {
		return err
	}
	if err := s.engine.Restore(ctx, src.SiteID, backup.SnapshotID(src.SnapshotID), site.ID, src.Scope); err != nil {
		return permanentRestoreError(err)
	}
	r.Logf("Restored the snapshot's %s.", describeScope(src.Scope))
	return nil
}

// describeScope names what a restore of the scope replaces, for logs.
func describeScope(o backup.RestoreOptions) string {
	var files string
	switch o.Files {
	case backup.RestoreFilesAll:
		files = "wp-content"
	case backup.RestoreFilesCode:
		files = strings.Join(backup.CodeDirs, ", ")
	}
	switch {
	case files != "" && o.Database:
		return files + " and database"
	case files != "":
		return files
	}
	return "database"
}

// rollbackRestore restores the pre-restore snapshot, unless the restore
// completed or never started. That snapshot is of the whole site, so it is
// restored in full whatever the job's own scope was.
func (s *Service) rollbackRestore(ctx context.Context, r *jobs.Run, noun string) error {
	siteID := r.Job().TargetID
	for _, key := range []string{dataRestoreComplete, dataRolledBack} {
		if done, err := flag(r, key); err != nil || done {
			return err
		}
	}
	if started, err := flag(r, dataRestoreStarted); err != nil || !started {
		return err
	}
	rec, found, err := s.backups.JobBackup(ctx, r)
	if err != nil {
		return err
	}
	giveUp := func(err error) error {
		r.Warnf("Couldn't restore the pre-%s backup: %v", noun, err)
		return r.Set(dataRollbackError, err.Error())
	}
	if !found || rec.Status != store.BackupSucceeded || rec.SnapshotID == "" {
		return giveUp(fmt.Errorf("the job has no pre-%s snapshot", noun))
	}
	if err := s.engine.Restore(ctx, siteID, backup.SnapshotID(rec.SnapshotID), siteID, backup.FullRestore()); err != nil {
		err = permanentRestoreError(err)
		if !lastAttempt(r, err) {
			return err
		}
		return giveUp(fmt.Errorf("backup %s (snapshot %s): %w", rec.ID, shortSnapshot(rec.SnapshotID), err))
	}
	// Best effort: the restored data is what matters; a restart and a
	// cache purge can be done later.
	if err := s.restart(ctx, siteID); err != nil {
		r.Warnf("Couldn't restart the site after rolling back: %v", err)
	}
	if err := s.caches.FlushCache(ctx, siteID); err != nil {
		r.Warnf("Couldn't flush the object cache after rolling back: %v", err)
	}
	if err := s.prov.PurgeCache(ctx, siteID); err != nil && !errors.Is(err, provisioner.ErrSiteNotFound) {
		r.Warnf("Couldn't purge the page cache after rolling back: %v", err)
	}
	if err := r.Set(dataRolledBack, true); err != nil {
		return err
	}
	r.Warnf("Rolled the site back to its pre-%s backup %s (snapshot %s).", noun, rec.ID, shortSnapshot(rec.SnapshotID))
	return nil
}

// restartRestoredSite restarts the site's containers after its wp-content
// was replaced.
func (s *Service) restartRestoredSite(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	if err := s.restart(ctx, site.ID); err != nil {
		return err
	}
	r.Logf("Restarted the site, so it runs the restored code.")
	return nil
}

// restart stops and starts the site's containers.
func (s *Service) restart(ctx context.Context, siteID string) error {
	for _, step := range []func(context.Context, string) error{s.prov.StopSite, s.prov.StartSite} {
		if err := step(ctx, siteID); err != nil {
			if errors.Is(err, provisioner.ErrSiteNotFound) {
				return jobs.Permanent(err)
			}
			return fmt.Errorf("restarting the site: %w", err)
		}
	}
	return nil
}

// wpcliRestored runs WP-CLI on the restored site. WP-CLI exiting with an
// error there is permanent: WordPress that can't load its restored plugins
// or database won't on the next attempt either, and the site is in
// maintenance mode until the job ends.
func (s *Service) wpcliRestored(ctx context.Context, siteID string, args ...string) (string, error) {
	out, err := s.prov.ExecWPCLI(ctx, siteID, args)
	var ee *provisioner.ExecError
	if errors.As(err, &ee) {
		// The job's error and the site's status message keep what WP-CLI
		// said, not a PHP stack trace of several kilobytes.
		short := *ee
		short.Stdout, short.Stderr = "", trimWPCLIError(ee.Stderr)
		return out, jobs.Permanent(fmt.Errorf("wp %s: %w", args[0], &short))
	}
	if errors.Is(err, provisioner.ErrSiteNotFound) {
		return out, jobs.Permanent(err)
	}
	return out, err
}

// maxWPCLIError bounds the WP-CLI output kept in a restore job's error.
const maxWPCLIError = 500

// trimWPCLIError keeps WP-CLI's error output up to a PHP stack trace, at
// most maxWPCLIError bytes.
func trimWPCLIError(stderr string) string {
	msg, _, _ := strings.Cut(stderr, "\nStack trace:")
	msg = strings.TrimSpace(msg)
	if len(msg) > maxWPCLIError {
		msg = msg[:maxWPCLIError-3] + "..."
	}
	return msg
}

// upgradeRestoredDatabase applies the site's core's database schema to the
// restored database, which may come from an older core.
func (s *Service) upgradeRestoredDatabase(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	// The object cache still holds the pre-restore site's options,
	// db_version among them.
	if err := s.caches.FlushCache(ctx, site.ID); err != nil {
		return fmt.Errorf("flushing the object cache: %w", err)
	}
	if _, err := s.wpcliRestored(ctx, site.ID, "core", "update-db"); err != nil {
		return fmt.Errorf("upgrading the restored database to WordPress %s: %w", site.WPCoreVersion, err)
	}
	r.Logf("The restored database is up to date with WordPress %s.", site.WPCoreVersion)
	return nil
}

// rewriteRestoredURLs rewrites the restored database's URLs to the site's
// if they name another host, or a host rewriteFrom returns.
func (s *Service) rewriteRestoredURLs(ctx context.Context, r *jobs.Run, rewriteFrom func(context.Context, restoreSource) ([]string, error)) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	var also []string
	if rewriteFrom != nil {
		src, err := jobRestoreSource(r)
		if err != nil {
			return err
		}
		if also, err = rewriteFrom(ctx, src); err != nil {
			return err
		}
	}
	return s.rewriteRestoredHosts(ctx, r, site, also...)
}

// sourceSiteHost returns the source site's current host, if it still exists:
// a host to rewrite for a job that copies between sites, in case the
// snapshot predates a go-live and its home and siteurl name another.
func (s *Service) sourceSiteHost(ctx context.Context, src restoreSource) ([]string, error) {
	site, err := store.NewSites(s.db).Get(ctx, src.SiteID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []string{strings.TrimPrefix(URL(site), "https://")}, nil
}

// rewriteRestoredHosts rewrites the hosts that the restored database's home and
// siteurl name, and the hosts in also, to the site's host, then sets home
// and siteurl to the site's URL. Hosts that are already the site's are
// skipped, so it does nothing to a database that names the site.
func (s *Service) rewriteRestoredHosts(ctx context.Context, r *jobs.Run, site store.Site, also ...string) error {
	if err := s.caches.FlushCache(ctx, site.ID); err != nil {
		return fmt.Errorf("flushing the object cache: %w", err)
	}
	target := URL(site)
	targetHost := strings.TrimPrefix(target, "https://")
	var sources []string
	for _, h := range also {
		if h = strings.ToLower(h); h != "" && h != targetHost && !slices.Contains(sources, h) {
			sources = append(sources, h)
		}
	}
	for _, option := range []string{"home", "siteurl"} {
		out, err := s.wpcliRestored(ctx, site.ID, "option", "get", option)
		if err != nil {
			return fmt.Errorf("reading the restored site's %s URL: %w", option, err)
		}
		u, err := url.Parse(strings.TrimSpace(out))
		if err != nil || u.Host == "" {
			return jobs.Permanent(fmt.Errorf("the restored site's %s URL %q has no host", option, strings.TrimSpace(out)))
		}
		if h := strings.ToLower(u.Host); h != targetHost && !slices.Contains(sources, h) {
			sources = append(sources, h)
		}
	}
	if len(sources) == 0 {
		r.Logf("The restored site's URLs are already %s.", target)
		return nil
	}
	for _, from := range sources {
		for _, sep := range []string{"//", `\/\/`} {
			out, err := s.wpcliRestored(ctx, site.ID,
				"search-replace", sep+from, sep+targetHost,
				"--all-tables-with-prefix", "--skip-columns=guid", "--format=count")
			if err != nil {
				return fmt.Errorf("replacing %s with %s: %w", sep+from, sep+targetHost, err)
			}
			r.Logf("Replaced %s with %s: %s.", sep+from, sep+targetHost, countOf(out))
		}
	}
	// search-replace changed the database under the object cache.
	if err := s.caches.FlushCache(ctx, site.ID); err != nil {
		return fmt.Errorf("flushing the object cache: %w", err)
	}
	for _, option := range []string{"home", "siteurl"} {
		if _, err := s.wpcliRestored(ctx, site.ID, "option", "update", option, target); err != nil {
			return fmt.Errorf("setting %s: %w", option, err)
		}
	}
	r.Logf("WordPress's address is %s.", target)
	return nil
}

// disableRestoreMaintenance marks the restore complete and puts the
// maintenance flag back as it was before the restore.
func (s *Service) disableRestoreMaintenance(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	if err := r.Set(dataRestoreComplete, true); err != nil {
		return err
	}
	var before bool
	if _, err := r.Get(dataMaintenanceBefore, &before); err != nil {
		return err
	}
	if before {
		r.Logf("The site was in maintenance mode before the restore, so it stays in it.")
		return nil
	}
	if _, err := store.NewSites(s.db).SetMaintenance(ctx, site.ID, false); err != nil {
		return err
	}
	if err := s.prov.SetMaintenance(ctx, site.ID, false); err != nil {
		return err
	}
	r.Logf("The site's domains serve the site again.")
	return nil
}

// finishRestore sets the status from the live phase and puts the restore
// in the job's result.
func (s *Service) finishRestore(ctx context.Context, r *jobs.Run) error {
	return s.finishRestoreAs(ctx, r, "pre_restore_backup_id")
}

// finishRestoreAs is finishRestore with the result key that names the job's
// own backup, the one that undoes it.
func (s *Service) finishRestoreAs(ctx context.Context, r *jobs.Run, backupKey string) error {
	if err := s.finishUpdate(ctx, r); err != nil {
		return err
	}
	src, err := jobRestoreSource(r)
	if err != nil {
		return err
	}
	result := map[string]string{"site_id": r.Job().TargetID, "source_site_id": src.SiteID, "snapshot_id": src.SnapshotID}
	if rec, found, err := s.backups.JobBackup(ctx, r); err != nil {
		return err
	} else if found {
		result[backupKey] = rec.ID
	}
	for k, v := range result {
		if err := r.SetResult(k, v); err != nil {
			return err
		}
	}
	return nil
}

// finishFailedRestore is the last Undo of a failed job of the restore
// pipeline: it sets the site's status and says in its status message what the
// failed job left behind. A site being deleted is left alone.
func (s *Service) finishFailedRestore(ctx context.Context, r *jobs.Run, noun string) error {
	siteID := r.Job().TargetID
	var rollbackErr, revertErr string
	if _, err := r.Get(dataRollbackError, &rollbackErr); err != nil {
		return err
	}
	if _, err := r.Get(dataRevertError, &revertErr); err != nil {
		return err
	}
	started, err := flag(r, dataRestoreStarted)
	if err != nil {
		return err
	}
	complete, err := flag(r, dataRestoreComplete)
	if err != nil {
		return err
	}
	backupNote := ""
	if rec, found, err := s.backups.JobBackup(ctx, r); err != nil {
		return err
	} else if found && rec.Status == store.BackupSucceeded {
		backupNote = " (pre-" + noun + " backup " + rec.ID + ")"
	}
	status := s.observedStatus(ctx, siteID)
	var prefix string
	switch {
	case complete:
		prefix = "the " + noun + " finished, but the job failed afterwards; the site may still be in maintenance mode"
	case rollbackErr != "":
		status = store.SiteFailed
		prefix = noun + " failed, and rolling back to the pre-" + noun + " backup failed too" + backupNote +
			"; the site stays in maintenance mode. Restore the pre-" + noun + " backup to try again"
	case revertErr != "":
		status = store.SiteFailed
		prefix = noun + " failed and was rolled back" + backupNote + ", but the site's previous WordPress version couldn't be put back"
	case started:
		prefix = noun + " failed and the site was rolled back to its pre-" + noun + " backup" + backupNote +
			"; it stays in maintenance mode until you turn it off"
	default:
		prefix = noun + " failed before changing the site"
	}
	msg := statusMessage(prefix, r.Job().Error)
	changed, err := store.NewSites(s.db).SetStatus(ctx, siteID, status, msg, store.SiteUpdating)
	if err != nil {
		return err
	}
	if changed {
		r.Warnf("The site is %s: %s.", status, prefix)
	}
	return nil
}
