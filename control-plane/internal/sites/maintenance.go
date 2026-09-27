package sites

import (
	"context"
	"errors"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Maintenance mode (#44, design doc §5.4).
//
// [Service.SetMaintenance] records the site's maintenance flag, its desired
// state, and queues a [MaintenanceJobType] job that applies it with
// provisioner.SetMaintenance, which puts the site's domains on the
// platform's maintenance page (or takes them off) within seconds without
// touching its containers. Because the flag is part of the site record,
// SiteConfig carries it, and every re-render (updates, the startup
// reconciliation) keeps the site in or out of maintenance mode.
//
// The job applies whatever the record says when it runs, so toggles in
// quick succession converge on the last one. If it fails for good, the
// flag is put back, unless another request has changed it since.
//
// Restores and rollouts (M3) use the same two pieces in their own steps:
// set the flag with store.Sites.SetMaintenance, then call
// provisioner.SetMaintenance, and the reverse when they are done.

// MaintenanceJobType applies a site's maintenance flag.
const MaintenanceJobType = "site.maintenance"

// stepApplyMaintenance is the site.maintenance job's step. Keep it stable:
// a queued job resumes by step name.
const stepApplyMaintenance = "apply"

// maintenancePayload records the change a site.maintenance job applies, so
// a failed job can put the previous flag back.
type maintenancePayload struct {
	Enabled  bool `json:"enabled"`
	Previous bool `json:"previous"`
}

// SetMaintenance records whether the site is in maintenance mode and queues
// a [MaintenanceJobType] job that applies it. It returns the site as saved
// and the job. The site's status doesn't change. Like updates, it is
// refused with ErrBusy while another job is provisioning, updating, or
// deleting the site, and with ErrNotProvisioned for a site whose
// provisioning failed.
func (s *Service) SetMaintenance(ctx context.Context, siteID string, enabled bool, createdByUserID string) (store.Site, store.Job, error) {
	var (
		site store.Site
		job  store.Job
	)
	err := s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		current, err := s.manageable(ctx, sites, siteID)
		if err != nil {
			return err
		}
		if _, err := sites.SetMaintenance(ctx, siteID, enabled); err != nil {
			return err
		}
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            MaintenanceJobType,
			TargetType:      "site",
			TargetID:        siteID,
			Payload:         maintenancePayload{Enabled: enabled, Previous: current.Maintenance},
			LockKey:         LockKey(siteID),
			CreatedByUserID: createdByUserID,
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
	s.log.InfoContext(ctx, "site maintenance change queued", "site_id", siteID, "job_id", job.ID, "enabled", enabled)
	return site, job, nil
}

func (s *Service) maintenanceHandler() jobs.Handler {
	return jobs.Handler{
		Type: MaintenanceJobType,
		Steps: []jobs.Step{
			{Name: stepApplyMaintenance, Do: s.applyMaintenance, Undo: s.revertMaintenance},
		},
	}
}

// applyMaintenance applies the site's recorded maintenance flag.
func (s *Service) applyMaintenance(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	if err := s.prov.SetMaintenance(ctx, site.ID, site.Maintenance); err != nil {
		if errors.Is(err, provisioner.ErrSiteNotFound) {
			return jobs.Permanent(err)
		}
		return err
	}
	if err := r.SetResult("site_id", site.ID); err != nil {
		return err
	}
	if site.Maintenance {
		r.Logf("The site's domains show the maintenance page.")
	} else {
		r.Logf("The site's domains serve the site again.")
	}
	return nil
}

// revertMaintenance puts the previous flag back after the job failed for
// good, unless another request has changed it since, and applies it on a
// best-effort basis: the provisioner failing is likely why the job failed.
func (s *Service) revertMaintenance(ctx context.Context, r *jobs.Run) error {
	var p maintenancePayload
	if err := r.Payload(&p); err != nil {
		return jobs.Permanent(err)
	}
	siteID := r.Job().TargetID
	changed, err := store.NewSites(s.db).SetMaintenance(ctx, siteID, p.Previous, p.Enabled)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := s.prov.SetMaintenance(ctx, siteID, p.Previous); err != nil {
		r.Warnf("Couldn't apply the previous maintenance setting: %v", err)
	}
	r.Warnf("Restored the previous maintenance setting (%v).", p.Previous)
	return nil
}
