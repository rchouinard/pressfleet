package api_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alerts"
	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const downRule = `{"name":"Site down","scope":"site","kind":"expression","expr":"pressfleet_site_up == 0","severity":"critical","for_seconds":120,` +
	`"labels":{"team":"ops"},"annotations":{"summary":"Site {{ .Labels.site_id }} is down"}}`

func TestAlertRuleEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}})
		call := func(method, target, body string) (int, []byte) {
			t.Helper()
			w := do(t, h, request{authed: true, method: method, target: target, body: body})
			return w.Code, w.Body.Bytes()
		}
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}

		// Create an expression rule: operator and threshold don't apply.
		w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-rules",
			body: strings.Replace(downRule, `"severity"`, `"operator":"gt","threshold":5,"severity"`, 1)})
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body)
		}
		down := decodeJSON[gen.AlertRule](t, w.Body.Bytes())
		if !id.Valid(down.ID) || down.Name != "Site down" || down.Scope != "site" || down.Kind != "expression" ||
			down.Operator != nil || down.Threshold != nil || down.ForSeconds != 120 || !down.Enabled || down.Builtin ||
			down.ClientGroupID != nil || down.Labels["team"] != "ops" || down.Description != "" || down.CreatedAt.IsZero() {
			t.Errorf("created = %s", w.Body)
		}
		if loc := w.Header().Get("Location"); loc != "/api/v1/alert-rules/"+down.ID {
			t.Errorf("Location = %q", loc)
		}
		if code, body := call(http.MethodPost, "/api/v1/alert-rules", downRule); code != http.StatusConflict {
			t.Errorf("create duplicate name: %d %s", code, body)
		}

		// Create a threshold rule limited to a client group, disabled.
		code, body := call(http.MethodPost, "/api/v1/alert-rules", `{"name":"Busy","scope":"site","kind":"threshold","expr":"busy","operator":"gte",`+
			`"threshold":0.05,"severity":"warning","enabled":false,"client_group_id":"`+group.ID+`"}`)
		if code != http.StatusCreated {
			t.Fatalf("create threshold rule: %d %s", code, body)
		}
		busy := decodeJSON[gen.AlertRule](t, body)
		if busy.Enabled || busy.Operator == nil || *busy.Operator != "gte" || busy.Threshold == nil || *busy.Threshold != 0.05 ||
			busy.ClientGroupID == nil || *busy.ClientGroupID != group.ID || busy.ForSeconds != 0 || len(busy.Annotations) != 0 {
			t.Errorf("created threshold rule = %s", body)
		}

		// Validation: every problem is reported.
		w = do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-rules",
			body: `{"name":"Bad","scope":"platform","kind":"threshold","expr":"sum(x","severity":"info","client_group_id":"` + group.ID + `",` +
				`"annotations":{"summary":"{{ .Labels.x "},"labels":{"site_id":"x"}}`})
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create invalid rule: %d %s", w.Code, w.Body)
		}
		fields := map[string]bool{}
		for _, e := range decodeProblem(t, w).Errors {
			fields[e.Field] = true
		}
		for _, f := range []string{"/client_group_id", "/operator", "/expr", "/annotations/summary", "/labels/site_id"} {
			if !fields[f] {
				t.Errorf("no error for %s in %v", f, fields)
			}
		}
		if code, body := call(http.MethodPost, "/api/v1/alert-rules", `{"name":"Nowhere","scope":"site","kind":"expression","expr":"x","severity":"info","client_group_id":"`+id.New()+`"}`); code != http.StatusUnprocessableEntity {
			t.Errorf("create for a missing client group: %d %s", code, body)
		}
		for name, b := range map[string]string{
			"unknown field":    `{"name":"x","scope":"site","kind":"expression","expr":"x","severity":"info","enabled_by":"me"}`,
			"unknown severity": `{"name":"x","scope":"site","kind":"expression","expr":"x","severity":"urgent"}`,
			"missing expr":     `{"name":"x","scope":"site","kind":"expression","severity":"info"}`,
		} {
			if code, body := call(http.MethodPost, "/api/v1/alert-rules", b); code != http.StatusUnprocessableEntity {
				t.Errorf("create with %s: %d %s", name, code, body)
			}
		}

		// Get.
		code, body = call(http.MethodGet, "/api/v1/alert-rules/"+down.ID, "")
		if got := decodeJSON[gen.AlertRule](t, body); code != http.StatusOK || got.ID != down.ID || got.Expr != "pressfleet_site_up == 0" {
			t.Errorf("get: %d %s", code, body)
		}
		if code, body := call(http.MethodGet, "/api/v1/alert-rules/"+id.New(), ""); code != http.StatusNotFound {
			t.Errorf("get missing: %d %s", code, body)
		}

		// Update: only the fields present change, and an expression
		// rule can become a threshold rule.
		code, body = call(http.MethodPatch, "/api/v1/alert-rules/"+down.ID,
			`{"kind":"threshold","operator":"lt","threshold":1,"for_seconds":0,"labels":{},"client_group_id":"`+group.ID+`"}`)
		if code != http.StatusOK {
			t.Fatalf("update: %d %s", code, body)
		}
		got := decodeJSON[gen.AlertRule](t, body)
		if got.Kind != "threshold" || got.Operator == nil || *got.Operator != "lt" || got.Threshold == nil || *got.Threshold != 1 ||
			got.ForSeconds != 0 || len(got.Labels) != 0 || got.ClientGroupID == nil || *got.ClientGroupID != group.ID ||
			got.Name != "Site down" || got.Annotations["summary"] != "Site {{ .Labels.site_id }} is down" || got.Severity != "critical" {
			t.Errorf("update = %s", body)
		}
		code, body = call(http.MethodPatch, "/api/v1/alert-rules/"+down.ID, `{"client_group_id":"","kind":"expression"}`)
		if got := decodeJSON[gen.AlertRule](t, body); code != http.StatusOK || got.ClientGroupID != nil || got.Operator != nil || got.Threshold != nil {
			t.Errorf("remove the client group and the threshold: %d %s", code, body)
		}
		if code, body := call(http.MethodPatch, "/api/v1/alert-rules/"+down.ID, `{"name":"Busy"}`); code != http.StatusConflict {
			t.Errorf("rename to a taken name: %d %s", code, body)
		}
		if code, body := call(http.MethodPatch, "/api/v1/alert-rules/"+down.ID, `{"scope":"platform","client_group_id":"`+group.ID+`"}`); code != http.StatusUnprocessableEntity {
			t.Errorf("platform rule limited to a group: %d %s", code, body)
		}
		if code, body := call(http.MethodPatch, "/api/v1/alert-rules/"+down.ID, `{"expr":"x{"}`); code != http.StatusUnprocessableEntity {
			t.Errorf("update with a broken expression: %d %s", code, body)
		}
		if code, body := call(http.MethodPatch, "/api/v1/alert-rules/"+down.ID, `{}`); code != http.StatusUnprocessableEntity {
			t.Errorf("empty update: %d %s", code, body)
		}
		if code, body := call(http.MethodPatch, "/api/v1/alert-rules/"+id.New(), `{"name":"x"}`); code != http.StatusNotFound {
			t.Errorf("update missing: %d %s", code, body)
		}
		// A failed update changed nothing.
		if code, body = call(http.MethodGet, "/api/v1/alert-rules/"+down.ID, ""); decodeJSON[gen.AlertRule](t, body).Name != "Site down" {
			t.Errorf("rule after failed updates: %d %s", code, body)
		}

		// Enable and disable, twice each.
		for _, c := range []struct {
			action  string
			enabled bool
		}{{"disable", false}, {"disable", false}, {"enable", true}, {"enable", true}} {
			code, body := call(http.MethodPost, "/api/v1/alert-rules/"+busy.ID+"/"+c.action, "")
			if got := decodeJSON[gen.AlertRule](t, body); code != http.StatusOK || got.Enabled != c.enabled {
				t.Errorf("%s: %d %s", c.action, code, body)
			}
		}
		if code, body := call(http.MethodPost, "/api/v1/alert-rules/"+id.New()+"/enable", ""); code != http.StatusNotFound {
			t.Errorf("enable missing: %d %s", code, body)
		}

		// List: filters and paging.
		list := func(target string) (names []string, next *string) {
			t.Helper()
			code, body := call(http.MethodGet, target, "")
			if code != http.StatusOK {
				t.Fatalf("list %s: %d %s", target, code, body)
			}
			l := decodeJSON[gen.AlertRuleList](t, body)
			for _, r := range l.Items {
				names = append(names, r.Name)
			}
			return names, l.NextCursor
		}
		if names, next := list("/api/v1/alert-rules?limit=1"); strings.Join(names, ",") != "Busy" || next == nil {
			t.Errorf("first page = %v, next %v", names, next)
		} else if names, next := list("/api/v1/alert-rules?limit=1&cursor=" + *next); strings.Join(names, ",") != "Site down" || next != nil {
			t.Errorf("second page = %v, next %v", names, next)
		}
		if names, _ := list("/api/v1/alert-rules?enabled=false"); len(names) != 0 {
			t.Errorf("disabled rules = %v", names)
		}
		if names, _ := list("/api/v1/alert-rules?client_group_id=" + group.ID); strings.Join(names, ",") != "Busy" {
			t.Errorf("rules of the group = %v", names)
		}
		if names, _ := list("/api/v1/alert-rules?scope=platform"); len(names) != 0 {
			t.Errorf("platform rules = %v", names)
		}
		for _, c := range []string{"bogus!", "Ym9ndXM"} {
			if code, body := call(http.MethodGet, "/api/v1/alert-rules?cursor="+c, ""); code != http.StatusBadRequest {
				t.Errorf("list with cursor %q: %d %s", c, code, body)
			}
		}

		// Delete takes the rule's alerts with it.
		alert := &store.Alert{RuleID: busy.ID, Fingerprint: "f", State: store.AlertFiring, Severity: store.SeverityWarning, StartedAt: time.Now()}
		if err := store.NewAlerts(db).Insert(ctx, alert); err != nil {
			t.Fatal(err)
		}
		if code, body := call(http.MethodDelete, "/api/v1/alert-rules/"+busy.ID, ""); code != http.StatusNoContent {
			t.Errorf("delete: %d %s", code, body)
		}
		if code, body := call(http.MethodDelete, "/api/v1/alert-rules/"+busy.ID, ""); code != http.StatusNotFound {
			t.Errorf("delete twice: %d %s", code, body)
		}
		if code, body := call(http.MethodGet, "/api/v1/alerts/"+alert.ID, ""); code != http.StatusNotFound {
			t.Errorf("get the alert of a deleted rule: %d %s", code, body)
		}
	})
}

