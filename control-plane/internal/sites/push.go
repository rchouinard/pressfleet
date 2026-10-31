package sites

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Pushing a staging site to its production site (#204, design doc §5.4, §5.5,
// §5.11). It is the one staging workflow that writes to a production site.
//
// [Service.PushStaging] checks both sites and the request, then in one
// transaction queues a site.backup job of kind sync on the staging site, under
// the staging site's lock key, and a [PushJobType] job on the production site,
// under the production site's, after it (package jobs, "Job chains"), and sets
// the production site to updating. The staging site's status doesn't change:
// a backup is all that happens to it, and that can run on a stopped site too.
//
// The site.push job is the restore pipeline (restore.go) with a scope, the
// staging site's fresh snapshot as its source, and these differences:
//
//   - prepare refuses, before the pre-push backup and maintenance mode, a
//     staging site that has gone or failed meanwhile, a failed backup, a
//     snapshot that isn't in the repository, and a snapshot whose core the
//     production site's policy doesn't allow. The rollback then only sets the
//     production site's status from the live phase and says why.
//   - rewrite_urls also rewrites the staging site's current host. The database
//     names the staging preview domain in home, siteurl, and content.
//   - configure_plugins converges the default plugin stack after a database
//     push. A push that keeps the database only puts back what the pushed
//     files removed (see repairPushedPlugins).
//   - finish records the push on the staging site, if it still exists, and the
//     job's result names the pre-push backup: what to restore to undo the push.
//
// A rollback restores the pre-push backup in full, whatever the scope, moves
// the core back if the job moved it, and leaves the site in maintenance mode
// (§5.4 step 7), like any restore pipeline job.
//
// # The core guard
//
// A database brings its schema with it, so a database push can move production
// to a newer core (update_core). Pushing must not be a way around a hold or a
// major release that wasn't approved (§5.5), so a database push is refused if
// the core it would move production to is on a branch production's policy
// doesn't allow; coreupdate.Plan.AllowsBranch decides, from the same plan the
// rollout follows. It is checked when the push is requested, against the
// staging site's record, and again in prepare, against the snapshot's core,
// which is what counts: the staging site's core can change while the job waits.
// The guard looks at the core production would move to rather than the
// snapshot's, because update_core takes the oldest image with at least the
// snapshot's core, which is on a newer branch if the snapshot's branch has left
// the manifest. A push without the database doesn't move the core, so it isn't
// guarded.
//
// # What happens to the staging site while the push waits
//
// The sync backup holds the staging site's lock key only until it finishes, so
// the staging site can change or be deleted before site.push is leased:
//
//   - Deleted before the backup ran: a deletion request sets the site deleting,
//     which fails the backup, and so the push.
//   - Deleted or being deleted after the backup: prepare refuses. The snapshot
//     outlives a deletion that keeps backups, and could be restored safely, but
//     production shouldn't get a copy of a site its operator just threw away.
//   - Deleted after prepare: the job carries on from the snapshot if it can.
//     The URL rewrite finds the staging host in home and siteurl, and finish
//     has nowhere to record the push, which it logs. If the deletion took the
//     snapshot along, the restore fails and the job rolls back.
//   - Marked failed: prepare refuses, as PushStaging does. We can't tell
//     whether the snapshot was taken before whatever failed.
//   - Stopped, or being updated by a job queued after the backup: the snapshot
//     is complete, so the push goes ahead.
//   - Refreshed: a refresh sets the staging site to updating, so PushStaging
//     refuses while one is queued or running. One requested after the push was
//     queued needs either a fresh backup of production, which is refused while
//     production is updating, or a named snapshot, and then it queues behind
//     the sync backup on the staging site's lock key.
//
// # Settings
//
// Nothing about the production site's settings comes from the snapshot. Its
// PHP version, resources, domains, preview protection, mail switch, and
// environment are in its site record and its rendered wp-config.php and routes,
// which a push doesn't change, except to re-render them from the record when it
// moves the core.

// PushJobType copies a staging site's fresh snapshot into its production site.
const PushJobType = "site.push"

// dataPushBackupID is the job data key of the ID of the sync backup the job
// waited for.
const dataPushBackupID = "push_backup_id"

// resultPrePushBackup is the job result key of the production site's pre-push
// backup, which undoes the push when restored.
const resultPrePushBackup = "pre_push_backup_id"

// ErrStagingFailed means a staging site marked failed can't be pushed: it has
// nothing reliable to copy to production.
var ErrStagingFailed = errors.New("the staging site is marked failed; restore or fix it before pushing it to production")

// StagingPush is a request to push a staging site to its production site.
type StagingPush struct {
	// Files is how much of wp-content to copy.
	Files backup.RestoreFiles
	// Database says whether to replace the database with the snapshot's.
	Database bool
	// CreatedByUserID is the user starting the jobs.
	CreatedByUserID string
}

// StagingPushQueued is what PushStaging changed and queued.
type StagingPushQueued struct {
	// Site is the production site, with status updating.
	Site store.Site
	// Job is the site.push job.
	Job store.Job
	// BackupJob is the sync backup of the staging site that Job waits for.
	BackupJob store.Job
}

// PushStaging checks the request and queues the jobs that copy the staging
// site into its production site, which gets status updating. A production site
// as the target, an invalid scope, or a database whose core production's policy
// doesn't allow is a *ValidationError. Either site being worked on by a job is
// ErrBusy, or never having finished provisioning ErrNotProvisioned; a staging
// site marked failed is ErrStagingFailed, and a production site that isn't
// running ErrNotRunning, since the job needs WP-CLI. Nothing changes then.
func (s *Service) PushStaging(ctx context.Context, stagingID string, req StagingPush) (StagingPushQueued, error) {
	if !s.restores() {
		return StagingPushQueued{}, ErrNoBackups
	}
	// Fail fast; the transaction checks again.
	_, prod, err := s.pushableSites(ctx, store.NewSites(s.db), stagingID)
	if err != nil {
		return StagingPushQueued{}, err
	}
	scope := backup.RestoreOptions{Files: req.Files, Database: req.Database}
	if err := scope.Validate(); err != nil {
		return StagingPushQueued{}, &ValidationError{Fields: []FieldError{{"/files", err.Error()}}}
	}

	var out StagingPushQueued
	err = s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		// The rows stay locked until the push is queued, production first like
		// CreateStaging and Delete. A deletion of the staging site is either
		// seen here, or comes after this commit and prepare finds it.
		for _, siteID := range []string{prod.ID, stagingID} {
			if _, err := sites.Lock(ctx, siteID); errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("site %s: %w", siteID, ErrNotFound)
			} else if err != nil {
				return err
			}
		}
		staging, prod, err := s.pushableSites(ctx, sites, stagingID)
		if err != nil {
			return err
		}
		if scope.Database {
			var refused *pushCoreError
			if err := s.checkPushCore(ctx, q, prod, staging.WPCoreVersion); errors.As(err, &refused) {
				return &ValidationError{Fields: []FieldError{{"/database", refused.Error()}}}
			} else if err != nil {
				return err
			}
		}
		// The backup runs under the staging site's lock key, the push under
		// production's; a job holds one key.
		if out.BackupJob, err = sitebackup.Enqueue(ctx, q, stagingID, store.BackupSync, req.CreatedByUserID); err != nil {
			return err
		}
		// Conditional, so of two concurrent requests that both saw the site
		// running, only one gets to queue.
		changed, err := sites.SetStatus(ctx, prod.ID, store.SiteUpdating, "", store.SiteRunning)
		if err != nil {
			return err
		}
		if !changed {
			return fmt.Errorf("%w: the production site's status just changed", ErrBusy)
		}
		out.Job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:       PushJobType,
			TargetType: store.JobTargetSite,
			TargetID:   prod.ID,
			Payload: restorePayload{
				SourceSiteID: stagingID,
				Scope:        &restoreScope{Files: scope.Files, Database: scope.Database},
			},
			LockKey:         store.SiteLockKey(prod.ID),
			CreatedByUserID: req.CreatedByUserID,
			After:           out.BackupJob.ID,
		})
		if err != nil {
			return err
		}
		out.Site, err = sites.Get(ctx, prod.ID)
		return err
	})
	if err != nil {
		return StagingPushQueued{}, err
	}
	s.log.InfoContext(ctx, "staging site push queued", "site_id", stagingID, "production_site_id", prod.ID,
		"files", scope.Files, "database", scope.Database, "job_id", out.Job.ID, "backup_job_id", out.BackupJob.ID)
	return out, nil
}

