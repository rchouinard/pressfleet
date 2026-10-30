package sites

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/domains"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Go-live (#45, design doc §5.2): switching a site from its preview domain
// to a primary domain, with optional aliases that redirect to it.
//
// [Service.GoLive] validates the domains and checks their DNS (package
// domains) before it changes anything: a domain that doesn't resolve to
// this host fails the request with a *DNSCheckError, and trying again once
// the records have propagated is all it takes. Only domains the site
// doesn't have yet are checked, and the check can be skipped for hosts
// behind a CDN or load balancer. The request then, in one transaction,
// records the domains as the site's desired state, adds the host names the
// site's URLs must be rewritten from (the preview domain and any earlier
// primary domain) to store.Site.URLRewriteFrom, sets the status to
// updating, and queues a [GoLiveJobType] job whose payload holds the
// previous domains. The job's steps are:
//
//  1. prepare: check the site is still going live. Its Undo runs last in a
//     rollback (see below).
//  2. route: provisioner.UpdateSite with the new domains. The driver adds
//     routes for the primary domain and each alias, which Traefik gets
//     certificates for with HTTP-01, and redirects the preview domain and
//     the aliases to the primary domain; nothing is recreated. Undo puts
//     the previous domains back.
//  3. rewrite_urls: `wp search-replace` from each host name in
//     URLRewriteFrom to the primary domain, in every table of the site's
//     database except the guid column (WordPress's convention: a post's
//     GUID must never change), serialized data included, for both plain
//     (//host) and JSON-escaped (\/\/host) URLs; then `home` and `siteurl`
//     are set to https://<primary domain>, and URLRewriteFrom is cleared.
//     Every run is idempotent: what was rewritten isn't found again.
//  4. purge_object_cache and purge_page_cache (cache.go), so no page or
//     object still carries the preview URLs.
//  5. finish: set the status from the live phase.
//
// # Failure
//
// Up to the URL rewrite, a failed go-live is rolled back: the previous
// routes and domains are put back, so the site is on its preview domain
// again (or its previous primary domain), and its status message says why.
//
// Once the rewrite has started, the job no longer rolls back: reverting
// search-replace isn't safe (it would also rewrite URLs of the new domain
// that were in the content before), and the new domain is routed already.
// The site keeps its new domains, both the preview redirect and the
// primary domain keep working, and the status message says the go-live
// didn't finish. The host names still to rewrite stay in URLRewriteFrom,
// so going live again with the same domains, which the status message
// suggests, finishes the job.
//
// # Changing domains later
//
// Going live again with another primary domain moves the site there: the
// old primary domain's URLs are rewritten too, and it stops being routed
// unless it is listed as an alias. Going live again with the same primary
// domain and other aliases changes the aliases, as does [Service.SetAliases]
// (aliases.go) without touching WordPress. Going back to the preview
// domain alone isn't supported.

// GoLiveJobType switches a site to its primary domain.
const GoLiveJobType = "site.go_live"

// Step names of the site.go_live job, besides prepare, the cache purge
// steps, and finish. Keep them stable: a queued job resumes by step name.
const (
	stepRoute       = "route"
	stepRewriteURLs = "rewrite_urls"
)

// dataURLRewriteStarted is the site.go_live job data key recording that
// rewrite_urls has started to change the site's database, after which the
// job doesn't roll back.
const dataURLRewriteStarted = "url_rewrite_started"

// MaxAliases is how many aliases a site may have.
const MaxAliases = 20

// ErrNotRunning means the site must be running for the request (going live,
// restoring), which needs WP-CLI.
var ErrNotRunning = errors.New("the site must be running for this; start it first")

// DNSCheckError means a request's domains failed the DNS check, or the
// check couldn't run. Nothing was changed; the request can be sent again
// once DNS has propagated.
type DNSCheckError struct {
	// Fields has one entry per domain that doesn't resolve to this host,
	// named by its JSON Pointer in the request.
	Fields []FieldError
	// Detail, if not empty, says why the check couldn't run at all.
	Detail string
}

