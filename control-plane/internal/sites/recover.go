package sites

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Recovering sites on a new host (#96).
//
// When the state database is restored from its disaster recovery backup
// (package backup/statebackup), the site records come back but nothing
// else does: the new host has no site containers, volumes, or databases,
// and its object cache holds no site users. [PrepareRecovery] runs in the
// transaction that loads the backup. It fails every unfinished job (their
// work was lost with the old host, and resuming them would act on resources
// that don't exist), and for each site that was running, stopped, or being
// updated it sets the status to updating and queues a [RecoverJobType] job
// on the site's lock key. The site comes back stopped if an interrupted stop
// job asked for that, or if it was stopped before the job updating it
// (Sites.ResumeStatus); an interrupted start job brings it back running. The startup reconciliation leaves updating sites alone. Sites
// that were provisioning or being deleted are marked failed: their jobs were
// interrupted, and the operator decides what to do with them.
//
// The recover job's steps are:
//
//  1. prepare: pick the newest snapshot of the site in its client group's
//     repository (the repository, not the restored backup records, is the
//     truth: it may hold snapshots newer than the state backup). If it
//     was taken on a newer WordPress core than the site record says, the
//     site moves to an image with at least that core (as a restore does).
//     A staging site with no snapshot of its own is recovered from its
//     production site's newest one instead (most have none: their scheduled
//     backups are off). The job records the production site as the
//     snapshot's source and warns about it. The snapshot is read from the
//     client group's repository by the restic task of the staging site's
//     own containers; nothing needs the production site's containers, its
//     status, or its job's lock, so the two sites may recover at the same
//     time, in either order. Only the production site's record is read
//     (for its client group and URL), and the restored state has it.
//  2. create_database and create_object_cache: as provisioning does, with
//     the restored credentials. Both are idempotent.
//  3. start_stack: provisioner.CreateSite renders the site with the current
//     configuration, which includes the platform login key, so a key the
//     restore generated reaches the site here.
//  4. restore: backup.Engine.Restore puts the snapshot's wp-content and
//     database in place.
//  5. restart_site and upgrade_database: as a restore does.
//  6. rewrite_urls: only for a staging site restored from its production
//     site's snapshot, once its database is in place and its site is up:
//     the production URLs become the staging site's preview URL, as when
//     it was cloned. A snapshot of the site's own needs no rewrite.
//  7. finish: a site that was stopped is stopped again, and the status
//     is set from the containers.
//
// Every step converges, so a retried job, or one queued again, ends in the
// same place. A job that fails for good marks the site failed with the
// reason and leaves what it created for the operator to look at: deleting
// the site (or restoring it from a backup, see restore.go) removes or replaces
// it, and there is no earlier state to roll back to.

// RecoverJobType recreates a site from its newest backup after the state
// database was restored on a new host.
const RecoverJobType = "site.recover"

// Step names of the site.recover job. Keep them stable: a queued job
// resumes by step name.
const (
	stepRecoverPrepare = "prepare"
	stepRecoverRestore = "restore"
	stepRecoverFinish  = "finish"
)

// Job data keys of the site.recover job.
const (
	dataRecoverSnapshot = "snapshot_id"
	// dataRecoverSource is the site whose repository snapshots hold
	// dataRecoverSnapshot, when it isn't the recovering site's own: the
	// production site of a staging site.
	dataRecoverSource = "source_site_id"
	// dataRecoverSnapshotTime is when dataRecoverSnapshot was taken. A job
	// queued before it existed lacks it, which reads as unknown.
	dataRecoverSnapshotTime = "snapshot_time"
)

// interruptedByRestore is the error of jobs failed by PrepareRecovery.
const interruptedByRestore = "interrupted: the platform's state was restored from a backup on a new host"

// recoverPayload is the site.recover job's payload.
type recoverPayload struct {
	// Status is the status the site comes back with: running or stopped.
	Status store.SiteStatus `json:"status"`
}

// Recovery is what PrepareRecovery did.
type Recovery struct {
	// Recovering are the sites that got a recover job.
	Recovering []string
	// Failed are the sites marked failed because their provisioning or
	// deletion was interrupted.
	Failed []string
	// InterruptedJobs counts the unfinished jobs that were failed.
	InterruptedJobs int
}

// PrepareRecovery readies restored state for its new host, in the
// transaction q that loads it; see the comment at the top of this file.
func PrepareRecovery(ctx context.Context, q store.Querier, now time.Time) (Recovery, error) {
	var rec Recovery
	// Read before the jobs are failed below.
	power, err := interruptedPower(ctx, q)
	if err != nil {
		return rec, fmt.Errorf("prepare site recovery: %w", err)
	}
	if rec.InterruptedJobs, err = store.NewJobs(q).FailUnfinished(ctx, interruptedByRestore, now); err != nil {
		return rec, err
	}
	sites := store.NewSites(q)
	ids, err := sites.ListIDs(ctx)
	if err != nil {
		return rec, fmt.Errorf("prepare site recovery: %w", err)
	}
	for _, id := range ids {
		site, err := sites.Get(ctx, id)
		if err != nil {
			return rec, fmt.Errorf("prepare site recovery: %w", err)
		}
		switch site.Status {
		case store.SiteFailed:
			continue
		case store.SiteProvisioning, store.SiteDeleting:
			what := "provisioning"
			if site.Status == store.SiteDeleting {
				what = "deletion"
			}
			msg := "the site's " + what + " was interrupted by restoring the platform on a new host; delete the site, or create it again"
			if _, err := sites.SetStatus(ctx, id, store.SiteFailed, msg, site.Status); err != nil {
				return rec, fmt.Errorf("prepare site recovery: %w", err)
			}
			rec.Failed = append(rec.Failed, id)
			continue
		}
		if site.WPSaltsSecretRef == "" {
			continue
		}
		p := recoverPayload{Status: store.SiteRunning}
		if stopped, err := recoverStopped(ctx, sites, site, power); err != nil {
			return rec, fmt.Errorf("prepare site recovery: %w", err)
		} else if stopped {
			p.Status = store.SiteStopped
		}
		if _, err := sites.SetStatus(ctx, id, store.SiteUpdating, "recovering from its newest backup", site.Status); err != nil {
			return rec, fmt.Errorf("prepare site recovery: %w", err)
		}
		if _, err := jobs.Enqueue(ctx, q, jobs.Spec{
			Type: RecoverJobType, TargetType: store.JobTargetSite, TargetID: id,
			LockKey: store.SiteLockKey(id), Payload: p,
		}); err != nil {
			return rec, fmt.Errorf("prepare site recovery: %w", err)
		}
		rec.Recovering = append(rec.Recovering, id)
	}
	return rec, nil
}

// recoverStopped reports whether a site comes back stopped: as its
// interrupted start or stop job asked (power), or as it was before the job
// that was updating it, or as it is.
func recoverStopped(ctx context.Context, sites store.Sites, site store.Site, power map[string]store.SiteStatus) (bool, error) {
	if want, ok := power[site.ID]; ok {
		return want == store.SiteStopped, nil
	}
	if site.Status != store.SiteUpdating {
		return site.Status == store.SiteStopped, nil
	}
	before, err := sites.ResumeStatus(ctx, site.ID)
	if err != nil {
		return false, err
	}
	return before == store.SiteStopped, nil
}

// interruptedPower returns, for each site with an unfinished start or stop
// job, the status its newest one asked for. Those jobs don't change the
// site's status until they finish, so the status alone would bring the
// site back as it was before the request.
func interruptedPower(ctx context.Context, q store.Querier) (map[string]store.SiteStatus, error) {
	power := map[string]store.SiteStatus{}
	f := store.JobFilter{Types: []string{StartJobType, StopJobType}, States: unfinished, TargetType: store.JobTargetSite, Limit: 200}
	for {
		page, err := store.NewJobs(q).List(ctx, f)
		if err != nil {
			return nil, err
		}
		// Newest first: the first job seen for a site is its latest request.
		for _, j := range page.Jobs {
			if _, seen := power[j.TargetID]; seen {
				continue
			}
			power[j.TargetID] = store.SiteRunning
			if j.Type == StopJobType {
				power[j.TargetID] = store.SiteStopped
			}
		}
		if page.NextCursor == "" {
			return power, nil
		}
		f.Before = page.NextCursor
	}
}

func (s *Service) recoverHandler() jobs.Handler {
	return jobs.Handler{
		Type: RecoverJobType,
		Steps: []jobs.Step{
			{Name: stepRecoverPrepare, Do: s.prepareRecovery, Undo: s.failRecovery},
			{Name: stepCreateDatabase, Do: s.createDatabase},
			{Name: stepCreateCache, Do: s.createObjectCache},
			{Name: stepStartStack, Do: s.startRecoveredStack},
			{Name: stepRecoverRestore, Do: s.restoreRecovered},
			{Name: stepRestartSite, Do: s.restartRestoredSite},
			{Name: stepUpgradeDatabase, Do: s.upgradeRecoveredDatabase},
			{Name: stepRewriteURLs, Do: s.rewriteRecoveredURLs},
			{Name: stepRecoverFinish, Do: s.finishRecovery},
		},
	}
}

// recoveringSite loads the site of a recover job, which must be updating.
func (s *Service) recoveringSite(ctx context.Context, r *jobs.Run) (store.Site, error) {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return store.Site{}, err
	}
	if site.Status != store.SiteUpdating {
		return store.Site{}, jobs.Permanent(fmt.Errorf("the site is %s, not recovering", site.Status))
	}
	return site, nil
}