func TestBuiltinAlertRules(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}})
		if _, err := alerts.SeedDefaults(ctx, db, storetest.Logger()); err != nil {
			t.Fatal(err)
		}
		call := func(method, target, body string) (int, []byte) {
			t.Helper()
			w := do(t, h, request{authed: true, method: method, target: target, body: body})
			return w.Code, w.Body.Bytes()
		}
		code, body := call(http.MethodGet, "/api/v1/alert-rules?limit=200", "")
		l := decodeJSON[gen.AlertRuleList](t, body)
		if code != http.StatusOK || len(l.Items) != len(alerts.DefaultRules()) {
			t.Fatalf("list: %d %s", code, body)
		}
		var disk gen.AlertRule
		for _, r := range l.Items {
			if !r.Builtin || !r.Enabled {
				t.Errorf("rule %q: builtin = %v, enabled = %v", r.Name, r.Builtin, r.Enabled)
			}
			if r.Name == "Disk nearly full" {
				disk = r
			}
		}

		// They can be edited and disabled, but not deleted.
		code, body = call(http.MethodPatch, "/api/v1/alert-rules/"+disk.ID, `{"threshold":0.8,"for_seconds":60}`)
		if got := decodeJSON[gen.AlertRule](t, body); code != http.StatusOK || *got.Threshold != 0.8 || got.ForSeconds != 60 || !got.Builtin {
			t.Errorf("edit built-in rule: %d %s", code, body)
		}
		code, body = call(http.MethodPost, "/api/v1/alert-rules/"+disk.ID+"/disable", "")
		if got := decodeJSON[gen.AlertRule](t, body); code != http.StatusOK || got.Enabled {
			t.Errorf("disable built-in rule: %d %s", code, body)
		}
		w := do(t, h, request{authed: true, method: http.MethodDelete, target: "/api/v1/alert-rules/" + disk.ID})
		if w.Code != http.StatusConflict || !strings.Contains(decodeProblem(t, w).Detail, "built-in") {
			t.Errorf("delete built-in rule: %d %s", w.Code, w.Body)
		}

		// Seeding again at the next start keeps the edits.
		if n, err := alerts.SeedDefaults(ctx, db, storetest.Logger()); err != nil || n != 0 {
			t.Fatalf("SeedDefaults = %d, %v", n, err)
		}
		code, body = call(http.MethodGet, "/api/v1/alert-rules/"+disk.ID, "")
		if got := decodeJSON[gen.AlertRule](t, body); code != http.StatusOK || got.Enabled || *got.Threshold != 0.8 {
			t.Errorf("built-in rule after seeding again: %d %s", code, body)
		}
	})
}

