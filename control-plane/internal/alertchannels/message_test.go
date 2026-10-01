package alertchannels

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func sampleMessage(kind string) Message {
	fired := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	m := Message{
		Kind: kind,
		Key:  "alert1:" + kind,
		Alert: AlertInfo{
			ID: "alert1", RuleID: "rule1", RuleName: "Site down", State: kind, Severity: "critical",
			Summary: "Site example.com is down", Description: "The HTTP check failed.", SiteID: "site1",
			Labels: map[string]string{"site_id": "site1"}, StartedAt: fired.Add(-2 * time.Minute), FiredAt: &fired,
		},
		SentAt: fired.Add(5 * time.Second),
	}
	if kind == KindResolved {
		resolved := fired.Add(time.Hour)
		m.Alert.ResolvedAt = &resolved
	}
	return m
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return m
}

// TestGenericPayloadIsStable pins the generic webhook payload: receivers
// depend on its field names.
func TestGenericPayloadIsStable(t *testing.T) {
	body, err := webhookBody(FormatGeneric, sampleMessage(KindFiring))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"version":1,"event":"firing","key":"alert1:firing","text":"[FIRING] critical: Site example.com is down\nThe HTTP check failed.",` +
		`"sent_at":"2026-09-30T12:00:05Z","alert":{"id":"alert1","rule_id":"rule1","rule_name":"Site down","state":"firing","severity":"critical",` +
		`"summary":"Site example.com is down","description":"The HTTP check failed.","site_id":"site1","value":0,"labels":{"site_id":"site1"},` +
		`"started_at":"2026-09-30T11:58:00Z","fired_at":"2026-09-30T12:00:00Z"}}`
	if string(body) != want {
		t.Errorf("payload =\n%s\nwant\n%s", body, want)
	}
}

func TestSlackPayload(t *testing.T) {
	body, err := webhookBody(FormatSlack, sampleMessage(KindResolved))
	if err != nil {
		t.Fatal(err)
	}
	p := decode(t, body)
	if text, _ := p["text"].(string); text != "*[RESOLVED] critical: Site example.com is down*" {
		t.Errorf("text = %q", text)
	}
	att := p["attachments"].([]any)[0].(map[string]any)
	if att["color"] != "#2e7d32" || att["text"] != "The HTTP check failed." || len(att["fields"].([]any)) < 4 {
		t.Errorf("attachment = %v", att)
	}
}

func TestTeamsPayload(t *testing.T) {
	body, err := webhookBody(FormatTeams, sampleMessage(KindFiring))
	if err != nil {
		t.Fatal(err)
	}
	p := decode(t, body)
	att := p["attachments"].([]any)[0].(map[string]any)
	card := att["content"].(map[string]any)
	if p["type"] != "message" || att["contentType"] != "application/vnd.microsoft.card.adaptive" || card["type"] != "AdaptiveCard" {
		t.Errorf("payload = %s", body)
	}
	first := card["body"].([]any)[0].(map[string]any)
	if first["text"] != "[FIRING] critical: Site example.com is down" || first["color"] != "Attention" {
		t.Errorf("first block = %v", first)
	}
}

func TestTestMessage(t *testing.T) {
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	body, err := webhookBody(FormatGeneric, testMessage("Hook", at))
	if err != nil {
		t.Fatal(err)
	}
	p := decode(t, body)
	if p["event"] != "test" || !strings.HasPrefix(p["text"].(string), "[TEST] ") {
		t.Errorf("payload = %s", body)
	}
}

func TestSignatureIsOverTimestampAndBody(t *testing.T) {
	// What a receiver computes, as the documentation describes it.
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write([]byte("1700000000.{}"))
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	got := Sign("secret", "1700000000", []byte("{}"))
	if got != want {
		t.Fatalf("Sign = %q, want %q", got, want)
	}
	if got == Sign("secret", "1700000001", []byte("{}")) || got == Sign("secret", "1700000000", []byte("{ }")) || got == Sign("other", "1700000000", []byte("{}")) {
		t.Error("the signature doesn't depend on the timestamp, body, and secret")
	}
}

func TestBuildEmailSanitizesHeaders(t *testing.T) {
	m := sampleMessage(KindFiring)
	m.Alert.Summary = "bad\r\nBcc: attacker@example.com"
	m.Alert.Labels = map[string]string{"x": "<script>alert(1)</script>"}
	raw, err := buildEmail(Relay{FromEmail: "pf@example.com", FromName: "Evil\r\nBcc: x@example.com"}, []string{"ops@example.com"}, m)
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := strings.Cut(string(raw), "\r\n\r\n")
	for _, line := range strings.Split(head, "\r\n") {
		if strings.HasPrefix(line, "Bcc:") {
			t.Errorf("header injected: %q", line)
		}
	}
	if html, err := m.html(); err != nil || strings.Contains(html, "<script>") || !strings.Contains(html, "&lt;script&gt;") ||
		!strings.Contains(html, "solid #c62828") {
		t.Errorf("label text isn't escaped in the HTML part: %v\n%s", err, html)
	}
	if !strings.Contains(string(raw), "\r\n") || strings.Contains(strings.ReplaceAll(string(raw), "\r\n", ""), "\n") {
		t.Error("message doesn't use CRLF line endings throughout")
	}
}

func TestBlockedAddresses(t *testing.T) {
	for addr, blocked := range map[string]bool{
		"127.0.0.1":            true,
		"127.1.2.3":            true,
		"::1":                  true,
		"::ffff:127.0.0.1":     true,
		"169.254.169.254":      true,
		"169.254.0.1":          true,
		"fe80::1":              true,
		"fd00:ec2::254":        true,
		"0.0.0.0":              true,
		"::":                   true,
		"224.0.0.1":            true,
		"ff02::1":              true,
		"10.0.0.5":             false,
		"192.168.1.10":         false,
		"172.16.0.1":           false,
		"8.8.8.8":              false,
		"2606:4700:4700::1111": false,
		"fd12:3456::1":         false,
	} {
		if got := blockedAddr(netip.MustParseAddr(addr)); got != blocked {
			t.Errorf("blockedAddr(%s) = %v, want %v", addr, got, blocked)
		}
	}
	if err := checkLiteralHost("127.0.0.1", false); err == nil {
		t.Error("checkLiteralHost accepted loopback")
	}
	if err := checkLiteralHost("127.0.0.1", true); err != nil {
		t.Errorf("checkLiteralHost with local targets allowed = %v", err)
	}
	if err := checkLiteralHost("example.com", false); err != nil {
		t.Errorf("checkLiteralHost(name) = %v", err)
	}
}

func TestDefaultBackoff(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, w := range want {
		if got := DefaultBackoff(i + 1); got != w {
			t.Errorf("DefaultBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://hooks.slack.com/services/T0/B0/XXXX": "https://hooks.slack.com/…",
		"https://example.com":                         "https://example.com",
		"https://example.com/":                        "https://example.com",
		"http://chat.local:8080/hook?token=abc":       "http://chat.local:8080/…",
		"https://user:pass@example.com/":              "https://example.com/…",
		"not a url":                                   "",
		"":                                            "",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
}
