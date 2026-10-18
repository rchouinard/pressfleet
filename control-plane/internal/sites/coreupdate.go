package sites

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/catalog"
	"github.com/rchouinard/pressfleet/control-plane/internal/coreupdate"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// WordPress core updates (#57, design doc §5.5).
//
// A [CoreUpdateJobType] job moves one site to another WordPress core
// release. The core rollout (rollout.go) queues one for the next site whose
// plan (package coreupdate) has a target, and [Service.UpdateCore] queues
// one by hand. Either way the site must be running; its status is updating
// while the job works. The job's steps are:
//
//  1. prepare: check the site still runs the version the job moves it
//     from. Its Undo runs last in a rollback and records the outcome.
//  2. record_backup and take_snapshot (package backup/sitebackup): a
//     pre-update backup (kind pre_update).
//  3. record_schema: the database schema version (the db_version option)
//     before the update.
//  4. recreate: record the new core version on the site, which selects both
//     of its images (site-php and site-nginx carry the same WordPress
//     release, so the two containers never run different cores), and
//     recreate the containers on them with provisioner.UpdateSite, which
//     waits for them to be healthy.
//  5. upgrade_database: `wp core update-db`, then the schema version
//     after it.
//  6. health_check: an HTTP request to the site's web server
//     (provisioner.Prober) must not fail or answer 5xx, and WP-CLI must
//     report the new core version and load WordPress (`wp core
//     is-installed`). It retries for up to the health timeout while the
//     containers settle; a wrong core version fails at once.
//  7. finish: set the status from the live phase and record the outcome.
//
// # Revert
//
// Any failure before the site is healthy reverts the update, newest step
// first: if the schema version changed (or update-db started and its result
// is unknown), the pre-update backup is restored (wp-content and database),
// since a database upgraded for the new core can't safely run on the old
// one; then the site is moved back to its previous core version, which
// recreates both containers on the previous images. The site ends up
// running, with a status message saying the update failed and was
// reverted. If the backup can't be restored or the images can't be put
// back, the site is marked failed, and the status message says what to do.
//
// The failure is recorded (store.RolloutFailure), so the rollout doesn't
// try the same version on the site again; an update started by hand, or a
// newer release, does. When the rollout queued the job, its outcome counts
// towards halting the rollout (MaxFailures in a row), and the job queues the
// rollout's next pass so the next site follows at once.
//
// # Reuse
//
// The pieces that move a site to other images are separate from what
// decides the target, so platform image updates (#58) can reuse them: the
// backup steps, recordSchema, restoreChangedSchema, healthCheck (with
// Service.CheckHealth), the rollout outcome (recordRolloutOutcome) and the
// rollout pass itself (rollout.go, with a rolloutSource of their own).

// CoreUpdateJobType moves a site to another WordPress core release.
const CoreUpdateJobType = "site.core_update"

// Step names of the site.core_update job, besides prepare, the backup
// steps, and finish. Keep them stable: a queued job resumes by step name.
const (
	stepRecordSchema    = "record_schema"
	stepRecreate        = "recreate"
	stepUpgradeCoreDB   = "upgrade_database"
	stepHealthCheck     = "health_check"
	stepFinishCoreImage = "finish"
)

// Job data keys of the site.core_update job.
const (
	dataSchemaBefore   = "db_version_before"
	dataSchemaAfter    = "db_version_after"
	dataSchemaStarted  = "schema_update_started"
	dataImagesChanged  = "images_changed"
	dataHealthy        = "healthy"
	dataBackupRestored = "backup_restored"
	dataRestoreError   = "restore_error"
)

// Health check defaults.
const (
	DefaultHealthTimeout  = 90 * time.Second
	DefaultHealthInterval = 3 * time.Second
)

// Sentinel errors of core updates.
var (
	// ErrNoCoreUpdate means the site has no core release to move to: it is
	// up to date as far as its policy allows.
	ErrNoCoreUpdate = errors.New("the site has no WordPress core update to apply")
	// ErrUnhealthy means a site failed its health check.
	ErrUnhealthy = errors.New("the site failed its health check")
	// errWrongCore means the site runs another core than its images
	// should have; waiting doesn't change that.
	errWrongCore = errors.New("the site runs another WordPress core version than expected")
)

