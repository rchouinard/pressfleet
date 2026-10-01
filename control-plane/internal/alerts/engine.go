package alerts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Engine defaults.
const (
	// DefaultInterval is how often rules are evaluated. VictoriaMetrics
	// collectors scrape about every 15 to 60 seconds, so evaluating more
	// often would only see the same data.
	DefaultInterval = 30 * time.Second
	// DefaultQueryTimeout bounds one metrics query.
	DefaultQueryTimeout = 20 * time.Second
	// DefaultNotifyTimeout bounds one call to the notifier.
	DefaultNotifyTimeout = 30 * time.Second
	// ResolvedRetention is how long resolved alerts are kept as history.
	ResolvedRetention = 90 * 24 * time.Hour

	pruneEvery = time.Hour
	// maxRendered caps a rendered summary or description.
	maxRendered = 1000
)

// Options configures an Engine.
type Options struct {
	// DB is the state database. Required.
	DB *store.DB
	// Querier asks the metrics store. Required.
	Querier Querier
	// Notifier receives alert events. If nil, events are logged and
	// delivered nowhere (NewLogNotifier).
	Notifier Notifier
	// Interval is the time between evaluations; DefaultInterval if zero.
	Interval time.Duration
	// Now is the clock; time.Now if nil. Tests set it to control `for`
	// durations.
	Now func() time.Time
	// Logger is required.
	Logger *slog.Logger
	// QueryTimeout and NotifyTimeout bound a metrics query and a call to
	// the notifier; the defaults apply if zero.
	QueryTimeout  time.Duration
	NotifyTimeout time.Duration
}

// Engine evaluates alert rules on a schedule and emits alert events.
type Engine struct {
	db            *store.DB
	querier       Querier
	notifier      Notifier
	interval      time.Duration
	now           func() time.Time
	log           *slog.Logger
	queryTimeout  time.Duration
	notifyTimeout time.Duration

	// mu serializes evaluations and guards the fields below.
	mu sync.Mutex
	// failing holds the last query error of each rule whose last
	// evaluation failed, so a persistent failure is logged once.
	failing   map[string]string
	lastPrune time.Time
}

