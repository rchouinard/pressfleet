package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sites"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// logSite creates a site with a running runtime in the fake driver and
// returns it with a handler that serves the logs.
func logSite(t *testing.T, db *store.DB) (*siteEnv, http.Handler, store.Site) {
	t.Helper()
	ctx := context.Background()
	e := newSiteEnv(t, db, testPreviewBase)
	h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}, Sites: e.svc, Logs: e.prov})
	group := &store.ClientGroup{Name: "Acme"}
	if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	site, _, err := e.svc.Create(ctx, sites.NewSite{ClientGroupID: group.ID, Name: "Acme", AdminEmail: "o@example.com", PHPVersion: "8.4"})
	if err != nil {
		t.Fatal(err)
	}
	return e, h, site
}

func logLines(entries []gen.SiteLogEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Message)
	}
	return out
}

func TestSiteLogsEndpoint(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e, h, site := logSite(t, db)
		logs := "/api/v1/sites/" + site.ID + "/logs"

		// Still being created: no containers, so an empty page.
		w := do(t, h, request{authed: true, method: http.MethodGet, target: logs})
		if page := decodeJSON[gen.SiteLogList](t, w.Body.Bytes()); w.Code != http.StatusOK || page.Items == nil || len(page.Items) != 0 || page.NextCursor != nil {
			t.Fatalf("site without runtime: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)

		base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
		at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
		e.prov.AddLogs(site.ID,
			provisioner.LogEntry{Time: at(1), Component: provisioner.ComponentNginx, Stream: provisioner.Stdout, Line: `{"time":"x","status":200,"uri":"/"}`},
			provisioner.LogEntry{Time: at(2), Component: provisioner.ComponentPHP, Stream: provisioner.Stderr, Line: "PHP Warning:  Undefined variable $x"},
			provisioner.LogEntry{Time: at(3), Component: provisioner.ComponentNginx, Stream: provisioner.Stderr, Line: "2026/10/01 12:00:03 [error] 7#7: upstream timed out"},
			provisioner.LogEntry{Time: at(4), Component: provisioner.ComponentPHP, Stream: provisioner.Stderr, Line: "[01-Oct-2026 12:00:04]  [pool www] pid 9"},
			provisioner.LogEntry{Time: at(4), Component: provisioner.ComponentPHP, Stream: provisioner.Stderr, Line: "script_filename = /var/www/wordpress/index.php"},
			provisioner.LogEntry{Time: at(5), Component: provisioner.ComponentNginx, Stream: provisioner.Stdout, Line: `{"time":"y","status":503,"uri":"/down"}`},
		)
		get := func(query string) (gen.SiteLogList, *httptest.ResponseRecorder) {
			t.Helper()
			w := do(t, h, request{authed: true, method: http.MethodGet, target: logs + query})
			if w.Code != http.StatusOK {
				return gen.SiteLogList{}, w
			}
			return decodeJSON[gen.SiteLogList](t, w.Body.Bytes()), w
		}

		page, w := get("")
		if len(page.Items) != 6 || page.NextCursor != nil || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("all: %s", w.Body)
		}
		first, last := page.Items[0], page.Items[5]
		if !first.Time.Equal(at(1)) || first.Source != gen.SiteLogSourceAccess || first.Level != gen.SiteLogLevelInfo ||
			first.Component != gen.SiteLogEntryComponentNginx || first.Stream == nil || *first.Stream != gen.Stdout ||
			last.Level != gen.SiteLogLevelError || last.Source != gen.SiteLogSourceAccess {
			t.Errorf("entries = %s", w.Body)
		}

		for _, tt := range []struct {
			name, query string
			want        []string
		}{
			{"source", "?source=error", []string{"2026/10/01 12:00:03 [error] 7#7: upstream timed out"}},
			{"several sources", "?source=error&source=php", []string{"PHP Warning:  Undefined variable $x", "2026/10/01 12:00:03 [error] 7#7: upstream timed out"}},
			{"slow keeps its lines together", "?source=slow", []string{"[01-Oct-2026 12:00:04]  [pool www] pid 9", "script_filename = /var/www/wordpress/index.php"}},
			{"level", "?level=error", []string{"2026/10/01 12:00:03 [error] 7#7: upstream timed out", `{"time":"y","status":503,"uri":"/down"}`}},
			{"contains", "?q=UNDEFINED", []string{"PHP Warning:  Undefined variable $x"}},
			{"window", "?since=" + url.QueryEscape(at(2).Format(time.RFC3339)) + "&until=" + url.QueryEscape(at(4).Format(time.RFC3339)),
				[]string{"PHP Warning:  Undefined variable $x", "2026/10/01 12:00:03 [error] 7#7: upstream timed out"}},
			{"filters combine", "?source=php&level=warn&q=variable", []string{"PHP Warning:  Undefined variable $x"}},
			{"nothing matches", "?q=zzz", nil},
		} {
			t.Run(tt.name, func(t *testing.T) {
				page, w := get(tt.query)
				if w.Code != http.StatusOK {
					t.Fatalf("%d %s", w.Code, w.Body)
				}
				if got := logLines(page.Items); !slices.Equal(got, tt.want) {
					t.Errorf("lines = %q, want %q", got, tt.want)
				}
			})
		}

		// Paging walks back in time, and entries that share a time stay on
		// one page.
		var pages [][]string
		query := "?limit=2"
		for range 10 {
			page, w := get(query)
			if w.Code != http.StatusOK {
				t.Fatalf("page: %d %s", w.Code, w.Body)
			}
			pages = append(pages, logLines(page.Items))
			if page.NextCursor == nil {
				break
			}
			query = "?limit=2&cursor=" + *page.NextCursor
		}
		if len(pages) != 3 || len(pages[0]) != 3 || len(pages[1]) != 2 || len(pages[2]) != 1 {
			t.Fatalf("pages = %q", pages)
		}
		var all []string
		for _, p := range slices.Backward(pages) {
			all = append(all, p...)
		}
		if want := logLines(mustAll(t, get)); !slices.Equal(all, want) {
			t.Errorf("paged = %q, want %q", all, want)
		}

		// A cursor can't page past `since`.
		page, _ = get("?limit=1&since=" + url.QueryEscape(at(5).Format(time.RFC3339)))
		if len(page.Items) != 1 || page.NextCursor != nil {
			t.Errorf("single entry window = %+v", page)
		}

		for _, tt := range []struct {
			query string
			want  int
		}{
			{"?source=nope", http.StatusBadRequest},
			{"?level=fatal", http.StatusBadRequest},
			{"?limit=0", http.StatusBadRequest},
			{"?limit=1001", http.StatusBadRequest},
			{"?cursor=abc", http.StatusBadRequest},
			{"?cursor=-5", http.StatusBadRequest},
			{"?since=" + url.QueryEscape(at(3).Format(time.RFC3339)) + "&until=" + url.QueryEscape(at(2).Format(time.RFC3339)), http.StatusBadRequest},
		} {
			if w := do(t, h, request{authed: true, method: http.MethodGet, target: logs + tt.query}); w.Code != tt.want {
				t.Errorf("GET %s = %d %s, want %d", tt.query, w.Code, w.Body, tt.want)
			}
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/01j9zq3v6t8k4m2n5p7r9s1x3y/logs"}); w.Code != http.StatusNotFound {
			t.Errorf("unknown site: %d %s", w.Code, w.Body)
		}
		if w := do(t, h, request{method: http.MethodGet, target: logs}); w.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated: %d", w.Code)
		}
	})
}

