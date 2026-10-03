package sites_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// TestPreviewProtection covers preview password protection (#107): the
// password is generated when protection is first turned on, reaches the
// provisioner only as a bcrypt hash, survives turning protection off, is
// replaced by RegeneratePreviewPassword, and is deleted with the site. A
// failed update puts the previous setting back but keeps the password it
// returned.
func TestPreviewProtection(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db)
		site := e.running(t, "Guarded")
		stored := func() string {
			t.Helper()
			got := e.site(t, site.ID)
			if got.PreviewPasswordSecretRef == "" {
				return ""
			}
			pw, err := e.sec.Get(ctx, secrets.Ref(got.PreviewPasswordSecretRef))
			if err != nil {
				t.Fatal(err)
			}
			return pw.Reveal()
		}
		hashMatches := func(password string) bool {
			s, _ := e.prov.Site(site.ID)
			a := s.Config.PreviewAuth
			return a != nil && bcrypt.CompareHashAndPassword([]byte(a.PasswordHash), []byte(password)) == nil
		}

		saved, _, creds, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PreviewProtection: ptr(true)})
		if err != nil {
			t.Fatal(err)
		}
		if creds == nil || creds.Username != site.Slug || !saved.PreviewProtection || saved.PreviewUsername != site.Slug {
			t.Fatalf("enabling: site %+v, credentials %+v", saved, creds)
		}
		e.runJobs(t)
		if stored() != creds.Password.Reveal() || !hashMatches(creds.Password.Reveal()) {
			t.Error("the stored password or the provisioner's hash doesn't match the returned password")
		}
		if s, _ := e.prov.Site(site.ID); s.Config.PreviewAuth.Username != site.Slug {
			t.Errorf("preview auth = %+v", s.Config.PreviewAuth)
		}

		// Off keeps the password; on again returns nothing new.
		if _, _, c, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PreviewProtection: ptr(false)}); err != nil || c != nil {
			t.Fatalf("disabling: %v, %+v", err, c)
		}
		e.runJobs(t)
		if s, _ := e.prov.Site(site.ID); s.Config.PreviewAuth != nil || stored() != creds.Password.Reveal() {
			t.Errorf("after disabling: preview auth %+v", s.Config.PreviewAuth)
		}

		// A failed update reverts the setting, not the password.
		var calls atomic.Int32
		e.prov.Err = func(op, _ string) error {
			if op == "UpdateSite" && calls.Add(1) <= store.DefaultMaxAttempts {
				return errors.New("nginx unhealthy")
			}
			return nil
		}
		if _, _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PreviewProtection: ptr(true), PreviewUsername: ptr("client")}); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if got := e.site(t, site.ID); got.PreviewProtection || got.PreviewUsername != site.Slug || got.Status != store.SiteRunning {
			t.Errorf("after a failed update: %+v", got)
		}
		e.prov.Err = nil

		// Regenerating replaces the password, and deletes the old secret.
		old := e.site(t, site.ID).PreviewPasswordSecretRef
		if _, _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PreviewProtection: ptr(true)}); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		regen, job, err := e.svc.RegeneratePreviewPassword(ctx, site.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if regen.Username != site.Slug || regen.Password == creds.Password || job.Type != sites.UpdateJobType {
			t.Errorf("regenerated = %+v, job %s", regen, job.Type)
		}
		if got := e.site(t, site.ID); got.Status != store.SiteUpdating {
			t.Errorf("status while the new password is applied = %s", got.Status)
		}
		e.runJobs(t)
		if !hashMatches(regen.Password.Reveal()) || stored() != regen.Password.Reveal() {
			t.Error("the new password isn't the one applied and stored")
		}
		if _, err := e.sec.Get(ctx, secrets.Ref(old)); err == nil {
			t.Error("the old preview password secret is still there")
		}

		// Validation and busy sites.
		var ve *sites.ValidationError
		if _, _, _, err := e.svc.Update(ctx, site.ID, sites.SiteUpdate{PreviewUsername: ptr("a b")}); !errors.As(err, &ve) ||
			ve.Fields[0].Field != "/preview_protection/username" {
			t.Errorf("bad username: %v", err)
		}
		if _, _, err := e.svc.RegeneratePreviewPassword(ctx, "01j9zq3v6t8k4m2n5p7r9s1x3y", ""); !errors.Is(err, sites.ErrNotFound) {
			t.Errorf("missing site: %v", err)
		}

		// Deleting the site deletes the password.
		ref := e.site(t, site.ID).PreviewPasswordSecretRef
		if _, err := e.svc.Delete(ctx, site.ID, "", sites.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		e.runJobs(t)
		if _, err := e.sec.Get(ctx, secrets.Ref(ref)); err == nil {
			t.Error("the preview password secret outlived the site")
		}
	})
}
