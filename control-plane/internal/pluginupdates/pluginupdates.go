// Package pluginupdates reports each site's pending plugin and theme
// updates (#125, design doc §5.6, §9 Q2).
//
// WordPress updates plugins and themes itself (auto-updates or wp-admin);
// the platform only reports what is pending, and never runs an update or
// takes a backup around one. Each site has a recurring schedule, created
// with [EnsureSchedule] when the site is created and removed with the site,
// that enqueues a [JobType] job every [Interval]. The job runs
//
//	wp plugin list --update=available --format=json ...
//	wp theme list --update=available --format=json ...
//
// in the site's php container and stores the result as the site's
// store.UpdateReport, replacing the previous one. WP-CLI refreshes
// WordPress's update data from wordpress.org when it is stale, as wp-admin
// does; plugins with their own update servers report through the same
// data. [Service.Check] queues a check outside the schedule, for after
// someone has updated the site.
//
// # Site health
//
// Pending updates count towards a site's health as a warning, never as a
// failure. The metrics collector (metrics.UpdatesCollector) exports the
// counts as pressfleet_site_pending_updates, and the default alert rule
// updates_pending raises a warning for a site whose updates have been
// pending for a while, which gives WordPress's own auto-updates time to
// apply them first. The dashboard shows the counts as soon as they are
// reported.
//
// # Skipping and failures
//
// Like the WP-Cron job (package sitecron), a check succeeds without running
// WP-CLI, and says why in its log, when the site no longer exists, isn't
// serving, or its containers are missing or stopped. A failed check (WP-CLI
// exits non-zero, times out, or prints something that isn't the expected
// JSON) fails the job and keeps the previous report; it isn't retried, the
// next slot checks again. Checks use the site's lock key, so they never run
// while the site is being changed, and are bounded by [RunTimeout].
package pluginupdates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// ErrSiteNotFound means the site doesn't exist.
var ErrSiteNotFound = errors.New("site not found")

const (
	// JobType is the type of the job that checks a site for updates.
	JobType = "site.update_check"
	// Interval is how often each site is checked. WordPress itself checks
	// wordpress.org twice a day, so checking more often finds nothing new.
	// Each site's checks are staggered across the interval.
	Interval = 12 * time.Hour
	// RunTimeout bounds one check, both WP-CLI runs together. WP-CLI may
	// ask wordpress.org for update data, which can be slow.
	RunTimeout = 3 * time.Minute
	// MaxAttempts is 1: a failed check isn't retried, the next slot
	// checks again.
	MaxAttempts = 1
	// maxLoggedOutput is how much of WP-CLI's output goes into the job log
	// when it fails.
	maxLoggedOutput = 2 << 10
	// stepName is the handler's only step. Keep it stable: queued jobs
	// resume by step name.
	stepName = "check_updates"
)

// listFields are the fields asked of `wp plugin list` and `wp theme list`.
const listFields = "name,title,version,update_version"

// Args returns the WP-CLI arguments that list the plugins (kind
// store.UpdateKindPlugin) or themes with an update available. They only
// read: nothing here ever runs `wp plugin update`.
func Args(kind string) []string {
	return []string{kind, "list", "--update=available", "--fields=" + listFields, "--format=json"}
}

// ScheduleName returns the name of a site's update check schedule.
func ScheduleName(siteID string) string { return "update-check:" + siteID }

// EnsureSchedule creates or updates the site's check schedule. It is
// idempotent. Call it with the transaction that creates the site.
func EnsureSchedule(ctx context.Context, q store.Querier, siteID string) error {
	if siteID == "" {
		return errors.New("ensure update check schedule: site ID is required")
	}
	_, err := jobs.UpsertSchedule(ctx, q, jobs.ScheduleSpec{
		Name:            ScheduleName(siteID),
		JobType:         JobType,
		TargetType:      store.JobTargetSite,
		TargetID:        siteID,
		LockKey:         store.SiteLockKey(siteID),
		MaxAttempts:     MaxAttempts,
		Interval:        Interval,
		Stagger:         Interval,
		MissedRunPolicy: store.MissedCoalesce,
	})
	if err != nil {
		return fmt.Errorf("ensure update check schedule for site %s: %w", siteID, err)
	}
	return nil
}

// RemoveSchedule deletes the site's check schedule. It is idempotent.
func RemoveSchedule(ctx context.Context, q store.Querier, siteID string) error {
	err := jobs.DeleteSchedule(ctx, q, ScheduleName(siteID))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("remove update check schedule for site %s: %w", siteID, err)
	}
	return nil
}