// prepareRecovery picks the snapshot to recover from, and moves the site to
// a core at least as new as the snapshot's.
func (s *Service) prepareRecovery(ctx context.Context, r *jobs.Run) error {
	site, err := s.recoveringSite(ctx, r)
	if err != nil {
		return err
	}
	var snapID string
	if ok, err := r.Get(dataRecoverSnapshot, &snapID); err != nil || ok {
		return err
	}
	source := site.ID
	snaps, err := s.engine.ListSnapshots(ctx, source)
	if err != nil {
		return fmt.Errorf("listing the site's backups: %w", err)
	}
	if len(snaps) == 0 && site.IsStaging() {
		source = site.ParentSiteID
		if snaps, err = s.engine.ListSnapshots(ctx, source); err != nil {
			return fmt.Errorf("listing the production site's backups: %w", err)
		}
	}
	if len(snaps) == 0 {
		if site.IsStaging() {
			return jobs.Permanent(errors.New("neither the staging site nor its production site has a backup to recover from"))
		}
		return jobs.Permanent(errors.New("the site has no backup to recover from"))
	}
	snap := snaps[len(snaps)-1]
	core, err := s.coreFor(snap.WPCoreVersion, site.WPCoreVersion)
	if err != nil {
		return jobs.Permanent(err)
	}
	if core != "" {
		if _, err := store.NewSites(s.db).SetWPCoreVersion(ctx, site.ID, core); err != nil {
			return err
		}
		r.Logf("The backup was taken on WordPress %s; the site moves to WordPress %s.", snap.WPCoreVersion, core)
	}
	if source != site.ID {
		if err := r.Set(dataRecoverSource, source); err != nil {
			return err
		}
	}
	// Before the snapshot, which is what marks this step as done: a retry
	// that finds the snapshot must find the time too.
	if err := setSnapshotTime(r, dataRecoverSnapshotTime, snap.Time); err != nil {
		return err
	}
	if err := r.Set(dataRecoverSnapshot, string(snap.ID)); err != nil {
		return err
	}
	if source != site.ID {
		r.Warnf("The staging site has no backup of its own, so it is recovered from the newest backup of its production site %s (taken %s); its content is as of then.",
			source, snap.Time.UTC().Format(time.RFC3339))
	}
	r.Logf("Recovering the site from backup %s, taken %s.", shortSnapshot(string(snap.ID)), snap.Time.UTC().Format(time.RFC3339))
	return nil
}

