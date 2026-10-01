package alerts_test

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

func TestValidateRule(t *testing.T) {
	valid := func() store.AlertRule {
		return store.AlertRule{
			Name: "Busy", Scope: store.AlertScopeSite, Kind: store.AlertKindThreshold, Expr: `sum by (site_id) (rate(x{a="("}[5m]))`,
			Operator: store.OpGreater, Threshold: 1, For: time.Minute, Severity: store.SeverityWarning,
			Labels:      map[string]string{"team": "ops"},
			Annotations: map[string]string{"summary": "{{ .Labels.site_id }} is {{ humanizePercentage .Value }}"},
		}
	}
	if err := alerts.ValidateRule(valid()); err != nil {
		t.Fatalf("valid rule: %v", err)
	}
	for _, r := range alerts.DefaultRules() {
		if err := alerts.ValidateRule(r); err != nil {
			t.Errorf("default rule %s: %v", r.BuiltinKey, err)
		}
	}

	tests := []struct {
		name   string
		change func(*store.AlertRule)
		field  string
	}{
		{"no name", func(r *store.AlertRule) { r.Name = "" }, "/name"},
		{"long name", func(r *store.AlertRule) { r.Name = strings.Repeat("x", 101) }, "/name"},
		{"long description", func(r *store.AlertRule) { r.Description = strings.Repeat("x", 1001) }, "/description"},
		{"bad scope", func(r *store.AlertRule) { r.Scope = "global" }, "/scope"},
		{"platform rule limited to a group", func(r *store.AlertRule) { r.Scope, r.ClientGroupID = store.AlertScopePlatform, "g" }, "/client_group_id"},
		{"bad kind", func(r *store.AlertRule) { r.Kind = "magic" }, "/kind"},
		{"threshold without operator", func(r *store.AlertRule) { r.Operator = "" }, "/operator"},
		{"bad operator", func(r *store.AlertRule) { r.Operator = "~" }, "/operator"},
		{"NaN threshold", func(r *store.AlertRule) { r.Threshold = math.NaN() }, "/threshold"},
		{"no expression", func(r *store.AlertRule) { r.Expr = "  " }, "/expr"},
		{"long expression", func(r *store.AlertRule) { r.Expr = strings.Repeat("x", 5000) }, "/expr"},
		{"unclosed paren", func(r *store.AlertRule) { r.Expr = "sum(x" }, "/expr"},
		{"stray bracket", func(r *store.AlertRule) { r.Expr = "x)" }, "/expr"},
		{"mismatched brackets", func(r *store.AlertRule) { r.Expr = "x{a=1)" }, "/expr"},
		{"unterminated string", func(r *store.AlertRule) { r.Expr = `x{a="b}` }, "/expr"},
		{"negative duration", func(r *store.AlertRule) { r.For = -time.Second }, "/for_seconds"},
		{"too long duration", func(r *store.AlertRule) { r.For = 8 * 24 * time.Hour }, "/for_seconds"},
		{"fractional duration", func(r *store.AlertRule) { r.For = 1500 * time.Millisecond }, "/for_seconds"},
		{"bad severity", func(r *store.AlertRule) { r.Severity = "urgent" }, "/severity"},
		{"bad label name", func(r *store.AlertRule) { r.Labels = map[string]string{"1x": "y"} }, "/labels/1x"},
		{"reserved label", func(r *store.AlertRule) { r.Labels = map[string]string{"site_id": "y"} }, "/labels/site_id"},
		{"long label value", func(r *store.AlertRule) { r.Labels = map[string]string{"a": strings.Repeat("x", 201)} }, "/labels/a"},
		{"broken template", func(r *store.AlertRule) { r.Annotations = map[string]string{"summary": "{{ .Labels.x "} }, "/annotations/summary"},
		{"unknown template function", func(r *store.AlertRule) { r.Annotations = map[string]string{"summary": "{{ nope .Value }}"} }, "/annotations/summary"},
		{"template calling a missing method", func(r *store.AlertRule) { r.Annotations = map[string]string{"summary": "{{ .Value.Foo }}"} }, "/annotations/summary"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.change(&r)
			err := alerts.ValidateRule(r)
			var ve *alerts.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("ValidateRule = %v, want a *ValidationError", err)
			}
			found := false
			for _, f := range ve.Fields {
				found = found || f.Field == tc.field
			}
			if !found {
				t.Errorf("fields = %+v, want one for %s", ve.Fields, tc.field)
			}
		})
	}

	// Every problem is reported at once.
	r := valid()
	r.Name, r.Severity, r.Expr = "", "x", ""
	var ve *alerts.ValidationError
	if err := alerts.ValidateRule(r); !errors.As(err, &ve) || len(ve.Fields) != 3 || !strings.Contains(err.Error(), "/name") {
		t.Errorf("ValidateRule with three problems = %v", err)
	}
}

func TestExpressionRuleIgnoresThreshold(t *testing.T) {
	r := store.AlertRule{Name: "Down", Scope: store.AlertScopePlatform, Kind: store.AlertKindExpression, Expr: "up == 0", Severity: store.SeverityInfo}
	if err := alerts.ValidateRule(r); err != nil {
		t.Errorf("an expression rule needs no operator: %v", err)
	}
}