// ReconcileSchedules gives every site a check schedule, so that sites
// created before update checks existed get one, and removes the schedules
// of sites that no longer exist. The control plane runs it at startup.
func ReconcileSchedules(ctx context.Context, db *store.DB) error {
	ids, err := store.NewSites(db).ListIDs(ctx)
	if err != nil {
		return fmt.Errorf("reconcile update check schedules: %w", err)
	}
	exists := make(map[string]bool, len(ids))
	for _, siteID := range ids {
		exists[siteID] = true
		if err := EnsureSchedule(ctx, db, siteID); err != nil {
			return fmt.Errorf("reconcile update check schedules: %w", err)
		}
	}
	all, err := store.NewSchedules(db).List(ctx)
	if err != nil {
		return fmt.Errorf("reconcile update check schedules: %w", err)
	}
	for _, sch := range all {
		if sch.JobType == JobType && sch.TargetType == store.JobTargetSite && !exists[sch.TargetID] {
			if err := RemoveSchedule(ctx, db, sch.TargetID); err != nil {
				return fmt.Errorf("reconcile update check schedules: %w", err)
			}
		}
	}
	return nil
}

// Handler returns the handler for JobType jobs. db is where it reads site
// statuses and writes reports; prov runs WP-CLI.
func Handler(db *store.DB, prov provisioner.Provisioner) jobs.Handler {
	r := runner{db: db, prov: prov}
	return jobs.Handler{
		Type:  JobType,
		Steps: []jobs.Step{{Name: stepName, Do: r.run}},
	}
}

// runnableStatuses are the site statuses in which a check runs; see
// package sitecron, which runs WP-CLI on the same terms.
var runnableStatuses = map[store.SiteStatus]bool{store.SiteRunning: true, store.SiteFailed: true}

type runner struct {
	db   *store.DB
	prov provisioner.Provisioner
}

func (rn runner) run(ctx context.Context, r *jobs.Run) error {
	job := r.Job()
	if job.TargetType != store.JobTargetSite || job.TargetID == "" {
		return jobs.Permanent(fmt.Errorf("%s job targets %s %q, want a site", JobType, job.TargetType, job.TargetID))
	}
	siteID := job.TargetID
	site, err := store.NewSites(rn.db).Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		r.Logf("skipped: site %s no longer exists", siteID)
		return RemoveSchedule(ctx, rn.db, siteID)
	}
	if err != nil {
		return err
	}
	if !runnableStatuses[site.Status] {
		r.Logf("skipped: site is %s", site.Status)
		return nil
	}

	runCtx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()
	var updates []store.PendingUpdate
	for _, kind := range []string{store.UpdateKindPlugin, store.UpdateKindTheme} {
		found, skip, err := rn.list(runCtx, ctx, r, siteID, kind)
		if err != nil || skip {
			return err
		}
		updates = append(updates, found...)
	}
	if err := store.NewUpdateReports(rn.db).Put(ctx, store.UpdateReport{SiteID: siteID, Updates: updates}); err != nil {
		if errors.Is(err, store.ErrReference) {
			r.Logf("skipped: site %s was deleted during the check", siteID)
			return nil
		}
		return err
	}
	r.Logf("%s", summary(updates))
	return nil
}

// list runs WP-CLI for one kind. skip is true, with a logged reason, when
// the site's containers can't run it.
func (rn runner) list(runCtx, ctx context.Context, r *jobs.Run, siteID, kind string) (_ []store.PendingUpdate, skip bool, _ error) {
	args := Args(kind)
	out, err := rn.prov.ExecWPCLI(runCtx, siteID, args)
	var execErr *provisioner.ExecError
	switch {
	case err == nil:
	case errors.Is(err, provisioner.ErrSiteNotFound):
		r.Logf("skipped: the site's containers don't exist")
		return nil, true, nil
	case errors.Is(err, provisioner.ErrSiteNotRunning):
		r.Logf("skipped: the site's containers aren't running")
		return nil, true, nil
	case errors.As(err, &execErr):
		r.Warnf("wp %s exited with status %d: %s", strings.Join(args, " "), execErr.ExitCode, tail(execErr.Stderr, maxLoggedOutput))
		return nil, false, fmt.Errorf("check %s updates on site %s: WP-CLI exited with status %d", kind, siteID, execErr.ExitCode)
	case runCtx.Err() != nil && ctx.Err() == nil:
		r.Warnf("checking for updates timed out after %s", RunTimeout)
		return nil, false, fmt.Errorf("check %s updates on site %s: timed out after %s: %w", kind, siteID, RunTimeout, err)
	default:
		return nil, false, fmt.Errorf("check %s updates on site %s: %w", kind, siteID, err)
	}
	updates, err := Parse(kind, out)
	if err != nil {
		r.Warnf("wp %s printed unexpected output: %s", strings.Join(args, " "), tail(out, maxLoggedOutput))
		return nil, false, fmt.Errorf("check %s updates on site %s: %w", kind, siteID, err)
	}
	return updates, false, nil
}