// startRecoveredStack creates the site's containers with its recorded
// domains, as provisioning does.
func (s *Service) startRecoveredStack(ctx context.Context, r *jobs.Run) error {
	site, err := s.recoveringSite(ctx, r)
	if err != nil {
		return err
	}
	aliases, err := store.NewSites(s.db).Aliases(ctx, site.ID)
	if err != nil {
		return err
	}
	dbRef, cacheRef, err := s.secretRefs(ctx, r)
	if err != nil {
		return err
	}
	cfg := SiteConfig(site, aliases, dbRef, cacheRef)
	if err := cfg.Validate(); err != nil {
		return jobs.Permanent(err)
	}
	if cfg.PinnedImages, err = s.pinnedImages(ctx, site); err != nil {
		return err
	}
	st, err := s.prov.CreateSite(ctx, cfg)
	if err != nil {
		return err
	}
	if err := s.recordRender(ctx, site.ID); err != nil {
		return err
	}
	r.Logf("Containers are up (%s).", st.Phase)
	return nil
}

// restoreRecovered restores the chosen snapshot into the site.
func (s *Service) restoreRecovered(ctx context.Context, r *jobs.Run) error {
	site, err := s.recoveringSite(ctx, r)
	if err != nil {
		return err
	}
	var snapID string
	ok, err := r.Get(dataRecoverSnapshot, &snapID)
	if err != nil {
		return err
	}
	if !ok {
		return jobs.Permanent(errors.New("the job has no snapshot to recover from"))
	}
	source, err := recoverSource(r, site.ID)
	if err != nil {
		return err
	}
	if err := s.engine.Restore(ctx, source, backup.SnapshotID(snapID), site.ID, backup.FullRestore()); err != nil {
		return permanentRestoreError(err)
	}
	r.Logf("Restored the backup's wp-content and database.")
	return nil
}