func mustAll(t *testing.T, get func(string) (gen.SiteLogList, *httptest.ResponseRecorder)) []gen.SiteLogEntry {
	t.Helper()
	page, w := get("?limit=1000")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return page.Items
}

// Viewers may read logs, but a caller without a role may not.
func TestSiteLogsAuthorization(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e, _, site := logSite(t, db)
		e.runJobs(t)
		for _, role := range []store.Role{store.RoleViewer, store.RoleOperator} {
			h := newHandler(t, api.Options{DB: db, Sites: e.svc, Logs: e.prov, Authenticator: &fakeAuth{principal: &auth.Principal{
				UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: role, Method: auth.MethodSession, SessionID: "test"}}})
			for _, path := range []string{"/logs", "/job-logs"} {
				if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + site.ID + path}); w.Code != http.StatusOK {
					t.Errorf("%s reading %s: %d %s", role, path, w.Code, w.Body)
				}
			}
		}
		// A read-only token of a viewer reads too: it changes nothing.
		h := newHandler(t, api.Options{DB: db, Sites: e.svc, Logs: e.prov, Authenticator: &fakeAuth{principal: &auth.Principal{
			UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodToken, TokenID: "t", Scope: store.ScopeReadOnly}}})
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/" + site.ID + "/logs"}); w.Code != http.StatusOK {
			t.Errorf("read-only token: %d %s", w.Code, w.Body)
		}
	})
}

