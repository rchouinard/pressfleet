package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// AlertState is where an alert is in its life.
type AlertState string

// Alert states.
const (
	// AlertPending: the condition holds but hasn't for the rule's duration
	// yet. Nothing has been sent.
	AlertPending AlertState = "pending"
	// AlertFiring: the condition has held for the rule's duration and still
	// does.
	AlertFiring AlertState = "firing"
	// AlertResolved: a firing alert's condition stopped holding.
	AlertResolved AlertState = "resolved"
)

// Valid reports whether s is a known state.
func (s AlertState) Valid() bool {
	return s == AlertPending || s == AlertFiring || s == AlertResolved
}

// AlertEventType is an event an alert emits: it fires once and resolves
// once.
type AlertEventType string

// Alert event types.
const (
	AlertEventFiring   AlertEventType = "firing"
	AlertEventResolved AlertEventType = "resolved"
)

// Alert is one incident of an alert rule for one series of its expression
// (design doc §4.3).
type Alert struct {
	ID     string
	RuleID string
	// RuleName is filled in by Get and List.
	RuleName string
	// Fingerprint identifies the series within the rule: a hash of its
	// labels.
	Fingerprint string
	// Labels are the series' labels plus the rule's own.
	Labels map[string]string
	// SiteID and ClientGroupID copy the series' site_id and client_group_id
	// labels, if it has them.
	SiteID        string
	ClientGroupID string
	State         AlertState
	Severity      AlertSeverity
	// Summary and Description are the rule's templates, rendered when the
	// alert started and refreshed while it holds.
	Summary     string
	Description string
	// Value is the series' latest value.
	Value float64
	// StartedAt is when the condition first held, FiredAt when the alert
	// started firing, and ResolvedAt when it resolved. UpdatedAt is when it
	// last changed: its value, text, or state.
	StartedAt  time.Time
	FiredAt    *time.Time
	ResolvedAt *time.Time
	UpdatedAt  time.Time
	// FiringNotifiedAt and ResolvedNotifiedAt are when the notifier
	// accepted the event; nil while it hasn't, or hasn't happened.
	FiringNotifiedAt   *time.Time
	ResolvedNotifiedAt *time.Time
}

// alertIDs makes alert IDs that sort in creation order even within a
// millisecond, so listings are newest first and events go out in the order
// their alerts started.
var alertIDs id.Monotonic

// Alerts reads and writes alerts.
type Alerts struct {
	q Querier
}

// NewAlerts returns an Alerts that runs queries on q.
func NewAlerts(q Querier) Alerts {
	return Alerts{q: q}
}

const alertColumns = "a.id, a.rule_id, r.name, a.fingerprint, a.labels, a.site_id, a.client_group_id, a.state, a.severity, " +
	"a.summary, a.description, a.value, a.started_at, a.fired_at, a.resolved_at, a.updated_at, " +
	"a.firing_notified_at, a.resolved_notified_at"

const alertSelect = "SELECT " + alertColumns + " FROM alerts a JOIN alert_rules r ON r.id = a.rule_id"

func scanAlert(s scanner) (Alert, error) {
	var (
		a                   Alert
		labels              string
		site, group         *string
		state, severity     string
		firedAt, resolvedAt *time.Time
		firedNote, resNote  *time.Time
	)
	err := s.Scan(&a.ID, &a.RuleID, &a.RuleName, &a.Fingerprint, &labels, &site, &group, &state, &severity,
		&a.Summary, &a.Description, &a.Value, &a.StartedAt, &firedAt, &resolvedAt, &a.UpdatedAt, &firedNote, &resNote)
	if err != nil {
		return Alert{}, err
	}
	a.State, a.Severity = AlertState(state), AlertSeverity(severity)
	a.FiredAt, a.ResolvedAt, a.FiringNotifiedAt, a.ResolvedNotifiedAt = firedAt, resolvedAt, firedNote, resNote
	if site != nil {
		a.SiteID = *site
	}
	if group != nil {
		a.ClientGroupID = *group
	}
	if a.Labels, err = decodeStringMap(labels); err != nil {
		return Alert{}, fmt.Errorf("alert %s labels: %w", a.ID, err)
	}
	return a, nil
}

