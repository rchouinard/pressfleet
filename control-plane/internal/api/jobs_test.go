package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func decodeJSON[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("decoding %s: %v", body, err)
	}
	return v
}

func TestJobsEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		pool := jobs.NewPool(db, jobs.Options{Logger: storetest.Logger()})
		err := pool.Register(jobs.Handler{Type: "site.create", Steps: []jobs.Step{
			{Name: "create_database", Do: func(_ context.Context, r *jobs.Run) error {
				r.Logf("created database")
				return r.SetResult("database", "site_x")
			}},
			{Name: "install_wordpress", Do: func(context.Context, *jobs.Run) error {
				return jobs.Permanent(errors.New("wp core install failed"))
			}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		site := id.New()
		failed, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: "site.create", TargetType: "site", TargetID: site})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.RunNext(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // a later ID timestamp, for a stable newest-first order
		queued, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: "site.update", TargetType: "site", TargetID: site})
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
		other, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: "site.update", TargetType: "client_group", TargetID: id.New()})
		if err != nil {
			t.Fatal(err)
		}

		h := newHandler(t, api.Options{DB: db})

		// Get: steps with their logs.
		w := do(t, h, request{method: http.MethodGet, target: "/api/v1/jobs/" + failed.ID})
		if w.Code != http.StatusOK {
			t.Fatalf("get: %d %s", w.Code, w.Body)
		}
		j := decodeJSON[gen.Job](t, w.Body.Bytes())
		if j.ID != failed.ID || j.State != gen.JobStateFailed || j.Error == nil || *j.Error != "install_wordpress: wp core install failed" ||
			j.Target.Type != gen.JobTargetTypeSite || j.Target.ID != site || j.StartedAt == nil || j.FinishedAt == nil ||
			j.Result["database"] != "site_x" || j.MaxAttempts != store.DefaultMaxAttempts {
			t.Errorf("job = %s", w.Body)
		}
		if len(j.Steps) != 2 || j.Steps[0].Name != "create_database" || j.Steps[0].State != gen.JobStepStateSucceeded ||
			j.Steps[1].State != gen.JobStepStateFailed || j.Steps[0].Log == nil || len(*j.Steps[0].Log) != 1 ||
			(*j.Steps[0].Log)[0].Message != "created database" || j.Steps[1].Log == nil || len(*j.Steps[1].Log) != 1 ||
			(*j.Steps[1].Log)[0].Level != gen.JobLogEntryLevelError {
			t.Errorf("steps = %s", w.Body)
		}
		if w := do(t, h, request{method: http.MethodGet, target: "/api/v1/jobs/" + id.New()}); w.Code != http.StatusNotFound {
			t.Errorf("get missing: %d %s", w.Code, w.Body)
		}

		// List: newest first, paged, without logs.
		var got []string
		target := "/api/v1/jobs?limit=2"
		for page := 0; ; page++ {
			w := do(t, h, request{method: http.MethodGet, target: target})
			if w.Code != http.StatusOK {
				t.Fatalf("list: %d %s", w.Code, w.Body)
			}
			l := decodeJSON[gen.JobList](t, w.Body.Bytes())
			for _, j := range l.Items {
				got = append(got, j.ID)
				for _, s := range j.Steps {
					if s.Log != nil {
						t.Errorf("list includes step logs: %s", w.Body)
					}
				}
			}
			if l.NextCursor == nil {
				break
			}
			if page > 2 {
				t.Fatal("too many pages")
			}
			target = "/api/v1/jobs?limit=2&cursor=" + *l.NextCursor
		}
		if len(got) != 3 || got[0] != other.ID || got[1] != queued.ID || got[2] != failed.ID {
			t.Errorf("list = %v", got)
		}

		for _, tt := range []struct {
			query string
			want  []string
		}{
			{"state=queued", []string{other.ID, queued.ID}},
			{"state=failed", []string{failed.ID}},
			{"target_type=site&target_id=" + site, []string{queued.ID, failed.ID}},
			{"target_type=client_group", []string{other.ID}},
		} {
			w := do(t, h, request{method: http.MethodGet, target: "/api/v1/jobs?" + tt.query})
			if w.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", tt.query, w.Code, w.Body)
			}
			l := decodeJSON[gen.JobList](t, w.Body.Bytes())
			var ids []string
			for _, j := range l.Items {
				ids = append(ids, j.ID)
			}
			if len(ids) != len(tt.want) || (len(ids) > 0 && ids[0] != tt.want[0]) || l.NextCursor != nil {
				t.Errorf("%s: %v, want %v", tt.query, ids, tt.want)
			}
		}

		if w := do(t, h, request{method: http.MethodGet, target: "/api/v1/jobs?cursor=bogus"}); w.Code != http.StatusBadRequest {
			t.Errorf("bad cursor: %d %s", w.Code, w.Body)
		}
	})
}
