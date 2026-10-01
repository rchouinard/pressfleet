package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics/metricstest"
)

func TestMetricsQuerier(t *testing.T) {
	t.Run("converts series to samples", func(t *testing.T) {
		fake := &metricstest.Querier{Result: metrics.Result{Type: metrics.Vector, Series: []metrics.Series{
			{Labels: map[string]string{"site_id": "a"}, Points: []metrics.Point{{Value: 1}}},
			{Labels: map[string]string{"site_id": "b"}}, // no points: skipped
		}}}
		got, err := metricsQuerier{fake}.Query(context.Background(), "up", time.Time{})
		if err != nil || len(got) != 1 || got[0].Labels["site_id"] != "a" || got[0].Value != 1 {
			t.Fatalf("Query() = %+v, %v", got, err)
		}
	})
	t.Run("outage is unavailable", func(t *testing.T) {
		fake := &metricstest.Querier{Err: metrics.ErrUnavailable}
		_, err := metricsQuerier{fake}.Query(context.Background(), "up", time.Time{})
		if !errors.Is(err, alerts.ErrMetricsUnavailable) {
			t.Fatalf("err = %v, want alerts.ErrMetricsUnavailable", err)
		}
	})
	t.Run("a bad query is not an outage", func(t *testing.T) {
		fake := &metricstest.Querier{Err: metrics.ErrQuery}
		_, err := metricsQuerier{fake}.Query(context.Background(), "up", time.Time{})
		if err == nil || errors.Is(err, alerts.ErrMetricsUnavailable) {
			t.Fatalf("err = %v, want a non-outage error", err)
		}
	})
}