// CoreUpdateOptions configure core updates and their rollout. Zero values
// select the defaults.
type CoreUpdateOptions struct {
	// Policy is the install-wide part of the rollout policy.
	Policy coreupdate.Settings
	// Window is the maintenance window rollouts start site updates in;
	// the zero Window has none.
	Window UpdateWindow
	// MaxFailures is how many site updates in a row may fail before a
	// rollout halts; zero means 3.
	MaxFailures int
	// HealthTimeout bounds how long a health check waits for a site to
	// become healthy, and HealthInterval is the time between its tries.
	HealthTimeout  time.Duration
	HealthInterval time.Duration
}

// UpdateWindow is a daily maintenance window in UTC.
type UpdateWindow struct {
	// Start is the time since midnight UTC the window opens.
	Start time.Duration
	// Length is how long it stays open; zero means there is no window.
	Length time.Duration
}

// Contains reports whether t is inside the window; always, if there is
// none.
func (w UpdateWindow) Contains(t time.Time) bool {
	if w.Length <= 0 || w.Length >= 24*time.Hour {
		return true
	}
	t = t.UTC()
	sinceMidnight := t.Sub(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC))
	offset := (sinceMidnight - w.Start + 24*time.Hour) % (24 * time.Hour)
	return offset < w.Length
}

// coreUpdatePayload is the payload of a site.core_update job.
type coreUpdatePayload struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Rollout is the rollout that queued the job, whose failure count its
	// outcome changes; empty for an update started by hand.
	Rollout string `json:"rollout,omitempty"`
}

func (s *Service) coreUpdates() bool { return s.restores() }

func (s *Service) coreUpdateHandler() jobs.Handler {
	steps := []jobs.Step{{Name: stepPrepareUpdate, Do: s.prepareCoreUpdate, Undo: s.finishFailedCoreUpdate}}
	steps = append(steps, s.backups.Steps(store.BackupPreUpdate)...)
	steps = append(steps,
		jobs.Step{Name: stepRecordSchema, Do: s.recordSchema},
		jobs.Step{Name: stepRecreate, Do: s.recreateOnCore, Undo: s.revertCore},
		jobs.Step{Name: stepUpgradeCoreDB, Do: s.upgradeCoreDatabase, Undo: s.restoreChangedSchema},
		jobs.Step{Name: stepHealthCheck, Do: s.checkCoreHealth},
		jobs.Step{Name: stepFinishCoreImage, Do: s.finishCoreUpdate},
	)
	return jobs.Handler{Type: CoreUpdateJobType, Steps: steps}
}

// UpdateCore queues a [CoreUpdateJobType] job that moves the site to the
// target of its core update plan now, whatever the maintenance window and
// even if the rollout failed with that version before. It returns
// ErrNoCoreUpdate if the plan has no target (a major release that isn't
// approved is no target), ErrNotRunning for a site that isn't running, and
// ErrBusy or ErrNotProvisioned like other changes.
func (s *Service) UpdateCore(ctx context.Context, siteID, createdByUserID string) (store.Site, store.Job, error) {
	if !s.coreUpdates() {
		return store.Site{}, store.Job{}, ErrNoBackups
	}
	var (
		site store.Site
		job  store.Job
	)
	err := s.db.InTx(ctx, func(q store.Querier) error {
		current, err := s.manageable(ctx, store.NewSites(q), siteID)
		if err != nil {
			return err
		}
		plan, err := s.corePlan(ctx, q, current)
		if err != nil {
			return err
		}
		if plan.Target == "" {
			return fmt.Errorf("%w (it runs WordPress %s)", ErrNoCoreUpdate, current.WPCoreVersion)
		}
		job, err = s.enqueueCoreUpdate(ctx, q, current, plan.Target, "", createdByUserID)
		if err != nil {
			return err
		}
		site, err = store.NewSites(q).Get(ctx, siteID)
		return err
	})
	if err != nil {
		return store.Site{}, store.Job{}, err
	}
	s.log.InfoContext(ctx, "core update queued", "site_id", siteID, "to", site.WPCoreVersion, "job_id", job.ID)
	return site, job, nil
}

