package sites

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/sitebackup"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Refreshing a staging site from its production site (#203, design doc §5.4,
// §5.11).
//
// [Service.RefreshStaging] checks the staging site (running, with no job
// working on it) and the request, then in one transaction sets the staging
// site to updating and, unless the request names a snapshot, queues a
// site.backup job of kind sync on the production site, under the production
// site's lock key, with a [RefreshJobType] job on the staging site, under its
// own lock key, after it (package jobs, "Job chains").
//
// The site.refresh job is the restore pipeline (restore.go) with a scope, the
// production site's snapshot as its source, and these differences:
//
//   - prepare reads the snapshot from the awaited backup job's result, or from
//     the payload, and checks it is still in the repository. A failed backup
//     fails the job here, permanently (jobs.AwaitedJob.Err). That is before
//     the pre-refresh backup and maintenance mode, so the rollback only
//     has to set the status back from the live phase: the staging site's data,
//     core, and maintenance flag were never touched.
//   - rewrite_urls also rewrites the production site's current host, like a
//     clone, in case the snapshot predates a go-live.
//   - configure_plugins, after rewrite_urls, converges the default plugin
//     stack, like a clone. The dump brings production's active_plugins and
//     Nginx Helper settings, which may not match the code this staging site
//     runs: a database-only refresh keeps the staging site's own wp-content,
//     and production may have drifted since its last reconciliation. The
//     plugins talk to the object cache and the page cache with the staging
//     site's own wp-config.php, so they only have to be there and be on. A
//     refresh without the database skips it: converging writes to the
//     database, which that scope promises to leave alone, and the next
//     reconciliation repairs whatever such a refresh removed.
//   - finish records the snapshot and the time on the staging site. Only a
//     refresh that got this far records them.
//
// Which other steps each scope runs is explained at databaseInScope.
//
// A rollback restores the pre-refresh backup in full, whatever the scope,
// like any restore pipeline job, and leaves the site in maintenance mode.
//
// Nothing about the staging site's settings comes from the copied data. The
// mail switch, preview protection, domains, credentials, and environment are
// in its site record and its rendered wp-config.php and routes, which a
// refresh doesn't change, except to re-render them from the record when it
// moves the core.

// RefreshJobType copies a production site's snapshot into its staging site.
const RefreshJobType = "site.refresh"

// dataRefreshBackupID is the job data key of the ID of the sync backup the
// job waited for.
const dataRefreshBackupID = "refresh_backup_id"

// StagingRefresh is a request to refresh a staging site.
type StagingRefresh struct {
	// Files is how much of wp-content to copy.
	Files backup.RestoreFiles
	// Database says whether to replace the database with the snapshot's.
	Database bool
	// SnapshotID is an existing snapshot of the production site, or a unique
	// prefix of at least 8 characters, to copy instead of taking a fresh sync
	// backup.
	SnapshotID string
	// CreatedByUserID is the user starting the jobs.
	CreatedByUserID string
}

// StagingRefreshQueued is what RefreshStaging changed and queued.
type StagingRefreshQueued struct {
	// Site is the staging site, with status updating.
	Site store.Site
	// Job is the site.refresh job.
	Job store.Job
	// BackupJob is the sync backup of the production site that Job waits for;
	// nil when the request named a snapshot.
	BackupJob *store.Job
	// SnapshotID is the full ID of the snapshot the request named, if any.
	SnapshotID string
}

// RefreshStaging checks the request and queues the jobs that copy the
// production site into the staging site, with status updating. A production
// site as the target, or an invalid scope or snapshot, is a *ValidationError.
// A staging site a job is working on is ErrBusy, one that never finished
// provisioning ErrNotProvisioned, and any other that isn't running
// ErrNotRunning, since the job needs WP-CLI. When a fresh backup is needed, a
// production site a job is working on is ErrBusy and one marked failed
// ErrSourceFailed. Nothing changes then.
func (s *Service) RefreshStaging(ctx context.Context, stagingID string, req StagingRefresh) (StagingRefreshQueued, error) {
	if !s.restores() {
		return StagingRefreshQueued{}, ErrNoBackups
	}
	// Fail fast, before asking the repository; the transaction checks again.
	staging, err := s.refreshableStaging(ctx, store.NewSites(s.db), stagingID)
	if err != nil {
		return StagingRefreshQueued{}, err
	}
	scope := backup.RestoreOptions{Files: req.Files, Database: req.Database}
	if err := scope.Validate(); err != nil {
		return StagingRefreshQueued{}, &ValidationError{Fields: []FieldError{{"/files", err.Error()}}}
	}
	var snapID string
	if req.SnapshotID != "" {
		snapID, err = s.checkRefreshSnapshot(ctx, staging, scope, req.SnapshotID)
		if err != nil {
			return StagingRefreshQueued{}, err
		}
	}

	var out StagingRefreshQueued
	err = s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		if _, err := s.refreshableStaging(ctx, sites, stagingID); err != nil {
			return err
		}
		spec := jobs.Spec{
			Type:       RefreshJobType,
			TargetType: store.JobTargetSite,
			TargetID:   stagingID,
			Payload: restorePayload{
				SourceSiteID: staging.ParentSiteID, SnapshotID: snapID,
				Scope: &restoreScope{Files: scope.Files, Database: scope.Database},
			},
			LockKey:         store.SiteLockKey(stagingID),
			CreatedByUserID: req.CreatedByUserID,
		}
		if snapID == "" {
			prod, err := sites.Get(ctx, staging.ParentSiteID)
			if err != nil {
				return err
			}
			if err := checkStagingSource(prod); err != nil {
				return err
			}
			// The backup runs under the production site's lock key, the
			// refresh under the staging site's; a job holds one key.
			bj, err := sitebackup.Enqueue(ctx, q, prod.ID, store.BackupSync, req.CreatedByUserID)
			if err != nil {
				return err
			}
			out.BackupJob, spec.After = &bj, bj.ID
		}
		// Conditional, so of two concurrent requests that both saw the site
		// running, only one gets to queue.
		changed, err := sites.SetStatus(ctx, stagingID, store.SiteUpdating, "", store.SiteRunning)
		if err != nil {
			return err
		}
		if !changed {
			return fmt.Errorf("%w: the site's status just changed", ErrBusy)
		}
		if out.Job, err = jobs.Enqueue(ctx, q, spec); err != nil {
			return err
		}
		out.Site, err = sites.Get(ctx, stagingID)
		return err
	})
	if err != nil {
		return StagingRefreshQueued{}, err
	}
	out.SnapshotID = snapID
	attrs := []any{"site_id", stagingID, "parent_site_id", staging.ParentSiteID, "files", scope.Files,
		"database", scope.Database, "job_id", out.Job.ID}
	if out.BackupJob != nil {
		attrs = append(attrs, "backup_job_id", out.BackupJob.ID)
	} else {
		attrs = append(attrs, "snapshot_id", snapID)
	}
	s.log.InfoContext(ctx, "staging site refresh queued", attrs...)
	return out, nil
}

