package metrics_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
)

func newClient(t *testing.T, h http.Handler, mod func(*metrics.Options)) *metrics.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	opts := metrics.Options{URL: srv.URL, Backoff: time.Millisecond, Timeout: 2 * time.Second}
	if mod != nil {
		mod(&opts)
	}
	c, err := metrics.NewClient(opts)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewClientRejectsBadURL(t *testing.T) {
	for _, u := range []string{"", "victoria-metrics:8428", "ftp://x"} {
		if _, err := metrics.NewClient(metrics.Options{URL: u}); err == nil {
			t.Errorf("NewClient(%q) succeeded", u)
		}
	}
}

func TestWriteFormatAndBatching(t *testing.T) {
	var bodies []string
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/import/prometheus" || r.Method != http.MethodPost {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		w.WriteHeader(http.StatusNoContent)
	}), func(o *metrics.Options) { o.BatchSize = 2 })

	at := time.UnixMilli(1700000000123)
	samples := []metrics.Sample{
		{Name: "pressfleet_a", Labels: map[string]string{"z": "1", "a": "x\"y\\z\nw"}, Value: 1.5, Time: at},
		{Name: "pressfleet_b", Value: 2, Time: at},
		{Name: "pressfleet_c", Value: 3, Time: at},
	}
	if err := c.Write(context.Background(), samples); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("got %d requests, want 2", len(bodies))
	}
	want := "pressfleet_a{a=\"x\\\"y\\\\z\\nw\",z=\"1\"} 1.5 1700000000123\npressfleet_b 2 1700000000123\n"
	if bodies[0] != want {
		t.Errorf("first body = %q, want %q", bodies[0], want)
	}
	if bodies[1] != "pressfleet_c 3 1700000000123\n" {
		t.Errorf("second body = %q", bodies[1])
	}
}

func TestWriteDropsInvalidSamples(t *testing.T) {
	var body string
	c := newClient(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}), nil)
	err := c.Write(context.Background(), []metrics.Sample{
		{Name: "bad name", Value: 1},
		{Name: "pressfleet_ok", Value: 1},
		{Name: "pressfleet_label", Labels: map[string]string{"__x": "1"}, Value: 1},
	})
	if !errors.Is(err, metrics.ErrInvalidSample) {
		t.Fatalf("err = %v, want ErrInvalidSample", err)
	}
	if !strings.HasPrefix(body, "pressfleet_ok 1 ") || strings.Count(body, "\n") != 1 {
		t.Errorf("body = %q, want only the valid sample", body)
	}
}

func TestWriteRetries(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), nil)
	if err := c.Write(context.Background(), []metrics.Sample{{Name: "pressfleet_a", Value: 1}}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3", calls.Load())
	}
}

func TestWriteGivesUp(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}), func(o *metrics.Options) { o.MaxAttempts = 2 })
	err := c.Write(context.Background(), []metrics.Sample{{Name: "pressfleet_a", Value: 1}})
	if !errors.Is(err, metrics.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

func TestWriteClientErrorIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "bad", http.StatusBadRequest)
	}), nil)
	err := c.Write(context.Background(), []metrics.Sample{{Name: "pressfleet_a", Value: 1}})
	if !errors.Is(err, metrics.ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
}

func TestWriteTimeout(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Reading the body lets the server notice the client hanging up.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}), func(o *metrics.Options) { o.Timeout = 20 * time.Millisecond; o.MaxAttempts = 2 })
	err := c.Write(context.Background(), []metrics.Sample{{Name: "pressfleet_a", Value: 1}})
	if !errors.Is(err, metrics.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
}

func TestWriteContextCanceled(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}), func(o *metrics.Options) { o.Backoff = time.Hour })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := c.Write(ctx, []metrics.Sample{{Name: "pressfleet_a", Value: 1}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func TestQuery(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = r.ParseForm()
		if r.Form.Get("query") != "up" || r.Form.Get("time") != "1700000000.000" {
			t.Errorf("form = %v", r.Form)
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"__name__":"up","site_id":"s1"},"value":[1700000000,"1"]},
			{"metric":{"site_id":"s2"},"value":[1700000000.5,"NaN"]}]}}`)
	}), nil)
	res, err := c.Query(context.Background(), "up", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if res.Type != metrics.Vector || len(res.Series) != 2 {
		t.Fatalf("result = %+v", res)
	}
	s := res.Series[0]
	if s.Labels["site_id"] != "s1" || len(s.Points) != 1 || s.Points[0].Value != 1 || !s.Points[0].Time.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("series 0 = %+v", s)
	}
	if v := res.Series[1].Points[0].Value; v == v {
		t.Errorf("series 1 value = %v, want NaN", v)
	}
}

func TestQueryInstantWithoutTime(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if _, ok := r.Form["time"]; ok {
			t.Errorf("time sent: %v", r.Form)
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"scalar","result":[1700000000,"3"]}}`)
	}), nil)
	res, err := c.Query(context.Background(), "1+2", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != 1 || res.Series[0].Points[0].Value != 3 {
		t.Errorf("result = %+v", res)
	}
}

func TestQueryRange(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = r.ParseForm()
		if r.Form.Get("start") != "100.000" || r.Form.Get("end") != "200.000" || r.Form.Get("step") != "15" {
			t.Errorf("form = %v", r.Form)
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"site_id":"s1"},"values":[[100,"1"],[115,"2"]]}]}}`)
	}), nil)
	res, err := c.QueryRange(context.Background(), "up", time.Unix(100, 0), time.Unix(200, 0), 15*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Type != metrics.Matrix || len(res.Series) != 1 || len(res.Series[0].Points) != 2 || res.Series[0].Points[1].Value != 2 {
		t.Errorf("result = %+v", res)
	}
	if _, err := c.QueryRange(context.Background(), "up", time.Unix(100, 0), time.Unix(200, 0), 0); !errors.Is(err, metrics.ErrQuery) {
		t.Errorf("zero step: err = %v, want ErrQuery", err)
	}
}

func TestQueryError(t *testing.T) {
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"status":"error","errorType":"bad_data","error":"cannot parse"}`)
	}), nil)
	_, err := c.Query(context.Background(), "up{", time.Time{})
	if !errors.Is(err, metrics.ErrQuery) || !strings.Contains(err.Error(), "cannot parse") {
		t.Errorf("err = %v", err)
	}
}