// enqueueCoreUpdate sets a running site's status to updating and queues
// its core update, in q's transaction.
func (s *Service) enqueueCoreUpdate(ctx context.Context, q store.Querier, site store.Site, to, rollout, createdByUserID string) (store.Job, error) {
	if site.Status != store.SiteRunning {
		return store.Job{}, fmt.Errorf("%w (it is %s)", ErrNotRunning, site.Status)
	}
	changed, err := store.NewSites(q).SetStatus(ctx, site.ID, store.SiteUpdating, "", store.SiteRunning)
	if err != nil {
		return store.Job{}, err
	}
	if !changed {
		return store.Job{}, fmt.Errorf("%w: the site's status changed", ErrBusy)
	}
	return jobs.Enqueue(ctx, q, jobs.Spec{
		Type:            CoreUpdateJobType,
		TargetType:      store.JobTargetSite,
		TargetID:        site.ID,
		Payload:         coreUpdatePayload{From: site.WPCoreVersion, To: to, Rollout: rollout},
		LockKey:         store.SiteLockKey(site.ID),
		CreatedByUserID: createdByUserID,
	})
}

func coreJobPayload(r *jobs.Run) (coreUpdatePayload, error) {
	var p coreUpdatePayload
	if err := r.Payload(&p); err != nil {
		return p, err
	}
	for _, v := range []string{p.From, p.To} {
		if _, err := catalog.Branch(v); err != nil {
			return p, jobs.Permanent(fmt.Errorf("the core update job's payload: %w", err))
		}
	}
	return p, nil
}

func (s *Service) prepareCoreUpdate(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := coreJobPayload(r)
	if err != nil {
		return err
	}
	if site.WPCoreVersion != p.From && site.WPCoreVersion != p.To {
		return jobs.Permanent(fmt.Errorf("the site runs WordPress %s, not %s, since the update was queued", site.WPCoreVersion, p.From))
	}
	kind := "a minor or security release"
	if branchOf(p.From) != branchOf(p.To) {
		kind = "a major release"
	}
	r.Logf("Updating WordPress from %s to %s (%s), after taking a pre-update backup.", p.From, p.To, kind)
	return nil
}

func branchOf(version string) string {
	b, _ := catalog.Branch(version)
	return b
}

// wpcliSite runs WP-CLI on a site a job is changing. WP-CLI exiting with
// an error is permanent, as in a restore (wpcliRestored).
func (s *Service) wpcliSite(ctx context.Context, siteID string, args ...string) (string, error) {
	out, err := s.wpcliRestored(ctx, siteID, args...)
	return strings.TrimSpace(out), err
}

// recordSchema records the site's database schema version before the
// update, so a revert knows whether update-db changed it.
func (s *Service) recordSchema(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	var before string
	if found, err := r.Get(dataSchemaBefore, &before); err != nil || found {
		return err
	}
	before, err = s.wpcliSite(ctx, site.ID, "option", "get", "db_version")
	if err != nil {
		return fmt.Errorf("reading the database schema version: %w", err)
	}
	if err := r.Set(dataSchemaBefore, before); err != nil {
		return err
	}
	r.Logf("The database schema version is %s.", before)
	return nil
}

// recreateOnCore records the new core version and recreates both
// containers on its images.
func (s *Service) recreateOnCore(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := coreJobPayload(r)
	if err != nil {
		return err
	}
	// First, so a revert happens even if this attempt dies half way.
	if err := r.Set(dataImagesChanged, true); err != nil {
		return err
	}
	if _, err := store.NewSites(s.db).SetWPCoreVersion(ctx, site.ID, p.To); err != nil {
		return err
	}
	site.WPCoreVersion = p.To
	if _, err := s.converge(ctx, r, site); err != nil {
		return err
	}
	r.Logf("Recreated the site's php and nginx containers on the WordPress %s images.", p.To)
	return nil
}

