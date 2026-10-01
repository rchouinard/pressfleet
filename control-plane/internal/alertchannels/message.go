package alertchannels

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Event kinds a message carries. The first two are alert event types; "test"
// is the synthetic message of the test-send endpoint.
const (
	KindFiring   = "firing"
	KindResolved = "resolved"
	KindTest     = "test"
)

// PayloadVersion is the version of the generic webhook payload. It changes
// only if a field is removed or changes meaning; new fields don't change
// it.
const PayloadVersion = 1

// AlertInfo describes the alert of a message. Its JSON form is the "alert"
// object of the generic webhook payload, and is stable: see docs/configuration.md.
type AlertInfo struct {
	ID            string            `json:"id"`
	RuleID        string            `json:"rule_id"`
	RuleName      string            `json:"rule_name"`
	State         string            `json:"state"`
	Severity      string            `json:"severity"`
	Summary       string            `json:"summary"`
	Description   string            `json:"description"`
	SiteID        string            `json:"site_id,omitempty"`
	ClientGroupID string            `json:"client_group_id,omitempty"`
	Value         float64           `json:"value"`
	Labels        map[string]string `json:"labels"`
	StartedAt     time.Time         `json:"started_at"`
	FiredAt       *time.Time        `json:"fired_at,omitempty"`
	ResolvedAt    *time.Time        `json:"resolved_at,omitempty"`
}

// Message is what a channel delivers: one alert event, or a test. It is
// what the deliveries table keeps as the event's snapshot, so a retry sends
// what the first attempt would have.
type Message struct {
	// Kind is KindFiring, KindResolved, or KindTest.
	Kind string `json:"kind"`
	// Key is stable for an event, and a receiver can deduplicate on it:
	// retries carry the same key.
	Key   string    `json:"key"`
	Alert AlertInfo `json:"alert"`
	// SentAt is set when an attempt is made, not stored.
	SentAt time.Time `json:"-"`
}

// newMessage makes the message for an alert event.
func newMessage(e alerts.Event) Message {
	a := e.Alert
	labels := a.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return Message{
		Kind: string(e.Type),
		Key:  e.Key(),
		Alert: AlertInfo{
			ID: a.ID, RuleID: a.RuleID, RuleName: e.Rule.Name, State: string(a.State), Severity: string(a.Severity),
			Summary: a.Summary, Description: a.Description, SiteID: a.SiteID, ClientGroupID: a.ClientGroupID,
			Value: a.Value, Labels: labels, StartedAt: a.StartedAt, FiredAt: a.FiredAt, ResolvedAt: a.ResolvedAt,
		},
	}
}

// testMessage is what a test send delivers.
func testMessage(channel string, at time.Time) Message {
	return Message{
		Kind: KindTest,
		Key:  "test:" + strconv.FormatInt(at.UnixNano(), 36),
		Alert: AlertInfo{
			ID: "test", RuleID: "test", RuleName: "Test notification", State: KindTest, Severity: string(store.SeverityInfo),
			Summary:     "Test notification from PressFleet",
			Description: "This is a test of the alert channel " + channel + ". No alert has fired.",
			Labels:      map[string]string{}, StartedAt: at,
		},
		SentAt: at,
	}
}

// heading is the one-line form of a message, for subjects and the "text"
// field: "[FIRING] critical: Site example.com is down".
func (m Message) heading() string {
	tag := strings.ToUpper(m.Kind)
	if m.Kind == KindTest {
		return "[TEST] " + m.Alert.Summary
	}
	return fmt.Sprintf("[%s] %s: %s", tag, m.Alert.Severity, m.Alert.Summary)
}

// facts are the details shown under the heading, in order.
func (m Message) facts() [][2]string {
	if m.Kind == KindTest {
		return nil
	}
	a := m.Alert
	facts := [][2]string{{"Rule", a.RuleName}, {"Severity", a.Severity}}
	if a.SiteID != "" {
		facts = append(facts, [2]string{"Site", a.SiteID})
	}
	if a.ClientGroupID != "" {
		facts = append(facts, [2]string{"Client group", a.ClientGroupID})
	}
	facts = append(facts, [2]string{"Value", strconv.FormatFloat(a.Value, 'g', 6, 64)},
		[2]string{"Started", a.StartedAt.UTC().Format(time.RFC3339)})
	if a.FiredAt != nil {
		facts = append(facts, [2]string{"Fired", a.FiredAt.UTC().Format(time.RFC3339)})
	}
	if a.ResolvedAt != nil {
		facts = append(facts, [2]string{"Resolved", a.ResolvedAt.UTC().Format(time.RFC3339)})
	}
	return facts
}

// sortedLabels returns the alert's labels as key, value pairs by key.
func (m Message) sortedLabels() [][2]string {
	keys := make([]string, 0, len(m.Alert.Labels))
	for k := range m.Alert.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]string, len(keys))
	for i, k := range keys {
		out[i] = [2]string{k, m.Alert.Labels[k]}
	}
	return out
}

// color is the message's accent: red for critical, amber for warnings,
// blue for information, and green once resolved.
func (m Message) color() string {
	switch {
	case m.Kind == KindResolved:
		return "#2e7d32"
	case m.Alert.Severity == string(store.SeverityCritical):
		return "#c62828"
	case m.Alert.Severity == string(store.SeverityWarning):
		return "#ef6c00"
	}
	return "#1565c0"
}

