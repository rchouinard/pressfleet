package sites

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/coreupdate"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Rollouts (#57, #95, design doc §5.5): moving every site to new images or
// templates, one site at a time.
//
// A rollout is a series of passes, each a [RolloutJobType] job with the
// lock key rollout:<name>, so passes of one rollout never overlap. A pass:
//
//  1. syncs the rollout's state (store.Rollouts.Sync) with a fingerprint of
//     what it rolls out; new releases lift a halt and reset the failure
//     count;
//  2. stops if the rollout is halted, if a maintenance window is set and
//     now is outside it, or if a site update of the rollout's job type is
//     still queued or running (one site at a time, including updates
//     started by hand);
//  3. otherwise queues the site update of the first candidate site whose
//     last failed update wasn't to the same target, and ends.
//
// The site update's last step (success) or last Undo (failure) records its
// outcome (recordRolloutOutcome): a success resets the failure count; a
// failure counts, and MaxFailures failures in a row halt the rollout until
// an operator resumes it ([Service.ResumeRollout]) or the releases change.
// Either way it queues the next pass at once, so the rollout moves from
// site to site without waiting for the schedule. The schedule
// ([EnsureRolloutSchedule], every RolloutInterval) picks it up again after
// a window closes and opens, and the control plane queues a pass when it
// starts ([Service.EnqueueRollout]), which is how new images in the
// version manifest, and new templates after an upgrade, are detected.
//
// There are three rollouts. The core rollout ("core") moves sites to newer
// WordPress releases: its candidates are the running sites whose core
// update plan has a target (coreupdate.Decide). The image rollout
// ("images", imageupdate.go) moves sites to new builds of the images they
// already run: PHP patch releases, security fixes, and base OS updates.
// The templates rollout ("templates", templateupdate.go) re-renders the
// running sites that an earlier release rendered, after a platform upgrade
// that changed how the driver renders sites; its fingerprint is the
// driver's template fingerprint, so a release with other templates lifts a
// halt. Each runs one site at a time on its own; a site is only ever in one
// update, since all of them need it running and set it updating.

// RolloutJobType runs one pass of a rollout. Its target is the platform,
// with the ID rollout_<name>.
const RolloutJobType = "rollout.pass"

// RolloutInterval is the time between scheduled passes of a rollout.
const RolloutInterval = 15 * time.Minute

// coreRollout names the WordPress core rollout.
const coreRollout = "core"

const stepRolloutPass = "pass"

// rolloutTargetType is the target type of rollout passes, whose target ID
// is rolloutTargetID.
const rolloutTargetType = "platform"

// rolloutTargetID is the target ID of a rollout's passes.
func rolloutTargetID(name string) string { return "rollout_" + name }

// RolloutLockKey is the lock key of a rollout's passes.
func RolloutLockKey(name string) string { return "rollout:" + name }

// RolloutScheduleName names a rollout's schedule.
func RolloutScheduleName(name string) string { return "rollout:" + name }

// ErrUnknownRollout means a rollout name isn't one the service runs.
var ErrUnknownRollout = errors.New("unknown rollout")

// rolloutCandidate is a site update a rollout pass may queue.
type rolloutCandidate struct {
	site   store.Site
	target string
	// images are the image rollout's changes to the site's pins.
	images []imageChange
}

// rolloutSource is what one rollout rolls out.
type rolloutSource struct {
	name string
	// jobType is the job type of its site updates.
	jobType string
	// fingerprint identifies the releases it rolls out.
	fingerprint func(ctx context.Context) (string, error)
	// candidates lists the site updates to make, in order.
	candidates func(ctx context.Context) ([]rolloutCandidate, error)
	// enqueue queues a candidate's site update in q's transaction, with
	// the site's status updating.
	enqueue func(ctx context.Context, q store.Querier, c rolloutCandidate) (store.Job, error)
}

