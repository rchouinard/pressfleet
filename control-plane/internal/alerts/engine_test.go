package alerts_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/alerts/alertstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// harness is an engine with fakes for everything outside the database.
type harness struct {
	t        *testing.T
	db       *store.DB
	engine   *alerts.Engine
	querier  *alertstest.Querier
	notifier *alertstest.Notifier
	clock    *alertstest.Clock
}

func newHarness(t *testing.T, db *store.DB) *harness {
	t.Helper()
	h := &harness{
		t: t, db: db, querier: alertstest.NewQuerier(), notifier: &alertstest.Notifier{},
		clock: alertstest.NewClock(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)),
	}
	h.engine = h.newEngine()
	return h
}

// newEngine returns another engine on the same database, as after a restart.
func (h *harness) newEngine() *alerts.Engine {
	h.t.Helper()
	e, err := alerts.New(alerts.Options{
		DB: h.db, Querier: h.querier, Notifier: h.notifier, Now: h.clock.Now, Logger: storetest.Logger(),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// evaluate runs one evaluation with the engine, then advances the clock.
func (h *harness) evaluate(advance time.Duration) {
	h.t.Helper()
	h.evaluateWith(h.engine, advance)
}

func (h *harness) evaluateWith(e *alerts.Engine, advance time.Duration) {
	h.t.Helper()
	if err := e.Evaluate(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.clock.Advance(advance)
}

func (h *harness) addRule(r store.AlertRule) store.AlertRule {
	h.t.Helper()
	if r.Severity == "" {
		r.Severity = store.SeverityWarning
	}
	if err := alerts.ValidateRule(r); err != nil {
		h.t.Fatal(err)
	}
	if err := store.NewAlertRules(h.db).Create(context.Background(), &r); err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) alerts(f store.AlertFilter) []store.Alert {
	h.t.Helper()
	list, err := store.NewAlerts(h.db).List(context.Background(), f)
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

// events returns the types of the events the notifier accepted so far.
func (h *harness) events() []string {
	var out []string
	for _, e := range h.notifier.Events() {
		out = append(out, string(e.Type)+":"+e.Alert.Labels["site_id"])
	}
	return out
}

func (h *harness) wantEvents(want ...string) {
	h.t.Helper()
	if got := h.events(); !slices.Equal(got, want) {
		h.t.Errorf("events = %v, want %v", got, want)
	}
}

func (h *harness) wantEventsAnyOrder(want ...string) {
	h.t.Helper()
	got := h.events()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		h.t.Errorf("events = %v, want %v", got, want)
	}
}

func siteDownRule(forDuration time.Duration) store.AlertRule {
	return store.AlertRule{
		Name: "Down", Scope: store.AlertScopeSite, Kind: store.AlertKindExpression, Expr: "up == 0", For: forDuration,
		Severity: store.SeverityCritical, Enabled: true, Labels: map[string]string{"team": "ops"},
		Annotations: map[string]string{"summary": "Site {{ .Labels.site_id }} is down", "description": "value {{ .Value }}"},
	}
}

func TestLifecycle(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHarness(t, db)
		rule := h.addRule(siteDownRule(2 * time.Minute))
		a, b := alertstest.Sample(0, "site_id", "a", "__name__", "up"), alertstest.Sample(0, "site_id", "b")

		// Nothing holds: nothing happens.
		h.evaluate(time.Minute)
		if got := h.alerts(store.AlertFilter{}); len(got) != 0 {
			t.Fatalf("alerts = %+v, want none", got)
		}

		// Site a goes down: pending, then firing once the duration has
		// passed, with one event.
		h.querier.Set("up == 0", a)
		h.evaluate(time.Minute) // t+1: pending
		got := h.alerts(store.AlertFilter{})
		if len(got) != 1 || got[0].State != store.AlertPending || got[0].SiteID != "a" || got[0].FiredAt != nil {
			t.Fatalf("after the first evaluation: %+v", got)
		}
		h.wantEvents()
		started := got[0].StartedAt
		h.evaluate(time.Minute) // t+2: pending for 1 minute of 2
		h.wantEvents()
		h.evaluate(time.Minute) // t+3: pending for 2 minutes: fires
		got = h.alerts(store.AlertFilter{})
		if len(got) != 1 || got[0].State != store.AlertFiring || got[0].FiredAt == nil || !got[0].StartedAt.Equal(started) {
			t.Fatalf("after the duration: %+v", got)
		}
		h.wantEvents("firing:a")
		fired := h.notifier.Events()[0]
		if fired.Alert.Summary != "Site a is down" || fired.Alert.Description != "value 0" || fired.Rule.ID != rule.ID ||
			fired.Alert.Labels["team"] != "ops" || fired.Alert.Labels["site_id"] != "a" || fired.Alert.Labels["__name__"] != "" ||
			fired.Alert.Severity != store.SeverityCritical || fired.Key() != got[0].ID+":firing" {
			t.Errorf("firing event = %+v", fired)
		}

		// Still firing: no repeated events, and the value is kept current.
		for range 3 {
			h.evaluate(time.Minute)
		}
		h.wantEvents("firing:a")
		if n := len(h.alerts(store.AlertFilter{})); n != 1 {
			t.Errorf("a condition that keeps holding made %d alerts, want 1", n)
		}

		// A second site goes down while the first is firing.
		h.querier.Set("up == 0", a, b)
		h.evaluate(time.Minute)
		h.evaluate(time.Minute)
		h.evaluate(time.Minute)
		h.wantEvents("firing:a", "firing:b")

		// Site a comes back: exactly one resolved event.
		h.querier.Set("up == 0", b)
		h.evaluate(time.Minute)
		h.wantEvents("firing:a", "firing:b", "resolved:a")
		resolved := h.alerts(store.AlertFilter{State: store.AlertResolved})
		if len(resolved) != 1 || resolved[0].SiteID != "a" || resolved[0].ResolvedAt == nil {
			t.Fatalf("resolved alerts = %+v", resolved)
		}
		for range 3 {
			h.evaluate(time.Minute)
		}
		h.wantEvents("firing:a", "firing:b", "resolved:a")

		// Site a goes down again: a new alert, with its own events.
		h.querier.Set("up == 0", a, b)
		h.evaluate(time.Minute)
		h.evaluate(2 * time.Minute)
		h.evaluate(time.Minute)
		h.wantEvents("firing:a", "firing:b", "resolved:a", "firing:a")
		if n := len(h.alerts(store.AlertFilter{SiteID: "a"})); n != 2 {
			t.Errorf("alerts of site a = %d, want 2 (one resolved, one firing)", n)
		}
	})
}

func TestImmediateFiringAndDroppedPending(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHarness(t, db)
		immediate := siteDownRule(0)
		immediate.Name, immediate.Expr = "Immediate", "failed == 1"
		h.addRule(immediate)
		slow := siteDownRule(10 * time.Minute)
		slow.Name, slow.Expr = "Slow", "slow == 1"
		h.addRule(slow)

		h.querier.Set("failed == 1", alertstest.Sample(1, "site_id", "a"))
		h.querier.Set("slow == 1", alertstest.Sample(1, "site_id", "b"))
		h.evaluate(time.Minute)
		h.wantEvents("firing:a") // no waiting without a duration

		// The slow one stops holding before it fires: it leaves no trace
		// and sends nothing.
		h.querier.Set("slow == 1")
		h.evaluate(time.Minute)
		if got := h.alerts(store.AlertFilter{SiteID: "b"}); len(got) != 0 {
			t.Errorf("alerts of a condition that stopped before firing = %+v, want none", got)
		}
		h.querier.Set("failed == 1")
		h.evaluate(time.Minute)
		h.wantEvents("firing:a", "resolved:a")
	})
}

