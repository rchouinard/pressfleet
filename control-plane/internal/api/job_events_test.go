package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// jobEventLog is the JobEventLogEntry schema, which gen leaves out.
type jobEventLog struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Step    string    `json:"step"`
}

// jobEvent is a parsed job event stream event.
type jobEvent struct {
	sseEvent
	job *gen.Job
	log *jobEventLog
}

func parseJobEvent(t *testing.T, ev sseEvent) jobEvent {
	t.Helper()
	out := jobEvent{sseEvent: ev}
	switch ev.event {
	case "job":
		out.job = new(gen.Job)
		if err := json.Unmarshal([]byte(ev.data), out.job); err != nil {
			t.Fatalf("job event %q: %v", ev.data, err)
		}
	case "log":
		out.log = new(jobEventLog)
		if err := json.Unmarshal([]byte(ev.data), out.log); err != nil {
			t.Fatalf("log event %q: %v", ev.data, err)
		}
	default:
		t.Fatalf("unexpected event %+v", ev)
	}
	return out
}

// streamEnds waits for the server to end the stream.
func streamEnds(t *testing.T, events <-chan sseEvent) {
	t.Helper()
	select {
	case ev, ok := <-events:
		if ok {
			t.Fatalf("event %+v after the end", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream did not end within 10s")
	}
}

// openJobEvents opens a job's event stream on srv.
func openJobEvents(t *testing.T, srv *httptest.Server, jobID, lastID string) (*http.Response, <-chan sseEvent) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/api/v1/jobs/"+jobID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Test-Auth", "ok")
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp, readEvents(bufio.NewReader(resp.Body))
}

