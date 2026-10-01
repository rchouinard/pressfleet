package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
)

// AlertScope says what an alert rule watches (design doc §4.3).
type AlertScope string

// Alert rule scopes.
const (
	// AlertScopePlatform rules watch the platform itself: the host, shared
	// services, and the control plane. Their series don't name a site.
	AlertScopePlatform AlertScope = "platform"
	// AlertScopeSite rules watch every site; each series must carry a
	// site_id label, and an alert is raised per site.
	AlertScopeSite AlertScope = "site"
)

// Valid reports whether s is a known scope.
func (s AlertScope) Valid() bool { return s == AlertScopePlatform || s == AlertScopeSite }

// AlertRuleKind says how an expression's result becomes a condition.
type AlertRuleKind string

// Alert rule kinds.
const (
	// AlertKindThreshold compares the value of every series the expression
	// returns with the rule's threshold.
	AlertKindThreshold AlertRuleKind = "threshold"
	// AlertKindExpression treats every series the expression returns as a
	// true condition; the expression itself filters, like "up == 0".
	AlertKindExpression AlertRuleKind = "expression"
)

// Valid reports whether k is a known kind.
func (k AlertRuleKind) Valid() bool { return k == AlertKindThreshold || k == AlertKindExpression }

// AlertOperator compares a sample's value with a rule's threshold.
type AlertOperator string

// Alert operators.
const (
	OpGreater      AlertOperator = "gt"
	OpGreaterEqual AlertOperator = "gte"
	OpLess         AlertOperator = "lt"
	OpLessEqual    AlertOperator = "lte"
	OpEqual        AlertOperator = "eq"
	OpNotEqual     AlertOperator = "ne"
)

// Valid reports whether o is a known operator.
func (o AlertOperator) Valid() bool {
	switch o {
	case OpGreater, OpGreaterEqual, OpLess, OpLessEqual, OpEqual, OpNotEqual:
		return true
	}
	return false
}

// Compare reports whether value o threshold holds.
func (o AlertOperator) Compare(value, threshold float64) bool {
	switch o {
	case OpGreater:
		return value > threshold
	case OpGreaterEqual:
		return value >= threshold
	case OpLess:
		return value < threshold
	case OpLessEqual:
		return value <= threshold
	case OpEqual:
		return value == threshold
	case OpNotEqual:
		return value != threshold
	}
	return false
}

// AlertSeverity is how urgent an alert is.
type AlertSeverity string

// Alert severities.
const (
	SeverityInfo     AlertSeverity = "info"
	SeverityWarning  AlertSeverity = "warning"
	SeverityCritical AlertSeverity = "critical"
)

// Valid reports whether s is a known severity.
func (s AlertSeverity) Valid() bool {
	return s == SeverityInfo || s == SeverityWarning || s == SeverityCritical
}

// ErrBuiltinRule means the operation isn't allowed on a built-in alert
// rule: they can be edited and disabled, but not deleted.
var ErrBuiltinRule = errors.New("built-in alert rules can't be deleted")

