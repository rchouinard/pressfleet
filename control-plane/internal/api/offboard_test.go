package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/offboard"
	"github.com/rchouinard/pressfleet/control-plane/internal/export"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// fakeRepositories stands in for repository.Manager: deleting forgets the
// group's repository location, as the real one does last.
type fakeRepositories struct {
	db      *store.DB
	deleted []string
}

func (f *fakeRepositories) Delete(ctx context.Context, groupID string) error {
	f.deleted = append(f.deleted, groupID)
	return store.NewClientGroups(f.db).SetBackupRepository(ctx, groupID, "")
}

func TestOffboardEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		repos := &fakeRepositories{db: db}
		svc, err := offboard.New(offboard.Options{DB: db, Repositories: repos, Logger: storetest.Logger()})
		if err != nil {
			t.Fatal(err)
		}
		// The jobs record who queued them.
		if _, err := db.ExecContext(ctx, "INSERT INTO users (id, email, password_hash, role, created_at, updated_at) VALUES (?, ?, 'x', 'admin', ?, ?)",
			"01j9zq3v6t8k4m2n5p7r9s1x3y", "admin@example.com", time.Now(), time.Now()); err != nil {
			t.Fatal(err)
		}
		authn := &fakeAuth{}
		h := newHandler(t, api.Options{DB: db, Authenticator: authn, Offboarding: svc})
		pool := jobs.NewPool(db, jobs.Options{Logger: storetest.Logger(), Backoff: func(int) time.Duration { return 0 }})
		if err := pool.Register(svc.Handler()); err != nil {
			t.Fatal(err)
		}
		runJobs := func() {
			t.Helper()
			for range 20 {
				ran, err := pool.RunNext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !ran {
					return
				}
			}
			t.Fatal("jobs never finished")
		}
		groups := store.NewClientGroups(db)
		newGroup := func(name string, withRepository bool) string {
			t.Helper()
			g := store.ClientGroup{Name: name}
			if err := groups.Create(ctx, &g); err != nil {
				t.Fatal(err)
			}
			if withRepository {
				if err := groups.SetBackupRepository(ctx, g.ID, "/data/backups/repositories/"+g.ID); err != nil {
					t.Fatal(err)
				}
			}
			return g.ID
		}
		repoTarget := func(groupID string) string { return "/api/v1/client-groups/" + groupID + "/backup-repository" }
		groupTarget := func(groupID string) string {
			return "/api/v1/client-groups/" + groupID + "?delete_backup_repository=true"
		}
		conflict := func(target, want string) {
			t.Helper()
			w := do(t, h, request{authed: true, method: http.MethodDelete, target: target})
			if p := decodeProblem(t, w); w.Code != http.StatusConflict || !strings.Contains(p.Detail, want) {
				t.Errorf("DELETE %s: %d %+v, want 409 mentioning %q", target, w.Code, p, want)
			}
		}

		// Nothing to delete.
		bare := newGroup("Bare", false)
		conflict(repoTarget(bare), "no backup repository")
		if w := do(t, h, request{authed: true, method: http.MethodDelete, target: repoTarget(id.New())}); w.Code != http.StatusNotFound {
			t.Errorf("unknown group: %d %s", w.Code, w.Body)
		}

		// Not while the group has sites.
		acme := newGroup("Acme", true)
		siteID, now := id.New(), time.Now()
		if _, err := db.ExecContext(ctx, `INSERT INTO sites
			(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
			VALUES (?, ?, 'Site', 'site', 'running', 'site.preview.example', '8.4', '6.9.1', 20000, ?, ?)`,
			siteID, acme, now, now); err != nil {
			t.Fatal(err)
		}
		conflict(repoTarget(acme), "still has sites")
		conflict(groupTarget(acme), "still has 1 site")
		if _, err := db.ExecContext(ctx, "DELETE FROM sites WHERE id = ?", siteID); err != nil {
			t.Fatal(err)
		}

		// Not while an export from the group's backups is in flight.
		exp, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: export.BackupJobType, TargetType: store.JobTargetClientGroup, TargetID: acme})
		if err != nil {
			t.Fatal(err)
		}
		conflict(repoTarget(acme), exp.ID)
		if _, err := db.ExecContext(ctx, "UPDATE jobs SET state = 'succeeded' WHERE id = ?", exp.ID); err != nil {
			t.Fatal(err)
		}

		// Operators can't offboard, with or without the group.
		authn.principal = &auth.Principal{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3z", Role: store.RoleOperator, Method: auth.MethodSession, SessionID: "op"}
		for _, target := range []string{repoTarget(acme), groupTarget(acme)} {
			if w := do(t, h, request{authed: true, method: http.MethodDelete, target: target}); w.Code != http.StatusForbidden {
				t.Errorf("operator DELETE %s: %d %s", target, w.Code, w.Body)
			}
		}
		authn.principal = nil

		// The repository alone.
		w := do(t, h, request{authed: true, method: http.MethodDelete, target: repoTarget(acme)})
		if w.Code != http.StatusAccepted {
			t.Fatalf("delete repository: %d %s", w.Code, w.Body)
		}
		accepted := decodeJSON[gen.JobAccepted](t, w.Body.Bytes())
		if accepted.Job.Type != offboard.DeleteJobType || accepted.Job.Target.Type != gen.JobTargetTypeClientGroup || accepted.Job.Target.ID != acme {
			t.Errorf("job = %+v", accepted.Job)
		}
		if loc := w.Header().Get("Location"); loc != "/api/v1/jobs/"+accepted.Job.ID {
			t.Errorf("Location = %q", loc)
		}
		job, err := store.NewJobs(db).Get(ctx, accepted.Job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if job.LockKey != "repo:"+acme || strings.Contains(string(job.Payload), "delete_client_group") {
			t.Errorf("queued job: lock %q, payload %s", job.LockKey, job.Payload)
		}
		runJobs()
		g, err := groups.Get(ctx, acme)
		if err != nil {
			t.Fatalf("the group went with its repository: %v", err)
		}
		if g.BackupRepository != "" || !slices.Equal(repos.deleted, []string{acme}) {
			t.Errorf("after the job: repository %q, deleted %v", g.BackupRepository, repos.deleted)
		}
		auditDetails(t, db, acme, accepted.Job.ID, false)

		// The group with its repository: a job deletes both.
		globex := newGroup("Globex", true)
		w = do(t, h, request{authed: true, method: http.MethodDelete, target: groupTarget(globex)})
		if w.Code != http.StatusAccepted {
			t.Fatalf("delete group with its repository: %d %s", w.Code, w.Body)
		}
		accepted = decodeJSON[gen.JobAccepted](t, w.Body.Bytes())
		runJobs()
		if j, err := store.NewJobs(db).Get(ctx, accepted.Job.ID); err != nil || j.State != store.JobSucceeded {
			t.Fatalf("job: %+v, %v", j, err)
		}
		if _, err := groups.Get(ctx, globex); err == nil {
			t.Error("the group still exists")
		}
		auditDetails(t, db, globex, accepted.Job.ID, true)

		// Without a repository the flag deletes the group at once.
		if w := do(t, h, request{authed: true, method: http.MethodDelete, target: groupTarget(bare)}); w.Code != http.StatusNoContent {
			t.Errorf("delete group without a repository: %d %s", w.Code, w.Body)
		}
	})
}

// auditDetails checks the offboarding's audit event.
func auditDetails(t *testing.T, db *store.DB, groupID, jobID string, withGroup bool) {
	t.Helper()
	events, err := store.NewAuditEvents(db).List(context.Background(), store.AuditFilter{TargetType: "client_group", TargetID: groupID})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events.Events {
		if e.Action == "client_group.offboard" && e.Result == store.AuditSuccess && e.Details["job_id"] == jobID {
			if got := e.Details["delete_backup_repository"] == "true"; got != withGroup {
				d, _ := json.Marshal(e.Details)
				t.Errorf("audit details = %s", d)
			}
			return
		}
	}
	t.Errorf("no successful client_group.offboard audit event for job %s: %+v", jobID, events.Events)
}

func newOffboarding(t *testing.T, db *store.DB) *offboard.Service {
	t.Helper()
	svc, err := offboard.New(offboard.Options{DB: db, Repositories: &fakeRepositories{db: db}, Logger: storetest.Logger()})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}