// rolloutSources returns the rollouts the service runs: the core rollout
// if it can take backups, the image rollout if the driver pins images, and
// the templates rollout if the driver fingerprints and snapshots its
// renders.
func (s *Service) rolloutSources() map[string]rolloutSource {
	out := map[string]rolloutSource{}
	if s.coreUpdates() {
		out[coreRollout] = rolloutSource{
			name:    coreRollout,
			jobType: CoreUpdateJobType,
			fingerprint: func(context.Context) (string, error) {
				return s.coreFingerprint(), nil
			},
			candidates: s.coreCandidates,
			enqueue: func(ctx context.Context, q store.Querier, c rolloutCandidate) (store.Job, error) {
				return s.enqueueCoreUpdate(ctx, q, c.site, c.target, coreRollout, "")
			},
		}
	}
	if s.pinner != nil {
		out[imageRollout] = rolloutSource{
			name:        imageRollout,
			jobType:     ImageUpdateJobType,
			fingerprint: s.imageFingerprint,
			candidates:  s.imageCandidates,
			enqueue: func(ctx context.Context, q store.Querier, c rolloutCandidate) (store.Job, error) {
				return s.enqueueImageUpdate(ctx, q, c.site, c.images, imageRollout)
			},
		}
	}
	if s.templates != nil {
		out[templateRollout] = rolloutSource{
			name:    templateRollout,
			jobType: TemplateUpdateJobType,
			fingerprint: func(context.Context) (string, error) {
				return s.templates.TemplateFingerprint(), nil
			},
			candidates: s.templateCandidates,
			enqueue: func(ctx context.Context, q store.Querier, c rolloutCandidate) (store.Job, error) {
				return s.enqueueTemplateUpdate(ctx, q, c.site, c.target, templateRollout)
			},
		}
	}
	return out
}

func (s *Service) rolloutHandler() jobs.Handler {
	return jobs.Handler{Type: RolloutJobType, Steps: []jobs.Step{{Name: stepRolloutPass, Do: s.rolloutPass}}}
}

// EnqueueRollout queues a pass of every rollout, unless one is already
// waiting. The control plane calls it at startup, so new images in the
// version manifest start rolling out.
func (s *Service) EnqueueRollout(ctx context.Context) error {
	for name := range s.rolloutSources() {
		if err := enqueueRolloutPass(ctx, s.db, name); err != nil {
			return err
		}
	}
	return nil
}

// enqueueRolloutPass queues a pass of the rollout unless one is queued
// already.
func enqueueRolloutPass(ctx context.Context, q store.Querier, name string) error {
	page, err := store.NewJobs(q).List(ctx, store.JobFilter{
		States: []store.JobState{store.JobQueued}, Types: []string{RolloutJobType},
		TargetType: rolloutTargetType, TargetID: rolloutTargetID(name), Limit: 1,
	})
	if err != nil {
		return fmt.Errorf("queue rollout %s: %w", name, err)
	}
	if len(page.Jobs) > 0 {
		return nil
	}
	if _, err := jobs.Enqueue(ctx, q, jobs.Spec{
		Type: RolloutJobType, TargetType: rolloutTargetType, TargetID: rolloutTargetID(name), LockKey: RolloutLockKey(name),
	}); err != nil {
		return fmt.Errorf("queue rollout %s: %w", name, err)
	}
	return nil
}

// EnsureRolloutSchedule creates or updates every rollout's schedule. It is
// idempotent.
func (s *Service) EnsureRolloutSchedule(ctx context.Context) error {
	for name := range s.rolloutSources() {
		if _, err := jobs.UpsertSchedule(ctx, s.db, jobs.ScheduleSpec{
			Name: RolloutScheduleName(name), JobType: RolloutJobType, TargetType: rolloutTargetType, TargetID: rolloutTargetID(name),
			LockKey: RolloutLockKey(name), Interval: RolloutInterval, MissedRunPolicy: store.MissedCoalesce,
		}); err != nil {
			return fmt.Errorf("schedule rollout %s: %w", name, err)
		}
	}
	return nil
}

