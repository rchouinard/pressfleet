package sites

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Site template updates (#95, design doc §4.7, §5.5).
//
// A platform upgrade can change how the driver renders sites: the per-site
// Compose templates (templates/compose) or the Go code that fills them in.
// Re-rendering every site at once, as the startup reconciliation does for
// install-wide settings (reconcile.go), would put such a change on every
// site together, without a health check. So the driver fingerprints its
// rendering (provisioner.TemplateFingerprinter), every successful render
// records the fingerprint in the site's record (Service.recordRender), and
// the templates rollout ("templates", rollout.go) moves the sites rendered
// with another fingerprint, empty included, to the current one, one site at
// a time. Until it reaches a running site, the reconciliation leaves the
// site's containers alone.
//
// # The site update
//
// A [TemplateUpdateJobType] job re-renders one site with the current
// templates and configuration. Its steps:
//
//  1. prepare: check the job is for this release's templates. Its Undo
//     runs last in a rollback and records the outcome.
//  2. render: have the driver keep a copy of the site's rendered
//     definition (provisioner.RenderSnapshotter), then re-render it with
//     provisioner.UpdateSite, which recreates the containers whose
//     definition changed and waits for them to be healthy.
//  3. health_check: Service.CheckHealth, with the site's own core version.
//  4. plugins: converge the default plugins, as the reconciliation would
//     have after re-rendering the site.
//  5. finish: set the status from the live phase and record the outcome.
//
// The templates the site was rendered with before aren't in this release,
// so a failed re-render can't be undone by rendering again: if anything
// fails before the site is healthy, the render step's Undo puts the copy
// back (RestoreRender) and the previous fingerprint with it; the site ends
// up running as before with a status message saying so. If that fails too,
// the site is marked failed. Nothing inside WordPress changes, so there is
// no backup, as for an image update.

// TemplateUpdateJobType re-renders a site with the current templates.
const TemplateUpdateJobType = "site.template_update"

// templateRollout names the templates rollout.
const templateRollout = "templates"

// TemplateRolloutName is the name of the templates rollout, for
// Service.Rollout and Service.ResumeRollout.
const TemplateRolloutName = templateRollout

// Step names of the site.template_update job besides prepare and
// health_check, which it shares with the core update. Keep them stable: a
// queued job resumes by step name.
const (
	stepRenderTemplates  = "render"
	stepTemplatePlugins  = "plugins"
	stepFinishTemplates  = "finish"
	dataRenderSnapshot   = "render_snapshot"
	dataTemplateRendered = "templates_rendered"
)

// TemplateDriver is what the templates rollout needs of the driver.
type TemplateDriver interface {
	provisioner.TemplateFingerprinter
	provisioner.RenderSnapshotter
}

// templateUpdatePayload is the payload of a site.template_update job.
type templateUpdatePayload struct {
	// From is the fingerprint the site was rendered with (empty if
	// unknown) and To the one it moves to.
	From string `json:"from"`
	To   string `json:"to"`
	// Rollout is the rollout that queued the job.
	Rollout string `json:"rollout,omitempty"`
}

// recordRender records that the site was just rendered with the driver's
// current templates. Without fingerprints there is nothing to record.
func (s *Service) recordRender(ctx context.Context, siteID string) error {
	if s.templates == nil {
		return nil
	}
	_, err := store.NewSites(s.db).SetTemplateFingerprint(ctx, siteID, s.templates.TemplateFingerprint())
	return err
}

// awaitsTemplates reports whether the templates rollout will re-render the
// site: it runs, and was rendered with other templates. Stopped sites aren't
// the rollout's (it can't check their health), so the reconciliation
// re-renders them.
func (s *Service) awaitsTemplates(site store.Site) bool {
	return s.templates != nil && site.Status == store.SiteRunning &&
		site.TemplateFingerprint != s.templates.TemplateFingerprint()
}

// templateCandidates lists the running sites rendered with other templates,
// oldest site first.
func (rt *Rollouts) templateCandidates(ctx context.Context) ([]rolloutCandidate, error) {
	running, err := rt.s.runningSites(ctx)
	if err != nil {
		return nil, err
	}
	current := rt.s.templates.TemplateFingerprint()
	var out []rolloutCandidate
	for _, site := range running {
		if site.TemplateFingerprint != current {
			out = append(out, rolloutCandidate{site: site, target: current})
		}
	}
	return out, nil
}

// TemplateProgress returns the fingerprint of the driver's templates and
// how many running sites are rendered with others, for the templates
// rollout's status. Without fingerprints it returns ErrUnknownRollout.
func (rt *Rollouts) TemplateProgress(ctx context.Context) (fingerprint string, pending int, err error) {
	if rt.s.templates == nil {
		return "", 0, fmt.Errorf("%w %q", ErrUnknownRollout, templateRollout)
	}
	candidates, err := rt.templateCandidates(ctx)
	if err != nil {
		return "", 0, err
	}
	return rt.s.templates.TemplateFingerprint(), len(candidates), nil
}

// enqueueTemplateUpdate sets a running site's status to updating and queues
// its template update, in q's transaction. A site rendered with the target
// templates since it was listed is skipped.
func (rt *Rollouts) enqueueTemplateUpdate(ctx context.Context, q store.Querier, site store.Site, target, rollout string) (store.Job, error) {
	if site.TemplateFingerprint == target {
		return store.Job{}, errSkipCandidate
	}
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
		Type:       TemplateUpdateJobType,
		TargetType: store.JobTargetSite,
		TargetID:   site.ID,
		Payload:    templateUpdatePayload{From: site.TemplateFingerprint, To: target, Rollout: rollout},
		LockKey:    store.SiteLockKey(site.ID),
	})
}