// revertCore moves the site back to its previous core, recreating both
// containers on the previous images, unless the update succeeded. If that
// keeps failing, it gives up on its last attempt and records why, so the
// final Undo marks the site failed.
func (s *Service) revertCore(ctx context.Context, r *jobs.Run) error {
	if healthy, err := flag(r, dataHealthy); err != nil || healthy {
		return err
	}
	if changed, err := flag(r, dataImagesChanged); err != nil || !changed {
		return err
	}
	var restoreErr string
	if _, err := r.Get(dataRestoreError, &restoreErr); err != nil {
		return err
	}
	if restoreErr != "" {
		r.Warnf("Keeping the newer WordPress core: the database was upgraded for it and couldn't be restored.")
		return nil
	}
	site, err := s.jobSite(ctx, r)
	if err != nil || site.Status != store.SiteUpdating {
		return nil
	}
	p, err := coreJobPayload(r)
	if err != nil {
		return err
	}
	if _, err := store.NewSites(s.db).SetWPCoreVersion(ctx, site.ID, p.From); err != nil {
		return err
	}
	site.WPCoreVersion = p.From
	if _, err := s.converge(ctx, r, site); err != nil {
		if !lastAttempt(r, err) {
			return err
		}
		r.Warnf("Couldn't move the site back to WordPress %s: %v", p.From, err)
		return r.Set(dataRevertError, err.Error())
	}
	// The object cache may hold options of the newer core.
	if err := s.caches.FlushCache(ctx, site.ID); err != nil {
		r.Warnf("Couldn't flush the object cache after reverting: %v", err)
	}
	r.Warnf("Moved the site back to WordPress %s: both containers run the previous images again.", p.From)
	return nil
}

// upgradeCoreDatabase applies the new core's database schema.
func (s *Service) upgradeCoreDatabase(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	if err := r.Set(dataSchemaStarted, true); err != nil {
		return err
	}
	if _, err := s.wpcliSite(ctx, site.ID, "core", "update-db"); err != nil {
		return fmt.Errorf("upgrading the database to WordPress %s: %w", site.WPCoreVersion, err)
	}
	after, err := s.wpcliSite(ctx, site.ID, "option", "get", "db_version")
	if err != nil {
		return fmt.Errorf("reading the database schema version: %w", err)
	}
	if err := r.Set(dataSchemaAfter, after); err != nil {
		return err
	}
	var before string
	if _, err := r.Get(dataSchemaBefore, &before); err != nil {
		return err
	}
	if after == before {
		r.Logf("The database schema didn't change (%s).", after)
	} else {
		r.Logf("Upgraded the database schema from %s to %s.", before, after)
	}
	return nil
}

// restoreChangedSchema restores the pre-update backup if the database
// schema changed, or may have: a database upgraded for a newer core can't
// safely run on the previous one.
func (s *Service) restoreChangedSchema(ctx context.Context, r *jobs.Run) error {
	for _, key := range []string{dataHealthy, dataBackupRestored} {
		if done, err := flag(r, key); err != nil || done {
			return err
		}
	}
	if started, err := flag(r, dataSchemaStarted); err != nil || !started {
		return err
	}
	var before, after string
	if _, err := r.Get(dataSchemaBefore, &before); err != nil {
		return err
	}
	found, err := r.Get(dataSchemaAfter, &after)
	if err != nil {
		return err
	}
	if found && after == before {
		r.Logf("The database schema didn't change, so the pre-update backup isn't needed.")
		return nil
	}
	return s.restoreJobBackup(ctx, r, "pre-update")
}