// New returns an Engine.
func New(opts Options) (*Engine, error) {
	switch {
	case opts.DB == nil:
		return nil, errors.New("alerts: DB is required")
	case opts.Querier == nil:
		return nil, errors.New("alerts: Querier is required")
	case opts.Logger == nil:
		return nil, errors.New("alerts: Logger is required")
	}
	e := &Engine{
		db: opts.DB, querier: opts.Querier, notifier: opts.Notifier, interval: opts.Interval, now: opts.Now, log: opts.Logger,
		queryTimeout: opts.QueryTimeout, notifyTimeout: opts.NotifyTimeout, failing: map[string]string{},
	}
	if e.notifier == nil {
		e.notifier = NewLogNotifier(e.log)
	}
	if e.interval <= 0 {
		e.interval = DefaultInterval
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.queryTimeout <= 0 {
		e.queryTimeout = DefaultQueryTimeout
	}
	if e.notifyTimeout <= 0 {
		e.notifyTimeout = DefaultNotifyTimeout
	}
	return e, nil
}

// Run evaluates the rules now and then every interval, until ctx ends. It
// always returns nil: a failed evaluation is logged and tried again.
func (e *Engine) Run(ctx context.Context) error {
	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()
	for {
		if err := e.Evaluate(ctx); err != nil && ctx.Err() == nil {
			e.log.Error("evaluating alert rules failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Evaluate runs one evaluation of every rule at the current time, then
// delivers the events that are due. It returns an error only if the
// database can't be used; a rule whose query fails is logged and left as
// it is.
func (e *Engine) Evaluate(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now().UTC()

	rules, err := store.NewAlertRules(e.db).List(ctx, store.AlertRuleFilter{})
	if err != nil {
		return err
	}
	unresolved, err := store.NewAlerts(e.db).Unresolved(ctx)
	if err != nil {
		return err
	}
	byRule := map[string][]store.Alert{}
	for _, a := range unresolved {
		byRule[a.RuleID] = append(byRule[a.RuleID], a)
	}

	ruleByID := make(map[string]store.AlertRule, len(rules))
	for _, rule := range rules {
		ruleByID[rule.ID] = rule
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !rule.Enabled {
			delete(e.failing, rule.ID)
			if err := e.apply(ctx, rule, nil, byRule[rule.ID], now); err != nil {
				e.log.Error("closing the alerts of a disabled rule failed", "rule", rule.Name, "error", err)
			}
			continue
		}
		if err := e.evaluateRule(ctx, rule, byRule[rule.ID], now); err != nil {
			e.log.Error("evaluating alert rule failed", "rule", rule.Name, "error", err)
		}
	}
	e.deliver(ctx, ruleByID)
	e.prune(ctx, now)
	return nil
}

// condition is a series for which a rule's condition holds.
type condition struct {
	fingerprint string
	labels      map[string]string // the alert's labels: the series' plus the rule's
	value       float64
}

func (e *Engine) evaluateRule(ctx context.Context, rule store.AlertRule, existing []store.Alert, now time.Time) error {
	qctx, cancel := context.WithTimeout(ctx, e.queryTimeout)
	samples, err := e.querier.Query(qctx, rule.Expr, now)
	cancel()
	if err != nil {
		// Leave every alert as it is: not being able to ask isn't an answer.
		if msg := err.Error(); e.failing[rule.ID] != msg {
			e.failing[rule.ID] = msg
			e.log.Warn("querying metrics for an alert rule failed; its alerts are left as they are",
				"rule", rule.Name, "unavailable", errors.Is(err, ErrMetricsUnavailable), "error", err)
		}
		return nil
	}
	if _, was := e.failing[rule.ID]; was {
		delete(e.failing, rule.ID)
		e.log.Info("querying metrics for an alert rule works again", "rule", rule.Name)
	}
	conds, skipped := conditions(rule, samples)
	if skipped > 0 {
		e.log.Warn("alert rule returned series without a site_id label, which a site rule needs; they were ignored",
			"rule", rule.Name, "series", skipped)
	}
	return e.apply(ctx, rule, conds, existing, now)
}

// conditions returns the series for which the rule's condition holds, and
// how many series a site rule had to skip for lacking a site_id.
func conditions(rule store.AlertRule, samples []Sample) (conds []condition, skipped int) {
	seen := map[string]bool{}
	for _, s := range samples {
		if math.IsNaN(s.Value) {
			continue
		}
		if rule.Kind == store.AlertKindThreshold && !rule.Operator.Compare(s.Value, rule.Threshold) {
			continue
		}
		series := maps.Clone(s.Labels)
		delete(series, "__name__")
		if rule.Scope == store.AlertScopeSite && series["site_id"] == "" {
			skipped++
			continue
		}
		if rule.ClientGroupID != "" && series["client_group_id"] != rule.ClientGroupID {
			continue
		}
		fp := fingerprint(series)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		labels := maps.Clone(series)
		if labels == nil {
			labels = map[string]string{}
		}
		maps.Copy(labels, rule.Labels)
		conds = append(conds, condition{fingerprint: fp, labels: labels, value: s.Value})
	}
	slices.SortFunc(conds, func(a, b condition) int { return strings.Compare(a.fingerprint, b.fingerprint) })
	return conds, skipped
}

// fingerprint identifies a series by its labels.
func fingerprint(labels map[string]string) string {
	h := sha256.New()
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		fmt.Fprintf(h, "%d:%s=%d:%s;", len(k), k, len(labels[k]), labels[k])
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// apply brings the rule's unresolved alerts in line with the conditions
// that hold now, in one transaction. Changes that raced with another
// evaluation (an alert that is already firing, resolved, or inserted) are
// ignored: the other evaluation made the same change.
func (e *Engine) apply(ctx context.Context, rule store.AlertRule, conds []condition, existing []store.Alert, now time.Time) error {
	if len(conds) == 0 && len(existing) == 0 {
		return nil
	}
	return e.db.InTx(ctx, func(q store.Querier) error {
		alerts := store.NewAlerts(q)
		current := map[string]*store.Alert{}
		for i := range existing {
			current[existing[i].Fingerprint] = &existing[i]
		}
		holds := map[string]bool{}
		for _, c := range conds {
			holds[c.fingerprint] = true
			summary, description := e.renderAnnotations(rule, c)
			a := current[c.fingerprint]
			if a == nil {
				fresh := store.Alert{
					RuleID: rule.ID, Fingerprint: c.fingerprint, Labels: c.labels, SiteID: c.labels["site_id"],
					ClientGroupID: c.labels["client_group_id"], State: store.AlertPending, Severity: rule.Severity,
					Summary: summary, Description: description, Value: c.value, StartedAt: now,
				}
				if rule.For <= 0 {
					fresh.State, fresh.FiredAt = store.AlertFiring, &now
				}
				if err := ignore(alerts.Insert(ctx, &fresh), store.ErrConflict); err != nil {
					return err
				}
				continue
			}
			changed := a.Value != c.value || a.Severity != rule.Severity || a.Summary != summary || a.Description != description
			a.Value, a.Severity, a.Summary, a.Description = c.value, rule.Severity, summary, description
			switch {
			case a.State == store.AlertPending && now.Sub(a.StartedAt) >= rule.For:
				if err := ignore(alerts.Fire(ctx, a, now), store.ErrNotFound); err != nil {
					return err
				}
			case changed:
				if err := ignore(alerts.Refresh(ctx, a), store.ErrNotFound); err != nil {
					return err
				}
			}
		}
		for _, a := range existing {
			if holds[a.Fingerprint] {
				continue
			}
			var err error
			if a.State == store.AlertPending {
				err = alerts.DeletePending(ctx, a.ID)
			} else {
				err = alerts.Resolve(ctx, &a, now)
			}
			if err := ignore(err, store.ErrNotFound); err != nil {
				return err
			}
		}
		return nil
	})
}

func ignore(err, target error) error {
	if errors.Is(err, target) {
		return nil
	}
	return err
}

// renderAnnotations renders the rule's summary and description for a
// series. A template that fails to render (it passed validation, so this
// is unusual) is used as it is, so the alert still says something.
func (e *Engine) renderAnnotations(rule store.AlertRule, c condition) (summary, description string) {
	data := templateData{Labels: c.labels, Value: c.value, Threshold: rule.Threshold, Rule: rule.Name}
	out := func(key string) string {
		text := rule.Annotations[key]
		s, err := render(text, data)
		if err != nil {
			e.log.Warn("rendering an alert rule template failed", "rule", rule.Name, "annotation", key, "error", err)
			s = text
		}
		if len(s) > maxRendered {
			s = strings.ToValidUTF8(s[:maxRendered], "")
		}
		return s
	}
	summary, description = out(AnnotationSummary), out(AnnotationDescription)
	if summary == "" {
		summary = rule.Name
	}
	return summary, description
}

// deliver offers the notifier every event it hasn't accepted, oldest alert
// first, a firing event before its alert's resolved event. An event the
// notifier rejects stays due, and stops its alert's later event, so events
// of one alert are never delivered out of order.
func (e *Engine) deliver(ctx context.Context, rules map[string]store.AlertRule) {
	alerts := store.NewAlerts(e.db)
	due, err := alerts.Unnotified(ctx)
	if err != nil {
		e.log.Error("listing alert events to deliver failed", "error", err)
		return
	}
	for _, a := range due {
		rule, ok := rules[a.RuleID]
		if !ok {
			continue // the rule was created after this evaluation began; the next one delivers
		}
		for _, t := range []struct {
			typ  store.AlertEventType
			at   *time.Time
			done *time.Time
		}{
			{store.AlertEventFiring, a.FiredAt, a.FiringNotifiedAt},
			{store.AlertEventResolved, a.ResolvedAt, a.ResolvedNotifiedAt},
		} {
			if t.at == nil || t.done != nil {
				continue
			}
			if !e.send(ctx, Event{Type: t.typ, Alert: a, Rule: rule}) {
				break
			}
			if err := alerts.MarkNotified(ctx, a.ID, t.typ, e.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
				// The event will be offered again, so a notifier that
				// can't deduplicate sends it twice. Rare enough: the
				// database was just working.
				e.log.Error("recording a delivered alert event failed", "alert_id", a.ID, "event", string(t.typ), "error", err)
				return
			}
		}
	}
}

// send offers one event to the notifier and reports whether it accepted it.
func (e *Engine) send(ctx context.Context, ev Event) bool {
	nctx, cancel := context.WithTimeout(ctx, e.notifyTimeout)
	defer cancel()
	if err := e.notifier.Notify(nctx, ev); err != nil {
		if ctx.Err() == nil {
			e.log.Warn("the notifier didn't accept an alert event; it is offered again after the next evaluation",
				"alert_id", ev.Alert.ID, "event", string(ev.Type), "rule", ev.Rule.Name, "error", err)
		}
		return false
	}
	return true
}

// prune deletes alerts that resolved long ago, at most once an hour.
func (e *Engine) prune(ctx context.Context, now time.Time) {
	if !e.lastPrune.IsZero() && now.Sub(e.lastPrune) < pruneEvery {
		return
	}
	e.lastPrune = now
	if _, err := store.NewAlerts(e.db).DeleteResolvedBefore(ctx, now.Add(-ResolvedRetention)); err != nil {
		e.log.Warn("deleting old resolved alerts failed", "error", err)
	}
}