func TestAlertEndpoints(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}})
		call := func(target string) (int, []byte) {
			t.Helper()
			w := do(t, h, request{authed: true, method: http.MethodGet, target: target})
			return w.Code, w.Body.Bytes()
		}
		group := &store.ClientGroup{Name: "Acme"}
		if err := store.NewClientGroups(db).Create(ctx, group); err != nil {
			t.Fatal(err)
		}
		rule := store.AlertRule{Name: "Down", Scope: store.AlertScopeSite, Kind: store.AlertKindExpression, Expr: "x", Severity: store.SeverityCritical, Enabled: true}
		other := store.AlertRule{Name: "Other", Scope: store.AlertScopePlatform, Kind: store.AlertKindExpression, Expr: "y", Severity: store.SeverityInfo, Enabled: true}
		for _, r := range []*store.AlertRule{&rule, &other} {
			if err := store.NewAlertRules(db).Create(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
		siteA, siteB := id.New(), id.New()
		alertsRepo := store.NewAlerts(db)
		insert := func(r store.AlertRule, fp, site, groupID string, state store.AlertState, start time.Time) store.Alert {
			a := store.Alert{
				RuleID: r.ID, Fingerprint: fp, Labels: map[string]string{"site_id": site}, SiteID: site, ClientGroupID: groupID,
				State: state, Severity: r.Severity, Summary: "Site down", Description: "details", Value: 1.5, StartedAt: start,
			}
			if state != store.AlertPending {
				fired := start.Add(time.Minute)
				a.FiredAt = &fired
			}
			if state == store.AlertResolved {
				resolved := start.Add(time.Hour)
				a.ResolvedAt = &resolved
			}
			if err := alertsRepo.Insert(ctx, &a); err != nil {
				t.Fatal(err)
			}
			return a
		}
		a1 := insert(rule, "1", siteA, group.ID, store.AlertResolved, t0)
		a2 := insert(rule, "2", siteB, "", store.AlertFiring, t0.Add(time.Hour))
		a3 := insert(other, "3", "", "", store.AlertPending, t0.Add(2*time.Hour))

		code, body := call("/api/v1/alerts/" + a1.ID)
		if code != http.StatusOK {
			t.Fatalf("get: %d %s", code, body)
		}
		got := decodeJSON[gen.Alert](t, body)
		if got.ID != a1.ID || got.RuleID != rule.ID || got.RuleName != "Down" || got.State != "resolved" ||
			got.Severity != "critical" || got.SiteID == nil || *got.SiteID != siteA || got.ClientGroupID == nil || *got.ClientGroupID != group.ID ||
			got.Labels["site_id"] != siteA || got.Summary != "Site down" || got.Value != 1.5 || got.FiredAt == nil || got.ResolvedAt == nil ||
			!got.StartedAt.Equal(t0) {
			t.Errorf("get = %s", body)
		}
		if _, body := call("/api/v1/alerts/" + a3.ID); decodeJSON[gen.Alert](t, body).SiteID != nil || decodeJSON[gen.Alert](t, body).FiredAt != nil {
			t.Errorf("get pending platform alert = %s", body)
		}
		if code, body := call("/api/v1/alerts/" + id.New()); code != http.StatusNotFound {
			t.Errorf("get missing: %d %s", code, body)
		}

		ids := func(target string) []string {
			t.Helper()
			code, body := call(target)
			if code != http.StatusOK {
				t.Fatalf("list %s: %d %s", target, code, body)
			}
			var out []string
			for _, a := range decodeJSON[gen.AlertList](t, body).Items {
				out = append(out, a.ID)
			}
			return out
		}
		if got := ids("/api/v1/alerts"); strings.Join(got, ",") != strings.Join([]string{a3.ID, a2.ID, a1.ID}, ",") {
			t.Errorf("list = %v, want newest first", got)
		}
		for target, want := range map[string][]string{
			"/api/v1/alerts?state=firing":                                         {a2.ID},
			"/api/v1/alerts?state=resolved":                                       {a1.ID},
			"/api/v1/alerts?rule_id=" + other.ID:                                  {a3.ID},
			"/api/v1/alerts?site_id=" + siteB:                                     {a2.ID},
			"/api/v1/alerts?client_group_id=" + group.ID:                          {a1.ID},
			"/api/v1/alerts?since=" + t0.Add(30*time.Minute).Format(time.RFC3339): {a3.ID, a2.ID},
			"/api/v1/alerts?until=" + t0.Add(30*time.Minute).Format(time.RFC3339): {a1.ID},
			"/api/v1/alerts?limit=1":                                              {a3.ID},
		} {
			if got := ids(target); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("list %s = %v, want %v", target, got, want)
			}
		}
		// Paging.
		_, body = call("/api/v1/alerts?limit=2")
		l := decodeJSON[gen.AlertList](t, body)
		if len(l.Items) != 2 || l.NextCursor == nil {
			t.Fatalf("first page = %s", body)
		}
		if got := ids("/api/v1/alerts?limit=2&cursor=" + *l.NextCursor); strings.Join(got, ",") != a1.ID {
			t.Errorf("second page = %v", got)
		}
		for _, target := range []string{"/api/v1/alerts?cursor=bogus", "/api/v1/alerts?state=gone", "/api/v1/alerts?since=yesterday"} {
			if code, body := call(target); code != http.StatusBadRequest {
				t.Errorf("list %s: %d %s", target, code, body)
			}
		}
	})
}

