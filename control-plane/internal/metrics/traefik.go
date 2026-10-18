package metrics

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/ingress"
)

// Metric names TraefikCollector emits. The catalog in package metricsapi
// and the default alert rules query them.
const (
	MetricSiteHTTPRequests        = "pressfleet_site_http_requests_total"
	MetricSiteHTTPRequestDuration = "pressfleet_site_http_request_duration_seconds"

	traefikServiceRequests       = "traefik_service_requests_total"
	traefikServiceDurationPrefix = "traefik_service_request_duration_seconds"
	// traefikDockerProvider is appended to the names of services that the
	// Docker provider (the nginx labels) defines.
	traefikDockerProvider = "@docker"
	// traefikMaxResponseBytes bounds what one scrape reads.
	traefikMaxResponseBytes = 64 << 20
)

// DefaultTraefikURL is where the platform's Traefik serves Prometheus
// metrics on the platform network (templates/platform/traefik.yaml.tmpl).
// The port isn't published on the host.
var DefaultTraefikURL = fmt.Sprintf("http://%s:%d/metrics", ingress.TraefikService, ingress.InternalPort)

// ErrTraefikScrape is wrapped by the error [TraefikCollector.Collect]
// returns when Traefik's metrics can't be fetched or read.
var ErrTraefikScrape = errors.New("scraping Traefik metrics")

// TraefikCollector reports each site's HTTP traffic from Traefik's
// Prometheus endpoint (design doc §7.1 Traffic):
//
//   - pressfleet_site_http_requests_total{status_class}: requests served,
//     by status class "1xx" to "5xx" ("other" for a code outside 100-599);
//   - pressfleet_site_http_request_duration_seconds_bucket{le}, _sum, and
//     _count: the request duration histogram, with Traefik's buckets.
//
// Traefik's per-service series are used rather than its per-router ones.
// Every router of a site, whether defined by the nginx labels or the
// domains file, points at the one service pf-<site-id>@docker
// ([ingress.SiteService]), so the service name identifies the site exactly,
// where router names (pf-<site-id>-<suffix>) would need parsing. The
// consequence: requests that Traefik answers itself and never forwards (the
// maintenance page, the redirects of aliases and of a live site's preview
// domain) are not counted, since the site did not serve them.
//
// Traefik's method and protocol labels are summed away to keep the series
// count small, and series of services that match no site, such as the
// dashboard's, are ignored.
type TraefikCollector struct {
	// Every is the collection interval; zero means the service default.
	Every time.Duration
	// URL is Traefik's metrics endpoint; empty means [DefaultTraefikURL].
	URL string
	// HTTPClient fetches the endpoint; nil means [http.DefaultClient],
	// since the service's per-run deadline already bounds the request.
	HTTPClient *http.Client
}

var _ Collector = TraefikCollector{}

// Name implements [Collector].
func (TraefikCollector) Name() string { return "traefik" }

// Interval implements [Collector].
func (c TraefikCollector) Interval() time.Duration { return c.Every }

// Collect implements [Collector].
func (c TraefikCollector) Collect(ctx context.Context, targets []Target) ([]Sample, error) {
	body, err := c.fetch(ctx)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	series, err := parseExposition(io.LimitReader(body, traefikMaxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTraefikScrape, err)
	}
	return traefikSamples(series, targets), nil
}

func (c TraefikCollector) fetch(ctx context.Context) (io.ReadCloser, error) {
	url := c.URL
	if url == "" {
		url = DefaultTraefikURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTraefikScrape, err)
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTraefikScrape, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%w: %s answered %s", ErrTraefikScrape, url, resp.Status)
	}
	return resp.Body, nil
}

// traefikHistogram accumulates one site's duration histogram.
type traefikHistogram struct {
	buckets    map[string]float64
	sum, count float64
}