// restoreJobBackup restores the backup the job's backup steps took into
// the job's site, then flushes its caches. If restoring keeps failing, it
// gives up on its last attempt without failing the rollback and records
// why (dataRestoreError); on success it sets dataBackupRestored.
func (s *Service) restoreJobBackup(ctx context.Context, r *jobs.Run, what string) error {
	siteID := r.Job().TargetID
	rec, found, err := s.backups.JobBackup(ctx, r)
	if err != nil {
		return err
	}
	giveUp := func(err error) error {
		r.Warnf("Couldn't restore the %s backup: %v", what, err)
		return r.Set(dataRestoreError, err.Error())
	}
	if !found || rec.Status != store.BackupSucceeded || rec.SnapshotID == "" {
		return giveUp(fmt.Errorf("the job has no %s snapshot", what))
	}
	if err := s.engine.Restore(ctx, siteID, backup.SnapshotID(rec.SnapshotID), siteID); err != nil {
		err = permanentRestoreError(err)
		if !lastAttempt(r, err) {
			return err
		}
		return giveUp(fmt.Errorf("backup %s (snapshot %s): %w", rec.ID, shortSnapshot(rec.SnapshotID), err))
	}
	if err := s.caches.FlushCache(ctx, siteID); err != nil {
		r.Warnf("Couldn't flush the object cache after restoring: %v", err)
	}
	if err := s.prov.PurgeCache(ctx, siteID); err != nil && !errors.Is(err, provisioner.ErrSiteNotFound) {
		r.Warnf("Couldn't purge the page cache after restoring: %v", err)
	}
	if err := r.Set(dataBackupRestored, true); err != nil {
		return err
	}
	r.Warnf("Restored the %s backup %s (snapshot %s): the database schema had changed.", what, rec.ID, shortSnapshot(rec.SnapshotID))
	return nil
}

func (s *Service) checkCoreHealth(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	p, err := coreJobPayload(r)
	if err != nil {
		return err
	}
	if err := s.CheckHealth(ctx, site, p.To); err != nil {
		return jobs.Permanent(err)
	}
	if err := r.Set(dataHealthy, true); err != nil {
		return err
	}
	r.Logf("The site is healthy on WordPress %s: it answers HTTP requests, and WP-CLI loads it.", p.To)
	return nil
}

// CheckHealth checks that a running site works (§5.5): an HTTP request to
// its web server gets an answer below 500, and WP-CLI reports core version
// wantCore (unless it is empty) and loads WordPress with its plugins. It
// tries again until the health timeout while the site's containers settle,
// except after a wrong core version, which waiting doesn't fix. Its error
// wraps ErrUnhealthy.
func (s *Service) CheckHealth(ctx context.Context, site store.Site, wantCore string) error {
	deadline := time.Now().Add(s.core.HealthTimeout)
	for {
		err := s.healthOnce(ctx, site, wantCore)
		if err == nil {
			return nil
		}
		if errors.Is(err, errWrongCore) || !time.Now().Before(deadline) || ctx.Err() != nil {
			return fmt.Errorf("%w: %w", ErrUnhealthy, err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ErrUnhealthy, ctx.Err())
		case <-time.After(s.core.HealthInterval):
		}
	}
}

func (s *Service) healthOnce(ctx context.Context, site store.Site, wantCore string) error {
	if s.prober != nil {
		// A query string bypasses the page cache, which would otherwise
		// answer for PHP, even with a stale page when PHP fails.
		req := provisioner.ProbeRequest{
			Host: strings.TrimPrefix(URL(site), "https://"),
			Path: "/?pressfleet-health-check=" + strings.ToLower(rand.Text()[:8]),
		}
		res, err := s.prober.ProbeHTTP(ctx, site.ID, req)
		if err != nil {
			return fmt.Errorf("HTTP request: %w", err)
		}
		if res.StatusCode >= 500 {
			return fmt.Errorf("HTTP request: the site answered %d", res.StatusCode)
		}
	}
	if wantCore != "" {
		got, err := s.wpcli(ctx, site.ID, "core", "version")
		if err != nil {
			return err
		}
		if got != wantCore {
			return fmt.Errorf("%w: WP-CLI reports WordPress %s, want %s", errWrongCore, got, wantCore)
		}
	}
	if _, err := s.wpcli(ctx, site.ID, "core", "is-installed"); err != nil {
		return err
	}
	return nil
}