// refreshableStaging returns the site if it is a staging site that can be
// refreshed now. A production site is refused before its status is looked at:
// it could never be refreshed, whatever it is doing.
func (s *Service) refreshableStaging(ctx context.Context, sites store.Sites, siteID string) (store.Site, error) {
	site, err := sites.Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Site{}, fmt.Errorf("site %s: %w", siteID, ErrNotFound)
	}
	if err != nil {
		return store.Site{}, err
	}
	if !site.IsStaging() {
		return store.Site{}, &ValidationError{Fields: []FieldError{{"/site_id", "is a production site; only staging sites are refreshed"}}}
	}
	if site, err = s.manageable(ctx, sites, siteID); err != nil {
		return store.Site{}, err
	}
	if site.Status != store.SiteRunning {
		return store.Site{}, fmt.Errorf("%w (it is %s)", ErrNotRunning, site.Status)
	}
	return site, nil
}

// checkRefreshSnapshot checks that the snapshot is the production site's, and
// that an image can run its database if the scope copies that. It returns the
// snapshot's full ID.
func (s *Service) checkRefreshSnapshot(ctx context.Context, staging store.Site, scope backup.RestoreOptions, snapID string) (string, error) {
	invalid := func(detail string) error {
		return &ValidationError{Fields: []FieldError{{"/snapshot_id", detail}}}
	}
	snap, err := s.findSnapshot(ctx, staging.ParentSiteID, snapID)
	if errors.Is(err, backup.ErrSnapshotNotFound) {
		return "", invalid("is not a snapshot of site " + staging.ParentSiteID + " in its backup repository")
	}
	if err != nil {
		return "", err
	}
	if scope.Database {
		if _, err := s.coreFor(snap.WPCoreVersion, staging.WPCoreVersion); err != nil {
			return "", invalid(err.Error())
		}
	}
	return string(snap.ID), nil
}

func (s *Service) refreshHandler() jobs.Handler {
	return s.restoreFlowHandler(restoreFlow{
		jobType:         RefreshJobType,
		noun:            "refresh",
		prepare:         s.prepareRefresh,
		rewriteFrom:     s.sourceSiteHost,
		convergePlugins: true,
		finish:          s.finishRefresh,
	})
}

// prepareRefresh resolves the snapshot to copy. Everything it can refuse, it
// refuses before the job's first change to the staging site.
func (s *Service) prepareRefresh(ctx context.Context, r *jobs.Run) error {
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
	snap, backupID, err := s.copySnapshot(ctx, r, p.SourceSiteID, p.SnapshotID)
	if err != nil {
		return err
	}
	if scope.Database {
		// A fresh backup may have run after a core update of the production
		// site, to a core no site image has any more.
		if _, err := s.coreFor(snap.WPCoreVersion, site.WPCoreVersion); err != nil {
			return jobs.Permanent(err)
		}
	}
	for key, v := range map[string]string{
		dataRefreshBackupID: backupID, dataRestoreSnapshot: string(snap.ID), dataRestoreSnapshotCore: snap.WPCoreVersion,
	} {
		if err := r.Set(key, v); err != nil {
			return err
		}
	}
	r.Logf("Refreshing %s from snapshot %s of site %s (%s), after taking a pre-refresh backup.",
		site.PreviewDomain, shortSnapshot(string(snap.ID)), p.SourceSiteID, describeScope(scope))
	return nil
}

// finishRefresh finishes like a restore, then records the refresh on the
// staging site, so a site whose status can't be settled isn't reported as
// refreshed.
func (s *Service) finishRefresh(ctx context.Context, r *jobs.Run) error {
	if err := s.finishRestore(ctx, r); err != nil {
		return err
	}
	src, err := jobRestoreSource(r)
	if err != nil {
		return err
	}
	if _, err := store.NewSites(s.db).SetStagingRefreshed(ctx, r.Job().TargetID, src.SnapshotID, s.now()); err != nil {
		return err
	}
	var backupID string
	if _, err := r.Get(dataRefreshBackupID, &backupID); err != nil {
		return err
	}
	if backupID != "" {
		return r.SetResult("backup_id", backupID)
	}
	return nil
}