func TestSiteJobLogsEndpoint(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e, h, site := logSite(t, db)
		e.runJobs(t)
		jobs := store.NewJobs(db)
		page, err := jobs.List(ctx, store.JobFilter{TargetType: store.JobTargetSite, TargetID: site.ID})
		if err != nil || len(page.Jobs) == 0 {
			t.Fatalf("site jobs = %+v, %v", page, err)
		}
		job := page.Jobs[0]
		steps, err := jobs.Steps(ctx, job.ID)
		if err != nil || len(steps) == 0 {
			t.Fatalf("steps = %+v, %v", steps, err)
		}
		base := time.Now().UTC()
		for i, l := range []struct {
			step       int
			level, msg string
		}{{-1, "info", "job line"}, {steps[0].Seq, "warn", "step line"}, {steps[0].Seq, "error", "step failure"}} {
			if err := jobs.AppendLog(ctx, job.ID, l.step, l.level, l.msg, base.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
		target := "/api/v1/sites/" + site.ID + "/job-logs"

		w := do(t, h, request{authed: true, method: http.MethodGet, target: target + "?limit=2"})
		if w.Code != http.StatusOK {
			t.Fatalf("list: %d %s", w.Code, w.Body)
		}
		p1 := decodeJSON[gen.SiteJobLogList](t, w.Body.Bytes())
		if len(p1.Items) != 2 || p1.NextCursor == nil || p1.Items[0].Message != "step failure" || p1.Items[0].Level != gen.SiteJobLogEntryLevelError ||
			p1.Items[0].Step == nil || *p1.Items[0].Step != steps[0].Name || p1.Items[0].JobID != job.ID || p1.Items[0].JobType != job.Type ||
			p1.Items[1].Message != "step line" {
			t.Errorf("first page = %s", w.Body)
		}
		w = do(t, h, request{authed: true, method: http.MethodGet, target: target + "?limit=2&cursor=" + *p1.NextCursor})
		p2 := decodeJSON[gen.SiteJobLogList](t, w.Body.Bytes())
		var rest []string
		for _, l := range p2.Items {
			rest = append(rest, l.Message)
		}
		if len(rest) == 0 || rest[0] != "job line" || !slices.Contains(rest, "job line") {
			t.Errorf("second page = %s", w.Body)
		}
		for _, l := range p2.Items {
			if l.Message == "job line" && l.Step != nil {
				t.Errorf("line outside a step has step %q", *l.Step)
			}
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: target + "?cursor=x"}); w.Code != http.StatusBadRequest {
			t.Errorf("bad cursor: %d %s", w.Code, w.Body)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/sites/01j9zq3v6t8k4m2n5p7r9s1x3y/job-logs"}); w.Code != http.StatusNotFound {
			t.Errorf("unknown site: %d %s", w.Code, w.Body)
		}
	})
}