func TestStreamJobEvents(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		// Only the pool's notifications can move the stream along in time.
		api.SetJobEventPoll(t, time.Hour)
		changes := jobs.NewChanges()
		pool := jobs.NewPool(db, jobs.Options{Logger: storetest.Logger(), Changes: changes})
		release := make(chan struct{})
		released := false
		releaseStep := func() {
			if !released {
				released = true
				close(release)
			}
		}
		defer releaseStep()
		if err := pool.Register(jobs.Handler{Type: "site.create", Steps: []jobs.Step{
			{Name: "first", Do: func(ctx context.Context, r *jobs.Run) error {
				r.Logf("one")
				select {
				case <-release:
				case <-ctx.Done():
				}
				return nil
			}},
			{Name: "second", Do: func(_ context.Context, r *jobs.Run) error {
				r.Warnf("two")
				return nil
			}},
		}}); err != nil {
			t.Fatal(err)
		}
		job, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: "site.create", TargetType: "site", TargetID: id.New()})
		if err != nil {
			t.Fatal(err)
		}

		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, JobChanges: changes})
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close) // runs after the stream bodies are closed

		resp, events := openJobEvents(t, srv, job.ID, "")
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("response = %d %v", resp.StatusCode, resp.Header)
		}
		first := parseJobEvent(t, nextEvent(t, events))
		if first.job == nil || first.job.ID != job.ID || first.job.State != gen.JobStateQueued || first.id != "" {
			t.Fatalf("first event = %+v", first)
		}

		ran := make(chan error, 1)
		go func() {
			_, err := pool.RunNext(ctx)
			ran <- err
		}()

		// While the first step runs, the stream shows it running and its
		// log line, without waiting for a poll.
		var logs []jobEvent
		var last *gen.Job
		for last == nil || len(logs) == 0 || len(last.Steps) != 2 || last.Steps[0].State != gen.JobStepStateRunning {
			ev := parseJobEvent(t, nextEvent(t, events))
			if ev.log != nil {
				logs = append(logs, ev)
			} else {
				last = ev.job
			}
		}
		if last.State != gen.JobStateRunning || last.Steps[1].State != gen.JobStepStatePending || last.Steps[0].Log != nil {
			t.Errorf("running job = %+v", last)
		}

		// Once the job finishes, its last log line comes before the job
		// event that shows it, and the stream ends.
		releaseStep()
		var final jobEvent
		for final.job == nil || final.job.State != gen.JobStateSucceeded {
			final = parseJobEvent(t, nextEvent(t, events))
			if final.log != nil {
				logs = append(logs, final)
			}
		}
		streamEnds(t, events)
		if err := <-ran; err != nil {
			t.Fatal(err)
		}
		if len(final.job.Steps) != 2 || final.job.Steps[1].State != gen.JobStepStateSucceeded || final.job.FinishedAt == nil {
			t.Errorf("final job = %+v", final.job)
		}
		if len(logs) != 2 ||
			logs[0].log.Message != "one" || logs[0].log.Step != "first" || logs[0].log.Level != "info" || logs[0].log.Time.IsZero() ||
			logs[1].log.Message != "two" || logs[1].log.Step != "second" || logs[1].log.Level != "warn" {
			t.Fatalf("logs = %+v", logs)
		}
		id0, _ := strconv.ParseInt(logs[0].id, 10, 64)
		id1, _ := strconv.ParseInt(logs[1].id, 10, 64)
		if id0 <= 0 || id1 <= id0 {
			t.Errorf("log event IDs = %q, %q", logs[0].id, logs[1].id)
		}

		// A finished job: the log, then the job, then the end. A client
		// that reconnects gets only the lines after the one it has.
		for _, tt := range []struct {
			lastID string
			want   []string
		}{
			{"", []string{"log:one", "log:two", "job:succeeded"}},
			{logs[0].id, []string{"log:two", "job:succeeded"}},
			{logs[1].id, []string{"job:succeeded"}},
		} {
			_, events := openJobEvents(t, srv, job.ID, tt.lastID)
			var got []string
			for range tt.want {
				ev := parseJobEvent(t, nextEvent(t, events))
				if ev.log != nil {
					got = append(got, "log:"+ev.log.Message)
				} else {
					got = append(got, "job:"+string(ev.job.State))
				}
			}
			streamEnds(t, events)
			if len(got) != len(tt.want) {
				t.Fatalf("Last-Event-ID %q: %v, want %v", tt.lastID, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("Last-Event-ID %q: %v, want %v", tt.lastID, got, tt.want)
					break
				}
			}
		}

		for _, tt := range []struct {
			name, jobID, lastID string
			want                int
		}{
			{"unknown job", id.New(), "", http.StatusNotFound},
			{"invalid Last-Event-ID", job.ID, "not-a-number", http.StatusBadRequest},
			{"negative Last-Event-ID", job.ID, "-1", http.StatusBadRequest},
		} {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+tt.jobID+"/events", nil)
			req.Header.Set("X-Test-Auth", "ok")
			if tt.lastID != "" {
				req.Header.Set("Last-Event-ID", tt.lastID)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Errorf("%s: %d %s, want %d", tt.name, w.Code, w.Body, tt.want)
			}
		}
	})
}

// TestStreamJobEventsPolls checks that a stream notices changes that no
// notification announces, such as those of another process.
func TestStreamJobEventsPolls(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		api.SetJobEventPoll(t, 20*time.Millisecond)
		pool := jobs.NewPool(db, jobs.Options{Logger: storetest.Logger()})
		if err := pool.Register(jobs.Handler{Type: "site.update", Steps: []jobs.Step{
			{Name: "only", Do: func(_ context.Context, r *jobs.Run) error {
				r.Logf("done")
				return nil
			}},
		}}); err != nil {
			t.Fatal(err)
		}
		job, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: "site.update", TargetType: "site", TargetID: id.New()})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}}))
		t.Cleanup(srv.Close)

		_, events := openJobEvents(t, srv, job.ID, "")
		if ev := parseJobEvent(t, nextEvent(t, events)); ev.job == nil || ev.job.State != gen.JobStateQueued {
			t.Fatalf("first event = %+v", ev)
		}
		if _, err := pool.RunNext(ctx); err != nil {
			t.Fatal(err)
		}
		var sawLog bool
		for {
			ev := parseJobEvent(t, nextEvent(t, events))
			if ev.log != nil {
				sawLog = ev.log.Message == "done"
				continue
			}
			if ev.job.State == gen.JobStateSucceeded {
				break
			}
		}
		streamEnds(t, events)
		if !sawLog {
			t.Error("the log line was not sent")
		}
	})
}
