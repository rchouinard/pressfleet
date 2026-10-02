package metrics_test

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

type fakeAliases map[string][]string

func (f fakeAliases) Aliases(_ context.Context, siteID string) ([]string, error) {
	return f[siteID], nil
}

type fakeSecrets map[secrets.Ref]string

func (f fakeSecrets) Get(_ context.Context, ref secrets.Ref) (redact.Secret, error) {
	v, ok := f[ref]
	if !ok {
		return "", store.ErrNotFound
	}
	return redact.Secret(v), nil
}

// seenRequest is what the test server saw.
type seenRequest struct {
	host, user, pass string
	hasAuth          bool
}

// uptimeServer starts a TLS server and returns a client that sends every
// request to it whatever the host, keeping the Host header.
func uptimeServer(t *testing.T, h func(http.ResponseWriter, seenRequest)) (*http.Client, *[]seenRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []seenRequest
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := seenRequest{host: r.Host}
		s.user, s.pass, s.hasAuth = r.BasicAuth()
		mu.Lock()
		seen = append(seen, s)
		mu.Unlock()
		h(w, s)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return client, &seen
}

func newUptime(t *testing.T, client *http.Client, aliases fakeAliases, sec fakeSecrets) *metrics.UptimeCollector {
	t.Helper()
	c, err := metrics.NewUptimeCollector(metrics.UptimeOptions{
		Aliases: aliases, Secrets: sec, Client: client, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func uptimeTarget(site store.Site) metrics.Target {
	return metrics.Target{SiteID: site.ID, ClientGroupID: "g1", Site: site}
}

// value returns the sample with the name and domain label ("" for none).
func value(samples []metrics.Sample, name, domain string) (float64, bool) {
	for _, s := range samples {
		if s.Name == name && s.Labels["domain"] == domain {
			return s.Value, true
		}
	}
	return 0, false
}

func TestUptimeCollector(t *testing.T) {
	site := store.Site{
		ID: "s1", Status: store.SiteRunning,
		PreviewDomain: "s1.preview.example", PrimaryDomain: "www.example.org",
	}
	client, seen := uptimeServer(t, func(w http.ResponseWriter, _ seenRequest) { w.WriteHeader(http.StatusOK) })
	c := newUptime(t, client, fakeAliases{"s1": {"example.org"}}, nil)

	samples, err := c.Collect(context.Background(), []metrics.Target{uptimeTarget(site)})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := value(samples, "pressfleet_site_up", ""); !ok || v != 1 {
		t.Errorf("up = %v, %v", v, ok)
	}
	if v, _ := value(samples, "pressfleet_site_http_check_status_code", ""); v != 200 {
		t.Errorf("status = %v", v)
	}
	if _, ok := value(samples, "pressfleet_site_http_check_duration_seconds", ""); !ok {
		t.Error("no duration sample")
	}
	for _, d := range []string{"s1.preview.example", "www.example.org", "example.org"} {
		v, ok := value(samples, "pressfleet_site_tls_certificate_expiry_timestamp_seconds", d)
		if !ok || time.Unix(int64(v), 0).Before(time.Now().Add(24*time.Hour)) {
			t.Errorf("expiry for %s = %v, %v", d, v, ok)
		}
	}
	var up int
	for _, s := range samples {
		if s.Name == "pressfleet_site_up" {
			up++
		}
		if s.Labels[metrics.LabelSiteID] != "s1" {
			t.Errorf("sample %s without the site label", s.Name)
		}
	}
	if up != 1 {
		t.Errorf("%d up samples, want 1", up)
	}
	for _, s := range *seen {
		if s.hasAuth {
			t.Errorf("credentials sent to %s without preview protection", s.host)
		}
	}
}

func TestUptimeCollectorPreviewAuth(t *testing.T) {
	client, seen := uptimeServer(t, func(w http.ResponseWriter, s seenRequest) {
		if s.host == "s1.preview.example" && (s.user != "preview" || s.pass != "hunter2") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	site := store.Site{
		ID: "s1", Status: store.SiteRunning, PreviewDomain: "s1.preview.example",
		PreviewProtection: true, PreviewUsername: "preview", PreviewPasswordSecretRef: "ref1",
	}
	c := newUptime(t, client, fakeAliases{}, fakeSecrets{"ref1": "hunter2"})
	samples, err := c.Collect(context.Background(), []metrics.Target{uptimeTarget(site)})
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := value(samples, "pressfleet_site_http_check_status_code", ""); v != 200 {
		t.Errorf("status = %v, want 200 with credentials", v)
	}
	if len(*seen) != 1 || !(*seen)[0].hasAuth {
		t.Errorf("requests = %+v", *seen)
	}

	// After go-live the primary domain, which is not protected, gets no
	// credentials; the preview domain still does.
	site.PrimaryDomain = "www.example.org"
	*seen = nil
	if _, err := c.Collect(context.Background(), []metrics.Target{uptimeTarget(site)}); err != nil {
		t.Fatal(err)
	}
	for _, s := range *seen {
		if want := s.host == "s1.preview.example"; s.hasAuth != want {
			t.Errorf("host %s: credentials sent = %v", s.host, s.hasAuth)
		}
	}

	// Unreadable credentials are an error, but the site is still checked
	// (and a protected preview that answers 401 is up).
	c = newUptime(t, client, fakeAliases{}, fakeSecrets{})
	site.PrimaryDomain = ""
	samples, err = c.Collect(context.Background(), []metrics.Target{uptimeTarget(site)})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if v, _ := value(samples, "pressfleet_site_up", ""); v != 1 {
		t.Errorf("up = %v with a 401", v)
	}
}

func TestUptimeCollectorStatuses(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		maintenance bool
		wantUp      float64
	}{
		{"ok", 200, false, 1},
		{"redirect", 301, false, 1},
		{"client error", 404, false, 1},
		{"server error", 502, false, 0},
		{"maintenance", 503, true, 1},
		{"unplanned 503", 503, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, _ := uptimeServer(t, func(w http.ResponseWriter, _ seenRequest) { w.WriteHeader(tt.status) })
			c := newUptime(t, client, fakeAliases{}, nil)
			site := store.Site{ID: "s1", Status: store.SiteRunning, PreviewDomain: "a.example", Maintenance: tt.maintenance}
			samples, err := c.Collect(context.Background(), []metrics.Target{uptimeTarget(site)})
			if err != nil {
				t.Fatal(err)
			}
			if v, _ := value(samples, "pressfleet_site_up", ""); v != tt.wantUp {
				t.Errorf("up = %v, want %v", v, tt.wantUp)
			}
			if v, _ := value(samples, "pressfleet_site_http_check_status_code", ""); v != float64(tt.status) {
				t.Errorf("status = %v", v)
			}
		})
	}
}

func TestUptimeCollectorUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens here any more
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}},
	}
	c := newUptime(t, client, fakeAliases{}, nil)
	site := store.Site{ID: "s1", Status: store.SiteRunning, PreviewDomain: "a.example"}
	samples, err := c.Collect(context.Background(), []metrics.Target{uptimeTarget(site)})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := value(samples, "pressfleet_site_up", ""); !ok || v != 0 {
		t.Errorf("up = %v, %v", v, ok)
	}
	if v, _ := value(samples, "pressfleet_site_http_check_status_code", ""); v != 0 {
		t.Errorf("status = %v, want 0", v)
	}
	if _, ok := value(samples, "pressfleet_site_tls_certificate_expiry_timestamp_seconds", "a.example"); ok {
		t.Error("expiry reported without a certificate")
	}
}