// AlertRule is a condition on metrics that raises alerts (design doc §4.3,
// §7.3). Package alerts validates rules and evaluates them.
type AlertRule struct {
	ID          string
	Name        string
	Description string
	Scope       AlertScope
	// ClientGroupID limits a site-scoped rule to the sites of one client
	// group; empty means every site.
	ClientGroupID string
	Kind          AlertRuleKind
	// Expr is the PromQL-style expression evaluated against VictoriaMetrics.
	Expr string
	// Operator and Threshold apply to threshold rules only.
	Operator  AlertOperator
	Threshold float64
	// For is how long the condition must hold before the alert fires.
	For      time.Duration
	Severity AlertSeverity
	Enabled  bool
	// Labels are added to every alert the rule raises.
	Labels map[string]string
	// Annotations hold the summary and description templates, keyed
	// "summary" and "description".
	Annotations map[string]string
	// BuiltinKey is the stable key of a default rule, empty for rules users
	// created. Package alerts seeds the default rules by it.
	BuiltinKey string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Builtin reports whether r is one of the default rules.
func (r AlertRule) Builtin() bool { return r.BuiltinKey != "" }

// AlertRules reads and writes alert rules.
type AlertRules struct {
	q Querier
}

// NewAlertRules returns an AlertRules that runs queries on q.
func NewAlertRules(q Querier) AlertRules {
	return AlertRules{q: q}
}

const alertRuleColumns = "id, name, description, scope, client_group_id, kind, expr, operator, threshold, for_seconds, " +
	"severity, enabled, labels, annotations, builtin_key, created_at, updated_at"

func scanAlertRule(s scanner) (AlertRule, error) {
	var (
		r                       AlertRule
		group, builtin          *string
		forSeconds              int64
		labels, annotations     string
		scope, kind, op, severe string
	)
	err := s.Scan(&r.ID, &r.Name, &r.Description, &scope, &group, &kind, &r.Expr, &op, &r.Threshold, &forSeconds,
		&severe, &r.Enabled, &labels, &annotations, &builtin, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return AlertRule{}, err
	}
	r.Scope, r.Kind, r.Operator, r.Severity = AlertScope(scope), AlertRuleKind(kind), AlertOperator(op), AlertSeverity(severe)
	r.For = time.Duration(forSeconds) * time.Second
	if group != nil {
		r.ClientGroupID = *group
	}
	if builtin != nil {
		r.BuiltinKey = *builtin
	}
	if r.Labels, err = decodeStringMap(labels); err != nil {
		return AlertRule{}, fmt.Errorf("alert rule %s labels: %w", r.ID, err)
	}
	if r.Annotations, err = decodeStringMap(annotations); err != nil {
		return AlertRule{}, fmt.Errorf("alert rule %s annotations: %w", r.ID, err)
	}
	return r, nil
}

// encodeStringMap encodes labels or annotations as a JSON object; a nil map
// is the empty object.
func encodeStringMap(m map[string]string) (string, error) {
	if m == nil {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	return string(b), err
}

func decodeStringMap(s string) (map[string]string, error) {
	m := map[string]string{}
	if s == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Create inserts r, filling in its ID and timestamps. It returns ErrConflict
// if the name (or built-in key) is taken and ErrReference if the client
// group doesn't exist. Callers validate r first (package alerts).
func (r AlertRules) Create(ctx context.Context, rule *AlertRule) error {
	created := *rule
	created.ID = id.New()
	created.CreatedAt = now()
	created.UpdatedAt = created.CreatedAt
	if err := r.insert(ctx, created); err != nil {
		return fmt.Errorf("create alert rule: %w", err)
	}
	*rule = created
	return nil
}

func (r AlertRules) insert(ctx context.Context, rule AlertRule) error {
	labels, err := encodeStringMap(rule.Labels)
	if err != nil {
		return err
	}
	annotations, err := encodeStringMap(rule.Annotations)
	if err != nil {
		return err
	}
	_, err = r.q.ExecContext(ctx,
		"INSERT INTO alert_rules ("+alertRuleColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		rule.ID, rule.Name, rule.Description, string(rule.Scope), nullString(rule.ClientGroupID), string(rule.Kind),
		rule.Expr, string(rule.Operator), rule.Threshold, int64(rule.For/time.Second), string(rule.Severity),
		rule.Enabled, labels, annotations, nullString(rule.BuiltinKey), rule.CreatedAt, rule.UpdatedAt)
	return err
}

// EnsureBuiltin inserts a default rule unless a rule with its BuiltinKey
// exists, whatever has become of that rule since: a user's edits, including
// disabling it, are never overwritten. It reports whether it inserted the
// rule. If a rule a user created already has the name, the new one gets
// the suffix " (built-in)".
func (r AlertRules) EnsureBuiltin(ctx context.Context, rule *AlertRule) (bool, error) {
	if rule.BuiltinKey == "" {
		return false, errors.New("ensure built-in alert rule: key is required")
	}
	var one int
	err := r.q.QueryRowContext(ctx, "SELECT 1 FROM alert_rules WHERE builtin_key = ?", rule.BuiltinKey).Scan(&one)
	switch {
	case err == nil:
		return false, nil
	case !errors.Is(err, ErrNotFound):
		return false, fmt.Errorf("ensure built-in alert rule %s: %w", rule.BuiltinKey, err)
	}
	created := *rule
	created.ID = id.New()
	created.CreatedAt = now()
	created.UpdatedAt = created.CreatedAt
	err = r.insert(ctx, created)
	if errors.Is(err, ErrConflict) {
		created.Name += " (built-in)"
		err = r.insert(ctx, created)
	}
	if err != nil {
		return false, fmt.Errorf("ensure built-in alert rule %s: %w", rule.BuiltinKey, err)
	}
	*rule = created
	return true, nil
}

// Get returns the rule with the given ID, or ErrNotFound.
func (r AlertRules) Get(ctx context.Context, ruleID string) (AlertRule, error) {
	rule, err := scanAlertRule(r.q.QueryRowContext(ctx, "SELECT "+alertRuleColumns+" FROM alert_rules WHERE id = ?", ruleID))
	if err != nil {
		return AlertRule{}, fmt.Errorf("get alert rule %s: %w", ruleID, err)
	}
	return rule, nil
}

// AlertRuleFilter selects and pages through alert rules in List's order.
type AlertRuleFilter struct {
	// Scope, if set, only returns rules of this scope.
	Scope AlertScope
	// ClientGroupID, if set, only returns rules limited to this group.
	ClientGroupID string
	// Enabled, if set, only returns rules that are enabled or disabled.
	Enabled *bool
	// AfterName and AfterID, if AfterID is set, are the pagination cursor:
	// only rules after the one with this name and ID are returned.
	AfterName, AfterID string
	// Limit caps the number of rules returned; zero means no limit.
	Limit int
}

// List returns alert rules ordered by name, then ID.
func (r AlertRules) List(ctx context.Context, f AlertRuleFilter) ([]AlertRule, error) {
	query, args := "SELECT "+alertRuleColumns+" FROM alert_rules WHERE 1 = 1", []any{}
	if f.Scope != "" {
		query += " AND scope = ?"
		args = append(args, string(f.Scope))
	}
	if f.ClientGroupID != "" {
		query += " AND client_group_id = ?"
		args = append(args, f.ClientGroupID)
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
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	defer func() { _ = rows.Close() }()
	rules := []AlertRule{}
	for rows.Next() {
		rule, err := scanAlertRule(rows)
		if err != nil {
			return nil, fmt.Errorf("list alert rules: %w", err)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	return rules, nil
}

// Update saves everything about rule except its ID, built-in key, and
// creation time, and sets UpdatedAt. It returns ErrNotFound if the rule
// doesn't exist, ErrConflict if the name is taken, and ErrReference if the
// client group doesn't exist.
func (r AlertRules) Update(ctx context.Context, rule *AlertRule) error {
	labels, err := encodeStringMap(rule.Labels)
	if err != nil {
		return fmt.Errorf("update alert rule %s: %w", rule.ID, err)
	}
	annotations, err := encodeStringMap(rule.Annotations)
	if err != nil {
		return fmt.Errorf("update alert rule %s: %w", rule.ID, err)
	}
	updated := now()
	var (
		createdAt time.Time
		builtin   *string
	)
	err = r.q.QueryRowContext(ctx,
		"UPDATE alert_rules SET name = ?, description = ?, scope = ?, client_group_id = ?, kind = ?, expr = ?, operator = ?, "+
			"threshold = ?, for_seconds = ?, severity = ?, enabled = ?, labels = ?, annotations = ?, updated_at = ? "+
			"WHERE id = ? RETURNING created_at, builtin_key",
		rule.Name, rule.Description, string(rule.Scope), nullString(rule.ClientGroupID), string(rule.Kind), rule.Expr,
		string(rule.Operator), rule.Threshold, int64(rule.For/time.Second), string(rule.Severity), rule.Enabled,
		labels, annotations, updated, rule.ID).Scan(&createdAt, &builtin)
	if err != nil {
		return fmt.Errorf("update alert rule %s: %w", rule.ID, err)
	}
	rule.CreatedAt, rule.UpdatedAt, rule.BuiltinKey = createdAt, updated, ""
	if builtin != nil {
		rule.BuiltinKey = *builtin
	}
	return nil
}

// Delete removes a rule and, with it, its alerts. It returns ErrNotFound if
// the rule doesn't exist and ErrBuiltinRule if it is a default rule.
func (r AlertRules) Delete(ctx context.Context, ruleID string) error {
	res, err := r.q.ExecContext(ctx, "DELETE FROM alert_rules WHERE id = ? AND builtin_key IS NULL", ruleID)
	if err != nil {
		return fmt.Errorf("delete alert rule %s: %w", ruleID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete alert rule %s: %w", ruleID, err)
	}
	if n > 0 {
		return nil
	}
	// Nothing deleted: either there is no such rule, or it is built in.
	rule, err := r.Get(ctx, ruleID)
	if err != nil {
		return fmt.Errorf("delete alert rule %s: %w", ruleID, err)
	}
	return fmt.Errorf("delete alert rule %s: %w", rule.ID, ErrBuiltinRule)
}
