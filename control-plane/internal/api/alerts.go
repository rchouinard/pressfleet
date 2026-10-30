package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Alert rules and alerts (#75, design doc §7.3). Rules are platform
// settings that Operators and Admins manage; every role reads them and the
// alerts they raise. Evaluation is package alerts' engine, not the API's.

// ListAlertRules lists alert rules by name. The cursor encodes the name and
// ID of the last rule on the previous page.
func (h *Handlers) ListAlertRules(ctx context.Context, req gen.ListAlertRulesRequestObject) (gen.ListAlertRulesResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.Platform()); err != nil {
		return nil, err
	}
	p := req.Params
	limit := pageLimit(p.Limit)
	f := store.AlertRuleFilter{
		Scope:         store.AlertScope(deref(p.Scope)),
		ClientGroupID: deref(p.ClientGroupID),
		Enabled:       p.Enabled,
		Limit:         limit + 1,
	}
	if c := p.Cursor; c != nil {
		var ok bool
		if f.AfterName, f.AfterID, ok = decodeNameCursor(*c); !ok {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "The cursor is invalid; pass the next_cursor of a previous page.")
		}
	}
	rules, err := store.NewAlertRules(h.db).List(ctx, f)
	if err != nil {
		return nil, err
	}
	rules, next := page(rules, limit, func(r store.AlertRule) string { return encodeNameCursor(r.Name, r.ID) })
	body := gen.AlertRuleList{Items: make([]gen.AlertRule, len(rules)), NextCursor: next}
	for i, r := range rules {
		body.Items[i] = alertRule(r)
	}
	return gen.ListAlertRules200JSONResponse(body), nil
}

// encodeNameCursor makes the opaque cursor for the page after the item with
// this name and ID.
func encodeNameCursor(name, itemID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(itemID + ":" + name))
}

func decodeNameCursor(c string) (name, itemID string, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", "", false
	}
	itemID, name, ok = strings.Cut(string(b), ":")
	return name, itemID, ok && id.Valid(itemID)
}

// CreateAlertRule creates an alert rule.
func (h *Handlers) CreateAlertRule(ctx context.Context, req gen.CreateAlertRuleRequestObject) (gen.CreateAlertRuleResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ManageAlertRules, authz.Platform()); err != nil {
		return nil, err
	}
	b := req.Body
	rule := store.AlertRule{
		Name:           b.Name,
		Description:    deref(b.Description),
		Scope:          store.AlertScope(b.Scope),
		ClientGroupID:  deref(b.ClientGroupID),
		Kind:           store.AlertRuleKind(b.Kind),
		Expr:           b.Expr,
		Operator:       store.AlertOperator(deref(b.Operator)),
		Threshold:      deref(b.Threshold),
		Severity:       store.AlertSeverity(b.Severity),
		Enabled:        b.Enabled == nil || *b.Enabled,
		IncludeStaging: deref(b.IncludeStaging),
		Labels:         deref(b.Labels),
		Annotations:    deref(b.Annotations),
	}
	rule.For = secondsDuration(deref(b.ForSeconds))
	if err := checkAlertRule(&rule); err != nil {
		return nil, err
	}
	if err := store.NewAlertRules(h.db).Create(ctx, &rule); err != nil {
		return nil, alertRuleError(err, rule.Name)
	}
	setAuditTarget(ctx, authz.AlertRule(rule.ID))
	location := "/api/v1/alert-rules/" + rule.ID
	return gen.CreateAlertRule201JSONResponse{
		Body:    alertRule(rule),
		Headers: gen.CreateAlertRule201ResponseHeaders{Location: &location},
	}, nil
}

// GetAlertRule returns an alert rule.
func (h *Handlers) GetAlertRule(ctx context.Context, req gen.GetAlertRuleRequestObject) (gen.GetAlertRuleResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.AlertRule(req.AlertRuleID)); err != nil {
		return nil, err
	}
	rule, err := store.NewAlertRules(h.db).Get(ctx, req.AlertRuleID)
	if err != nil {
		return nil, alertRuleError(err, "")
	}
	return gen.GetAlertRule200JSONResponse(alertRule(rule)), nil
}

