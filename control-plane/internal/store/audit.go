package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// AuditResult is the outcome of an audited request.
type AuditResult string

// Audit results.
const (
	// AuditSuccess: the request was allowed and succeeded.
	AuditSuccess AuditResult = "success"
	// AuditFailure: the request was allowed (or never reached the
	// authorization check) and failed, e.g. validation or a conflict.
	AuditFailure AuditResult = "failure"
	// AuditDenied: the authorization check refused the request.
	AuditDenied AuditResult = "denied"
)

// Valid reports whether r is a known result.
func (r AuditResult) Valid() bool {
	return r == AuditSuccess || r == AuditFailure || r == AuditDenied
}

// AuditEvent records who did what, when (design doc §4.6). It never holds
// request bodies or secrets: only IDs, the action name, and the client
// address. Events have no foreign keys, so they outlive the users, tokens,
// and targets they name.
type AuditEvent struct {
	ID         string
	OccurredAt time.Time
	// ActorUserID and ActorTokenID are empty when there is no such actor:
	// no token for session requests, and neither for a failed login.
	ActorUserID  string
	ActorTokenID string
	// Action is an authorization action (package authz), or a session
	// action such as "self.login".
	Action string
	// TargetType and TargetID are empty for actions on the platform or on
	// collections.
	TargetType string
	TargetID   string
	Result     AuditResult
	SourceIP   string
	// Details are extra facts about the event, such as an SFTP session's
	// key fingerprint and bytes transferred. Nil for most events. Like the
	// rest of the event, they never hold secrets.
	Details map[string]string
}

// AuditEvents reads and writes audit events.
type AuditEvents struct {
	q Querier
}

// NewAuditEvents returns an AuditEvents that runs queries on q.
func NewAuditEvents(q Querier) AuditEvents {
	return AuditEvents{q: q}
}

const auditColumns = "id, occurred_at, actor_user_id, actor_token_id, action, target_type, target_id, result, source_ip, details"

func scanAuditEvent(s scanner) (AuditEvent, error) {
	var (
		e                      AuditEvent
		user, tokenID, details sql.NullString
	)
	err := s.Scan(&e.ID, &e.OccurredAt, &user, &tokenID, &e.Action, &e.TargetType, &e.TargetID, &e.Result, &e.SourceIP, &details)
	e.ActorUserID, e.ActorTokenID = user.String, tokenID.String
	if err == nil && details.Valid {
		if jerr := json.Unmarshal([]byte(details.String), &e.Details); jerr != nil {
			err = fmt.Errorf("audit event %s: decoding details: %w", e.ID, jerr)
		}
	}
	return e, err
}

// auditDetails encodes an event's details for the details column: a JSON
// object, or NULL when there are none.
func auditDetails(d map[string]string) (any, error) {
	if len(d) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// auditIDs orders events recorded in the same millisecond by insertion, so
// the newest-first listing (by ID) shows requests in the order they
// finished.
var auditIDs id.Monotonic

// Insert records e, filling in its ID and, if zero, OccurredAt. IDs sort in
// insertion order within this process.
func (r AuditEvents) Insert(ctx context.Context, e *AuditEvent) error {
	switch {
	case e.Action == "":
		return errors.New("insert audit event: action is required")
	case !e.Result.Valid():
		return fmt.Errorf("insert audit event: invalid result %q", e.Result)
	}
	ins := *e
	if ins.OccurredAt.IsZero() {
		ins.OccurredAt = now()
	}
	ins.OccurredAt = utc(ins.OccurredAt)
	ins.ID = auditIDs.NewAt(ins.OccurredAt)
	details, err := auditDetails(ins.Details)
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	_, err = r.q.ExecContext(ctx,
		"INSERT INTO audit_events ("+auditColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		ins.ID, ins.OccurredAt, nullString(ins.ActorUserID), nullString(ins.ActorTokenID), ins.Action,
		ins.TargetType, ins.TargetID, string(ins.Result), ins.SourceIP, details)
	if err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}
	*e = ins
	return nil
}

// AuditFilter selects audit events for List. Zero fields don't filter.
type AuditFilter struct {
	ActorUserID string
	Action      string
	TargetType  string
	TargetID    string
	// Since and Until bound OccurredAt: Since <= OccurredAt < Until.
	Since, Until time.Time
	// Before is a cursor: only events with a smaller ID (older ones) are
	// returned. Pass the previous page's NextCursor.
	Before string
	// Limit is the page size: default 50, at most 200.
	Limit int
}

// AuditPage is one page of List results, newest first.
type AuditPage struct {
	Events []AuditEvent
	// NextCursor is the Before value for the next page, or empty on the
	// last page.
	NextCursor string
}

// List returns audit events newest first.
func (r AuditEvents) List(ctx context.Context, f AuditFilter) (AuditPage, error) {
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = 50
	case limit > 200:
		limit = 200
	}
	var (
		where []string
		args  []any
	)
	for _, c := range []struct{ col, val string }{
		{"actor_user_id", f.ActorUserID}, {"action", f.Action},
		{"target_type", f.TargetType}, {"target_id", f.TargetID},
	} {
		if c.val != "" {
			where = append(where, c.col+" = ?")
			args = append(args, c.val)
		}
	}
	if !f.Since.IsZero() {
		where = append(where, "occurred_at >= ?")
		args = append(args, f.Since)
	}
	if !f.Until.IsZero() {
		where = append(where, "occurred_at < ?")
		args = append(args, f.Until)
	}
	if f.Before != "" {
		where = append(where, "id < ?")
		args = append(args, f.Before)
	}
	query := "SELECT " + auditColumns + " FROM audit_events"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// Fetch one extra row to learn whether there is another page.
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := r.q.QueryContext(ctx, query, args...)
	if err != nil {
		return AuditPage{}, fmt.Errorf("list audit events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := AuditPage{Events: []AuditEvent{}}
	for rows.Next() {
		e, err := scanAuditEvent(rows)
		if err != nil {
			return AuditPage{}, fmt.Errorf("list audit events: %w", err)
		}
		page.Events = append(page.Events, e)
	}
	if err := rows.Err(); err != nil {
		return AuditPage{}, fmt.Errorf("list audit events: %w", err)
	}
	if len(page.Events) > limit {
		page.Events = page.Events[:limit]
		page.NextCursor = page.Events[limit-1].ID
	}
	return page, nil
}

// DeleteBefore deletes the events that occurred before t, returning how
// many it deleted. It is the audit retention cleanup.
func (r AuditEvents) DeleteBefore(ctx context.Context, t time.Time) (int64, error) {
	res, err := r.q.ExecContext(ctx, "DELETE FROM audit_events WHERE occurred_at < ?", t)
	if err != nil {
		return 0, fmt.Errorf("delete audit events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete audit events: %w", err)
	}
	return n, nil
}
