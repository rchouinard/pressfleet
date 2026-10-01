package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func sampleRule(name string) store.AlertRule {
	return store.AlertRule{
		Name:        name,
		Description: "Something is wrong",
		Scope:       store.AlertScopeSite,
		Kind:        store.AlertKindThreshold,
		Expr:        `pressfleet_site_up`,
		Operator:    store.OpLess,
		Threshold:   1,
		For:         2 * time.Minute,
		Severity:    store.SeverityCritical,
		Enabled:     true,
		Labels:      map[string]string{"team": "ops"},
		Annotations: map[string]string{"summary": "Site {{ .Labels.site_id }} is down"},
	}
}

func TestAlertRules(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		rules := store.NewAlertRules(db)
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}

		down := sampleRule("Site down")
		down.ClientGroupID = group.ID
		if err := rules.Create(ctx, &down); err != nil {
			t.Fatal(err)
		}
		if !id.Valid(down.ID) || down.CreatedAt.IsZero() || down.UpdatedAt != down.CreatedAt || down.Builtin() {
			t.Errorf("Create did not fill in the rule: %+v", down)
		}
		got, err := rules.Get(ctx, down.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, down) {
			t.Errorf("Get = %+v, want %+v", got, down)
		}

		// Empty maps and no client group round trip as empty.
		disk := sampleRule("Disk full")
		disk.Scope, disk.Labels, disk.Annotations, disk.Enabled = store.AlertScopePlatform, nil, nil, false
		if err := rules.Create(ctx, &disk); err != nil {
			t.Fatal(err)
		}
		if got, err := rules.Get(ctx, disk.ID); err != nil || got.Enabled || got.ClientGroupID != "" ||
			len(got.Labels) != 0 || len(got.Annotations) != 0 {
			t.Errorf("Get disk rule = %+v, %v", got, err)
		}

		if err := rules.Create(ctx, &store.AlertRule{Name: "Site down"}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("Create duplicate name: err = %v, want ErrConflict", err)
		}
		bad := sampleRule("Bad group")
		bad.ClientGroupID = id.New()
		if err := rules.Create(ctx, &bad); !errors.Is(err, store.ErrReference) {
			t.Errorf("Create with a missing client group: err = %v, want ErrReference", err)
		}
		if _, err := rules.Get(ctx, id.New()); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get missing: err = %v, want ErrNotFound", err)
		}

		// Filters and paging.
		all, err := rules.List(ctx, store.AlertRuleFilter{})
		if err != nil || len(all) != 2 || all[0].Name != "Disk full" || all[1].Name != "Site down" {
			t.Fatalf("List = %+v, %v", all, err)
		}
		on := true
		for name, tc := range map[string]struct {
			f    store.AlertRuleFilter
			want []string
		}{
			"scope":   {store.AlertRuleFilter{Scope: store.AlertScopePlatform}, []string{"Disk full"}},
			"group":   {store.AlertRuleFilter{ClientGroupID: group.ID}, []string{"Site down"}},
			"enabled": {store.AlertRuleFilter{Enabled: &on}, []string{"Site down"}},
			"page":    {store.AlertRuleFilter{AfterName: "Disk full", AfterID: disk.ID, Limit: 5}, []string{"Site down"}},
			"limit":   {store.AlertRuleFilter{Limit: 1}, []string{"Disk full"}},
		} {
			list, err := rules.List(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, r := range list {
				names = append(names, r.Name)
			}
			if !reflect.DeepEqual(names, tc.want) {
				t.Errorf("List by %s = %v, want %v", name, names, tc.want)
			}
		}

		// Update.
		edited, err := rules.Get(ctx, down.ID)
		if err != nil {
			t.Fatal(err)
		}
		edited.Name, edited.Threshold, edited.For, edited.Enabled, edited.ClientGroupID = "Site is down", 0.5, 0, false, ""
		edited.Labels = map[string]string{"a": "b"}
		if err := rules.Update(ctx, &edited); err != nil {
			t.Fatal(err)
		}
		if got, err := rules.Get(ctx, down.ID); err != nil || !reflect.DeepEqual(got, edited) {
			t.Errorf("Get after Update = %+v, %v; want %+v", got, err, edited)
		}
		if edited.CreatedAt != down.CreatedAt || edited.UpdatedAt.Before(down.UpdatedAt) {
			t.Errorf("Update timestamps: %+v", edited)
		}
		clash := edited
		clash.Name = "Disk full"
		if err := rules.Update(ctx, &clash); !errors.Is(err, store.ErrConflict) {
			t.Errorf("Update to a taken name: err = %v, want ErrConflict", err)
		}
		missing := edited
		missing.ID = id.New()
		if err := rules.Update(ctx, &missing); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Update missing: err = %v, want ErrNotFound", err)
		}

		// Deleting a rule deletes its alerts.
		alert := &store.Alert{RuleID: down.ID, Fingerprint: "f1", State: store.AlertPending, Severity: store.SeverityWarning, StartedAt: time.Now()}
		if err := store.NewAlerts(db).Insert(ctx, alert); err != nil {
			t.Fatal(err)
		}
		if err := rules.Delete(ctx, down.ID); err != nil {
			t.Fatal(err)
		}
		if err := rules.Delete(ctx, down.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Delete twice: err = %v, want ErrNotFound", err)
		}
		if _, err := store.NewAlerts(db).Get(ctx, alert.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get alert of a deleted rule: err = %v, want ErrNotFound", err)
		}

		// Deleting a client group deletes the rules limited to it.
		limited := sampleRule("Acme only")
		limited.ClientGroupID = group.ID
		if err := rules.Create(ctx, &limited); err != nil {
			t.Fatal(err)
		}
		if err := store.NewClientGroups(db).Delete(ctx, group.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := rules.Get(ctx, limited.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Get rule of a deleted client group: err = %v, want ErrNotFound", err)
		}
	})
}