// TestAlertPermissions checks design doc §4.6: every role reads rules and
// alerts, Operators and Admins manage rules, and read-only tokens only
// read.
func TestAlertPermissions(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		rule := store.AlertRule{Name: "Existing", Scope: store.AlertScopeSite, Kind: store.AlertKindExpression, Expr: "x", Severity: store.SeverityInfo, Enabled: true}
		if err := store.NewAlertRules(db).Create(ctx, &rule); err != nil {
			t.Fatal(err)
		}
		principal := func(role store.Role, method auth.Method, scope store.TokenScope) *auth.Principal {
			return &auth.Principal{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: role, Method: method, Scope: scope, SessionID: "s", TokenID: "t"}
		}
		callers := []struct {
			name              string
			p                 *auth.Principal
			canRead, canWrite bool
		}{
			{"viewer", principal(store.RoleViewer, auth.MethodSession, ""), true, false},
			{"operator", principal(store.RoleOperator, auth.MethodSession, ""), true, true},
			{"admin", principal(store.RoleAdmin, auth.MethodSession, ""), true, true},
			{"operator with a read-only token", principal(store.RoleOperator, auth.MethodToken, store.ScopeReadOnly), true, false},
			{"admin with a full token", principal(store.RoleAdmin, auth.MethodToken, store.ScopeFull), true, true},
		}
		for _, c := range callers {
			t.Run(c.name, func(t *testing.T) {
				h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{principal: c.p}})
				status := func(method, target, body string) int {
					return do(t, h, request{authed: true, method: method, target: target, body: body}).Code
				}
				for _, target := range []string{"/api/v1/alert-rules", "/api/v1/alert-rules/" + rule.ID, "/api/v1/alerts"} {
					if got := status(http.MethodGet, target, ""); (got == http.StatusOK) != c.canRead {
						t.Errorf("GET %s = %d, canRead = %v", target, got, c.canRead)
					}
				}
				writes := []struct{ method, target, body string }{
					{http.MethodPost, "/api/v1/alert-rules", `{"name":"` + id.New() + `","scope":"site","kind":"expression","expr":"x","severity":"info"}`},
					{http.MethodPatch, "/api/v1/alert-rules/" + rule.ID, `{"description":"x"}`},
					{http.MethodPost, "/api/v1/alert-rules/" + rule.ID + "/disable", ""},
					{http.MethodPost, "/api/v1/alert-rules/" + rule.ID + "/enable", ""},
					{http.MethodDelete, "/api/v1/alert-rules/" + id.New(), ""},
				}
				for _, wr := range writes {
					got := status(wr.method, wr.target, wr.body)
					if allowed := got != http.StatusForbidden; allowed != c.canWrite {
						t.Errorf("%s %s = %d, canWrite = %v", wr.method, wr.target, got, c.canWrite)
					}
				}
			})
		}
	})
}

