package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alertchannels"
	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/metrics"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
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

// alertChannels returns the alert channel service: it manages the channels
// of the API and is the alerts.Notifier that delivers events to them
// (SMTP email and webhooks, #76). Every event is logged as it is routed,
// and every delivery as it is made or fails.
func (a *app) alertChannels(db *store.DB, secretStore secrets.Store, keys *secrets.Keyring) (*alertchannels.Service, error) {
	relay, err := platformRelay(a.cfg.SMTP, keys)
	if err != nil {
		return nil, err
	}
	return alertchannels.New(alertchannels.Options{
		DB:                db,
		Secrets:           secretStore,
		PlatformSMTP:      relay,
		AllowLocalTargets: a.cfg.Alerts.WebhookAllowLocalTargets,
		WebhookTimeout:    a.cfg.Alerts.WebhookTimeout(),
		Logger:            a.log.With("component", "alertchannels"),
	})
}

// platformRelay returns the platform's SMTP relay (the smtp.* settings) as
// alert email channels use it, or nil if outbound email isn't configured.
// It decrypts the password; siteSMTP has already refused to start the
// control plane if it can't be.
func platformRelay(cfg config.SMTPConfig, keys *secrets.Keyring) (*alertchannels.Relay, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	var password redact.Secret
	if cfg.Password != "" {
		var err error
		if password, err = keys.OpenSetting(config.SMTPPasswordSetting, cfg.Password); err != nil {
			return nil, fmt.Errorf("decrypting the SMTP password: %w", err)
		}
	}
	return &alertchannels.Relay{
		Host:       cfg.Host,
		Port:       cfg.EffectivePort(),
		Encryption: cfg.Encryption,
		Username:   cfg.Username,
		Password:   password,
		FromEmail:  cfg.FromEmail,
		FromName:   cfg.FromName,
	}, nil
}

// alertEngine returns the alert evaluation engine, or nil if alerts.enabled
// is false. Events go to notifier.
func (a *app) alertEngine(db *store.DB, notifier alerts.Notifier) (*alerts.Engine, error) {
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
		Notifier: notifier,
		Interval: a.cfg.Alerts.EvaluationInterval(),
		Logger:   a.log.With("component", "alerts"),
	})
}