// sseEvent is one parsed server-sent event.
type sseEvent struct{ id, event, data string }

// readEvents parses events from the stream into a channel until it ends.
func readEvents(r *bufio.Reader) <-chan sseEvent {
	out := make(chan sseEvent, 16)
	go func() {
		defer close(out)
		var ev sseEvent
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if ev != (sseEvent{}) {
					out <- ev
					ev = sseEvent{}
				}
			case strings.HasPrefix(line, ":"):
			case strings.HasPrefix(line, "id: "):
				ev.id = line[4:]
			case strings.HasPrefix(line, "event: "):
				ev.event = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.data = line[6:]
			}
		}
	}()
	return out
}

func nextEvent(t *testing.T, events <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-events:
		if !ok {
			t.Fatal("stream ended")
		}
		return ev
	case <-time.After(10 * time.Second):
		t.Fatal("no event within 10s")
	}
	return sseEvent{}
}

func TestStreamSiteLogs(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e, h, site := logSite(t, db)
		stream := "/api/v1/sites/" + site.ID + "/logs/stream"
		// A site with no containers can't be followed.
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: stream}); w.Code != http.StatusConflict {
			t.Errorf("site without runtime: %d %s", w.Code, w.Body)
		}
		e.runJobs(t)

		base := time.Now().Add(-time.Minute).UTC()
		for i := range 3 {
			e.prov.AddLogs(site.ID, provisioner.LogEntry{Time: base.Add(time.Duration(i) * time.Second), Component: provisioner.ComponentPHP, Line: "PHP Warning:  old " + strconv.Itoa(i)})
		}
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close) // runs after the stream bodies are closed
		open := func(query string, lastID string) (*http.Response, <-chan sseEvent) {
			t.Helper()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+stream+query, nil)
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

		resp, events := open("?tail=2&level=warn", "")
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("response = %d %v", resp.StatusCode, resp.Header)
		}
		var lastID string
		for _, want := range []string{"PHP Warning:  old 1", "PHP Warning:  old 2"} {
			ev := nextEvent(t, events)
			var entry gen.SiteLogEntry
			if err := json.Unmarshal([]byte(ev.data), &entry); err != nil {
				t.Fatalf("event data %q: %v", ev.data, err)
			}
			if ev.event != "log" || entry.Message != want || entry.Source != gen.SiteLogSourcePhp || ev.id == "" {
				t.Errorf("backlog event = %+v, entry %+v", ev, entry)
			}
			lastID = ev.id
		}
		// Entries written afterwards arrive live, filtered.
		e.prov.AddLogs(site.ID,
			provisioner.LogEntry{Time: time.Now().UTC(), Component: provisioner.ComponentPHP, Line: "PHP Notice:  below the level"},
			provisioner.LogEntry{Time: time.Now().UTC().Add(time.Millisecond), Component: provisioner.ComponentPHP, Line: "PHP Warning:  live"},
		)
		ev := nextEvent(t, events)
		if !strings.Contains(ev.data, `"message":"PHP Warning:  live"`) {
			t.Errorf("live event = %+v", ev)
		}

		// A reconnecting client continues after the entry it has.
		_, events = open("?level=warn", lastID)
		ev = nextEvent(t, events)
		if !strings.Contains(ev.data, `"message":"PHP Warning:  live"`) {
			t.Errorf("resumed event = %+v", ev)
		}

		for _, tt := range []struct {
			query, lastID string
		}{{"?source=nope", ""}, {"?tail=1001", ""}, {"", "not-a-time"}} {
			req := httptest.NewRequest(http.MethodGet, stream+tt.query, nil)
			req.Header.Set("X-Test-Auth", "ok")
			if tt.lastID != "" {
				req.Header.Set("Last-Event-ID", tt.lastID)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("GET %s (Last-Event-ID %q) = %d %s", tt.query, tt.lastID, w.Code, w.Body)
			}
		}
	})
}