// Insert adds a new alert, filling in its ID and UpdatedAt. It returns
// ErrConflict if the rule already has an unresolved alert for the series
// (the fingerprint): that is the deduplication, so callers update the
// existing alert instead.
func (r Alerts) Insert(ctx context.Context, a *Alert) error {
	created := *a
	created.ID = alertIDs.NewAt(time.Now())
	created.UpdatedAt = now()
	labels, err := encodeStringMap(created.Labels)
	if err != nil {
		return fmt.Errorf("insert alert: %w", err)
	}
	_, err = r.q.ExecContext(ctx,
		"INSERT INTO alerts (id, rule_id, fingerprint, labels, site_id, client_group_id, state, severity, summary, description, "+
			"value, started_at, fired_at, resolved_at, updated_at, firing_notified_at, resolved_notified_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		created.ID, created.RuleID, created.Fingerprint, labels, nullString(created.SiteID), nullString(created.ClientGroupID),
		string(created.State), string(created.Severity), created.Summary, created.Description, created.Value,
		created.StartedAt, created.FiredAt, created.ResolvedAt, created.UpdatedAt, created.FiringNotifiedAt, created.ResolvedNotifiedAt)
	if err != nil {
		return fmt.Errorf("insert alert for rule %s: %w", created.RuleID, err)
	}
	*a = created
	return nil
}

// Get returns the alert with the given ID, or ErrNotFound.
func (r Alerts) Get(ctx context.Context, alertID string) (Alert, error) {
	a, err := scanAlert(r.q.QueryRowContext(ctx, alertSelect+" WHERE a.id = ?", alertID))
	if err != nil {
		return Alert{}, fmt.Errorf("get alert %s: %w", alertID, err)
	}
	return a, nil
}

// Unresolved returns every pending and firing alert, oldest first.
func (r Alerts) Unresolved(ctx context.Context) ([]Alert, error) {
	return r.list(ctx, alertSelect+" WHERE a.state <> ? ORDER BY a.id", string(AlertResolved))
}

// Unnotified returns the alerts with an event the notifier hasn't accepted:
// one that fired, or resolved, without its notification. Oldest first.
func (r Alerts) Unnotified(ctx context.Context) ([]Alert, error) {
	return r.list(ctx, alertSelect+" WHERE (a.fired_at IS NOT NULL AND a.firing_notified_at IS NULL) "+
		"OR (a.resolved_at IS NOT NULL AND a.resolved_notified_at IS NULL) ORDER BY a.id")
}

func (r Alerts) list(ctx context.Context, query string, args ...any) ([]Alert, error) {
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	alerts := []Alert{}
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, fmt.Errorf("list alerts: %w", err)
		}
		alerts = append(alerts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	return alerts, nil
}

// Refresh records a new evaluation of an unresolved alert: its latest
// value, and its rendered summary, description, and severity, which follow
// edits of the rule. It returns ErrNotFound if the alert doesn't exist or
// has resolved.
func (r Alerts) Refresh(ctx context.Context, a *Alert) error {
	updated := now()
	res, err := r.q.ExecContext(ctx,
		"UPDATE alerts SET value = ?, severity = ?, summary = ?, description = ?, updated_at = ? WHERE id = ? AND state <> ?",
		a.Value, string(a.Severity), a.Summary, a.Description, updated, a.ID, string(AlertResolved))
	if err := affected(res, err, "refresh alert "+a.ID); err != nil {
		return err
	}
	a.UpdatedAt = updated
	return nil
}

// Fire moves a pending alert to firing at the given time. It returns
// ErrNotFound if the alert isn't pending.
func (r Alerts) Fire(ctx context.Context, a *Alert, at time.Time) error {
	at = utc(at)
	res, err := r.q.ExecContext(ctx,
		"UPDATE alerts SET state = ?, fired_at = ?, value = ?, severity = ?, summary = ?, description = ?, updated_at = ? "+
			"WHERE id = ? AND state = ?",
		string(AlertFiring), at, a.Value, string(a.Severity), a.Summary, a.Description, at, a.ID, string(AlertPending))
	if err := affected(res, err, "fire alert "+a.ID); err != nil {
		return err
	}
	a.State, a.FiredAt, a.UpdatedAt = AlertFiring, &at, at
	return nil
}