// TestAlertRuleChangesAreAudited checks that rule changes are recorded as
// the manage action on the rule, and that denials are too.
func TestAlertRuleChangesAreAudited(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{}})
		w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-rules", body: downRule})
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body)
		}
		rule := decodeJSON[gen.AlertRule](t, w.Body.Bytes())
		do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-rules/" + rule.ID + "/disable"})
		do(t, h, request{authed: true, method: http.MethodDelete, target: "/api/v1/alert-rules/" + rule.ID})
		do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/alert-rules"})

		events := allEvents(t, db)
		if len(events) != 3 {
			t.Fatalf("audit events = %+v, want one per change and none for the read", events)
		}
		for _, e := range events {
			if e.Action != "alert_rule.manage" || e.TargetType != "alert_rule" || e.TargetID != rule.ID || e.Result != store.AuditSuccess {
				t.Errorf("audit event = %+v", e)
			}
		}

		viewer := newHandler(t, api.Options{DB: db, Authenticator: &fakeAuth{principal: &auth.Principal{
			UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleViewer, Method: auth.MethodSession, SessionID: "s",
		}}})
		do(t, viewer, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-rules", body: downRule})
		if events = allEvents(t, db); len(events) != 4 || events[0].Result != store.AuditDenied || events[0].Action != "alert_rule.manage" {
			t.Errorf("audit events after a denial = %+v", events)
		}
	})
}
