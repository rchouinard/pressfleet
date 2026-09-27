package sites

import (
	"context"
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/bcrypt"

	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Preview password protection (#107, design doc §5.1).
//
// A site's preview domain can ask for HTTP basic auth. It is a setting like
// the PHP version: [Service.Update] turns it on or off (and changes the
// username), and the site.update job applies it with
// provisioner.UpdateSite, which gives the preview router a basicAuth
// middleware. Only the preview domain is ever protected.
//
// The platform generates the password: the first time protection is turned
// on, and whenever [Service.RegeneratePreviewPassword] is called. The
// password is stored encrypted (secrets.KindSitePreviewPassword), for
// uptime checks, and returned only by the request that generated it. The
// site record also keeps its bcrypt hash, which is all the driver and
// Traefik see; a fixed hash keeps re-renders from changing the site's
// labels. Turning protection off keeps the credentials, so turning it on
// again restores them.

// PreviewCredentials is the basic auth login of a site's preview domain.
type PreviewCredentials struct {
	Username string
	Password redact.Secret
}

// previewBcryptCost is the bcrypt cost of preview password hashes. Traefik
// checks the hash on every request to a protected preview domain, every
// stylesheet and image included, so a higher cost would be paid many times
// per page view. It buys nothing here: the password is 130 random bits,
// which can't be guessed however cheap each guess is.
const previewBcryptCost = bcrypt.MinCost

// newPreviewPassword generates a preview password and its bcrypt hash.
func newPreviewPassword() (redact.Secret, string, error) {
	password := redact.Secret(rand.Text())
	hash, err := bcrypt.GenerateFromPassword([]byte(password.Reveal()), previewBcryptCost)
	if err != nil {
		return "", "", fmt.Errorf("hashing the preview password: %w", err)
	}
	return password, string(hash), nil
}

// replacePreviewPassword generates a new preview password for the site in
// q's transaction, stores it, and deletes the old one.
func (s *Service) replacePreviewPassword(ctx context.Context, q store.Querier, site store.Site) (redact.Secret, error) {
	password, hash, err := newPreviewPassword()
	if err != nil {
		return "", err
	}
	sec := s.secrets.With(q)
	ref, err := sec.Put(ctx, secrets.KindSitePreviewPassword, password)
	if err != nil {
		return "", err
	}
	if _, err := store.NewSites(q).SetPreviewPassword(ctx, site.ID, string(ref), hash); err != nil {
		return "", err
	}
	if old := site.PreviewPasswordSecretRef; old != "" {
		if err := sec.Delete(ctx, secrets.Ref(old)); err != nil {
			return "", err
		}
	}
	return password, nil
}

// RegeneratePreviewPassword replaces the site's preview password with a
// new one and queues a site.update job that gives it to the edge router;
// the site's status is updating until it has. It returns the new
// credentials, which are never returned again, and the job. It works
// whether or not protection is on; with it off, the password is kept for
// when it is turned on, and the job has nothing to change.
func (s *Service) RegeneratePreviewPassword(ctx context.Context, siteID, createdByUserID string) (PreviewCredentials, store.Job, error) {
	var (
		creds PreviewCredentials
		job   store.Job
	)
	err := s.db.InTx(ctx, func(q store.Querier) error {
		sites := store.NewSites(q)
		site, err := s.manageable(ctx, sites, siteID)
		if err != nil {
			return err
		}
		if site.PreviewUsername == "" {
			// Never turned on yet: pick the username it will have.
			set := site.Settings()
			set.PreviewUsername = site.Slug
			if _, err := sites.SetSettings(ctx, siteID, set); err != nil {
				return err
			}
			site = site.WithSettings(set)
		}
		password, err := s.replacePreviewPassword(ctx, q, site)
		if err != nil {
			return err
		}
		if _, err := sites.SetStatus(ctx, siteID, store.SiteUpdating, ""); err != nil {
			return err
		}
		job, err = jobs.Enqueue(ctx, q, jobs.Spec{
			Type:            UpdateJobType,
			TargetType:      "site",
			TargetID:        siteID,
			Payload:         updatePayload{Previous: site.Settings()},
			LockKey:         LockKey(siteID),
			CreatedByUserID: createdByUserID,
		})
		creds = PreviewCredentials{Username: site.PreviewUsername, Password: password}
		return err
	})
	if err != nil {
		return PreviewCredentials{}, store.Job{}, err
	}
	s.log.InfoContext(ctx, "site preview password regenerated", "site_id", siteID, "job_id", job.ID)
	return creds, job, nil
}

// previewAuth is the provisioner's view of the site's preview protection:
// nil unless it is on and has a password.
func previewAuth(site store.Site) *provisioner.BasicAuth {
	if !site.PreviewProtection || site.PreviewPasswordHash == "" {
		return nil
	}
	return &provisioner.BasicAuth{Username: site.PreviewUsername, PasswordHash: site.PreviewPasswordHash}
}