// UpdateAlertRule changes the fields of an alert rule that are present.
func (h *Handlers) UpdateAlertRule(ctx context.Context, req gen.UpdateAlertRuleRequestObject) (gen.UpdateAlertRuleResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ManageAlertRules, authz.AlertRule(req.AlertRuleID)); err != nil {
		return nil, err
	}
	b := req.Body
	var rule store.AlertRule
	err := h.db.InTx(ctx, func(q store.Querier) error {
		rules := store.NewAlertRules(q)
		var err error
		if rule, err = rules.Get(ctx, req.AlertRuleID); err != nil {
			return err
		}
		if v := b.Name; v != nil {
			rule.Name = *v
		}
		if v := b.Description; v != nil {
			rule.Description = *v
		}
		if v := b.Scope; v != nil {
			rule.Scope = store.AlertScope(*v)
		}
		if v := b.ClientGroupID; v != nil {
			rule.ClientGroupID = *v
		}
		if v := b.Kind; v != nil {
			rule.Kind = store.AlertRuleKind(*v)
		}
		if v := b.Expr; v != nil {
			rule.Expr = *v
		}
		if v := b.Operator; v != nil {
			rule.Operator = store.AlertOperator(*v)
		}
		if v := b.Threshold; v != nil {
			rule.Threshold = *v
		}
		if v := b.ForSeconds; v != nil {
			rule.For = secondsDuration(*v)
		}
		if v := b.Severity; v != nil {
			rule.Severity = store.AlertSeverity(*v)
		}
		if v := b.IncludeStaging; v != nil {
			rule.IncludeStaging = *v
		}
		if v := b.Labels; v != nil {
			rule.Labels = *v
		}
		if v := b.Annotations; v != nil {
			rule.Annotations = *v
		}
		if err := checkAlertRule(&rule); err != nil {
			return err
		}
		return rules.Update(ctx, &rule)
	})
	if err != nil {
		return nil, alertRuleError(err, rule.Name)
	}
	return gen.UpdateAlertRule200JSONResponse(alertRule(rule)), nil
}

// DeleteAlertRule deletes an alert rule and its alerts. Built-in rules
// can't be deleted.
func (h *Handlers) DeleteAlertRule(ctx context.Context, req gen.DeleteAlertRuleRequestObject) (gen.DeleteAlertRuleResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ManageAlertRules, authz.AlertRule(req.AlertRuleID)); err != nil {
		return nil, err
	}
	if err := store.NewAlertRules(h.db).Delete(ctx, req.AlertRuleID); err != nil {
		return nil, alertRuleError(err, "")
	}
	return gen.DeleteAlertRule204Response{}, nil
}

// EnableAlertRule enables an alert rule.
func (h *Handlers) EnableAlertRule(ctx context.Context, req gen.EnableAlertRuleRequestObject) (gen.EnableAlertRuleResponseObject, error) {
	rule, err := h.setAlertRuleEnabled(ctx, req.AlertRuleID, true)
	if err != nil {
		return nil, err
	}
	return gen.EnableAlertRule200JSONResponse(alertRule(rule)), nil
}

// DisableAlertRule disables an alert rule.
func (h *Handlers) DisableAlertRule(ctx context.Context, req gen.DisableAlertRuleRequestObject) (gen.DisableAlertRuleResponseObject, error) {
	rule, err := h.setAlertRuleEnabled(ctx, req.AlertRuleID, false)
	if err != nil {
		return nil, err
	}
	return gen.DisableAlertRule200JSONResponse(alertRule(rule)), nil
}

func (h *Handlers) setAlertRuleEnabled(ctx context.Context, ruleID string, enabled bool) (store.AlertRule, error) {
	if _, err := h.authorize(ctx, authz.ManageAlertRules, authz.AlertRule(ruleID)); err != nil {
		return store.AlertRule{}, err
	}
	var rule store.AlertRule
	err := h.db.InTx(ctx, func(q store.Querier) error {
		rules := store.NewAlertRules(q)
		var err error
		if rule, err = rules.Get(ctx, ruleID); err != nil {
			return err
		}
		if rule.Enabled == enabled {
			return nil
		}
		rule.Enabled = enabled
		return rules.Update(ctx, &rule)
	})
	if err != nil {
		return store.AlertRule{}, alertRuleError(err, "")
	}
	return rule, nil
}

