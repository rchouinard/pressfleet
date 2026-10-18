package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/metricsapi"
)

// Metrics query API (#77). Clients name metrics of the catalog in package
// metricsapi; they never send queries.

// GetMetricCatalog lists the metrics the metrics endpoints serve.
func (h *Handlers) GetMetricCatalog(ctx context.Context, _ gen.GetMetricCatalogRequestObject) (gen.GetMetricCatalogResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.Platform()); err != nil {
		return nil, err
	}
	entries := metricsapi.Entries()
	out := gen.GetMetricCatalog200JSONResponse{
		Items: make([]gen.MetricDescriptor, len(entries)),
		Limits: gen.MetricLimits{
			DefaultRangeSeconds: int(metricsapi.DefaultRange / time.Second),
			MaxRangeSeconds:     int(metricsapi.MaxRange / time.Second),
			MinStepSeconds:      int(metricsapi.MinStep / time.Second),
			MaxPoints:           metricsapi.MaxPoints,
			MaxMetrics:          metricsapi.MaxMetrics,
		},
	}
	for i, e := range entries {
		out.Items[i] = gen.MetricDescriptor{
			ID: e.ID, Title: e.Title, Description: e.Description,
			Unit: gen.MetricUnit(e.Unit), Kind: gen.MetricKind(e.Kind), Group: e.Group,
			Scope:          gen.MetricDescriptorScope(e.Scope),
			Labels:         append([]string{}, e.Labels...),
			DefaultSummary: e.Summary,
		}
	}
	return out, nil
}

// QueryPlatformMetrics returns time series of platform metrics.
func (h *Handlers) QueryPlatformMetrics(ctx context.Context, req gen.QueryPlatformMetricsRequestObject) (gen.QueryPlatformMetricsResponseObject, error) {
	p := req.Params
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.ClientGroup(deref(p.ClientGroupID))); err != nil {
		return nil, err
	}
	res, err := h.metrics.Range(ctx, metricsapi.Request{
		ClientGroupID: deref(p.ClientGroupID), Metrics: deref(p.Metric), Groups: deref(p.Group),
		Start: deref(p.Start), End: deref(p.End), Step: stepOf(p.Step),
	})
	if err != nil {
		return nil, metricsError(err)
	}
	return gen.QueryPlatformMetrics200JSONResponse(metricsRange(res)), nil
}

// GetPlatformMetricsSummary returns the current values of platform metrics.
func (h *Handlers) GetPlatformMetricsSummary(ctx context.Context, req gen.GetPlatformMetricsSummaryRequestObject) (gen.GetPlatformMetricsSummaryResponseObject, error) {
	p := req.Params
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.ClientGroup(deref(p.ClientGroupID))); err != nil {
		return nil, err
	}
	res, err := h.metrics.Summary(ctx, metricsapi.Request{
		ClientGroupID: deref(p.ClientGroupID), Metrics: deref(p.Metric), Groups: deref(p.Group),
	})
	if err != nil {
		return nil, metricsError(err)
	}
	return gen.GetPlatformMetricsSummary200JSONResponse(metricsSummary(res)), nil
}

// QuerySiteMetrics returns time series of one site's metrics.
func (h *Handlers) QuerySiteMetrics(ctx context.Context, req gen.QuerySiteMetricsRequestObject) (gen.QuerySiteMetricsResponseObject, error) {
	if err := h.checkMetricsSite(ctx, req.SiteID); err != nil {
		return nil, err
	}
	p := req.Params
	res, err := h.metrics.Range(ctx, metricsapi.Request{
		Site: true, SiteID: req.SiteID, Metrics: deref(p.Metric), Groups: deref(p.Group),
		Start: deref(p.Start), End: deref(p.End), Step: stepOf(p.Step),
	})
	if err != nil {
		return nil, metricsError(err)
	}
	return gen.QuerySiteMetrics200JSONResponse(metricsRange(res)), nil
}

