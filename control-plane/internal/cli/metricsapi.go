package cli

import (
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/metricsapi"
)

// metricsQueries returns what the metrics query API reads from
// VictoriaMetrics. With metrics.enabled false it has no store, and the API
// answers 503 as it does when the store is down.
func (a *app) metricsQueries() (*metricsapi.Service, error) {
	if !a.cfg.Metrics.Enabled {
		a.log.Info("the metrics API has no data source: metrics are turned off (metrics.enabled)")
		return metricsapi.New(nil, nil), nil
	}
	client, err := metrics.NewClient(metrics.Options{URL: a.cfg.Metrics.URL, Logger: a.log.With("component", "metricsapi")})
	if err != nil {
		return nil, err
	}
	return metricsapi.New(client, nil), nil
}