// ListAlerts lists alerts, newest first. The cursor is the ID of the last
// alert on the previous page.
func (h *Handlers) ListAlerts(ctx context.Context, req gen.ListAlertsRequestObject) (gen.ListAlertsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.Platform()); err != nil {
		return nil, err
	}
	p := req.Params
	limit := pageLimit(p.Limit)
	f := store.AlertFilter{
		State:         store.AlertState(deref(p.State)),
		RuleID:        deref(p.RuleID),
		SiteID:        deref(p.SiteID),
		ClientGroupID: deref(p.ClientGroupID),
		Since:         deref(p.Since),
		Until:         deref(p.Until),
		Limit:         limit + 1,
	}
	if p.Cursor != nil {
		if !id.Valid(*p.Cursor) {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "The cursor is invalid; pass the next_cursor of a previous page.")
		}
		f.Before = *p.Cursor
	}
	list, err := store.NewAlerts(h.db).List(ctx, f)
	if err != nil {
		return nil, err
	}
	list, next := page(list, limit, func(a store.Alert) string { return a.ID })
	body := gen.AlertList{Items: make([]gen.Alert, len(list)), NextCursor: next}
	for i, a := range list {
		body.Items[i] = alert(a)
	}
	return gen.ListAlerts200JSONResponse(body), nil
}

// GetAlert returns an alert.
func (h *Handlers) GetAlert(ctx context.Context, req gen.GetAlertRequestObject) (gen.GetAlertResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewMonitoring, authz.Alert(req.AlertID)); err != nil {
		return nil, err
	}
	a, err := store.NewAlerts(h.db).Get(ctx, req.AlertID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("alert: %w", ErrNotFound)
		}
		return nil, err
	}
	return gen.GetAlert200JSONResponse(alert(a)), nil
}

// checkAlertRule normalizes a rule and validates it. An expression rule has
// no operator or threshold, whatever the request said.
func checkAlertRule(r *store.AlertRule) error {
	if r.Kind == store.AlertKindExpression {
		r.Operator, r.Threshold = "", 0
	}
	if err := alerts.ValidateRule(*r); err != nil {
		var ve *alerts.ValidationError
		if errors.As(err, &ve) {
			out := &ValidationError{Fields: make([]httpserver.FieldError, len(ve.Fields))}
			for i, f := range ve.Fields {
				out.Fields[i] = httpserver.FieldError{Field: f.Field, Detail: f.Detail}
			}
			return out
		}
		return err
	}
	return nil
}

// alertRuleError maps store errors to API errors.
func alertRuleError(err error, name string) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("alert rule: %w", ErrNotFound)
	case errors.Is(err, store.ErrConflict):
		return fmt.Errorf("%w: an alert rule named %q already exists", ErrConflict, name)
	case errors.Is(err, store.ErrBuiltinRule):
		return fmt.Errorf("%w: built-in alert rules can't be deleted; disable the rule instead", ErrConflict)
	case errors.Is(err, store.ErrReference):
		return &ValidationError{Fields: []httpserver.FieldError{{Field: "/client_group_id", Detail: "no such client group"}}}
	}
	return err
}

func alertRule(r store.AlertRule) gen.AlertRule {
	out := gen.AlertRule{
		ID:             r.ID,
		Name:           r.Name,
		Description:    r.Description,
		Scope:          gen.AlertScope(r.Scope),
		ClientGroupID:  optional(r.ClientGroupID),
		Kind:           gen.AlertRuleKind(r.Kind),
		Expr:           r.Expr,
		ForSeconds:     int(r.For.Seconds()),
		Severity:       gen.AlertSeverity(r.Severity),
		Enabled:        r.Enabled,
		IncludeStaging: r.IncludeStaging,
		Labels:         nonNil(r.Labels),
		Annotations:    nonNil(r.Annotations),
		Builtin:        r.Builtin(),
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
	if r.Kind == store.AlertKindThreshold {
		op, threshold := gen.AlertOperator(r.Operator), r.Threshold
		out.Operator, out.Threshold = &op, &threshold
	}
	return out
}

func alert(a store.Alert) gen.Alert {
	return gen.Alert{
		ID:            a.ID,
		RuleID:        a.RuleID,
		RuleName:      a.RuleName,
		State:         gen.AlertState(a.State),
		Severity:      gen.AlertSeverity(a.Severity),
		Labels:        nonNil(a.Labels),
		SiteID:        optional(a.SiteID),
		ClientGroupID: optional(a.ClientGroupID),
		Summary:       a.Summary,
		Description:   a.Description,
		Value:         a.Value,
		StartedAt:     a.StartedAt,
		FiredAt:       a.FiredAt,
		ResolvedAt:    a.ResolvedAt,
		UpdatedAt:     a.UpdatedAt,
	}
}

// nonNil returns m, or an empty map for nil, so JSON objects aren't null.
func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func secondsDuration(n int) time.Duration { return time.Duration(n) * time.Second }