// traefikSamples turns Traefik's series into site samples, summing over
// the labels that are dropped. Sites without series get no samples, so
// "no traffic yet" reads as no data rather than zero.
func traefikSamples(series []expositionSeries, targets []Target) []Sample {
	bySvc := make(map[string]Target, len(targets))
	for _, t := range targets {
		bySvc[ingress.SiteService(t.SiteID)+traefikDockerProvider] = t
	}
	requests := map[string]map[string]float64{} // site -> status class -> count
	hists := map[string]*traefikHistogram{}     // site -> histogram
	hist := func(site string) *traefikHistogram {
		h := hists[site]
		if h == nil {
			h = &traefikHistogram{buckets: map[string]float64{}}
			hists[site] = h
		}
		return h
	}
	for _, s := range series {
		t, ok := bySvc[s.labels["service"]]
		if !ok {
			continue
		}
		switch s.name {
		case traefikServiceRequests:
			if requests[t.SiteID] == nil {
				requests[t.SiteID] = map[string]float64{}
			}
			requests[t.SiteID][statusClass(s.labels["code"])] += s.value
		case traefikServiceDurationPrefix + "_bucket":
			if le := s.labels["le"]; le != "" {
				hist(t.SiteID).buckets[le] += s.value
			}
		case traefikServiceDurationPrefix + "_sum":
			hist(t.SiteID).sum += s.value
		case traefikServiceDurationPrefix + "_count":
			hist(t.SiteID).count += s.value
		}
	}

	var out []Sample
	for _, t := range targets {
		for _, class := range sortedKeys(requests[t.SiteID]) {
			out = append(out, t.Sample(MetricSiteHTTPRequests, requests[t.SiteID][class], "status_class", class))
		}
		h := hists[t.SiteID]
		if h == nil {
			continue
		}
		for _, le := range sortedKeys(h.buckets) {
			out = append(out, t.Sample(MetricSiteHTTPRequestDuration+"_bucket", h.buckets[le], "le", le))
		}
		out = append(out,
			t.Sample(MetricSiteHTTPRequestDuration+"_sum", h.sum),
			t.Sample(MetricSiteHTTPRequestDuration+"_count", h.count))
	}
	return out
}

// statusClass maps an HTTP status code to "1xx" to "5xx", or "other".
func statusClass(code string) string {
	n, err := strconv.Atoi(code)
	if err != nil || n < 100 || n > 599 {
		return "other"
	}
	return strconv.Itoa(n/100) + "xx"
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// expositionSeries is one sample line of the Prometheus text format.
type expositionSeries struct {
	name   string
	labels map[string]string
	value  float64
}

var errBadExposition = errors.New("malformed sample")

// parseExposition reads the Prometheus text exposition format, ignoring
// comments and skipping NaN and infinite values, which a Sample can't
// carry. It is deliberately small: Traefik's output needs no more, and a
// parsing dependency would outweigh it. Histogram bounds ("le") stay
// strings, so "+Inf" survives.
func parseExposition(r io.Reader) ([]expositionSeries, error) {
	var out []expositionSeries
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		s, err := parseExpositionLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if math.IsNaN(s.value) || math.IsInf(s.value, 0) {
			continue
		}
		out = append(out, s)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading metrics: %w", err)
	}
	return out, nil
}

func parseExpositionLine(line string) (expositionSeries, error) {
	var s expositionSeries
	i := strings.IndexAny(line, "{ \t")
	if i <= 0 {
		return s, fmt.Errorf("%w: %q", errBadExposition, line)
	}
	s.name = line[:i]
	rest := line[i:]
	if rest[0] == '{' {
		labels, after, err := parseExpositionLabels(rest[1:])
		if err != nil {
			return s, err
		}
		s.labels, rest = labels, after
	}
	// The value, then an optional timestamp.
	fields := strings.Fields(rest)
	if len(fields) < 1 || len(fields) > 2 {
		return s, fmt.Errorf("%w: %q", errBadExposition, line)
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return s, fmt.Errorf("%w: value %q", errBadExposition, fields[0])
	}
	s.value = v
	return s, nil
}

// parseExpositionLabels reads name="value" pairs up to the closing brace
// and returns what follows it.
func parseExpositionLabels(s string) (map[string]string, string, error) {
	labels := map[string]string{}
	for {
		s = strings.TrimLeft(s, " \t,")
		if strings.HasPrefix(s, "}") {
			return labels, s[1:], nil
		}
		eq := strings.Index(s, `="`)
		if eq <= 0 {
			return nil, "", fmt.Errorf("%w: label near %q", errBadExposition, s)
		}
		name := strings.TrimSpace(s[:eq])
		s = s[eq+2:]
		var val strings.Builder
		closed := false
		for j := 0; j < len(s) && !closed; j++ {
			switch c := s[j]; {
			case c == '\\' && j+1 < len(s):
				j++
				if s[j] == 'n' {
					val.WriteByte('\n')
				} else { // \\ and \"
					val.WriteByte(s[j])
				}
			case c == '"':
				s, closed = s[j+1:], true
			default:
				val.WriteByte(c)
			}
		}
		if !closed {
			return nil, "", fmt.Errorf("%w: unterminated label value", errBadExposition)
		}
		labels[name] = val.String()
	}
}
