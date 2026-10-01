package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// alertQuerier returns what the alert engine asks VictoriaMetrics with: the
// metrics client of internal/metrics, behind the small alerts.Querier
// interface.
func (a *app) alertQuerier() (alerts.Querier, error) {
	client, err := metrics.NewClient(metrics.Options{URL: a.cfg.Metrics.URL, Logger: a.log.With("component", "alerts")})
	if err != nil {
		return nil, err
	}
	return metricsQuerier{client}, nil
}

// metricsQuerier adapts a [metrics.Querier] to [alerts.Querier].
type metricsQuerier struct{ q metrics.Querier }

// Query runs an instant query. Outages become alerts.ErrMetricsUnavailable,
// so the engine leaves alerts untouched instead of resolving them.
func (m metricsQuerier) Query(ctx context.Context, expr string, at time.Time) ([]alerts.Sample, error) {
	res, err := m.q.Query(ctx, expr, at)
	if err != nil {
		if errors.Is(err, metrics.ErrUnavailable) {
			return nil, fmt.Errorf("%w: %w", alerts.ErrMetricsUnavailable, err)
		}
		return nil, err
	}
	out := make([]alerts.Sample, 0, len(res.Series))
	for _, s := range res.Series {
		if len(s.Points) == 0 {
			continue
		}
		out = append(out, alerts.Sample{Labels: s.Labels, Value: s.Points[len(s.Points)-1].Value})
	}
	return out, nil
}

// alertNotifier returns where alert events go.
//
// MERGE SEAM (#76): the SMTP and webhook channels implement alerts.Notifier;
// until they exist, events are logged and delivered nowhere.
func (a *app) alertNotifier() alerts.Notifier {
	return alerts.NewLogNotifier(a.log.With("component", "alerts"))
}

// alertEngine returns the alert evaluation engine, or nil if alerts.enabled
// is false.
func (a *app) alertEngine(db *store.DB) (*alerts.Engine, error) {
	if !a.cfg.Alerts.Enabled {
		a.log.Info("alert rule evaluation is turned off (alerts.enabled)")
		return nil, nil
	}
	querier, err := a.alertQuerier()
	if err != nil {
		return nil, err
	}
	return alerts.New(alerts.Options{
		DB:       db,
		Querier:  querier,
		Notifier: a.alertNotifier(),
		Interval: a.cfg.Alerts.EvaluationInterval(),
		Logger:   a.log.With("component", "alerts"),
	})
}
