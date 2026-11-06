package changes_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/changes"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// fixture writes jobs and audit events.
type fixture struct {
	t  *testing.T
	db *store.DB
}

func (f fixture) group(name string) store.ClientGroup {
	f.t.Helper()
	g := store.ClientGroup{Name: name}
	if err := store.NewClientGroups(f.db).Create(context.Background(), &g); err != nil {
		f.t.Fatal(err)
	}
	return g
}

func (f fixture) user(email string) string {
	f.t.Helper()
	u := store.User{Email: email, PasswordHash: "x", Role: store.RoleAdmin}
	if err := store.NewUsers(f.db).Create(context.Background(), &u); err != nil {
		f.t.Fatal(err)
	}
	return u.ID
}

func (f fixture) site(slug, groupID, parentID string, uid int) store.Site {
	f.t.Helper()
	s := store.Site{
		ClientGroupID: groupID, ParentSiteID: parentID, Name: slug, Slug: slug, Status: store.SiteRunning,
		PreviewDomain: slug + ".preview.example", PHPVersion: "8.3", WPCoreVersion: "6.8", UID: uid,
	}
	if err := store.NewSites(f.db).Create(context.Background(), &s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// job queues a job on a site (or a client group, for targetType) and sets
// its state.
func (f fixture) job(typ, targetType, targetID, userID string, state store.JobState, payload string) store.Job {
	f.t.Helper()
	ctx := context.Background()
	j := store.Job{Type: typ, TargetType: targetType, TargetID: targetID, CreatedByUserID: userID, Payload: json.RawMessage(payload)}
	if err := store.NewJobs(f.db).Enqueue(ctx, &j); err != nil {
		f.t.Fatal(err)
	}
	if state != store.JobQueued {
		if _, err := f.db.ExecContext(ctx, "UPDATE jobs SET state = ? WHERE id = ?", state, j.ID); err != nil {
			f.t.Fatal(err)
		}
		j.State = state
	}
	// Distinct creation times, for a stable order.
	time.Sleep(2 * time.Millisecond)
	return j
}

func (f fixture) event(action string, target authz.Target, userID string, result store.AuditResult, at time.Time) store.AuditEvent {
	f.t.Helper()
	e := store.AuditEvent{
		Action: action, TargetType: string(target.Type), TargetID: target.ID, ActorUserID: userID,
		Result: result, OccurredAt: at,
	}
	if err := store.NewAuditEvents(f.db).Insert(context.Background(), &e); err != nil {
		f.t.Fatal(err)
	}
	return e
}

// describe sums a change up as kind, target, and job state.
func describe(c changes.Change) string {
	var target string
	switch {
	case c.Site != nil:
		target = "site " + c.Site.ID
		if c.Site.Slug != "" {
			target = "site " + c.Site.Slug
		}
		if c.Site.Staging {
			target += " (staging)"
		}
		if c.ClientGroup != nil {
			target += " in " + c.ClientGroup.Name
		}
	case c.ClientGroup != nil:
		target = "group " + c.ClientGroup.ID
		if c.ClientGroup.Name != "" {
			target = "group " + c.ClientGroup.Name
		}
	case c.TargetType != "":
		target = c.TargetType + " " + c.TargetID
	default:
		target = "platform"
	}
	s := c.Kind + " " + target
	if c.JobState != "" {
		s += " " + string(c.JobState)
	}
	return s
}

func TestSince(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		f := fixture{t, db}
		acme := f.group("Acme")
		prod := f.site("acme", acme.ID, "", 10001)
		staging := f.site("acme-staging", acme.ID, prod.ID, 10002)
		admin, other := f.user("admin@example.com"), f.user("other@example.com")

		// Before the upgrade: none of these is listed.
		f.job("site.create", store.JobTargetSite, prod.ID, admin, store.JobSucceeded, "")
		f.job("site.delete", store.JobTargetSite, id.New(), admin, store.JobSucceeded, "")
		f.event("user.create", authz.User(other), admin, store.AuditSuccess, time.Now().Add(-time.Hour))
		f.event("client_group.delete", authz.ClientGroup(id.New()), admin, store.AuditSuccess, time.Now().Add(-time.Minute))
		since := time.Now()
		time.Sleep(2 * time.Millisecond)

		// Blocking.
		created := f.site("new", acme.ID, "", 10003)
		imported := f.site("imported", acme.ID, "", 10004)
		f.job("site.create", store.JobTargetSite, created.ID, admin, store.JobSucceeded, "")
		f.job("site.import", store.JobTargetSite, imported.ID, admin, store.JobRunning, "")
		f.job("site.clone", store.JobTargetSite, staging.ID, admin, store.JobSucceeded, "")
		deletedStaging := id.New()
		f.job("site.delete", store.JobTargetSite, deletedStaging, admin, store.JobSucceeded,
			fmt.Sprintf(`{"site":{"slug":"acme-staging-2","client_group_id":%q,"parent_site_id":%q}}`, acme.ID, prod.ID))
		oldDelete := id.New()
		f.job("site.delete", store.JobTargetSite, oldDelete, admin, store.JobFailed, "") // no site recorded; can't be undone
		f.job("site.core_update", store.JobTargetSite, prod.ID, "", store.JobSucceeded, "")
		f.job("site.go_live", store.JobTargetSite, prod.ID, admin, store.JobSucceeded, "")
		goneGroup := id.New()
		f.event("client_group.delete", authz.ClientGroup(goneGroup), admin, store.AuditSuccess, time.Now())
		f.event("client_group.offboard", authz.ClientGroup(acme.ID), admin, store.AuditSuccess, time.Now())
		// Failed jobs that rolled back, and refused requests: not listed.
		f.job("site.core_update", store.JobTargetSite, staging.ID, "", store.JobFailed, "")
		f.job("site.create", store.JobTargetSite, id.New(), admin, store.JobFailed, "")
		f.event("client_group.delete", authz.ClientGroup(acme.ID), admin, store.AuditFailure, time.Now())
		f.event("client_group.offboard", authz.ClientGroup(acme.ID), other, store.AuditDenied, time.Now())

		// Converged.
		f.job("site.template_update", store.JobTargetSite, prod.ID, "", store.JobSucceeded, "")
		f.job("site.image_update", store.JobTargetSite, staging.ID, "", store.JobRollbackFailed, "")
		update := f.job("site.update", store.JobTargetSite, prod.ID, admin, store.JobSucceeded, "")
		// The update's own request: listed as its job.
		f.event("site.update", authz.Site(prod.ID), admin, store.AuditSuccess, update.CreatedAt.Add(time.Second))
		// A settings change without a job, such as a backup schedule, on
		// the site just cloned: the clone job isn't its job.
		f.event("site.update", authz.Site(staging.ID), admin, store.AuditSuccess, time.Now())
		// A core update now, listed as its job above; the core update
		// policy, which runs none.
		coreNow := f.job("site.core_update", store.JobTargetSite, created.ID, admin, store.JobQueued, "")
		f.event("core_update.approve", authz.Site(created.ID), admin, store.AuditSuccess, coreNow.CreatedAt.Add(time.Second))
		f.event("core_update.approve", authz.ClientGroup(acme.ID), admin, store.AuditSuccess, time.Now())
		f.event("platform.settings.manage", authz.Platform(), admin, store.AuditSuccess, time.Now())

		// Lost.
		f.job("site.refresh", store.JobTargetSite, staging.ID, admin, store.JobSucceeded, "")
		f.job("site.push", store.JobTargetSite, prod.ID, admin, store.JobSucceeded, "")
		f.job("site.backup", store.JobTargetSite, prod.ID, "", store.JobSucceeded, "")
		f.job("test.unknown", store.JobTargetSite, prod.ID, "", store.JobSucceeded, "")
		user := id.New()
		f.event("user.create", authz.User(user), admin, store.AuditSuccess, time.Now())
		f.event("alert_rule.manage", authz.Target{Type: authz.TargetAlertRule, ID: "rule1"}, admin, store.AuditSuccess, time.Now())

		// Not changes, or reported from elsewhere.
		f.job("site.wp_cron", store.JobTargetSite, prod.ID, "", store.JobSucceeded, "")
		f.job("backup.repository_delete", store.JobTargetClientGroup, acme.ID, admin, store.JobQueued, "")
		f.job("rollout.pass", "platform", "core", "", store.JobSucceeded, "")
		f.event("self.login", authz.User(admin), admin, store.AuditSuccess, time.Now())
		f.event("site.view", authz.Site(prod.ID), admin, store.AuditSuccess, time.Now())
		f.event("site.create", authz.Site(created.ID), admin, store.AuditSuccess, time.Now())
		f.event("site.database.session.create", authz.Site(prod.ID), admin, store.AuditSuccess, time.Now())

		r, err := changes.Since(ctx, db, since)
		if err != nil {
			t.Fatal(err)
		}
		if !r.Since.Equal(since) {
			t.Errorf("since = %v, want %v", r.Since, since)
		}
		for _, g := range []struct {
			name string
			got  []changes.Change
			want []string
		}{
			{"blocking", r.Blocking, []string{
				"site.create site new in Acme succeeded",
				"site.import site imported in Acme running",
				"site.clone site acme-staging (staging) in Acme succeeded",
				"site.delete site acme-staging-2 (staging) in Acme succeeded",
				"site.delete site " + oldDelete + " failed",
				"site.core_update site acme in Acme succeeded",
				"site.go_live site acme in Acme succeeded",
				"client_group.delete group " + goneGroup,
				"client_group.offboard group Acme",
				"site.core_update site new in Acme queued",
			}},
			{"converged", r.Converged, []string{
				"site.template_update site acme in Acme succeeded",
				"site.image_update site acme-staging (staging) in Acme rollback_failed",
				"site.update site acme in Acme succeeded",
				"site.update site acme-staging (staging) in Acme",
				"core_update.approve group Acme",
				"platform.settings.manage platform",
			}},
			{"lost", r.Lost, []string{
				"site.refresh site acme-staging (staging) in Acme succeeded",
				"site.push site acme in Acme succeeded",
				"site.backup site acme in Acme succeeded",
				"test.unknown site acme in Acme succeeded",
				"user.create user " + user,
				"alert_rule.manage alert_rule rule1",
			}},
		} {
			var got []string
			for i, c := range g.got {
				got = append(got, describe(c))
				if c.At.Before(since) {
					t.Errorf("%s: %s at %v, before %v", g.name, describe(c), c.At, since)
				}
				if i > 0 && c.At.Before(g.got[i-1].At) {
					t.Errorf("%s: %s is listed after a later change", g.name, describe(c))
				}
				if (c.JobID == "") == (c.AuditEventID == "") {
					t.Errorf("%s: %s: job %q, audit event %q; want one", g.name, describe(c), c.JobID, c.AuditEventID)
				}
			}
			if !slices.Equal(got, g.want) {
				t.Errorf("%s:\n  got  %s\n  want %s", g.name, strings.Join(got, "\n       "), strings.Join(g.want, "\n       "))
			}
		}

		// Nothing since now.
		r, err = changes.Since(ctx, db, time.Now().Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Blocking)+len(r.Converged)+len(r.Lost) != 0 || r.Blocking == nil || r.Converged == nil || r.Lost == nil {
			t.Errorf("changes since now = %+v, want three empty lists", r)
		}
	})
}