func TestAlertRuleBuiltins(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		rules := store.NewAlertRules(db)
		builtin := sampleRule("Site down")
		builtin.BuiltinKey = "site_down"
		if inserted, err := rules.EnsureBuiltin(ctx, &builtin); err != nil || !inserted {
			t.Fatalf("EnsureBuiltin = %v, %v; want inserted", inserted, err)
		}
		if !builtin.Builtin() || !id.Valid(builtin.ID) {
			t.Errorf("EnsureBuiltin did not fill in the rule: %+v", builtin)
		}

		// User edits survive seeding again.
		edited, err := rules.Get(ctx, builtin.ID)
		if err != nil {
			t.Fatal(err)
		}
		edited.Threshold, edited.Enabled, edited.Name = 0.25, false, "Renamed"
		if err := rules.Update(ctx, &edited); err != nil {
			t.Fatal(err)
		}
		if edited.BuiltinKey != "site_down" {
			t.Errorf("Update lost the built-in key: %+v", edited)
		}
		again := sampleRule("Site down")
		again.BuiltinKey = "site_down"
		if inserted, err := rules.EnsureBuiltin(ctx, &again); err != nil || inserted {
			t.Fatalf("second EnsureBuiltin = %v, %v; want not inserted", inserted, err)
		}
		if got, err := rules.Get(ctx, builtin.ID); err != nil || !reflect.DeepEqual(got, edited) {
			t.Errorf("rule after seeding again = %+v, %v; want %+v", got, err, edited)
		}
		if list, err := rules.List(ctx, store.AlertRuleFilter{}); err != nil || len(list) != 1 {
			t.Errorf("List = %+v, %v; want one rule", list, err)
		}

		// A built-in can't be deleted.
		if err := rules.Delete(ctx, builtin.ID); !errors.Is(err, store.ErrBuiltinRule) {
			t.Errorf("Delete built-in: err = %v, want ErrBuiltinRule", err)
		}
		if _, err := rules.Get(ctx, builtin.ID); err != nil {
			t.Errorf("built-in rule gone after refused delete: %v", err)
		}

		// A user's rule that has the name doesn't stop a new default rule.
		custom := sampleRule("Disk full")
		if err := rules.Create(ctx, &custom); err != nil {
			t.Fatal(err)
		}
		disk := sampleRule("Disk full")
		disk.BuiltinKey = "disk_full"
		if inserted, err := rules.EnsureBuiltin(ctx, &disk); err != nil || !inserted || disk.Name != "Disk full (built-in)" {
			t.Errorf("EnsureBuiltin with a taken name = %v, %v, %q", inserted, err, disk.Name)
		}
		if _, err := rules.EnsureBuiltin(ctx, &store.AlertRule{Name: "x"}); err == nil {
			t.Error("EnsureBuiltin without a key succeeded")
		}
	})
}