// listItem is one entry of `wp plugin list --format=json`. Versions are
// usually strings, but a plugin header like "Version: 2" can make WP-CLI
// print a number, so they are decoded loosely.
type listItem struct {
	Name          string          `json:"name"`
	Title         string          `json:"title"`
	Version       json.RawMessage `json:"version"`
	UpdateVersion json.RawMessage `json:"update_version"`
}

// Parse reads the output of the WP-CLI command [Args] returns for kind.
// It ignores lines before the JSON, such as PHP notices a plugin printed.
// The updates are sorted by name.
func Parse(kind, out string) ([]store.PendingUpdate, error) {
	var items []listItem
	found := false
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[") {
			continue
		}
		if err := json.Unmarshal([]byte(line), &items); err == nil {
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("WP-CLI output has no %s list", kind)
	}
	updates := make([]store.PendingUpdate, 0, len(items))
	for _, it := range items {
		if it.Name == "" {
			continue
		}
		updates = append(updates, store.PendingUpdate{
			Kind:          kind,
			Name:          it.Name,
			Title:         it.Title,
			Version:       looseString(it.Version),
			UpdateVersion: looseString(it.UpdateVersion),
		})
	}
	slices.SortFunc(updates, func(a, b store.PendingUpdate) int { return strings.Compare(a.Name, b.Name) })
	return updates, nil
}

// looseString returns a JSON string's value, or any other JSON value as
// written (a number), or "" for null and nothing.
func looseString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	if v := strings.TrimSpace(string(raw)); v != "null" {
		return v
	}
	return ""
}

// summary says what a check found, for the job log.
func summary(updates []store.PendingUpdate) string {
	var plugins, themes int
	for _, u := range updates {
		if u.Kind == store.UpdateKindPlugin {
			plugins++
		} else {
			themes++
		}
	}
	if plugins+themes == 0 {
		return "no plugin or theme updates pending"
	}
	return fmt.Sprintf("updates pending: %s, %s", plural(plugins, "plugin"), plural(themes, "theme"))
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// tail returns at most the last n bytes of s, trimmed, marking a cut.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + strings.ToValidUTF8(s[len(s)-n:], "")
}

// Service answers the API's questions about pending updates.
type Service struct {
	db *store.DB
}

// NewService returns a Service on db.
func NewService(db *store.DB) *Service { return &Service{db: db} }

// Report returns the site's latest report. A site that hasn't been checked
// yet has a report with a zero CheckedAt and no updates. A missing site is
// ErrSiteNotFound.
func (s *Service) Report(ctx context.Context, siteID string) (store.UpdateReport, error) {
	if _, err := store.NewSites(s.db).Get(ctx, siteID); errors.Is(err, store.ErrNotFound) {
		return store.UpdateReport{}, ErrSiteNotFound
	} else if err != nil {
		return store.UpdateReport{}, err
	}
	rep, err := store.NewUpdateReports(s.db).Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return store.UpdateReport{SiteID: siteID, Updates: []store.PendingUpdate{}}, nil
	}
	return rep, err
}

// Counts returns every checked site's counts, keyed by site ID.
func (s *Service) Counts(ctx context.Context) (map[string]store.UpdateCounts, error) {
	return store.NewUpdateReports(s.db).Counts(ctx)
}

// Check queues a check of the site now and returns its job; if one is
// already queued, it returns that one instead of queuing another. A
// missing site is ErrSiteNotFound.
func (s *Service) Check(ctx context.Context, siteID, userID string) (store.Job, error) {
	var job store.Job
	err := s.db.InTx(ctx, func(q store.Querier) error {
		if _, err := store.NewSites(q).Get(ctx, siteID); errors.Is(err, store.ErrNotFound) {
			return ErrSiteNotFound
		} else if err != nil {
			return err
		}
		page, err := store.NewJobs(q).List(ctx, store.JobFilter{
			States: []store.JobState{store.JobQueued}, Types: []string{JobType},
			TargetType: store.JobTargetSite, TargetID: siteID, Limit: 1,
		})
		if err != nil {
			return err
		}
		if len(page.Jobs) > 0 {
			job = page.Jobs[0]
			return nil
		}
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type: JobType, TargetType: store.JobTargetSite, TargetID: siteID,
			LockKey: store.SiteLockKey(siteID), MaxAttempts: MaxAttempts, CreatedByUserID: userID,
		})
		return err
	})
	return job, err
}
