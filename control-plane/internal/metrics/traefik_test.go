package metrics_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
)

const traefikBody = `# HELP traefik_service_requests_total How many HTTP requests processed on a service.
# TYPE traefik_service_requests_total counter
traefik_service_requests_total{code="200",method="GET",protocol="http",service="pf-site1@docker"} 10
traefik_service_requests_total{code="200",method="POST",protocol="http",service="pf-site1@docker"} 5
traefik_service_requests_total{code="404",method="GET",protocol="http",service="pf-site1@docker"} 2
traefik_service_requests_total{code="503",method="GET",protocol="http",service="pf-site1@docker"} 1
traefik_service_requests_total{code="200",method="GET",protocol="http",service="pf-site2@docker"} 7
traefik_service_requests_total{code="200",method="GET",protocol="http",service="pf-gone@docker"} 99
traefik_service_requests_total{code="200",method="GET",protocol="http",service="pf-dashboard@docker"} 99
# TYPE traefik_service_request_duration_seconds histogram
traefik_service_request_duration_seconds_bucket{code="200",method="GET",protocol="http",service="pf-site1@docker",le="0.1"} 8
traefik_service_request_duration_seconds_bucket{code="404",method="GET",protocol="http",service="pf-site1@docker",le="0.1"} 2
traefik_service_request_duration_seconds_bucket{code="200",method="GET",protocol="http",service="pf-site1@docker",le="+Inf"} 15
traefik_service_request_duration_seconds_bucket{code="404",method="GET",protocol="http",service="pf-site1@docker",le="+Inf"} 3
traefik_service_request_duration_seconds_sum{code="200",method="GET",protocol="http",service="pf-site1@docker"} 3.5
traefik_service_request_duration_seconds_sum{code="404",method="GET",protocol="http",service="pf-site1@docker"} 0.25
traefik_service_request_duration_seconds_count{code="200",method="GET",protocol="http",service="pf-site1@docker"} 15
traefik_service_request_duration_seconds_count{code="404",method="GET",protocol="http",service="pf-site1@docker"} 3
go_goroutines 12
`

func traefikTargets() []metrics.Target {
	return []metrics.Target{
		{SiteID: "site1", ClientGroupID: "g1"},
		{SiteID: "site2", ClientGroupID: "g1"},
		{SiteID: "site3", ClientGroupID: "g2"},
	}
}

func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// find returns the value of the sample named name for site with the label
// pairs, and whether there is one.
func find(samples []metrics.Sample, name, site string, kv ...string) (float64, bool) {
next:
	for _, s := range samples {
		if s.Name != name || s.Labels["site_id"] != site {
			continue
		}
		for i := 0; i < len(kv); i += 2 {
			if s.Labels[kv[i]] != kv[i+1] {
				continue next
			}
		}
		return s.Value, true
	}
	return 0, false
}

func TestTraefikCollector(t *testing.T) {
	srv := serve(t, http.StatusOK, traefikBody)
	c := metrics.TraefikCollector{URL: srv.URL}
	if c.Name() != "traefik" {
		t.Fatalf("name %q", c.Name())
	}
	got, err := c.Collect(context.Background(), traefikTargets())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, site string
		kv         []string
		want       float64
	}{
		{"pressfleet_site_http_requests_total", "site1", []string{"status_class", "2xx"}, 15},
		{"pressfleet_site_http_requests_total", "site1", []string{"status_class", "4xx"}, 2},
		{"pressfleet_site_http_requests_total", "site1", []string{"status_class", "5xx"}, 1},
		{"pressfleet_site_http_requests_total", "site2", []string{"status_class", "2xx"}, 7},
		{"pressfleet_site_http_request_duration_seconds_bucket", "site1", []string{"le", "0.1"}, 10},
		{"pressfleet_site_http_request_duration_seconds_bucket", "site1", []string{"le", "+Inf"}, 18},
		{"pressfleet_site_http_request_duration_seconds_sum", "site1", nil, 3.75},
		{"pressfleet_site_http_request_duration_seconds_count", "site1", nil, 18},
	}
	for _, tt := range tests {
		v, ok := find(got, tt.name, tt.site, tt.kv...)
		if !ok || v != tt.want {
			t.Errorf("%s %s %v = %v (found %v), want %v", tt.name, tt.site, tt.kv, v, ok, tt.want)
		}
	}
	for _, s := range got {
		switch s.Labels["site_id"] {
		case "site3", "gone", "dashboard":
			t.Errorf("sample for a site without traffic or a non-target: %+v", s)
		}
		if s.Labels["client_group_id"] == "" {
			t.Errorf("sample lacks client_group_id: %+v", s)
		}
		if err := s.Validate(); err != nil {
			t.Error(err)
		}
	}
}

func TestTraefikCollectorErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"http error", http.StatusInternalServerError, "boom"},
		{"malformed", http.StatusOK, "traefik_service_requests_total{code=\"200\" 5\n"},
		{"bad value", http.StatusOK, "traefik_service_requests_total{code=\"200\"} abc\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := serve(t, tt.status, tt.body)
			_, err := metrics.TraefikCollector{URL: srv.URL}.Collect(context.Background(), traefikTargets())
			if !errors.Is(err, metrics.ErrTraefikScrape) {
				t.Fatalf("got %v, want ErrTraefikScrape", err)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		srv := serve(t, http.StatusOK, "")
		srv.Close()
		_, err := metrics.TraefikCollector{URL: srv.URL}.Collect(context.Background(), nil)
		if !errors.Is(err, metrics.ErrTraefikScrape) {
			t.Fatalf("got %v", err)
		}
	})
}

func TestTraefikCollectorStatusClasses(t *testing.T) {
	body := strings.Join([]string{
		`traefik_service_requests_total{code="101",service="pf-site1@docker"} 1`,
		`traefik_service_requests_total{code="302",service="pf-site1@docker"} 2`,
		`traefik_service_requests_total{code="0",service="pf-site1@docker"} 4`,
		`traefik_service_requests_total{code="nan",service="pf-site1@docker"} 8`,
		`traefik_service_requests_total{code="200",service="pf-site1@docker"} NaN`,
		"",
	}, "\n")
	srv := serve(t, http.StatusOK, body)
	got, err := metrics.TraefikCollector{URL: srv.URL}.Collect(context.Background(), traefikTargets()[:1])
	if err != nil {
		t.Fatal(err)
	}
	for class, want := range map[string]float64{"1xx": 1, "3xx": 2, "other": 12} {
		if v, ok := find(got, "pressfleet_site_http_requests_total", "site1", "status_class", class); !ok || v != want {
			t.Errorf("%s = %v (found %v), want %v", class, v, ok, want)
		}
	}
	if _, ok := find(got, "pressfleet_site_http_requests_total", "site1", "status_class", "2xx"); ok {
		t.Error("NaN value was kept")
	}
}

func TestDefaultTraefikURL(t *testing.T) {
	if metrics.DefaultTraefikURL != "http://traefik:8082/metrics" {
		t.Fatal(metrics.DefaultTraefikURL)
	}
}
