package store

import (
	"context"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// AlertChannelType says how a channel delivers alert events.
type AlertChannelType string

// Alert channel types (design doc §7.3).
const (
	ChannelEmail   AlertChannelType = "email"
	ChannelWebhook AlertChannelType = "webhook"
)

// Valid reports whether t is a known channel type.
func (t AlertChannelType) Valid() bool { return t == ChannelEmail || t == ChannelWebhook }

// SeverityRank orders severities: info, warning, critical. Unknown
// severities rank below info.
func SeverityRank(s AlertSeverity) int {
	switch s {
	case SeverityInfo:
		return 1
	case SeverityWarning:
		return 2
	case SeverityCritical:
		return 3
	}
	return 0
}

// AlertChannel is somewhere alert events are delivered. Package
// alertchannels defines what Config and the secret hold for each type.
type AlertChannel struct {
	ID      string
	Name    string
	Type    AlertChannelType
	Enabled bool
	// MinSeverity is the lowest severity the channel receives.
	MinSeverity AlertSeverity
	// RuleID and ClientGroupID, if set, limit the channel to the alerts of
	// that rule or client group.
	RuleID        string
	ClientGroupID string
	// Config is a JSON object of the settings that aren't secret.
	Config string
	// SecretRef is the secrets table row holding the channel's credentials,
	// empty if it has none.
	SecretRef string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AlertChannels reads and writes alert channels.
type AlertChannels struct {
	q Querier
}

// NewAlertChannels returns an AlertChannels that runs queries on q.
func NewAlertChannels(q Querier) AlertChannels { return AlertChannels{q: q} }

const alertChannelColumns = "id, name, type, enabled, min_severity, rule_id, client_group_id, config, secret_ref, created_at, updated_at"

func scanAlertChannel(s scanner) (AlertChannel, error) {
	var (
		c                    AlertChannel
		typ, severity        string
		rule, group, secrets *string
	)
	err := s.Scan(&c.ID, &c.Name, &typ, &c.Enabled, &severity, &rule, &group, &c.Config, &secrets, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return AlertChannel{}, err
	}
	c.Type, c.MinSeverity = AlertChannelType(typ), AlertSeverity(severity)
	if rule != nil {
		c.RuleID = *rule
	}
	if group != nil {
		c.ClientGroupID = *group
	}
	if secrets != nil {
		c.SecretRef = *secrets
	}
	return c, nil
}

// Create inserts c, filling in its ID and timestamps. It returns ErrConflict
// if the name is taken.
func (r AlertChannels) Create(ctx context.Context, c *AlertChannel) error {
	created := *c
	created.ID = id.New()
	created.CreatedAt = now()
	created.UpdatedAt = created.CreatedAt
	_, err := r.q.ExecContext(ctx,
		"INSERT INTO alert_channels ("+alertChannelColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		created.ID, created.Name, string(created.Type), created.Enabled, string(created.MinSeverity),
		nullString(created.RuleID), nullString(created.ClientGroupID), created.Config, nullString(created.SecretRef),
		created.CreatedAt, created.UpdatedAt)
	if err != nil {
		return fmt.Errorf("create alert channel: %w", err)
	}
	*c = created
	return nil
}

// Get returns the channel with the given ID, or ErrNotFound.
func (r AlertChannels) Get(ctx context.Context, channelID string) (AlertChannel, error) {
	c, err := scanAlertChannel(r.q.QueryRowContext(ctx, "SELECT "+alertChannelColumns+" FROM alert_channels WHERE id = ?", channelID))
	if err != nil {
		return AlertChannel{}, fmt.Errorf("get alert channel %s: %w", channelID, err)
	}
	return c, nil
}

// AlertChannelFilter selects and pages through channels in List's order.
type AlertChannelFilter struct {
	// Type, if set, only returns channels of this type.
	Type AlertChannelType
	// Enabled, if set, only returns channels that are enabled or disabled.
	Enabled *bool
	// AfterName and AfterID, if AfterID is set, are the pagination cursor.
	AfterName, AfterID string
	// Limit caps the number of channels returned; zero means no limit.
	Limit int
}

// List returns channels ordered by name, then ID.
func (r AlertChannels) List(ctx context.Context, f AlertChannelFilter) ([]AlertChannel, error) {
	query, args := "SELECT "+alertChannelColumns+" FROM alert_channels WHERE 1 = 1", []any{}
	if f.Type != "" {
		query += " AND type = ?"
		args = append(args, string(f.Type))
	}
	if f.Enabled != nil {
		query += " AND enabled = ?"
		args = append(args, *f.Enabled)
	}
	if f.AfterID != "" {
		query += " AND (name > ? OR (name = ? AND id > ?))"
		args = append(args, f.AfterName, f.AfterName, f.AfterID)
	}
	query += " ORDER BY name, id"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list alert channels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	channels := []AlertChannel{}
	for rows.Next() {
		c, err := scanAlertChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("list alert channels: %w", err)
		}
		channels = append(channels, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list alert channels: %w", err)
	}
	return channels, nil
}

// Update saves everything about c except its ID and creation time, and sets
// UpdatedAt. It returns ErrNotFound if the channel doesn't exist and
// ErrConflict if the name is taken.
func (r AlertChannels) Update(ctx context.Context, c *AlertChannel) error {
	updated := now()
	var createdAt time.Time
	err := r.q.QueryRowContext(ctx,
		"UPDATE alert_channels SET name = ?, type = ?, enabled = ?, min_severity = ?, rule_id = ?, client_group_id = ?, "+
			"config = ?, secret_ref = ?, updated_at = ? WHERE id = ? RETURNING created_at",
		c.Name, string(c.Type), c.Enabled, string(c.MinSeverity), nullString(c.RuleID), nullString(c.ClientGroupID),
		c.Config, nullString(c.SecretRef), updated, c.ID).Scan(&createdAt)
	if err != nil {
		return fmt.Errorf("update alert channel %s: %w", c.ID, err)
	}
	c.CreatedAt, c.UpdatedAt = createdAt, updated
	return nil
}

// Delete removes a channel and its deliveries. It returns ErrNotFound if the
// channel doesn't exist. The caller deletes the channel's secret afterwards.
func (r AlertChannels) Delete(ctx context.Context, channelID string) error {
	res, err := r.q.ExecContext(ctx, "DELETE FROM alert_channels WHERE id = ?", channelID)
	return affected(res, err, "delete alert channel "+channelID)
}

// DeliveryState is where a delivery of an alert event to a channel stands.
type DeliveryState string

// Delivery states.
const (
	// DeliveryPending deliveries are tried again at NextAttemptAt.
	DeliveryPending DeliveryState = "pending"
	// DeliveryDelivered deliveries were accepted by the channel.
	DeliveryDelivered DeliveryState = "delivered"
	// DeliveryFailed deliveries ran out of attempts.
	DeliveryFailed DeliveryState = "failed"
)

// Valid reports whether s is a known state.
func (s DeliveryState) Valid() bool {
	return s == DeliveryPending || s == DeliveryDelivered || s == DeliveryFailed
}

// AlertDelivery is one alert event bound for one channel.
type AlertDelivery struct {
	ID        string
	AlertID   string
	EventType AlertEventType
	ChannelID string
	// ChannelName is filled in by Get, List, and Due.
	ChannelName string
	State       DeliveryState
	Attempts    int
	// LastError describes the latest failed attempt; it never holds
	// credentials.
	LastError     string
	NextAttemptAt time.Time
	DeliveredAt   *time.Time
	// Event is the JSON snapshot of the event, as the channel service
	// encodes it.
	Event     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AlertDeliveries reads and writes the deliveries of alert events.
type AlertDeliveries struct {
	q Querier
}

// NewAlertDeliveries returns an AlertDeliveries that runs queries on q.
func NewAlertDeliveries(q Querier) AlertDeliveries { return AlertDeliveries{q: q} }

const alertDeliveryColumns = "d.id, d.alert_id, d.event_type, d.channel_id, c.name, d.state, d.attempts, d.last_error, " +
	"d.next_attempt_at, d.delivered_at, d.event, d.created_at, d.updated_at"

const alertDeliverySelect = "SELECT " + alertDeliveryColumns + " FROM alert_deliveries d JOIN alert_channels c ON c.id = d.channel_id"

func scanAlertDelivery(s scanner) (AlertDelivery, error) {
	var (
		d            AlertDelivery
		event, state string
	)
	err := s.Scan(&d.ID, &d.AlertID, &event, &d.ChannelID, &d.ChannelName, &state, &d.Attempts, &d.LastError,
		&d.NextAttemptAt, &d.DeliveredAt, &d.Event, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return AlertDelivery{}, err
	}
	d.EventType, d.State = AlertEventType(event), DeliveryState(state)
	return d, nil
}

// Enqueue records that an alert event is to be delivered to a channel, due
// now. It reports whether it created the delivery: false means the pair
// already had one (the event was offered before), so nothing is sent twice.
// It returns ErrReference if the alert or channel doesn't exist.
func (r AlertDeliveries) Enqueue(ctx context.Context, alertID string, event AlertEventType, channelID, snapshot string) (bool, error) {
	t := now()
	res, err := r.q.ExecContext(ctx,
		"INSERT INTO alert_deliveries (id, alert_id, event_type, channel_id, state, attempts, last_error, next_attempt_at, event, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, 0, '', ?, ?, ?, ?) ON CONFLICT (alert_id, event_type, channel_id) DO NOTHING",
		id.New(), alertID, string(event), channelID, string(DeliveryPending), t, snapshot, t, t)
	if err != nil {
		return false, fmt.Errorf("enqueue alert delivery: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("enqueue alert delivery: %w", err)
	}
	return n > 0, nil
}

// Due returns pending deliveries whose next attempt is at or before at,
// oldest first (so an alert's firing event goes before its resolved event).
func (r AlertDeliveries) Due(ctx context.Context, at time.Time, limit int) ([]AlertDelivery, error) {
	return r.list(ctx, alertDeliverySelect+" WHERE d.state = ? AND d.next_attempt_at <= ? ORDER BY d.id LIMIT ?",
		string(DeliveryPending), utc(at), limit)
}

// Get returns a delivery, or ErrNotFound.
func (r AlertDeliveries) Get(ctx context.Context, deliveryID string) (AlertDelivery, error) {
	d, err := scanAlertDelivery(r.q.QueryRowContext(ctx, alertDeliverySelect+" WHERE d.id = ?", deliveryID))
	if err != nil {
		return AlertDelivery{}, fmt.Errorf("get alert delivery %s: %w", deliveryID, err)
	}
	return d, nil
}

// AlertDeliveryFilter selects and pages through deliveries, newest first.
type AlertDeliveryFilter struct {
	AlertID   string
	ChannelID string
	State     DeliveryState
	// Before, if set, is the cursor: only deliveries with a smaller ID.
	Before string
	// Limit caps the number returned; zero means no limit.
	Limit int
}

// List returns deliveries, newest first.
func (r AlertDeliveries) List(ctx context.Context, f AlertDeliveryFilter) ([]AlertDelivery, error) {
	query, args := alertDeliverySelect+" WHERE 1 = 1", []any{}
	if f.AlertID != "" {
		query += " AND d.alert_id = ?"
		args = append(args, f.AlertID)
	}
	if f.ChannelID != "" {
		query += " AND d.channel_id = ?"
		args = append(args, f.ChannelID)
	}
	if f.State != "" {
		query += " AND d.state = ?"
		args = append(args, string(f.State))
	}
	if f.Before != "" {
		query += " AND d.id < ?"
		args = append(args, f.Before)
	}
	query += " ORDER BY d.id DESC"
	if f.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, f.Limit)
	}
	return r.list(ctx, query, args...)
}

func (r AlertDeliveries) list(ctx context.Context, query string, args ...any) ([]AlertDelivery, error) {
	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list alert deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []AlertDelivery{}
	for rows.Next() {
		d, err := scanAlertDelivery(rows)
		if err != nil {
			return nil, fmt.Errorf("list alert deliveries: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list alert deliveries: %w", err)
	}
	return out, nil
}

// MarkDelivered records that the channel accepted the delivery, as attempt
// number attempts. It returns ErrNotFound if the delivery is gone.
func (r AlertDeliveries) MarkDelivered(ctx context.Context, deliveryID string, attempts int, at time.Time) error {
	res, err := r.q.ExecContext(ctx,
		"UPDATE alert_deliveries SET state = ?, attempts = ?, last_error = '', delivered_at = ?, updated_at = ? WHERE id = ?",
		string(DeliveryDelivered), attempts, utc(at), utc(at), deliveryID)
	return affected(res, err, "mark alert delivery "+deliveryID+" delivered")
}

// MarkAttemptFailed records a failed attempt (number attempts). If next is
// the zero time the delivery is out of attempts and becomes failed;
// otherwise it stays pending and is due again at next.
func (r AlertDeliveries) MarkAttemptFailed(ctx context.Context, deliveryID string, attempts int, lastError string, next, at time.Time) error {
	state := DeliveryFailed
	if !next.IsZero() {
		state = DeliveryPending
	} else {
		next = at
	}
	res, err := r.q.ExecContext(ctx,
		"UPDATE alert_deliveries SET state = ?, attempts = ?, last_error = ?, next_attempt_at = ?, updated_at = ? WHERE id = ?",
		string(state), attempts, lastError, utc(next), utc(at), deliveryID)
	return affected(res, err, "mark alert delivery "+deliveryID+" failed")
}