// rolloutPass runs one pass of the job's rollout.
func (s *Service) rolloutPass(ctx context.Context, r *jobs.Run) error {
	src, ok := s.rolloutSources()[strings.TrimPrefix(r.Job().TargetID, rolloutTargetID(""))]
	if !ok {
		return jobs.Permanent(fmt.Errorf("%w %q", ErrUnknownRollout, r.Job().TargetID))
	}
	fingerprint, err := src.fingerprint(ctx)
	if err != nil {
		return err
	}
	ro, err := store.NewRollouts(s.db).Sync(ctx, src.name, fingerprint)
	if err != nil {
		return err
	}
	if ro.Halted() {
		r.Logf("The rollout is halted since %s (%s); resume it once the failures are understood.",
			ro.HaltedAt.UTC().Format(time.RFC3339), ro.HaltReason)
		return r.SetResult("halted", true)
	}
	if now := s.now(); !s.core.Window.Contains(now) {
		r.Logf("Outside the maintenance window; site updates wait for it.")
		return nil
	}
	active, err := store.NewJobs(s.db).List(ctx, store.JobFilter{
		States: []store.JobState{store.JobQueued, store.JobRunning, store.JobRollingBack}, Types: []string{src.jobType}, Limit: 1,
	})
	if err != nil {
		return err
	}
	if len(active.Jobs) > 0 {
		r.Logf("Site %s is being updated (job %s); the next site follows when it is done.", active.Jobs[0].TargetID, active.Jobs[0].ID)
		return nil
	}
	candidates, err := src.candidates(ctx)
	if err != nil {
		return err
	}
	failures, err := store.NewRollouts(s.db).Failures(ctx, src.name)
	if err != nil {
		return err
	}
	for _, c := range candidates {
		if f, failed := failures[c.site.ID]; failed && f.Target == c.target {
			continue
		}
		var job store.Job
		err := s.db.InTx(ctx, func(q store.Querier) error {
			// The site may have changed since it was listed.
			current, err := store.NewSites(q).Get(ctx, c.site.ID)
			if err != nil {
				return err
			}
			if current.Status != store.SiteRunning || current.WPCoreVersion != c.site.WPCoreVersion ||
				current.PHPVersion != c.site.PHPVersion {
				return errSkipCandidate
			}
			c.site = current
			job, err = src.enqueue(ctx, q, c)
			return err
		})
		if errors.Is(err, errSkipCandidate) || errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		r.Logf("Updating site %s (%s) to %s (job %s).", c.site.ID, c.site.PreviewDomain, c.target, job.ID)
		s.log.InfoContext(ctx, "rollout site update queued", "rollout", src.name, "site_id", c.site.ID, "target", c.target, "job_id", job.ID)
		for k, v := range map[string]string{"site_id": c.site.ID, "target": c.target, "job_id": job.ID} {
			if err := r.SetResult(k, v); err != nil {
				return err
			}
		}
		return nil
	}
	r.Logf("Every site is up to date as far as its policy allows.")
	return nil
}

var errSkipCandidate = errors.New("the site changed since it was listed")

// recordRolloutOutcome records how a site update ended: failureMsg is
// empty for a success. The failure record keeps the rollout from trying
// the same target on the site again, also after an update started by hand;
// only an update the rollout queued (rollout not empty) counts towards
// halting it, and queues its next pass.
func (s *Service) recordRolloutOutcome(ctx context.Context, r *jobs.Run, name, rollout, target, failureMsg string) error {
	siteID := r.Job().TargetID
	rollouts := store.NewRollouts(s.db)
	if failureMsg != "" {
		// An update cut short by the site's deletion says nothing about
		// the release, so it doesn't count toward halting the rollout.
		site, err := store.NewSites(s.db).Get(ctx, siteID)
		if errors.Is(err, store.ErrNotFound) || err == nil && site.Status == store.SiteDeleting {
			r.Logf("The site is being deleted, so the failed update doesn't count against the %s rollout.", name)
			if rollout == "" {
				return nil
			}
			return enqueueRolloutPass(ctx, s.db, rollout)
		}
		if err != nil {
			return err
		}
	}
	if failureMsg == "" {
		if rollout != "" {
			if err := rollouts.RecordSuccess(ctx, name, siteID); err != nil {
				return err
			}
		} else if err := rollouts.ClearFailure(ctx, name, siteID); err != nil {
			return err
		}
	} else {
		ro, err := rollouts.RecordFailure(ctx, name, store.RolloutFailure{SiteID: siteID, Target: target, Message: failureMsg},
			rollout != "", s.core.MaxFailures)
		if errors.Is(err, store.ErrReference) {
			return nil // the site is gone
		}
		if err != nil {
			return err
		}
		if ro.Halted() {
			r.Warnf("The %s rollout is halted: %s.", name, ro.HaltReason)
			s.log.WarnContext(ctx, "rollout halted", "rollout", name, "reason", ro.HaltReason)
		}
	}
	if rollout == "" {
		return nil
	}
	return enqueueRolloutPass(ctx, s.db, rollout)
}