// pushableSites returns the staging site and its production site if a push can
// start now. A production site as the target is refused before its status is
// looked at: it could never be pushed, whatever it is doing.
func (s *Service) pushableSites(ctx context.Context, sites store.Sites, stagingID string) (staging, prod store.Site, err error) {
	site, err := sites.Get(ctx, stagingID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Site{}, store.Site{}, fmt.Errorf("site %s: %w", stagingID, ErrNotFound)
	}
	if err != nil {
		return store.Site{}, store.Site{}, err
	}
	if !site.IsStaging() {
		return store.Site{}, store.Site{}, &ValidationError{Fields: []FieldError{{"/site_id", "is a production site; only staging sites are pushed"}}}
	}
	if staging, err = s.manageable(ctx, sites, stagingID); err != nil {
		return store.Site{}, store.Site{}, err
	}
	if staging.Status == store.SiteFailed {
		return store.Site{}, store.Site{}, ErrStagingFailed
	}
	if prod, err = s.manageable(ctx, sites, staging.ParentSiteID); err != nil {
		return store.Site{}, store.Site{}, fmt.Errorf("the production site: %w", err)
	}
	if prod.Status != store.SiteRunning {
		return store.Site{}, store.Site{}, fmt.Errorf("the production site: %w (it is %s)", ErrNotRunning, prod.Status)
	}
	return staging, prod, nil
}