// Resolve moves a firing alert to resolved at the given time. It returns
// ErrNotFound if the alert isn't firing, so an alert resolves, and emits
// its resolved event, only once.
func (r Alerts) Resolve(ctx context.Context, a *Alert, at time.Time) error {
	at = utc(at)
	res, err := r.q.ExecContext(ctx,
		"UPDATE alerts SET state = ?, resolved_at = ?, updated_at = ? WHERE id = ? AND state = ?",
		string(AlertResolved), at, at, a.ID, string(AlertFiring))
	if err := affected(res, err, "resolve alert "+a.ID); err != nil {
		return err
	}
	a.State, a.ResolvedAt, a.UpdatedAt = AlertResolved, &at, at
	return nil
}

// DeletePending removes a pending alert whose condition stopped holding
// before it fired; it never sent anything, so it leaves no history. It
// returns ErrNotFound if the alert isn't pending.
func (r Alerts) DeletePending(ctx context.Context, alertID string) error {
	res, err := r.q.ExecContext(ctx, "DELETE FROM alerts WHERE id = ? AND state = ?", alertID, string(AlertPending))
	return affected(res, err, "delete pending alert "+alertID)
}

// MarkNotified records that the notifier accepted an alert's event. It
// returns ErrNotFound if the alert doesn't exist.
func (r Alerts) MarkNotified(ctx context.Context, alertID string, event AlertEventType, at time.Time) error {
	column := "firing_notified_at"
	if event == AlertEventResolved {
		column = "resolved_notified_at"
	}
	res, err := r.q.ExecContext(ctx, "UPDATE alerts SET "+column+" = ? WHERE id = ?", utc(at), alertID)
	return affected(res, err, "mark alert "+alertID+" notified")
}

// DeleteResolvedBefore removes alerts that resolved before cutoff, and
// returns how many.
func (r Alerts) DeleteResolvedBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := r.q.ExecContext(ctx, "DELETE FROM alerts WHERE state = ? AND resolved_at < ?", string(AlertResolved), cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete resolved alerts: %w", err)
	}
	return res.RowsAffected()
}

// affected turns an UPDATE or DELETE result into ErrNotFound when it
// changed no row.
func affected(res interface{ RowsAffected() (int64, error) }, err error, what string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return nil
}

// AlertFilter selects alerts for List. Zero fields select everything.
type AlertFilter struct {
	State         AlertState
	RuleID        string
	SiteID        string
	ClientGroupID string
	// Since and Until, if set, bound the time the alert started: at or
	// after Since, before Until.
	Since, Until time.Time
	// Before is the pagination cursor: only alerts with a smaller ID.
	Before string
	// Limit caps the number of alerts returned; zero means no limit.
	Limit int
}

// List returns alerts, newest first.
func (r Alerts) List(ctx context.Context, f AlertFilter) ([]Alert, error) {
	var (
		where []string
		args  []any
	)
	add := func(cond string, arg any) {
		where = append(where, cond)
		args = append(args, arg)
	}
	if f.State != "" {
		add("a.state = ?", string(f.State))
	}
	if f.RuleID != "" {
		add("a.rule_id = ?", f.RuleID)
	}
	if f.SiteID != "" {
		add("a.site_id = ?", f.SiteID)
	}
	if f.ClientGroupID != "" {
		add("a.client_group_id = ?", f.ClientGroupID)
	}
	if !f.Since.IsZero() {
		add("a.started_at >= ?", f.Since)
	}
	if !f.Until.IsZero() {
		add("a.started_at < ?", f.Until)
	}
	if f.Before != "" {
		add("a.id < ?", f.Before)
	}
	query := alertSelect
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY a.id DESC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}
	return r.list(ctx, query, args...)
}