// TestSincePages lists more changes than a page holds.
func TestSincePages(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		since := time.Now()
		const n = 450
		jobs := store.NewJobs(db)
		events := store.NewAuditEvents(db)
		for range n {
			j := store.Job{Type: "site.backup", TargetType: store.JobTargetSite, TargetID: id.New()}
			if err := jobs.Enqueue(ctx, &j); err != nil {
				t.Fatal(err)
			}
			e := store.AuditEvent{Action: "user.create", TargetType: "user", TargetID: id.New(), Result: store.AuditSuccess}
			if err := events.Insert(ctx, &e); err != nil {
				t.Fatal(err)
			}
		}
		r, err := changes.Since(ctx, db, since)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Lost) != 2*n {
			t.Errorf("%d lost changes, want %d", len(r.Lost), 2*n)
		}
	})
}

func TestReportJSON(t *testing.T) {
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	r := changes.Report{
		Since: at,
		Blocking: []changes.Change{{
			Kind: "site.delete", At: at, JobID: "j1", JobState: store.JobSucceeded,
			Site:        &changes.Site{ID: "s1", Slug: "acme-staging", Staging: true},
			ClientGroup: &changes.ClientGroup{ID: "g1", Name: "Acme"},
		}},
		Converged: []changes.Change{},
		Lost:      []changes.Change{{Kind: "user.create", At: at, AuditEventID: "e1", TargetType: "user", TargetID: "u1"}},
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"since":"2026-11-05T12:00:00Z",` +
		`"blocking":[{"kind":"site.delete","at":"2026-11-05T12:00:00Z","job_id":"j1","job_state":"succeeded",` +
		`"site":{"id":"s1","slug":"acme-staging","staging":true},"client_group":{"id":"g1","name":"Acme"}}],` +
		`"converged":[],` +
		`"lost":[{"kind":"user.create","at":"2026-11-05T12:00:00Z","audit_event_id":"e1","target_type":"user","target_id":"u1"}]}`
	if string(b) != want {
		t.Errorf("JSON:\n  got  %s\n  want %s", b, want)
	}
}
