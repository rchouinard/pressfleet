package alerts_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

func TestUnavailableQuerier(t *testing.T) {
	samples, err := alerts.UnavailableQuerier{}.Query(context.Background(), "up", time.Now())
	if !errors.Is(err, alerts.ErrMetricsUnavailable) || samples != nil {
		t.Errorf("Query = %v, %v; want ErrMetricsUnavailable", samples, err)
	}
}

func TestLogNotifier(t *testing.T) {
	var buf bytes.Buffer
	n := alerts.NewLogNotifier(slog.New(slog.NewTextHandler(&buf, nil)))
	e := alerts.Event{
		Type:  store.AlertEventFiring,
		Alert: store.Alert{ID: "a1", Severity: store.SeverityCritical, SiteID: "s1", Summary: "Disk almost full", Value: 97.5},
		Rule:  store.AlertRule{Name: "disk_usage"},
	}
	if err := n.Notify(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	for _, want := range []string{`msg="alert event"`, "event=firing", "alert_id=a1", "rule=disk_usage", "severity=critical",
		"site_id=s1", `summary="Disk almost full"`, "value=97.5"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line %q lacks %s", line, want)
		}
	}
	if e.Key() != "a1:firing" {
		t.Errorf("Key = %q", e.Key())
	}
}