func (rt *Rollouts) templateUpdateHandler() jobs.Handler {
	return jobs.Handler{Type: TemplateUpdateJobType, Steps: []jobs.Step{
		{Name: stepPrepareUpdate, Do: rt.prepareTemplateUpdate, Undo: rt.finishFailedTemplateUpdate},
		{Name: stepRenderTemplates, Do: rt.renderTemplates, Undo: rt.revertTemplates},
		{Name: stepHealthCheck, Do: rt.checkTemplateHealth},
		{Name: stepTemplatePlugins, Do: rt.templatePlugins},
		{Name: stepFinishTemplates, Do: rt.finishTemplateUpdate},
	}}
}

func templateJobPayload(r *jobs.Run) (templateUpdatePayload, error) {
	var p templateUpdatePayload
	if err := r.Payload(&p); err != nil {
		return p, err
	}
	if p.To == "" || p.From == p.To {
		return p, jobs.Permanent(fmt.Errorf("the template update job's payload has no change (%q to %q)", p.From, p.To))
	}
	return p, nil
}

// previousTemplates names the templates the site was rendered with, for
// messages.
func (p templateUpdatePayload) previousTemplates() string {
	if p.From == "" {
		return "an earlier release's"
	}
	return p.From
}

func (rt *Rollouts) prepareTemplateUpdate(ctx context.Context, r *jobs.Run) error {
	if _, err := rt.s.managedSite(ctx, r); err != nil {
		return err
	}
	p, err := templateJobPayload(r)
	if err != nil {
		return err
	}
	// The job can't render other templates than the running release's;
	// a later release's rollout queues its own.
	if current := rt.s.templates.TemplateFingerprint(); current != p.To {
		return jobs.Permanent(fmt.Errorf("the job is for templates %s, but this release has %s", p.To, current))
	}
	r.Logf("Re-rendering the site with this release's templates (%s; it was rendered with %s).", p.To, p.previousTemplates())
	return nil
}

// renderTemplates keeps a copy of the site's rendered definition, once, and
// re-renders it.
func (rt *Rollouts) renderTemplates(ctx context.Context, r *jobs.Run) error {
	site, err := rt.s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	// The copy is taken once: a retry of this step would otherwise copy
	// what the first attempt rendered.
	taken, err := flag(r, dataRenderSnapshot)
	if err != nil {
		return err
	}
	if !taken {
		err := rt.s.templates.SnapshotRender(ctx, site.ID)
		if errors.Is(err, provisioner.ErrSiteNotFound) {
			return jobs.Permanent(fmt.Errorf("the site has no rendered definition to keep: %w", err))
		}
		if err != nil {
			return err
		}
		if err := r.Set(dataRenderSnapshot, true); err != nil {
			return err
		}
	}
	// Before rendering, so a revert happens even if this attempt dies half
	// way.
	if err := r.Set(dataTemplateRendered, true); err != nil {
		return err
	}
	st, err := rt.s.converge(ctx, r, site)
	if err != nil {
		return err
	}
	r.Logf("Re-rendered the site with the new templates (%s); the containers whose definition changed were recreated.", st.Phase)
	return nil
}

