package alerts

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Limits on rules.
const (
	MaxNameLength       = 100
	MaxDescriptionLen   = 1000
	MaxExprLength       = 4096
	MaxFor              = 7 * 24 * time.Hour
	MaxLabels           = 20
	MaxLabelValueLength = 200
	MaxTemplateLength   = 1000
)

// Annotation keys the engine renders into alerts.
const (
	AnnotationSummary     = "summary"
	AnnotationDescription = "description"
)

// FieldError is one thing wrong with a rule. Field is the rule's JSON
// field, as a JSON Pointer.
type FieldError struct {
	Field  string
	Detail string
}

// ValidationError lists everything wrong with a rule.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Fields))
	for i, f := range e.Fields {
		parts[i] = f.Field + ": " + f.Detail
	}
	return "invalid alert rule: " + strings.Join(parts, "; ")
}

var labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// reservedLabels can't be set by a rule: they come from the series (or, for
// alertname, name the rule) and routing depends on them.
var reservedLabels = map[string]bool{"site_id": true, "client_group_id": true, "alertname": true, "severity": true}

// ValidateRule checks a rule before it is stored. It returns a
// *ValidationError naming every problem, or nil. It checks what can be
// checked without VictoriaMetrics: the expression's brackets and quotes
// balance, but its syntax is only fully checked when it is evaluated, and a
// rule whose expression fails there is logged by the engine.
func ValidateRule(r store.AlertRule) error {
	var errs []FieldError
	bad := func(field, format string, args ...any) {
		errs = append(errs, FieldError{Field: "/" + field, Detail: fmt.Sprintf(format, args...)})
	}

	if r.Name == "" || len(r.Name) > MaxNameLength {
		bad("name", "must be 1 to %d characters", MaxNameLength)
	}
	if len(r.Description) > MaxDescriptionLen {
		bad("description", "must be at most %d characters", MaxDescriptionLen)
	}
	if !r.Scope.Valid() {
		bad("scope", "must be %q or %q", store.AlertScopePlatform, store.AlertScopeSite)
	}
	if r.ClientGroupID != "" && r.Scope != store.AlertScopeSite {
		bad("client_group_id", "only site rules can be limited to a client group")
	}
	switch {
	case !r.Kind.Valid():
		bad("kind", "must be %q or %q", store.AlertKindThreshold, store.AlertKindExpression)
	case r.Kind == store.AlertKindThreshold:
		if !r.Operator.Valid() {
			bad("operator", "must be one of gt, gte, lt, lte, eq, ne for a threshold rule")
		}
		if math.IsNaN(r.Threshold) || math.IsInf(r.Threshold, 0) {
			bad("threshold", "must be a finite number")
		}
	}
	if err := checkExpr(r.Expr); err != nil {
		bad("expr", "%v", err)
	}
	if r.For < 0 || r.For > MaxFor || r.For%time.Second != 0 {
		bad("for_seconds", "must be a whole number of seconds from 0 to %d", int(MaxFor/time.Second))
	}
	if !r.Severity.Valid() {
		bad("severity", "must be %q, %q, or %q", store.SeverityInfo, store.SeverityWarning, store.SeverityCritical)
	}

	if len(r.Labels) > MaxLabels {
		bad("labels", "at most %d labels", MaxLabels)
	}
	for k, v := range r.Labels {
		switch {
		case !labelName.MatchString(k) || strings.HasPrefix(k, "__"):
			bad("labels/"+k, "label names must match [a-zA-Z_][a-zA-Z0-9_]* and not start with __")
		case reservedLabels[k]:
			bad("labels/"+k, "this label is set by the platform and can't be overridden")
		case len(v) > MaxLabelValueLength:
			bad("labels/"+k, "must be at most %d characters", MaxLabelValueLength)
		}
	}
	if len(r.Annotations) > MaxLabels {
		bad("annotations", "at most %d annotations", MaxLabels)
	}
	for k, v := range r.Annotations {
		switch {
		case !labelName.MatchString(k):
			bad("annotations/"+k, "annotation names must match [a-zA-Z_][a-zA-Z0-9_]*")
		case len(v) > MaxTemplateLength:
			bad("annotations/"+k, "must be at most %d characters", MaxTemplateLength)
		default:
			if _, err := render(v, sampleData(r)); err != nil {
				bad("annotations/"+k, "isn't a valid template: %v", err)
			}
		}
	}
	if len(errs) > 0 {
		return &ValidationError{Fields: errs}
	}
	return nil
}

// checkExpr is a lexical check of a PromQL expression: it isn't empty or
// too long, and its brackets and quotes balance.
func checkExpr(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return fmt.Errorf("is required")
	}
	if len(expr) > MaxExprLength {
		return fmt.Errorf("must be at most %d characters", MaxExprLength)
	}
	var stack []byte
	closer := map[byte]byte{')': '(', ']': '[', '}': '{'}
	for i := 0; i < len(expr); i++ {
		switch c := expr[i]; c {
		case '(', '[', '{':
			stack = append(stack, c)
		case ')', ']', '}':
			if len(stack) == 0 || stack[len(stack)-1] != closer[c] {
				return fmt.Errorf("has an unbalanced %q", c)
			}
			stack = stack[:len(stack)-1]
		case '"', '\'', '`':
			end := i + 1
			for ; end < len(expr) && expr[end] != c; end++ {
				if expr[end] == '\\' && c != '`' {
					end++
				}
			}
			if end >= len(expr) {
				return fmt.Errorf("has an unterminated string")
			}
			i = end
		}
	}
	if len(stack) > 0 {
		return fmt.Errorf("has an unclosed %q", stack[len(stack)-1])
	}
	return nil
}

// templateData is what summary and description templates can use.
type templateData struct {
	// Labels are the alert's labels, such as {{ .Labels.site_id }}.
	Labels map[string]string
	// Value is the series' value, and Threshold the rule's threshold.
	Value     float64
	Threshold float64
	// Rule is the rule's name.
	Rule string
}

func sampleData(r store.AlertRule) templateData {
	return templateData{
		Labels: map[string]string{"site_id": "sample", "client_group_id": "sample"}, Value: 1, Threshold: r.Threshold, Rule: r.Name,
	}
}

var templateFuncs = template.FuncMap{
	// humanizePercentage formats a ratio as a percentage: 0.0512 is "5.1%".
	"humanizePercentage": func(v float64) string { return fmt.Sprintf("%.1f%%", v*100) },
	// humanizeDuration formats seconds as a duration: 90061 is "1d 1h 1m".
	"humanizeDuration": humanizeDuration,
}

func humanizeDuration(seconds float64) string {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return fmt.Sprint(seconds)
	}
	sign := ""
	if seconds < 0 {
		sign, seconds = "-", -seconds
	}
	s := int64(seconds)
	if s < 60 {
		return fmt.Sprintf("%s%ds", sign, s)
	}
	days, hours, minutes := s/86400, s%86400/3600, s%3600/60
	var parts []string
	for _, p := range []struct {
		n    int64
		unit string
	}{{days, "d"}, {hours, "h"}, {minutes, "m"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d%s", p.n, p.unit))
		}
	}
	return sign + strings.Join(parts, " ")
}

// render executes a summary or description template.
func render(text string, data templateData) (string, error) {
	t, err := template.New("").Option("missingkey=zero").Funcs(templateFuncs).Parse(text)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}