// pushCoreError says why the core guard refuses a database push. Its message
// is for the person asking for the push.
type pushCoreError struct{ detail string }

func (e *pushCoreError) Error() string { return e.detail }

// checkPushCore is the core guard (see the top of this file): it returns a
// *pushCoreError if a database from core would move the production site to a
// core its policy doesn't allow, or to one no image has.
func (s *Service) checkPushCore(ctx context.Context, q store.Querier, prod store.Site, core string) error {
	if core == "" {
		// Every backup records its core, so this isn't a snapshot the job took.
		// Without knowing the schema, neither the guard nor update_core can do
		// their work.
		return &pushCoreError{"can't be checked against the production site's core policy: the staging site's snapshot doesn't say which WordPress core its database is from"}
	}
	target, err := s.coreFor(core, prod.WPCoreVersion)
	if err != nil {
		return &pushCoreError{err.Error()}
	}
	if target == "" {
		return nil
	}
	branch, err := catalog.Branch(target)
	if err != nil {
		return &pushCoreError{err.Error()}
	}
	plan, err := s.rollouts.corePlan(ctx, q, prod)
	if err != nil {
		return err
	}
	if plan.AllowsBranch(branch) {
		return nil
	}
	policy := "hasn't approved that branch"
	if plan.Held {
		policy = "holds it on branch " + plan.Branch
	}
	return &pushCoreError{fmt.Sprintf(
		"would move the production site from WordPress %s to %s, because the staging site's database is from WordPress %s, but production's core policy %s; approve branch %s for the production site first, or push without the database",
		prod.WPCoreVersion, target, core, policy, branch)}
}

func (s *Service) pushHandler() jobs.Handler {
	return s.restoreFlowHandler(restoreFlow{
		jobType:         PushJobType,
		noun:            "push",
		prepare:         s.preparePush,
		rewriteFrom:     s.sourceSiteHost,
		convergePlugins: true,
		repairPlugins:   s.repairPushedPlugins,
		finish:          s.finishPush,
	})
}

// preparePush resolves the snapshot to copy. Everything it can refuse, it
// refuses before the job's first change to the production site.
func (s *Service) preparePush(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := restoreJobPayload(r)
	if err != nil {
		return err
	}
	scope := p.scope()
	if err := scope.Validate(); err != nil {
		return jobs.Permanent(err)
	}
	if err := s.checkPushSource(ctx, site, p.SourceSiteID); err != nil {
		return err
	}
	snap, backupID, err := s.copySnapshot(ctx, r, p.SourceSiteID, p.SnapshotID)
	if err != nil {
		return err
	}
	if scope.Database {
		var refused *pushCoreError
		if err := s.checkPushCore(ctx, s.db, site, snap.WPCoreVersion); errors.As(err, &refused) {
			return jobs.Permanent(fmt.Errorf("refusing the database push: %w", refused))
		} else if err != nil {
			return err
		}
	}
	for key, v := range map[string]string{
		dataPushBackupID: backupID, dataRestoreSnapshot: string(snap.ID), dataRestoreSnapshotCore: snap.WPCoreVersion,
	} {
		if err := r.Set(key, v); err != nil {
			return err
		}
	}
	if err := setSnapshotTime(r, dataRestoreSnapshotTime, snap.Time); err != nil {
		return err
	}
	r.Logf("Pushing snapshot %s of staging site %s (%s) to %s, after taking a pre-push backup.",
		shortSnapshot(string(snap.ID)), p.SourceSiteID, describeScope(scope), URL(site))
	return nil
}

// checkPushSource refuses a staging site that went away or failed while the
// push waited. See the top of this file.
func (s *Service) checkPushSource(ctx context.Context, prod store.Site, stagingID string) error {
	staging, err := store.NewSites(s.db).Get(ctx, stagingID)
	if errors.Is(err, store.ErrNotFound) {
		return jobs.Permanent(fmt.Errorf("staging site %s was deleted while the push waited", stagingID))
	}
	if err != nil {
		return err
	}
	switch {
	case staging.ParentSiteID != prod.ID:
		return jobs.Permanent(fmt.Errorf("site %s isn't a staging site of this site", stagingID))
	case staging.Status == store.SiteDeleting:
		return jobs.Permanent(fmt.Errorf("staging site %s is being deleted", stagingID))
	case staging.Status == store.SiteFailed:
		return jobs.Permanent(fmt.Errorf("staging site %s was marked failed while the push waited", stagingID))
	}
	return nil
}