func (e *DNSCheckError) Error() string {
	if e.Detail != "" {
		return "DNS check failed: " + e.Detail
	}
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Detail
	}
	return "DNS check failed: " + strings.Join(parts, "; ")
}

// DNSChecker checks that domains resolve to this host; *domains.Checker
// implements it.
type DNSChecker interface {
	Check(ctx context.Context, names []string) ([]domains.Problem, error)
}

// GoLive is a request to switch a site to a primary domain.
type GoLive struct {
	PrimaryDomain string
	// Aliases replace the site's aliases.
	Aliases []string
	// SkipDNSCheck skips the DNS check, for hosts whose domains resolve to
	// a CDN or load balancer in front of them.
	SkipDNSCheck bool
	// CreatedByUserID is the user starting the job.
	CreatedByUserID string
}

// domainsPayload is the payload of site.go_live and site.aliases jobs: the
// domains before the request, which a rollback puts back.
type domainsPayload struct {
	Previous     store.SiteDomains `json:"previous"`
	SkipDNSCheck bool              `json:"skip_dns_check,omitempty"`
}

// GoLive validates the domains, checks their DNS unless told not to, saves
// them as the site's domains with status updating, and queues a
// [GoLiveJobType] job that applies them. It returns the site as saved, its
// aliases, and the job. Invalid domains are a *ValidationError, and
// domains that don't resolve to this host a *DNSCheckError; in both cases
// nothing changed. A site that isn't running is ErrNotRunning; one a job is
// working on, or that never finished provisioning, ErrBusy or
// ErrNotProvisioned.
func (s *Service) GoLive(ctx context.Context, siteID string, req GoLive) (store.Site, []string, store.Job, error) {
	primary, aliases, err := s.validateDomains(req.PrimaryDomain, req.Aliases)
	if err != nil {
		return store.Site{}, nil, store.Job{}, err
	}
	// Fail fast, before the DNS lookups; the transaction checks again.
	site, err := s.manageable(ctx, store.NewSites(s.db), siteID)
	if err != nil {
		return store.Site{}, nil, store.Job{}, err
	}
	if site.IsStaging() {
		return store.Site{}, nil, store.Job{}, &ValidationError{Fields: []FieldError{{"/primary_domain", "staging sites only have a preview domain"}}}
	}
	if site.Status != store.SiteRunning {
		return store.Site{}, nil, store.Job{}, fmt.Errorf("%w (it is %s)", ErrNotRunning, site.Status)
	}
	if !req.SkipDNSCheck {
		current, err := store.NewSites(s.db).Domains(ctx, siteID)
		if err != nil {
			return store.Site{}, nil, store.Job{}, err
		}
		if err := s.checkDNS(ctx, current, primary, aliases); err != nil {
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
		if current.Status != store.SiteRunning {
			return fmt.Errorf("%w (it is %s)", ErrNotRunning, current.Status)
		}
		prev, err := sites.Domains(ctx, siteID)
		if err != nil {
			return err
		}
		next := store.SiteDomains{
			Primary:        primary,
			Aliases:        aliases,
			URLRewriteFrom: rewriteSources(current, prev, primary),
		}
		if err := s.setDomains(ctx, sites, siteID, next); err != nil {
			return err
		}
		if _, err := sites.SetStatus(ctx, siteID, store.SiteUpdating, ""); err != nil {
			return err
		}
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            GoLiveJobType,
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
	s.log.InfoContext(ctx, "site go-live queued", "site_id", siteID, "job_id", job.ID,
		"primary_domain", primary, "aliases", len(aliases), "dns_check", !req.SkipDNSCheck)
	return site, aliases, job, nil
}

// rewriteSources are the host names whose URLs going live on primary
// rewrites: those an earlier go-live didn't finish, the preview domain
// (which redirects to the primary domain from now on), and the previous
// primary domain, if it changes.
func rewriteSources(site store.Site, prev store.SiteDomains, primary string) []string {
	var out []string
	for _, h := range slices.Concat(prev.URLRewriteFrom, []string{site.PreviewDomain, prev.Primary}) {
		if h != "" && h != primary && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}

// validateDomains normalizes a primary domain and aliases and checks them
// against each other and the platform's own domains. An empty primary is
// allowed when checking aliases alone (SetAliases passes the site's).
func (s *Service) validateDomains(primary string, aliases []string) (string, []string, error) {
	var fields []FieldError
	check := func(field, raw string) string {
		d, err := domains.Normalize(raw)
		if err != nil {
			fields = append(fields, FieldError{field, err.Error()})
			return ""
		}
		switch {
		case s.previewBase != "" && (d == s.previewBase || strings.HasSuffix(d, "."+s.previewBase)):
			fields = append(fields, FieldError{field, "is under the preview base domain " + s.previewBase + ", which the platform assigns"})
		case s.dashboard != "" && d == s.dashboard:
			fields = append(fields, FieldError{field, "is the dashboard's domain"})
		default:
			return d
		}
		return ""
	}
	p := check("/primary_domain", primary)
	if len(aliases) > MaxAliases {
		fields = append(fields, FieldError{"/aliases", fmt.Sprintf("must list at most %d aliases", MaxAliases)})
	}
	out := make([]string, 0, len(aliases))
	for i, raw := range aliases {
		field := "/aliases/" + strconv.Itoa(i)
		d := check(field, raw)
		switch {
		case d == "":
		case d == p:
			fields = append(fields, FieldError{field, "is the primary domain"})
		case slices.Contains(out, d):
			fields = append(fields, FieldError{field, "is listed twice"})
		default:
			out = append(out, d)
		}
	}
	if len(fields) > 0 {
		return "", nil, &ValidationError{Fields: fields}
	}
	slices.Sort(out)
	return p, out, nil
}

// checkDNS runs the DNS check on the domains the site doesn't have yet.
func (s *Service) checkDNS(ctx context.Context, current store.SiteDomains, primary string, aliases []string) error {
	pointer := map[string]string{}
	var names []string
	add := func(d, field string) {
		if d == "" || d == current.Primary || slices.Contains(current.Aliases, d) {
			return
		}
		pointer[d] = field
		names = append(names, d)
	}
	add(primary, "/primary_domain")
	for _, a := range aliases {
		add(a, "/aliases")
	}
	if len(names) == 0 {
		return nil
	}
	problems, err := s.dns.Check(ctx, names)
	if errors.Is(err, domains.ErrHostUnknown) {
		return &DNSCheckError{Detail: err.Error() +
			"; set public_ip (PRESSFLEET_PUBLIC_IP) to this host's public address, or skip the DNS check"}
	}
	if err != nil {
		return fmt.Errorf("checking DNS: %w", err)
	}
	if len(problems) == 0 {
		return nil
	}
	e := &DNSCheckError{}
	for _, p := range problems {
		e.Fields = append(e.Fields, FieldError{pointer[p.Domain], p.Domain + ": " + p.Detail})
	}
	return e
}

// setDomains saves the site's domains, refusing domains another site uses.
func (s *Service) setDomains(ctx context.Context, sites store.Sites, siteID string, d store.SiteDomains) error {
	var fields []FieldError
	for i, name := range append([]string{d.Primary}, d.Aliases...) {
		if name == "" {
			continue
		}
		owner, err := sites.DomainOwner(ctx, name)
		if err != nil {
			return err
		}
		if owner != "" && owner != siteID {
			field := "/primary_domain"
			if i > 0 {
				field = "/aliases"
			}
			fields = append(fields, FieldError{field, name + " is already used by another site"})
		}
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	_, err := sites.SetDomains(ctx, siteID, d)
	if errors.Is(err, store.ErrConflict) {
		// Another request took one of the domains since the check above.
		return &ValidationError{Fields: []FieldError{{"/primary_domain", "a domain is already used by another site"}}}
	}
	return err
}

func (s *Service) goLiveHandler() jobs.Handler {
	steps := []jobs.Step{
		{Name: stepPrepareUpdate, Do: s.prepareGoLive, Undo: s.revertGoLive},
		{Name: stepRoute, Do: s.routeDomains, Undo: s.revertRoute},
		{Name: stepRewriteURLs, Do: s.rewriteURLs},
	}
	steps = append(steps, s.cacheSteps()...)
	steps = append(steps, jobs.Step{Name: stepFinishManage, Do: s.finishDomains})
	return jobs.Handler{Type: GoLiveJobType, Steps: steps}
}

func (s *Service) prepareGoLive(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	if site.PrimaryDomain == "" {
		return jobs.Permanent(errors.New("the site has no primary domain to go live on"))
	}
	aliases, err := store.NewSites(s.db).Aliases(ctx, site.ID)
	if err != nil {
		return err
	}
	r.Logf("Going live on %s%s, from %s.", site.PrimaryDomain, aliasList(aliases), site.PreviewDomain)
	var p domainsPayload
	if err := r.Payload(&p); err == nil && p.SkipDNSCheck {
		r.Warnf("The DNS check was skipped at the user's request.")
	}
	return nil
}

func aliasList(aliases []string) string {
	if len(aliases) == 0 {
		return ""
	}
	return " (aliases " + strings.Join(aliases, ", ") + ")"
}

// routeDomains applies the site's recorded domains to its routes.
func (s *Service) routeDomains(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	st, err := s.converge(ctx, r, site)
	if err != nil {
		return err
	}
	r.Logf("Routed the site's domains (%s); the edge router gets their certificates with HTTP-01.", st.Phase)
	return nil
}

// rewriteStarted reports whether rewrite_urls has started to change the
// site's database.
func rewriteStarted(r *jobs.Run) (bool, error) {
	var started bool
	_, err := r.Get(dataURLRewriteStarted, &started)
	return started, err
}

// revertRoute puts the previous domains back on the site's routes, unless
// the URL rewrite has started, after which the new routes stay. Like
// revertApply, it gives up on its last attempt without failing the
// rollback and records why, so revertDomains marks the site failed.
func (s *Service) revertRoute(ctx context.Context, r *jobs.Run) error {
	site, err := s.jobSite(ctx, r)
	if err != nil || site.Status != store.SiteUpdating {
		return nil
	}
	if started, err := rewriteStarted(r); err != nil || started {
		if started {
			r.Logf("Keeping the new routes: the site's URLs were already being rewritten.")
		}
		return err
	}
	var p domainsPayload
	if err := r.Payload(&p); err != nil {
		return jobs.Permanent(err)
	}
	prev := site
	prev.PrimaryDomain = p.Previous.Primary
	_, err = s.convergeWith(ctx, r, prev, p.Previous.Aliases)
	if err == nil {
		r.Logf("Put the previous routes back.")
		return nil
	}
	if !jobs.IsPermanent(err) && r.Job().Attempts < r.Job().MaxAttempts {
		return err
	}
	r.Warnf("Couldn't put the previous routes back: %v", err)
	return r.Set(dataRevertError, err.Error())
}

// revertDomains is the last Undo of a failed site.go_live or site.aliases
// job. Before the URL rewrite started, it restores the previous domains in
// the site record; after, it keeps the new ones (see the top of this
// file). Either way it sets the status from what the containers are doing,
// with the error in the status message, or failed if the routes couldn't
// be reverted. A site being deleted is left alone.
func (s *Service) revertDomains(ctx context.Context, r *jobs.Run, what string) error {
	siteID := r.Job().TargetID
	var p domainsPayload
	if err := r.Payload(&p); err != nil {
		return jobs.Permanent(err)
	}
	var revertErr string
	if _, err := r.Get(dataRevertError, &revertErr); err != nil {
		return err
	}
	started, err := rewriteStarted(r)
	if err != nil {
		return err
	}
	status, msg := store.SiteFailed, statusMessage(what+" failed", r.Job().Error)
	switch {
	case started:
		status = s.observedStatus(ctx, siteID)
	case revertErr == "":
		status = s.observedStatus(ctx, siteID)
		msg = statusMessage(what+" failed and was reverted", r.Job().Error)
	}
	var site store.Site
	err = s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		site, err = sites.Get(ctx, siteID)
		if errors.Is(err, store.ErrNotFound) || (err == nil && site.Status != store.SiteUpdating) {
			return nil
		}
		if err != nil {
			return err
		}
		if started {
			msg = statusMessage("going live on "+site.PrimaryDomain+" didn't finish; the site is served there, "+
				"but some of its URLs may still name "+strings.Join(site.URLRewriteFrom, ", ")+
				". Go live again with the same domains to finish", r.Job().Error)
		} else if _, err := sites.SetDomains(ctx, siteID, p.Previous); err != nil {
			return err
		}
		_, err = sites.SetStatus(ctx, siteID, status, msg, store.SiteUpdating)
		return err
	})
	if err != nil {
		return err
	}
	if started {
		r.Warnf("Kept the new domains; the site is %s. Going live again with the same domains finishes the job.", status)
	} else {
		r.Warnf("Restored the previous domains; the site is %s.", status)
	}
	return nil
}

func (s *Service) revertGoLive(ctx context.Context, r *jobs.Run) error {
	return s.revertDomains(ctx, r, "going live")
}

// rewriteURLs rewrites the site's URLs from each host name in
// URLRewriteFrom to the primary domain, and sets home and siteurl.
func (s *Service) rewriteURLs(ctx context.Context, r *jobs.Run) error {
	site, err := s.managedSite(ctx, r)
	if err != nil {
		return err
	}
	target := site.PrimaryDomain
	if target == "" {
		return jobs.Permanent(errors.New("the site has no primary domain"))
	}
	// Ask WordPress first: a site that can't run WP-CLI fails here, before
	// its database changed, so the job can still roll back.
	home, err := s.prov.ExecWPCLI(ctx, site.ID, []string{"option", "get", "home"})
	if err != nil {
		return fmt.Errorf("reading the site's home URL: %w", err)
	}
	if err := r.Set(dataURLRewriteStarted, true); err != nil {
		return err
	}
	r.Logf("The site's home URL is %s.", strings.TrimSpace(home))
	for _, from := range site.URLRewriteFrom {
		if from == target {
			continue
		}
		// Plain URLs (http, https, and protocol-relative), and the escaped
		// form JSON gives them, as in block attributes.
		for _, sep := range []string{"//", `\/\/`} {
			out, err := s.prov.ExecWPCLI(ctx, site.ID, []string{
				"search-replace", sep + from, sep + target,
				"--all-tables-with-prefix", "--skip-columns=guid", "--format=count",
			})
			if err != nil {
				return fmt.Errorf("replacing %s with %s: %w", sep+from, sep+target, err)
			}
			r.Logf("Replaced %s with %s: %s.", sep+from, sep+target, countOf(out))
		}
	}
	// search-replace changed the database under the object cache, which
	// still holds the old options. `wp option update` would compare with
	// those, find a change the database has already made, and fail with
	// "Could not update option".
	if err := s.caches.FlushCache(ctx, site.ID); err != nil {
		return fmt.Errorf("flushing the object cache: %w", err)
	}
	url := "https://" + target
	for _, option := range []string{"home", "siteurl"} {
		if _, err := s.prov.ExecWPCLI(ctx, site.ID, []string{"option", "update", option, url}); err != nil {
			return fmt.Errorf("setting %s: %w", option, err)
		}
	}
	if _, err := store.NewSites(s.db).ClearURLRewrite(ctx, site.ID, site.URLRewriteFrom); err != nil {
		return err
	}
	r.Logf("WordPress's address is %s.", url)
	return nil
}

// countOf describes the output of `wp search-replace --format=count`.
func countOf(out string) string {
	n := strings.TrimSpace(out)
	if n == "1" {
		return "1 replacement"
	}
	if n == "" {
		n = "no"
	}
	return n + " replacements"
}

// finishDomains sets the status like an update does and returns the
// site's URL.
func (s *Service) finishDomains(ctx context.Context, r *jobs.Run) error {
	if err := s.finishUpdate(ctx, r); err != nil {
		return err
	}
	site, err := s.jobSite(ctx, r)
	if err != nil {
		return err
	}
	return r.SetResult("url", URL(site))
}
