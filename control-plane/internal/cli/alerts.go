package cli

import (
	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// alertQuerier returns what the alert engine asks VictoriaMetrics with.
//
// MERGE SEAM (#75, #67): package alerts only knows the small alerts.Querier
// interface, an instant PromQL query that returns labeled samples. Until the
// metrics client of internal/metrics (#67) is wired in here, the engine gets
// alerts.UnavailableQuerier, which always reports "metrics unavailable": the
// rules are seeded and the API works, but nothing fires or resolves. To wire
// the real client, return an adapter around its instant query, for example
//
//	type metricsQuerier struct{ c *metrics.Client }
//
//	func (q metricsQuerier) Query(ctx context.Context, expr string, at time.Time) ([]alerts.Sample, error) {
//		// run the query; convert each series' labels and value to an
//		// alerts.Sample; wrap connection failures in alerts.ErrMetricsUnavailable
//	}
func (a *app) alertQuerier() alerts.Querier {
	return alerts.UnavailableQuerier{}
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
	return alerts.New(alerts.Options{
		DB:       db,
		Querier:  a.alertQuerier(),
		Notifier: a.alertNotifier(),
		Interval: a.cfg.Alerts.EvaluationInterval(),
		Logger:   a.log.With("component", "alerts"),
	})
}