// GetSiteMetricsSummary returns the current values of one site's metrics.
func (h *Handlers) GetSiteMetricsSummary(ctx context.Context, req gen.GetSiteMetricsSummaryRequestObject) (gen.GetSiteMetricsSummaryResponseObject, error) {
	if err := h.checkMetricsSite(ctx, req.SiteID); err != nil {
		return nil, err
	}
	p := req.Params
	res, err := h.metrics.Summary(ctx, metricsapi.Request{
		Site: true, SiteID: req.SiteID, Metrics: deref(p.Metric), Groups: deref(p.Group),
	})
	if err != nil {
		return nil, metricsError(err)
	}
	return gen.GetSiteMetricsSummary200JSONResponse(metricsSummary(res)), nil
}

// checkMetricsSite authorizes the caller and makes sure the site exists, so
// an unknown site is a 404 and not an empty answer.
func (h *Handlers) checkMetricsSite(ctx context.Context, siteID string) error {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.Site(siteID)); err != nil {
		return err
	}
	if _, _, err := h.sites.Get(ctx, siteID); err != nil {
		return siteError(err)
	}
	return nil
}

// metricsRetryAfterSeconds is the retry delay sent with 503 when the metrics
// store can't be reached: long enough for a restarting store to come back.
const metricsRetryAfterSeconds = 10

// metricsError maps the metrics service's errors to responses.
func metricsError(err error) error {
	switch {
	case errors.Is(err, metricsapi.ErrInvalid):
		return httpserver.NewProblem(http.StatusBadRequest, err.Error())
	case errors.Is(err, metrics.ErrUnavailable):
		// The cause names VictoriaMetrics' address, which clients need not
		// see; the request log has it.
		p := httpserver.NewProblem(http.StatusServiceUnavailable, "The metrics store is unavailable; try again shortly.")
		p.RetryAfter = metricsRetryAfterSeconds
		return p
	}
	return err
}

func stepOf(seconds *int) time.Duration {
	if seconds == nil {
		return 0
	}
	// Seconds are capped so the conversion can't overflow; the service
	// rejects what is still too large for the range.
	return time.Duration(min(*seconds, 1<<30)) * time.Second
}

func metricsRange(r metricsapi.RangeResult) gen.MetricsRange {
	out := gen.MetricsRange{
		Start: r.Start, End: r.End, StepSeconds: int(r.Step / time.Second),
		Metrics: make([]gen.MetricResult, len(r.Results)),
	}
	for i, m := range r.Results {
		series := make([]gen.MetricSeries, len(m.Series))
		for j, s := range m.Series {
			points := make([]gen.MetricPoint, len(s.Points))
			for k, p := range s.Points {
				points[k] = gen.MetricPoint{Time: p.Time, Value: p.Value}
			}
			series[j] = gen.MetricSeries{Labels: s.Labels, Points: points, Latest: points[len(points)-1]}
		}
		out.Metrics[i] = gen.MetricResult{
			ID: m.Entry.ID, Unit: gen.MetricUnit(m.Entry.Unit), Kind: gen.MetricKind(m.Entry.Kind), Series: series,
		}
	}
	return out
}

func metricsSummary(r metricsapi.SummaryResult) gen.MetricsSummary {
	out := gen.MetricsSummary{Time: r.Time, Metrics: make([]gen.MetricSummaryResult, len(r.Results))}
	for i, m := range r.Results {
		values := make([]gen.MetricValue, len(m.Series))
		for j, s := range m.Series {
			last := s.Points[len(s.Points)-1]
			values[j] = gen.MetricValue{Labels: s.Labels, Time: last.Time, Value: last.Value}
		}
		out.Metrics[i] = gen.MetricSummaryResult{
			ID: m.Entry.ID, Unit: gen.MetricUnit(m.Entry.Unit), Kind: gen.MetricKind(m.Entry.Kind), Values: values,
		}
	}
	return out
}