// repairPushedPlugins is the configure_plugins step of a push that keeps the
// database. The pushed plugins directory replaces production's, and the staging
// site may lack a default plugin, or (with all of wp-content) the object cache
// drop-in. That matters more here than on a staging site:
//
//   - Without Nginx Helper, publishing stops purging the page cache, so visitors
//     keep getting stale pages.
//   - Without Two Factor, required two-factor isn't enforced (the must-use
//     plugin fails open, so as not to lock administrators out).
//   - Without Redis Object Cache or the drop-in, the site loses its object cache.
//
// Reconciliation repairs all of that, but only runs when the control plane
// starts, which may be months away. So this puts back what is missing, from the
// site image, without converging: it doesn't activate anything, or touch
// settings or capabilities, because the database that says so is production's
// own and stays as it is. A reinstalled plugin that production has active is
// active again as soon as its files are back. A push that removes nothing makes
// a single read-only WP-CLI call. Installing a plugin only makes WordPress
// forget its cached list of plugin updates.
//
// WP-CLI failing here means WordPress can't load the pushed code, which would
// otherwise only surface when visitors come back after maintenance mode, so it
// fails the job permanently and the push rolls back.
func (s *Service) repairPushedPlugins(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	// The object cache still holds what the replaced plugins stored, in their
	// own format, and WP-CLI is about to load the new ones.
	if err := s.caches.FlushCache(ctx, site.ID); err != nil {
		return fmt.Errorf("flushing the object cache: %w", err)
	}
	return permanentWPCLIError(s.repairDefaultPlugins(ctx, r, site.ID))
}

func (s *Service) repairDefaultPlugins(ctx context.Context, r *jobs.Run, siteID string) error {
	sp, err := s.listPlugins(ctx, siteID)
	if err != nil {
		return err
	}
	reinstalled := false
	for _, slug := range s.wantedPlugins() {
		if _, ok := sp.plugins[slug]; ok {
			continue
		}
		r.Warnf("The staging site's plugins don't include %s, which the platform depends on; reinstalling it on the production site.", slug)
		ok, err := s.installBundledPlugin(ctx, r, siteID, slug)
		if err != nil {
			return err
		}
		reinstalled = reinstalled || ok
	}
	if reinstalled {
		if sp, err = s.listPlugins(ctx, siteID); err != nil {
			return err
		}
	}
	return s.enableObjectCache(ctx, r, siteID, sp)
}

// permanentWPCLIError marks WP-CLI failing as permanent, keeping what it said
// but not a PHP stack trace of several kilobytes.
func permanentWPCLIError(err error) error {
	var ee *provisioner.ExecError
	if errors.As(err, &ee) {
		short := *ee
		short.Stdout, short.Stderr = "", trimWPCLIError(ee.Stderr)
		return jobs.Permanent(fmt.Errorf("WordPress can't load the pushed code: %w", &short))
	}
	if errors.Is(err, provisioner.ErrSiteNotFound) {
		return jobs.Permanent(err)
	}
	return err
}

// finishPush finishes like a restore, then records the push on the staging
// site, so a site whose status can't be settled isn't reported as pushed. The
// staging site may have been deleted since prepare, which changes nothing about
// the push.
func (s *Service) finishPush(ctx context.Context, r *jobs.Run) error {
	if err := s.finishRestoreAs(ctx, r, resultPrePushBackup); err != nil {
		return err
	}
	src, err := jobRestoreSource(r)
	if err != nil {
		return err
	}
	sites := store.NewSites(s.db)
	recorded, err := sites.SetStagingPushed(ctx, src.SiteID, s.now())
	if err != nil {
		return err
	}
	// Production's copy of what was pushed is now as old as the snapshot, so
	// that is where what a later push can lose starts.
	database, uploads := copiedAsOf(src)
	if _, err := sites.SetStagingAsOf(ctx, src.SiteID, database, uploads); err != nil {
		return err
	}
	if !recorded {
		r.Warnf("Staging site %s no longer exists, so the push isn't recorded on it.", src.SiteID)
	}
	var backupID string
	if _, err := r.Get(dataPushBackupID, &backupID); err != nil {
		return err
	}
	if backupID != "" {
		return r.SetResult("backup_id", backupID)
	}
	return nil
}