// wpcli runs WP-CLI for a health check, keeping only WP-CLI's error
// message in the error.
func (s *Service) wpcli(ctx context.Context, siteID string, args ...string) (string, error) {
	out, err := s.prov.ExecWPCLI(ctx, siteID, args)
	var ee *provisioner.ExecError
	if errors.As(err, &ee) {
		return "", fmt.Errorf("wp %s: exit status %d: %s", strings.Join(args, " "), ee.ExitCode, trimWPCLIError(ee.Stderr))
	}
	if err != nil {
		return "", fmt.Errorf("wp %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out), nil
}

// finishCoreUpdate sets the status from the live phase, records the
// success, and puts the update in the job's result.
func (s *Service) finishCoreUpdate(ctx context.Context, r *jobs.Run) error {
	if err := s.finishUpdate(ctx, r); err != nil {
		return err
	}
	p, err := coreJobPayload(r)
	if err != nil {
		return err
	}
	result := map[string]string{"from": p.From, "to": p.To}
	if rec, found, err := s.backups.JobBackup(ctx, r); err != nil {
		return err
	} else if found {
		result["pre_update_backup_id"] = rec.ID
	}
	for k, v := range result {
		if err := r.SetResult(k, v); err != nil {
			return err
		}
	}
	return s.recordRolloutOutcome(ctx, r, coreRollout, p.Rollout, p.To, "")
}

// finishFailedCoreUpdate is the last Undo of a failed site.core_update job:
// it sets the site's status, says in its status message what the failed
// update left behind, and records the failure.
func (s *Service) finishFailedCoreUpdate(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	p, err := coreJobPayload(r)
	if err != nil {
		return err
	}
	var restoreErr, revertErr string
	if _, err := r.Get(dataRestoreError, &restoreErr); err != nil {
		return err
	}
	if _, err := r.Get(dataRevertError, &revertErr); err != nil {
		return err
	}
	flags := map[string]bool{}
	for _, key := range []string{dataHealthy, dataImagesChanged, dataBackupRestored} {
		if flags[key], err = flag(r, key); err != nil {
			return err
		}
	}
	backupNote := ""
	if rec, found, err := s.backups.JobBackup(ctx, r); err != nil {
		return err
	} else if found && rec.Status == store.BackupSucceeded {
		backupNote = " (pre-update backup " + rec.ID + ")"
	}
	status := s.observedStatus(ctx, siteID)
	var prefix string
	switch {
	case flags[dataHealthy]:
		prefix = "the WordPress " + p.To + " update finished, but the job failed afterwards"
	case restoreErr != "":
		status = store.SiteFailed
		prefix = "the WordPress " + p.To + " update failed, and restoring the pre-update backup failed too" + backupNote +
			"; restore it to try again"
	case revertErr != "":
		status = store.SiteFailed
		prefix = "the WordPress " + p.To + " update failed, and the site couldn't be moved back to WordPress " + p.From
	case flags[dataImagesChanged] && flags[dataBackupRestored]:
		prefix = "the WordPress " + p.To + " update failed and was reverted to " + p.From + ", restoring the pre-update backup" + backupNote
	case flags[dataImagesChanged]:
		prefix = "the WordPress " + p.To + " update failed and was reverted to " + p.From
	default:
		prefix = "the WordPress " + p.To + " update failed before changing the site"
	}
	msg := statusMessage(prefix, r.Job().Error)
	changed, err := store.NewSites(s.db).SetStatus(ctx, siteID, status, msg, store.SiteUpdating)
	if err != nil {
		return err
	}
	if changed {
		r.Warnf("The site is %s: %s.", status, prefix)
	}
	if flags[dataHealthy] {
		return s.recordRolloutOutcome(ctx, r, coreRollout, p.Rollout, p.To, "")
	}
	return s.recordRolloutOutcome(ctx, r, coreRollout, p.Rollout, p.To, msg)
}

// corePlan returns the site's core update plan with its and its group's
// policies, at the service's time.
func (s *Service) corePlan(ctx context.Context, q store.Querier, site store.Site) (coreupdate.Plan, error) {
	policies := store.NewCorePolicies(q)
	sp, err := policies.Site(ctx, site.ID)
	if err != nil {
		return coreupdate.Plan{}, err
	}
	gp, err := policies.ClientGroup(ctx, site.ClientGroupID)
	if err != nil {
		return coreupdate.Plan{}, err
	}
	return coreupdate.Decide(s.catalog, site.WPCoreVersion, corePolicy(sp), corePolicy(gp), s.core.Policy, s.now())
}

func corePolicy(p store.CorePolicy) coreupdate.Policy {
	return coreupdate.Policy{Hold: p.Hold, HoldSince: p.HoldSince, ApprovedBranch: p.ApprovedBranch}
}