func TestUptimeCollectorSkipsSitesNotServing(t *testing.T) {
	client, seen := uptimeServer(t, func(http.ResponseWriter, seenRequest) {})
	c := newUptime(t, client, fakeAliases{}, nil)
	var targets []metrics.Target
	for _, st := range []store.SiteStatus{store.SiteStopped, store.SiteProvisioning, store.SiteUpdating, store.SiteFailed, store.SiteDeleting} {
		targets = append(targets, uptimeTarget(store.Site{ID: string(st), Status: st, PreviewDomain: "a.example"}))
	}
	samples, err := c.Collect(context.Background(), targets)
	if err != nil || len(samples) != 0 || len(*seen) != 0 {
		t.Errorf("samples = %v, requests = %v, err = %v", samples, *seen, err)
	}
}

func TestNewUptimeCollectorRequires(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	for name, o := range map[string]metrics.UptimeOptions{
		"aliases": {Secrets: fakeSecrets{}, Logger: log},
		"secrets": {Aliases: fakeAliases{}, Logger: log},
		"logger":  {Aliases: fakeAliases{}, Secrets: fakeSecrets{}},
	} {
		if _, err := metrics.NewUptimeCollector(o); err == nil {
			t.Errorf("missing %s accepted", name)
		}
	}
}