// recoverSource returns the site whose repository snapshots the job
// restores: the recovering site, unless prepare chose its production site.
func recoverSource(r *jobs.Run, siteID string) (string, error) {
	var source string
	if _, err := r.Get(dataRecoverSource, &source); err != nil {
		return "", err
	}
	if source == "" {
		return siteID, nil
	}
	return source, nil
}

// rewriteRecoveredURLs moves a staging site restored from its production
// site's snapshot to its preview URL, as cloning does. A site restored from
// its own snapshot keeps the URLs the snapshot has, as recovery always did.
func (s *Service) rewriteRecoveredURLs(ctx context.Context, r *jobs.Run) error {
	site, err := s.recoveringSite(ctx, r)
	if err != nil {
		return err
	}
	source, err := recoverSource(r, site.ID)
	if err != nil {
		return err
	}
	if source == site.ID {
		return nil
	}
	var also []string
	prod, err := store.NewSites(s.db).Get(ctx, source)
	switch {
	case err == nil:
		also = append(also, strings.TrimPrefix(URL(prod), "https://"))
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	return s.rewriteRestoredHosts(ctx, r, site, also...)
}

// upgradeRecoveredDatabase applies the site's core's database schema to the
// restored database, which may come from an older core.
func (s *Service) upgradeRecoveredDatabase(ctx context.Context, r *jobs.Run) error {
	site, err := s.recoveringSite(ctx, r)
	if err != nil {
		return err
	}
	if _, err := s.wpcliRestored(ctx, site.ID, "core", "update-db"); err != nil {
		return fmt.Errorf("upgrading the restored database to WordPress %s: %w", site.WPCoreVersion, err)
	}
	r.Logf("The restored database is up to date with WordPress %s.", site.WPCoreVersion)
	return nil
}

// finishRecovery stops a site that was stopped, and sets the status from
// its containers.
func (s *Service) finishRecovery(ctx context.Context, r *jobs.Run) error {
	site, err := s.recoveringSite(ctx, r)
	if err != nil {
		return err
	}
	var p recoverPayload
	if err := r.Payload(&p); err != nil {
		return jobs.Permanent(err)
	}
	if p.Status == store.SiteStopped {
		if err := s.prov.StopSite(ctx, site.ID); err != nil {
			return fmt.Errorf("stopping the site: %w", err)
		}
	}
	// Before the status leaves updating, which a retry of this step needs.
	sites := store.NewSites(s.db)
	if site.IsStaging() {
		// A copy of the production site's snapshot is as old as it; the
		// staging site's own backup holds data of an unknown age.
		database, uploads := store.AsOfClear(), store.AsOfClear()
		if source, err := recoverSource(r, site.ID); err != nil {
			return err
		} else if source != site.ID {
			taken, err := getSnapshotTime(r, dataRecoverSnapshotTime)
			if err != nil {
				return err
			}
			database, uploads = store.AsOfSet(taken), store.AsOfSet(taken)
		}
		if _, err := sites.SetStagingAsOf(ctx, site.ID, database, uploads); err != nil {
			return err
		}
	}
	status := s.observedStatus(ctx, site.ID)
	if _, err := sites.SetStatus(ctx, site.ID, status, "", store.SiteUpdating); err != nil {
		return err
	}
	if err := r.SetResult("site_id", site.ID); err != nil {
		return err
	}
	r.Logf("The site is recovered and %s.", status)
	return nil
}

// failRecovery marks the site failed with the job's error. Nothing is
// removed: there is no earlier state of the site to go back to.
func (s *Service) failRecovery(ctx context.Context, r *jobs.Run) error {
	msg := statusMessage("recovering the site from its backup failed", r.Job().Error)
	changed, err := store.NewSites(s.db).SetStatus(ctx, r.Job().TargetID, store.SiteFailed, msg, store.SiteUpdating)
	if err != nil {
		return err
	}
	if changed {
		r.Warnf("The site is marked failed.")
	}
	return nil
}
