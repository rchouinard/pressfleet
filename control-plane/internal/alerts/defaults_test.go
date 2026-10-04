package alerts_test

import (
	"context"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/alerts/alertstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const (
	siteID  = "01jd0000000000000000000001"
	groupID = "01jd0000000000000000000002"
)

// TestDefaultRules checks every default rule against a fake metrics store:
// it fires when its metrics say so (after its duration), keeps firing
// without repeating itself, and resolves once.
func TestDefaultRules(t *testing.T) {
	site := func(v float64, extra ...string) alerts.Sample {
		return alertstest.Sample(v, append([]string{"site_id", siteID, "client_group_id", groupID}, extra...)...)
	}
	tests := []struct {
		key string
		// bad is the query result that makes the rule fire. good is a
		// result under which it doesn't hold: samples that fail a
		// threshold, or none for rules whose expression filters.
		bad, good []alerts.Sample
		wantSite  string
		summary   string
	}{
		{alerts.KeySiteDown, []alerts.Sample{site(0)}, nil, siteID, "Site " + siteID + " is down"},
		{alerts.KeyElevated5xx, []alerts.Sample{site(0.12)}, []alerts.Sample{site(0.01)}, siteID, "12.0% of responses are 5xx"},
		{alerts.KeyCertExpiring, []alerts.Sample{site(10*86400, "domain", "www.example.com")}, []alerts.Sample{site(20*86400, "domain", "www.example.com")},
			siteID, "www.example.com expires in 10d"},
		{alerts.KeyCertRenewalFailing, []alerts.Sample{site(2*86400, "domain", "www.example.com")}, []alerts.Sample{site(5*86400, "domain", "www.example.com")},
			siteID, "wasn't renewed and expires in 2d"},
		{alerts.KeyBackupFailed, []alerts.Sample{site(0)}, nil, siteID, "latest backup of site " + siteID + " failed"},
		{alerts.KeyBackupStale, []alerts.Sample{site(40 * 3600)}, []alerts.Sample{site(3600)}, siteID, "backed up for 1d 16h"},
		{alerts.KeyDiskNearlyFull, []alerts.Sample{alertstest.Sample(0.95, "mountpoint", "/var/lib/docker")},
			[]alerts.Sample{alertstest.Sample(0.5, "mountpoint", "/var/lib/docker")}, "", "/var/lib/docker is 95.0% full"},
		{alerts.KeyContainerRestartLoop, []alerts.Sample{alertstest.Sample(6, "container", "pf-acme-php-1", "site_id", siteID)},
			[]alerts.Sample{alertstest.Sample(1, "container", "pf-acme-php-1", "site_id", siteID)}, siteID, "pf-acme-php-1 keeps restarting"},
		{alerts.KeyPHPFPMSaturation, []alerts.Sample{site(0.95)}, []alerts.Sample{site(0.4)}, siteID, "95.0% of PHP-FPM workers are busy"},
		{alerts.KeyRedisEvictionSpike, []alerts.Sample{alertstest.Sample(12.5)}, []alerts.Sample{alertstest.Sample(0.2)}, "", "evicting 12.5 keys per second"},
		{alerts.KeyUpdatesPending, []alerts.Sample{site(3, "kind", "plugin")}, []alerts.Sample{site(0, "kind", "plugin")}, siteID, "has 3 plugin updates pending"},
	}

	covered := map[string]bool{}
	for _, tc := range tests {
		covered[tc.key] = true
	}
	for _, r := range alerts.DefaultRules() {
		if !covered[r.BuiltinKey] {
			t.Errorf("default rule %s has no test case", r.BuiltinKey)
		}
	}

	for _, tc := range tests {
		t.Run(tc.key, func(t *testing.T) {
			storetest.Run(t, func(t *testing.T, db *store.DB) {
				ctx := context.Background()
				if _, err := alerts.SeedDefaults(ctx, db, storetest.Logger()); err != nil {
					t.Fatal(err)
				}
				var rule store.AlertRule
				rules, err := store.NewAlertRules(db).List(ctx, store.AlertRuleFilter{})
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range rules {
					if r.BuiltinKey == tc.key {
						rule = r
					}
				}
				if rule.ID == "" {
					t.Fatalf("rule %s wasn't seeded", tc.key)
				}
				h := newHarness(t, db)
				// Rules share expressions (the two certificate rules), so
				// events are filtered to the rule under test.
				mine := func() []string {
					var out []string
					for _, e := range h.notifier.Events() {
						if e.Rule.ID == rule.ID {
							out = append(out, string(e.Type))
						}
					}
					return out
				}

				// Nothing wrong: nothing fires.
				h.querier.Set(rule.Expr, tc.good...)
				h.evaluate(time.Minute)
				if got := h.alerts(store.AlertFilter{RuleID: rule.ID}); len(got) != 0 {
					t.Fatalf("healthy metrics raised %+v", got)
				}

				// Something wrong: pending until the rule's duration has
				// passed, then firing.
				h.querier.Set(rule.Expr, tc.bad...)
				h.evaluate(0)
				if rule.For > 0 {
					if got := mine(); len(got) != 0 {
						t.Fatalf("fired before its duration (%s): %v", rule.For, got)
					}
					h.clock.Advance(rule.For - time.Second)
					h.evaluate(0)
					if got := mine(); len(got) != 0 {
						t.Fatalf("fired before its duration (%s): %v", rule.For, got)
					}
					h.clock.Advance(time.Second)
					h.evaluate(0)
				}
				if got := mine(); !slices.Equal(got, []string{"firing"}) {
					t.Fatalf("events = %v, want one firing event", got)
				}
				firing := h.alerts(store.AlertFilter{RuleID: rule.ID, State: store.AlertFiring})
				if len(firing) != 1 || firing[0].SiteID != tc.wantSite || firing[0].Severity != rule.Severity {
					t.Fatalf("firing alerts = %+v, want one for site %q", firing, tc.wantSite)
				}
				if s := firing[0].Summary; !strings.Contains(s, tc.summary) || strings.Contains(s, "<no value>") {
					t.Errorf("summary = %q, want it to contain %q", s, tc.summary)
				}
				if d := firing[0].Description; d == "" || strings.Contains(d, "<no value>") {
					t.Errorf("description = %q", d)
				}

				// Still wrong: no more events.
				h.evaluate(time.Minute)
				h.evaluate(time.Minute)
				if got := mine(); !slices.Equal(got, []string{"firing"}) {
					t.Fatalf("events while it keeps firing = %v", got)
				}

				// Fixed: one resolved event, and the alert is resolved.
				h.querier.Set(rule.Expr, tc.good...)
				h.evaluate(time.Minute)
				h.evaluate(time.Minute)
				if got := mine(); !slices.Equal(got, []string{"firing", "resolved"}) {
					t.Fatalf("events after the fix = %v", got)
				}
				if got := h.alerts(store.AlertFilter{RuleID: rule.ID, State: store.AlertResolved}); len(got) != 1 {
					t.Errorf("resolved alerts = %+v", got)
				}

				// A rule that returns no series at all resolves too.
				h.querier.Set(rule.Expr, tc.bad...)
				h.evaluate(0)
				h.clock.Advance(rule.For)
				h.evaluate(time.Minute)
				h.querier.Set(rule.Expr)
				h.evaluate(time.Minute)
				if got := mine(); !slices.Equal(got, []string{"firing", "resolved", "firing", "resolved"}) {
					t.Fatalf("events after it fired and an empty result = %v", got)
				}
			})
		})
	}
}

func TestSeedDefaults(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		log := storetest.Logger()
		n, err := alerts.SeedDefaults(ctx, db, log)
		want := len(alerts.DefaultRules())
		if err != nil || n != want {
			t.Fatalf("SeedDefaults = %d, %v; want %d", n, err, want)
		}
		rules := store.NewAlertRules(db)
		list, err := rules.List(ctx, store.AlertRuleFilter{})
		if err != nil || len(list) != want {
			t.Fatalf("List = %d rules, %v; want %d", len(list), err, want)
		}
		for _, r := range list {
			if !r.Builtin() || !r.Enabled {
				t.Errorf("rule %q: builtin = %v, enabled = %v; want both true", r.Name, r.Builtin(), r.Enabled)
			}
		}

		// Seeding again changes nothing, including a user's edits.
		var disk store.AlertRule
		for _, r := range list {
			if r.BuiltinKey == alerts.KeyDiskNearlyFull {
				disk = r
			}
		}
		disk.Threshold, disk.Enabled, disk.For = 0.8, false, time.Hour
		disk.Annotations = map[string]string{"summary": "Mine"}
		if err := rules.Update(ctx, &disk); err != nil {
			t.Fatal(err)
		}
		if n, err := alerts.SeedDefaults(ctx, db, log); err != nil || n != 0 {
			t.Fatalf("second SeedDefaults = %d, %v; want 0", n, err)
		}
		got, err := rules.Get(ctx, disk.ID)
		if err != nil || got.Threshold != 0.8 || got.Enabled || got.For != time.Hour || got.Annotations["summary"] != "Mine" {
			t.Errorf("edited default rule after seeding = %+v, %v", got, err)
		}
		if list, _ := rules.List(ctx, store.AlertRuleFilter{}); len(list) != want {
			t.Errorf("seeding again left %d rules, want %d", len(list), want)
		}
		if err := rules.Delete(ctx, disk.ID); err == nil {
			t.Error("a default rule could be deleted")
		}
	})
}

var metricName = regexp.MustCompile(`pressfleet_[a-z0-9_]+`)

// TestDefaultMetricsAreDocumented keeps the metric table in the package
// documentation, DefaultMetrics, and the default rules' expressions in
// agreement, since collectors are written against the table.
func TestDefaultMetricsAreDocumented(t *testing.T) {
	used := map[string]bool{}
	for _, r := range alerts.DefaultRules() {
		for _, m := range metricName.FindAllString(r.Expr, -1) {
			used[m] = true
		}
	}
	for m := range used {
		if !slices.Contains(alerts.DefaultMetrics, m) {
			t.Errorf("a default rule uses %s, which isn't in DefaultMetrics", m)
		}
	}
	doc, err := os.ReadFile("alerts.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range alerts.DefaultMetrics {
		if !used[m] {
			t.Errorf("DefaultMetrics lists %s, which no default rule uses", m)
		}
		if !strings.Contains(string(doc), "//\t"+m+" ") {
			t.Errorf("%s is missing from the metric table in the package documentation", m)
		}
	}
}