// text renders the message as plain text, for email bodies.
func (m Message) text() string {
	var b strings.Builder
	b.WriteString(m.heading() + "\n")
	if d := m.Alert.Description; d != "" {
		b.WriteString("\n" + d + "\n")
	}
	if facts := m.facts(); len(facts) > 0 {
		b.WriteString("\n")
		for _, f := range facts {
			fmt.Fprintf(&b, "%-13s %s\n", f[0]+":", f[1])
		}
	}
	if labels := m.sortedLabels(); len(labels) > 0 {
		b.WriteString("\nLabels:\n")
		for _, l := range labels {
			fmt.Fprintf(&b, "  %s = %s\n", l[0], l[1])
		}
	}
	b.WriteString("\n-- \nSent by PressFleet\n")
	return b.String()
}

var htmlBody = template.Must(template.New("alert").Parse(`<!DOCTYPE html>
<html><body style="font-family: -apple-system, Segoe UI, Helvetica, Arial, sans-serif; color: #212121;">
<div style="border-left: 4px solid {{.Color}}; padding: 4px 16px;">
<h2 style="margin: 0 0 8px 0;">{{.Heading}}</h2>
{{if .Description}}<p>{{.Description}}</p>{{end}}
{{if .Facts}}<table style="border-collapse: collapse;">
{{range .Facts}}<tr><td style="padding: 2px 16px 2px 0; color: #616161;">{{index . 0}}</td><td>{{index . 1}}</td></tr>
{{end}}</table>{{end}}
{{if .Labels}}<p style="color: #616161; margin-bottom: 4px;">Labels</p>
<table style="border-collapse: collapse; font-family: monospace;">
{{range .Labels}}<tr><td style="padding: 0 16px 0 0;">{{index . 0}}</td><td>{{index . 1}}</td></tr>
{{end}}</table>{{end}}
</div>
<p style="color: #9e9e9e; font-size: 12px;">Sent by PressFleet</p>
</body></html>
`))

// html renders the message as a simple HTML document. html/template escapes
// everything, including text from alert labels that sites control.
func (m Message) html() (string, error) {
	var b bytes.Buffer
	err := htmlBody.Execute(&b, map[string]any{
		"Color": m.color(), "Heading": m.heading(), "Description": m.Alert.Description,
		"Facts": m.facts(), "Labels": m.sortedLabels(),
	})
	return b.String(), err
}

// subject is the email subject: the heading on one line, without anything
// that could inject a header.
func (m Message) subject() string {
	s := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, "[PressFleet] "+m.heading())
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200])
	}
	return s
}

// Webhook formats.
const (
	FormatGeneric = "generic"
	FormatSlack   = "slack"
	FormatTeams   = "teams"
)

// genericPayload is the generic webhook body; the top-level "text" makes it
// work as a Slack incoming webhook too.
type genericPayload struct {
	Version int       `json:"version"`
	Event   string    `json:"event"`
	Key     string    `json:"key"`
	Text    string    `json:"text"`
	SentAt  time.Time `json:"sent_at"`
	Alert   AlertInfo `json:"alert"`
}

// webhookBody encodes the message in the given format.
func webhookBody(format string, m Message) ([]byte, error) {
	switch format {
	case FormatSlack:
		return json.Marshal(slackPayload(m))
	case FormatTeams:
		return json.Marshal(teamsPayload(m))
	}
	text := m.heading()
	if d := m.Alert.Description; d != "" {
		text += "\n" + d
	}
	return json.Marshal(genericPayload{
		Version: PayloadVersion, Event: m.Kind, Key: m.Key, Text: text, SentAt: m.SentAt.UTC(), Alert: m.Alert,
	})
}

func slackPayload(m Message) map[string]any {
	fields := []map[string]any{}
	for _, f := range m.facts() {
		fields = append(fields, map[string]any{"title": f[0], "value": f[1], "short": true})
	}
	text := "*" + m.heading() + "*"
	attachment := map[string]any{
		"color": m.color(), "fallback": m.heading(), "fields": fields, "ts": m.SentAt.Unix(),
	}
	if d := m.Alert.Description; d != "" {
		attachment["text"] = d
	}
	return map[string]any{"text": text, "attachments": []any{attachment}}
}

// teamsPayload is a message with an Adaptive Card, which Teams Workflows
// webhooks accept.
func teamsPayload(m Message) map[string]any {
	accent := "Accent"
	switch {
	case m.Kind == KindResolved:
		accent = "Good"
	case m.Alert.Severity == string(store.SeverityCritical):
		accent = "Attention"
	case m.Alert.Severity == string(store.SeverityWarning):
		accent = "Warning"
	}
	body := []any{map[string]any{"type": "TextBlock", "text": m.heading(), "weight": "Bolder", "size": "Medium", "color": accent, "wrap": true}}
	if d := m.Alert.Description; d != "" {
		body = append(body, map[string]any{"type": "TextBlock", "text": d, "wrap": true})
	}
	if facts := m.facts(); len(facts) > 0 {
		set := make([]map[string]string, len(facts))
		for i, f := range facts {
			set[i] = map[string]string{"title": f[0], "value": f[1]}
		}
		body = append(body, map[string]any{"type": "FactSet", "facts": set})
	}
	return map[string]any{
		"type": "message",
		"attachments": []any{map[string]any{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content": map[string]any{
				"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
				"type":    "AdaptiveCard", "version": "1.4", "body": body,
			},
		}},
	}
}