// coreFingerprint identifies the WordPress releases in the catalog.
func (s *Service) coreFingerprint() string {
	var vs []string
	for _, w := range s.catalog.WordPressVersions() {
		vs = append(vs, w.Version)
	}
	return strings.Join(vs, " ")
}

// coreCandidates lists the running sites whose plan has a target, oldest
// site first.
func (s *Service) coreCandidates(ctx context.Context) ([]rolloutCandidate, error) {
	running, err := s.runningSites(ctx)
	if err != nil {
		return nil, err
	}
	policies := store.NewCorePolicies(s.db)
	sitePolicies, err := policies.Sites(ctx)
	if err != nil {
		return nil, err
	}
	groupPolicies, err := policies.ClientGroups(ctx)
	if err != nil {
		return nil, err
	}
	var out []rolloutCandidate
	now := s.now()
	for _, site := range running {
		plan, err := coreupdate.Decide(s.catalog, site.WPCoreVersion, corePolicy(sitePolicies[site.ID]),
			corePolicy(groupPolicies[site.ClientGroupID]), s.core.Policy, now)
		if err != nil {
			s.log.WarnContext(ctx, "planning a site's core update failed", "site_id", site.ID, "error", err)
			continue
		}
		if plan.Target != "" {
			out = append(out, rolloutCandidate{site: site, target: plan.Target})
		}
	}
	return out, nil
}

// RolloutStatus is a rollout's state for the API.
type RolloutStatus struct {
	store.Rollout
	// MaxFailures is how many failures in a row halt it.
	MaxFailures int
	// Window is the maintenance window.
	Window UpdateWindow
	// Failures are the sites whose last update failed.
	Failures []store.RolloutFailure
}

// Rollout returns the rollout's state.
func (s *Service) Rollout(ctx context.Context, name string) (RolloutStatus, error) {
	if _, ok := s.rolloutSources()[name]; !ok {
		return RolloutStatus{}, fmt.Errorf("%w %q", ErrUnknownRollout, name)
	}
	ro, err := store.NewRollouts(s.db).Get(ctx, name)
	if err != nil {
		return RolloutStatus{}, err
	}
	failures, err := store.NewRollouts(s.db).Failures(ctx, name)
	if err != nil {
		return RolloutStatus{}, err
	}
	st := RolloutStatus{Rollout: ro, MaxFailures: s.core.MaxFailures, Window: s.core.Window, Failures: []store.RolloutFailure{}}
	for _, id := range slices.Sorted(maps.Keys(failures)) {
		st.Failures = append(st.Failures, failures[id])
	}
	return st, nil
}

// ResumeRollout lifts a halt and queues a pass.
func (s *Service) ResumeRollout(ctx context.Context, name string) (RolloutStatus, error) {
	if _, ok := s.rolloutSources()[name]; !ok {
		return RolloutStatus{}, fmt.Errorf("%w %q", ErrUnknownRollout, name)
	}
	err := s.db.InTx(ctx, func(q store.Querier) error {
		if _, err := store.NewRollouts(q).Resume(ctx, name); err != nil {
			return err
		}
		return enqueueRolloutPass(ctx, q, name)
	})
	if err != nil {
		return RolloutStatus{}, err
	}
	s.log.InfoContext(ctx, "rollout resumed", "rollout", name)
	return s.Rollout(ctx, name)
}