func TestThresholdScopeAndClientGroup(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHarness(t, db)
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		rule := h.addRule(store.AlertRule{
			Name: "Busy", Scope: store.AlertScopeSite, Kind: store.AlertKindThreshold, Expr: "busy", Operator: store.OpGreaterEqual,
			Threshold: 0.9, Severity: store.SeverityWarning, Enabled: true, ClientGroupID: group.ID,
		})
		h.querier.Set("busy",
			alertstest.Sample(0.95, "site_id", "a", "client_group_id", group.ID),
			alertstest.Sample(0.9, "site_id", "b", "client_group_id", group.ID),  // at the threshold
			alertstest.Sample(0.89, "site_id", "c", "client_group_id", group.ID), // below it
			alertstest.Sample(0.99, "site_id", "d", "client_group_id", id.New()), // another group
			alertstest.Sample(0.99, "client_group_id", group.ID),                 // no site
			alertstest.Sample(0.99, "site_id", "a", "client_group_id", group.ID), // the same series twice
		)
		h.evaluate(time.Minute)
		h.wantEventsAnyOrder("firing:a", "firing:b")

		// Raising the threshold resolves the alert that no longer holds.
		rule.Threshold = 0.92
		if err := store.NewAlertRules(db).Update(ctx, &rule); err != nil {
			t.Fatal(err)
		}
		h.evaluate(time.Minute)
		h.wantEventsAnyOrder("firing:a", "firing:b", "resolved:b")
	})
}