// revertTemplates puts the site's previous rendered definition and
// fingerprint back unless the update succeeded. If that keeps failing, it
// gives up on its last attempt and records why, so the final Undo marks the
// site failed.
func (rt *Rollouts) revertTemplates(ctx context.Context, r *jobs.Run) error {
	if healthy, err := flag(r, dataHealthy); err != nil || healthy {
		return err
	}
	if rendered, err := flag(r, dataTemplateRendered); err != nil || !rendered {
		return err
	}
	site, err := rt.s.jobSite(ctx, r)
	if err != nil || site.Status != store.SiteUpdating {
		return nil
	}
	p, err := templateJobPayload(r)
	if err != nil {
		return err
	}
	if _, err := rt.s.templates.RestoreRender(ctx, site.ID); err != nil {
		if errors.Is(err, provisioner.ErrNoRenderSnapshot) || errors.Is(err, provisioner.ErrSiteNotFound) {
			err = jobs.Permanent(err)
		}
		if !lastAttempt(r, err) {
			return err
		}
		r.Warnf("Couldn't put the site's previous definition back: %v", err)
		return r.Set(dataRevertError, err.Error())
	}
	if _, err := store.NewSites(rt.s.db).SetTemplateFingerprint(ctx, site.ID, p.From); err != nil {
		return err
	}
	r.Warnf("Put the site's previous definition back: it runs as it was rendered with %s templates.", p.previousTemplates())
	return nil
}

func (rt *Rollouts) checkTemplateHealth(ctx context.Context, r *jobs.Run) error {
	site, err := rt.s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	if err := rt.s.CheckHealth(ctx, site, site.WPCoreVersion); err != nil {
		return jobs.Permanent(err)
	}
	if err := r.Set(dataHealthy, true); err != nil {
		return err
	}
	r.Logf("The site is healthy with the new templates: it answers HTTP requests, and WP-CLI loads WordPress %s.", site.WPCoreVersion)
	return nil
}

// templatePlugins converges the site's default plugins, as the
// reconciliation's plugins step does after it re-renders a site; the
// reconciliation left this site to the rollout.
func (rt *Rollouts) templatePlugins(ctx context.Context, r *jobs.Run) error {
	site, err := rt.s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	if err := rt.s.configurePlugins(ctx, r, site.ID); err != nil {
		return err
	}
	r.Logf("The site's default plugins are in place.")
	return nil
}

// finishTemplateUpdate sets the status from the live phase and records the
// success.
func (rt *Rollouts) finishTemplateUpdate(ctx context.Context, r *jobs.Run) error {
	if err := rt.s.finishUpdate(ctx, r); err != nil {
		return err
	}
	p, err := templateJobPayload(r)
	if err != nil {
		return err
	}
	if err := r.SetResult("templates", p.To); err != nil {
		return err
	}
	return rt.recordRolloutOutcome(ctx, r, templateRollout, p.Rollout, p.To, "")
}

// finishFailedTemplateUpdate is the last Undo of a failed
// site.template_update job: it sets the site's status, says in its status
// message what the failed update left behind, and records the failure.
func (rt *Rollouts) finishFailedTemplateUpdate(ctx context.Context, r *jobs.Run) error {
	siteID := r.Job().TargetID
	p, err := templateJobPayload(r)
	if err != nil {
		return err
	}
	var revertErr string
	if _, err := r.Get(dataRevertError, &revertErr); err != nil {
		return err
	}
	healthy, err := flag(r, dataHealthy)
	if err != nil {
		return err
	}
	rendered, err := flag(r, dataTemplateRendered)
	if err != nil {
		return err
	}
	status := rt.s.observedStatus(ctx, siteID)
	var prefix string
	switch {
	case healthy:
		prefix = "the template update finished, but the job failed afterwards"
	case revertErr != "":
		status = store.SiteFailed
		prefix = "the template update failed, and the site's previous definition couldn't be put back"
	case rendered:
		prefix = "the template update failed and was reverted to the previous definition"
	default:
		prefix = "the template update failed before changing the site"
	}
	msg := statusMessage(prefix, r.Job().Error)
	changed, err := store.NewSites(rt.s.db).SetStatus(ctx, siteID, status, msg, store.SiteUpdating)
	if err != nil {
		return err
	}
	if changed {
		r.Warnf("The site is %s: %s.", status, prefix)
	}
	// A job an earlier release queued says nothing about this release's
	// templates, so it doesn't count against their rollout.
	if p.To != rt.s.templates.TemplateFingerprint() {
		if p.Rollout == "" {
			return nil
		}
		return enqueueRolloutPass(ctx, rt.s.db, p.Rollout)
	}
	if healthy {
		return rt.recordRolloutOutcome(ctx, r, templateRollout, p.Rollout, p.To, "")
	}
	return rt.recordRolloutOutcome(ctx, r, templateRollout, p.Rollout, p.To, msg)
}
