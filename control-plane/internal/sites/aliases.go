package sites

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Alias management (#46, design doc §4.3, §5.2): changing a live site's
// aliases without going live again.
//
// [Service.SetAliases] replaces the aliases of a site that has a primary
// domain. It validates them like go-live and checks the DNS of the aliases
// the site doesn't have yet (so removing one, or keeping one whose records
// have since moved to a CDN, needs no check), records them with status
// updating, and queues an [AliasesJobType] job:
//
//  1. prepare: check the site is still live. Its Undo, the last step of a
//     rollback, puts the previous aliases back in the site record.
//  2. route: provisioner.UpdateSite. Each alias gets a router of its own
//     that redirects to the primary domain, and Traefik a certificate for
//     it with HTTP-01; removed aliases stop being routed. The routes live
//     in Traefik's file provider (see the Compose driver), so nothing is
//     recreated and the site serves throughout. Undo puts the previous
//     aliases back on the routes.
//  3. finish: set the status from the live phase.
//
// WordPress's URLs and the caches are left alone: aliases only redirect,
// so no page ever names them. The site doesn't need to be running.

// AliasesJobType applies a live site's aliases.
const AliasesJobType = "site.aliases"

// ErrNotLive means the site has no primary domain, so it has no aliases to
// change; it goes live first.
var ErrNotLive = errors.New("the site isn't live yet; go live first, with the aliases")

// SetAliases is a request to replace a live site's aliases.
type SetAliases struct {
	Aliases []string
	// SkipDNSCheck skips the DNS check of new aliases, for hosts behind a
	// CDN or load balancer.
	SkipDNSCheck bool
	// CreatedByUserID is the user starting the job.
	CreatedByUserID string
}

// SetAliases validates the aliases, checks the DNS of new ones unless told
// not to, saves them with status updating, and queues an [AliasesJobType]
// job that applies them. It returns the site as saved, its aliases, and the
// job. Errors are as for [Service.GoLive], except that the site needn't be
// running, and a site without a primary domain is ErrNotLive.
func (s *Service) SetAliases(ctx context.Context, siteID string, req SetAliases) (store.Site, []string, store.Job, error) {
	site, err := s.manageable(ctx, store.NewSites(s.db), siteID)
	if err != nil {
		return store.Site{}, nil, store.Job{}, err
	}
	if site.IsStaging() {
		return store.Site{}, nil, store.Job{}, &ValidationError{Fields: []FieldError{{"/aliases", "staging sites only have a preview domain"}}}
	}
	if site.PrimaryDomain == "" {
		return store.Site{}, nil, store.Job{}, ErrNotLive
	}
	_, aliases, err := s.validateDomains(site.PrimaryDomain, req.Aliases)
	if err != nil {
		return store.Site{}, nil, store.Job{}, err
	}
	if !req.SkipDNSCheck {
		current, err := store.NewSites(s.db).Domains(ctx, siteID)
		if err != nil {
			return store.Site{}, nil, store.Job{}, err
		}
		// The primary domain is the site's already: only new aliases are
		// checked.
		if err := s.checkDNS(ctx, current, "", aliases); err != nil {
			return store.Site{}, nil, store.Job{}, err
		}
	}

	var job store.Job
	err = s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		current, err := s.manageable(ctx, sites, siteID)
		if err != nil {
			return err
		}
		prev, err := sites.Domains(ctx, siteID)
		if err != nil {
			return err
		}
		if prev.Primary == "" || current.PrimaryDomain != site.PrimaryDomain {
			// Gone live elsewhere, or not at all, since the checks.
			return fmt.Errorf("%w: the site's primary domain changed; send the request again", ErrBusy)
		}
		next := store.SiteDomains{Primary: prev.Primary, Aliases: aliases, URLRewriteFrom: prev.URLRewriteFrom}
		if err := s.setDomains(ctx, sites, siteID, next); err != nil {
			return err
		}
		if _, err := sites.SetStatus(ctx, siteID, store.SiteUpdating, ""); err != nil {
			return err
		}
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            AliasesJobType,
			TargetType:      store.JobTargetSite,
			TargetID:        siteID,
			Payload:         domainsPayload{Previous: prev, SkipDNSCheck: req.SkipDNSCheck},
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
		return store.Site{}, nil, store.Job{}, err
	}
	s.log.InfoContext(ctx, "site alias change queued", "site_id", siteID, "job_id", job.ID,
		"aliases", len(aliases), "dns_check", !req.SkipDNSCheck)
	return site, aliases, job, nil
}

func (s *Service) aliasesHandler() jobs.Handler {
	return jobs.Handler{
		Type: AliasesJobType,
		Steps: []jobs.Step{
			{Name: stepPrepareUpdate, Do: s.prepareAliases, Undo: s.revertAliases},
			{Name: stepRoute, Do: s.routeDomains, Undo: s.revertRoute},
			{Name: stepFinishManage, Do: s.finishDomains},
		},
	}
}

func (s *Service) prepareAliases(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	if site.PrimaryDomain == "" {
		return jobs.Permanent(ErrNotLive)
	}
	aliases, err := store.NewSites(s.db).Aliases(ctx, site.ID)
	if err != nil {
		return err
	}
	if len(aliases) == 0 {
		r.Logf("Removing every alias of %s.", site.PrimaryDomain)
	} else {
		r.Logf("Redirecting %v to %s.", aliases, site.PrimaryDomain)
	}
	var p domainsPayload
	if err := r.Payload(&p); err == nil && p.SkipDNSCheck {
		r.Warnf("The DNS check was skipped at the user's request.")
	}
	return nil
}

func (s *Service) revertAliases(ctx context.Context, r *jobs.Run) error {
	return s.revertDomains(ctx, r, "changing aliases")
}