func TestAlerts(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		rules, alerts := store.NewAlertRules(db), store.NewAlerts(db)
		rule := sampleRule("Site down")
		if err := rules.Create(ctx, &rule); err != nil {
			t.Fatal(err)
		}
		other := sampleRule("Other")
		if err := rules.Create(ctx, &other); err != nil {
			t.Fatal(err)
		}
		t0 := time.Now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
		siteA, siteB := id.New(), id.New()
		newAlert := func(r store.AlertRule, fp, site string, start time.Time) *store.Alert {
			return &store.Alert{
				RuleID: r.ID, Fingerprint: fp, Labels: map[string]string{"site_id": site, "team": "ops"}, SiteID: site,
				State: store.AlertPending, Severity: store.SeverityCritical, Summary: "s", Description: "d", Value: 1, StartedAt: start,
			}
		}

		a := newAlert(rule, "fa", siteA, t0)
		if err := alerts.Insert(ctx, a); err != nil {
			t.Fatal(err)
		}
		if !id.Valid(a.ID) || a.UpdatedAt.IsZero() {
			t.Errorf("Insert did not fill in the alert: %+v", a)
		}
		got, err := alerts.Get(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.RuleName != "Site down" || got.State != store.AlertPending || got.FiredAt != nil || got.SiteID != siteA ||
			got.ClientGroupID != "" || got.Labels["team"] != "ops" || !got.StartedAt.Equal(t0) {
			t.Errorf("Get = %+v", got)
		}

		// One unresolved alert per rule and series.
		if err := alerts.Insert(ctx, newAlert(rule, "fa", siteA, t0)); !errors.Is(err, store.ErrConflict) {
			t.Errorf("Insert duplicate series: err = %v, want ErrConflict", err)
		}
		if err := alerts.Insert(ctx, newAlert(other, "fa", siteA, t0)); err != nil {
			t.Errorf("Insert the same series for another rule: %v", err)
		}
		if err := alerts.Insert(ctx, &store.Alert{RuleID: id.New(), Fingerprint: "x", State: store.AlertPending, StartedAt: t0}); !errors.Is(err, store.ErrReference) {
			t.Errorf("Insert for a missing rule: err = %v, want ErrReference", err)
		}

		// pending -> firing -> resolved, each once.
		a.Value = 2
		if err := alerts.Refresh(ctx, a); err != nil {
			t.Fatal(err)
		}
		fireAt := t0.Add(2 * time.Minute)
		if err := alerts.Fire(ctx, a, fireAt); err != nil {
			t.Fatal(err)
		}
		if err := alerts.Fire(ctx, a, fireAt); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Fire twice: err = %v, want ErrNotFound", err)
		}
		if err := alerts.DeletePending(ctx, a.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("DeletePending of a firing alert: err = %v, want ErrNotFound", err)
		}
		if got, err = alerts.Get(ctx, a.ID); err != nil || got.State != store.AlertFiring || got.Value != 2 ||
			got.FiredAt == nil || !got.FiredAt.Equal(fireAt) || got.ResolvedAt != nil {
			t.Fatalf("Get after Fire = %+v, %v", got, err)
		}

		unnotified, err := alerts.Unnotified(ctx)
		if err != nil || len(unnotified) != 1 || unnotified[0].ID != a.ID {
			t.Fatalf("Unnotified = %+v, %v; want the firing alert", unnotified, err)
		}
		if err := alerts.MarkNotified(ctx, a.ID, store.AlertEventFiring, fireAt); err != nil {
			t.Fatal(err)
		}
		if unnotified, err = alerts.Unnotified(ctx); err != nil || len(unnotified) != 0 {
			t.Fatalf("Unnotified after MarkNotified = %+v, %v", unnotified, err)
		}

		resolveAt := fireAt.Add(10 * time.Minute)
		if err := alerts.Resolve(ctx, a, resolveAt); err != nil {
			t.Fatal(err)
		}
		if err := alerts.Resolve(ctx, a, resolveAt); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Resolve twice: err = %v, want ErrNotFound", err)
		}
		if err := alerts.Refresh(ctx, a); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Refresh of a resolved alert: err = %v, want ErrNotFound", err)
		}
		if unnotified, err = alerts.Unnotified(ctx); err != nil || len(unnotified) != 1 || unnotified[0].FiringNotifiedAt == nil {
			t.Fatalf("Unnotified after Resolve = %+v, %v", unnotified, err)
		}
		if err := alerts.MarkNotified(ctx, a.ID, store.AlertEventResolved, resolveAt); err != nil {
			t.Fatal(err)
		}
		if err := alerts.MarkNotified(ctx, id.New(), store.AlertEventResolved, resolveAt); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("MarkNotified of a missing alert: err = %v, want ErrNotFound", err)
		}

		// The series can alert again once resolved; the history keeps both.
		again := newAlert(rule, "fa", siteA, resolveAt.Add(time.Minute))
		if err := alerts.Insert(ctx, again); err != nil {
			t.Fatalf("Insert after resolve: %v", err)
		}
		b := newAlert(rule, "fb", siteB, resolveAt.Add(2*time.Minute))
		b.ClientGroupID = id.New()
		if err := alerts.Insert(ctx, b); err != nil {
			t.Fatal(err)
		}
		if err := alerts.DeletePending(ctx, b.ID); err != nil {
			t.Fatal(err)
		}
		if err := alerts.Insert(ctx, b); err != nil {
			t.Fatal(err)
		}

		unresolved, err := alerts.Unresolved(ctx)
		if err != nil || len(unresolved) != 3 { // the other rule's, again, and b
			t.Fatalf("Unresolved = %d alerts, %v; want 3", len(unresolved), err)
		}

		// List: newest first, with filters and a cursor.
		ids := func(f store.AlertFilter) []string {
			t.Helper()
			list, err := alerts.List(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, a := range list {
				out = append(out, a.ID)
			}
			return out
		}
		all := ids(store.AlertFilter{})
		if len(all) != 4 {
			t.Fatalf("List = %d alerts, want 4", len(all))
		}
		for i := 1; i < len(all); i++ {
			if all[i-1] < all[i] {
				t.Errorf("List isn't newest first: %v", all)
			}
		}
		checks := []struct {
			name string
			f    store.AlertFilter
			want int
		}{
			{"rule", store.AlertFilter{RuleID: rule.ID}, 3},
			{"state", store.AlertFilter{State: store.AlertResolved}, 1},
			{"site", store.AlertFilter{SiteID: siteB}, 1},
			{"group", store.AlertFilter{ClientGroupID: b.ClientGroupID}, 1},
			{"since", store.AlertFilter{Since: resolveAt}, 2},
			{"until", store.AlertFilter{Until: resolveAt}, 2},
			{"limit", store.AlertFilter{Limit: 2}, 2},
			{"cursor", store.AlertFilter{Before: all[1]}, 2},
		}
		for _, c := range checks {
			if got := ids(c.f); len(got) != c.want {
				t.Errorf("List by %s = %d alerts, want %d", c.name, len(got), c.want)
			}
		}

		// Old resolved alerts are pruned; unresolved ones never.
		if n, err := alerts.DeleteResolvedBefore(ctx, resolveAt); err != nil || n != 0 {
			t.Errorf("DeleteResolvedBefore(resolve time) = %d, %v; want 0", n, err)
		}
		if n, err := alerts.DeleteResolvedBefore(ctx, resolveAt.Add(time.Second)); err != nil || n != 1 {
			t.Errorf("DeleteResolvedBefore = %d, %v; want 1", n, err)
		}
		if got := ids(store.AlertFilter{}); len(got) != 3 {
			t.Errorf("List after pruning = %d alerts, want 3", len(got))
		}
	})
}