// addSites inserts a production site and a staging clone of it, and returns
// their IDs. Rows are inserted directly because the engine reads nothing
// from a site but whether it has a parent.
func addSites(t *testing.T, db *store.DB) (prod, staging string) {
	t.Helper()
	ctx := context.Background()
	group := &store.ClientGroup{Name: "group-" + id.New()}
	if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
		t.Fatal(err)
	}
	prod, staging = id.New(), id.New()
	for _, s := range []struct{ id, parent string }{{prod, ""}, {staging, prod}} {
		uid, err := store.AllocateSiteUID(ctx, db)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		parent := any(nil)
		if s.parent != "" {
			parent = s.parent
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO sites
			(id, client_group_id, parent_site_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
			VALUES (?, ?, ?, 'Site', ?, 'running', ?, '8.4', '7.1.2', ?, ?, ?)`,
			s.id, group.ID, parent, s.id, s.id+".preview.example", uid, now, now); err != nil {
			t.Fatal(err)
		}
	}
	return prod, staging
}

func TestStagingSitesAreSkippedUnlessIncluded(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHarness(t, db)
		prod, staging := addSites(t, db)
		plain := h.addRule(siteDownRule(0))
		plain.Name = "Down, production only"
		optIn := siteDownRule(0)
		optIn.Name, optIn.IncludeStaging = "Down, with staging", true
		optIn = h.addRule(optIn)
		plain, err := store.NewAlertRules(db).Get(ctx, plain.ID)
		if err != nil {
			t.Fatal(err)
		}
		platform := h.addRule(store.AlertRule{
			Name: "Platform", Scope: store.AlertScopePlatform, Kind: store.AlertKindExpression, Expr: "host", Enabled: true,
		})

		h.querier.Set("up == 0", alertstest.Sample(0, "site_id", prod), alertstest.Sample(0, "site_id", staging))
		// A platform series that happens to name a staging site is not filtered.
		h.querier.Set("host", alertstest.Sample(1, "site_id", staging))
		h.evaluate(time.Minute)

		byRule := map[string][]string{}
		for _, a := range h.alerts(store.AlertFilter{}) {
			byRule[a.RuleID] = append(byRule[a.RuleID], a.SiteID)
		}
		for _, ids := range byRule {
			slices.Sort(ids)
		}
		want := map[string][]string{
			plain.ID:    {prod},
			optIn.ID:    sortedIDs(prod, staging),
			platform.ID: {staging},
		}
		if fmt.Sprint(byRule) != fmt.Sprint(want) {
			t.Errorf("alerts by rule = %v, want %v", byRule, want)
		}

		// Editing the opt-in rule to exclude staging resolves the staging
		// site's alert, as if its series had disappeared.
		optIn.IncludeStaging = false
		if err := store.NewAlertRules(db).Update(ctx, &optIn); err != nil {
			t.Fatal(err)
		}
		h.evaluate(time.Minute)
		resolved := h.alerts(store.AlertFilter{State: store.AlertResolved})
		if len(resolved) != 1 || resolved[0].SiteID != staging || resolved[0].RuleID != optIn.ID {
			t.Errorf("resolved alerts = %+v, want the staging alert of the edited rule", resolved)
		}
	})
}

func sortedIDs(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

func TestPlatformScope(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHarness(t, db)
		h.addRule(store.AlertRule{
			Name: "Disk", Scope: store.AlertScopePlatform, Kind: store.AlertKindThreshold, Expr: "disk", Operator: store.OpGreater,
			Threshold: 0.9, Enabled: true, Annotations: map[string]string{"summary": "{{ .Labels.mountpoint }} {{ humanizePercentage .Value }}"},
		})
		h.querier.Set("disk", alertstest.Sample(0.931, "mountpoint", "/"), alertstest.Sample(0.5, "mountpoint", "/data"))
		h.evaluate(time.Minute)
		got := h.alerts(store.AlertFilter{})
		if len(got) != 1 || got[0].SiteID != "" || got[0].Summary != "/ 93.1%" || got[0].State != store.AlertFiring {
			t.Errorf("alerts = %+v", got)
		}
	})
}

func TestRestartKeepsFiringWithoutRenotifying(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHarness(t, db)
		h.addRule(siteDownRule(0))
		h.querier.Set("up == 0", alertstest.Sample(0, "site_id", "a"))
		h.evaluate(time.Minute)
		h.wantEvents("firing:a")

		// A restarted engine (a fresh one on the same database) finds the
		// alert firing and doesn't announce it again, however often it
		// evaluates.
		restarted := h.newEngine()
		for range 3 {
			h.evaluateWith(restarted, time.Minute)
		}
		h.wantEvents("firing:a")
		if got := h.alerts(store.AlertFilter{}); len(got) != 1 || got[0].State != store.AlertFiring {
			t.Fatalf("alerts after a restart = %+v", got)
		}

		// It still resolves, once.
		h.querier.Set("up == 0")
		h.evaluateWith(restarted, time.Minute)
		h.evaluateWith(h.newEngine(), time.Minute)
		h.wantEvents("firing:a", "resolved:a")
	})
}

func TestUnacceptedEventsAreOfferedAgain(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHarness(t, db)
		h.addRule(siteDownRule(0))
		h.querier.Set("up == 0", alertstest.Sample(0, "site_id", "a"))

		// The notifier is down: the alert fires, and the event waits. The
		// resolved event can't pass it, so the order is kept.
		h.notifier.SetError(errors.New("smtp unreachable"))
		h.evaluate(time.Minute)
		h.querier.Set("up == 0")
		h.evaluate(time.Minute)
		h.wantEvents()
		if got := h.alerts(store.AlertFilter{State: store.AlertResolved}); len(got) != 1 {
			t.Fatalf("alerts = %+v, want one resolved", h.alerts(store.AlertFilter{}))
		}

		// After a restart with a working notifier both events arrive, in
		// order, once.
		h.notifier.SetError(nil)
		restarted := h.newEngine()
		h.evaluateWith(restarted, time.Minute)
		h.evaluateWith(restarted, time.Minute)
		h.evaluateWith(h.newEngine(), time.Minute)
		h.wantEvents("firing:a", "resolved:a")
	})
}

func TestMetricsOutageChangesNothing(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHarness(t, db)
		h.addRule(siteDownRule(0))
		h.querier.Set("up == 0", alertstest.Sample(0, "site_id", "a"))
		h.evaluate(time.Minute)
		h.wantEvents("firing:a")

		// While the metrics store can't be queried the alert stays firing,
		// even though the query "returns" nothing.
		h.querier.SetError(fmt.Errorf("victoriametrics: %w", alerts.ErrMetricsUnavailable))
		for range 3 {
			h.evaluate(time.Minute)
		}
		h.wantEvents("firing:a")
		if got := h.alerts(store.AlertFilter{State: store.AlertFiring}); len(got) != 1 {
			t.Fatalf("alerts during an outage = %+v", h.alerts(store.AlertFilter{}))
		}

		// When it is back and the condition is gone, the alert resolves.
		h.querier.SetError(nil)
		h.querier.Set("up == 0")
		h.evaluate(time.Minute)
		h.wantEvents("firing:a", "resolved:a")
	})
}

func TestDisabledRuleResolvesItsAlerts(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHarness(t, db)
		rule := h.addRule(siteDownRule(0))
		h.querier.Set("up == 0", alertstest.Sample(0, "site_id", "a"))
		h.evaluate(time.Minute)
		h.wantEvents("firing:a")

		rule.Enabled = false
		if err := store.NewAlertRules(db).Update(ctx, &rule); err != nil {
			t.Fatal(err)
		}
		queries := len(h.querier.Queries())
		h.evaluate(time.Minute)
		h.evaluate(time.Minute)
		h.wantEvents("firing:a", "resolved:a")
		if n := len(h.querier.Queries()); n != queries {
			t.Errorf("a disabled rule was queried %d more times", n-queries)
		}

		// Enabling it again raises a new alert.
		rule.Enabled = true
		if err := store.NewAlertRules(db).Update(ctx, &rule); err != nil {
			t.Fatal(err)
		}
		h.evaluate(time.Minute)
		h.wantEvents("firing:a", "resolved:a", "firing:a")
	})
}

func TestEditedRuleUpdatesFiringAlert(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHarness(t, db)
		rule := h.addRule(siteDownRule(0))
		h.querier.Set("up == 0", alertstest.Sample(0, "site_id", "a"))
		h.evaluate(time.Minute)

		rule.Severity = store.SeverityInfo
		rule.Annotations = map[string]string{"summary": "Edited {{ .Labels.site_id }}"}
		if err := store.NewAlertRules(db).Update(ctx, &rule); err != nil {
			t.Fatal(err)
		}
		h.evaluate(time.Minute)
		got := h.alerts(store.AlertFilter{})
		if len(got) != 1 || got[0].Severity != store.SeverityInfo || got[0].Summary != "Edited a" {
			t.Errorf("alert after the rule was edited = %+v", got)
		}
		h.wantEvents("firing:a") // an edit isn't an event
	})
}

func TestNewValidatesOptions(t *testing.T) {
	for name, opts := range map[string]alerts.Options{
		"no db":      {Querier: alerts.UnavailableQuerier{}, Logger: storetest.Logger()},
		"no querier": {Logger: storetest.Logger()},
	} {
		if _, err := alerts.New(opts); err == nil {
			t.Errorf("New(%s) succeeded", name)
		}
	}
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		if _, err := alerts.New(alerts.Options{DB: db, Querier: alerts.UnavailableQuerier{}}); err == nil {
			t.Error("New without a logger succeeded")
		}
	})
}

func TestRunStopsWithContext(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHarness(t, db)
		h.addRule(siteDownRule(0))
		h.querier.Set("up == 0", alertstest.Sample(0, "site_id", "a"))
		e, err := alerts.New(alerts.Options{
			DB: db, Querier: h.querier, Notifier: h.notifier, Interval: time.Hour, Now: h.clock.Now, Logger: storetest.Logger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- e.Run(ctx) }()
		deadline := time.Now().Add(10 * time.Second)
		for len(h.notifier.Events()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run = %v", err)
		}
		h.wantEvents("firing:a") // evaluated once at the start
	})
}
