package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

func TestTargetClientGroup(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		site := id.New()
		now := time.Now()
		if _, err := db.ExecContext(ctx, `INSERT INTO sites
			(id, client_group_id, name, slug, status, preview_domain, php_version, wp_core_version, uid, created_at, updated_at)
			VALUES (?, ?, 'Site', 'site', 'running', 'site.preview.example', '8.4', '6.9.1', 20000, ?, ?)`,
			site, group.ID, now, now); err != nil {
			t.Fatal(err)
		}
		jobs := store.NewJobs(db)
		siteJob := store.Job{Type: "site.update", TargetType: "site", TargetID: site}
		groupJob := store.Job{Type: "backup", TargetType: "client_group", TargetID: group.ID}
		orphanJob := store.Job{Type: "site.update", TargetType: "site", TargetID: id.New()}
		for _, j := range []*store.Job{&siteJob, &groupJob, &orphanJob} {
			if err := jobs.Enqueue(ctx, j); err != nil {
				t.Fatal(err)
			}
		}

		rules := store.NewAlertRules(db)
		limited := store.AlertRule{Name: "Limited", Scope: store.AlertScopeSite, Kind: store.AlertKindExpression, Expr: "x", Severity: store.SeverityInfo, ClientGroupID: group.ID}
		global := store.AlertRule{Name: "Global", Scope: store.AlertScopeSite, Kind: store.AlertKindExpression, Expr: "x", Severity: store.SeverityInfo}
		for _, r := range []*store.AlertRule{&limited, &global} {
			if err := rules.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		inGroup := store.Alert{RuleID: global.ID, Fingerprint: "a", State: store.AlertPending, StartedAt: now, ClientGroupID: group.ID}
		noGroup := store.Alert{RuleID: global.ID, Fingerprint: "b", State: store.AlertPending, StartedAt: now}
		for _, a := range []*store.Alert{&inGroup, &noGroup} {
			if err := store.NewAlerts(db).Insert(ctx, a); err != nil {
				t.Fatal(err)
			}
		}

		exp := store.Export{Kind: store.ExportSnapshot, SiteID: id.New(), ClientGroupID: group.ID}
		if err := store.NewExports(db).Create(ctx, &exp); err != nil {
			t.Fatal(err)
		}

		tests := []struct {
			typ, id, want string
		}{
			{"export", exp.ID, group.ID},
			{"export", id.New(), ""},
			{"alert_rule", limited.ID, group.ID},
			{"alert_rule", global.ID, ""},
			{"alert_rule", id.New(), ""},
			{"alert", inGroup.ID, group.ID},
			{"alert", noGroup.ID, ""},
			{"alert", id.New(), ""},
			{"client_group", group.ID, group.ID},
			{"site", site, group.ID},
			{"site", id.New(), ""},
			{"job", siteJob.ID, group.ID},
			{"job", groupJob.ID, group.ID},
			{"job", orphanJob.ID, ""},
			{"job", id.New(), ""},
			{"user", id.New(), ""},
			{"", "", ""},
		}
		for _, tt := range tests {
			got, err := store.TargetClientGroup(ctx, db, tt.typ, tt.id)
			if err != nil || got != tt.want {
				t.Errorf("TargetClientGroup(%s, %s) = %q, %v; want %q", tt.typ, tt.id, got, err, tt.want)
			}
		}
	})
}
