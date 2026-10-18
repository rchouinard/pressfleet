package api_test

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/alertchannels"
	"github.com/rchouinard/pressfleet/control-plane/internal/alertchannels/alertchannelstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// newAlertChannels returns an alert channel service that may connect to
// loopback, where the test receivers listen.
func newAlertChannels(t *testing.T, db *store.DB) *alertchannels.Service {
	t.Helper()
	svc, err := alertchannels.New(alertchannels.Options{
		DB: db, Secrets: secrets.NewStore(db, secretstest.Keyring(t, 1)), AllowLocalTargets: true,
		WebhookTimeout: 5 * time.Second, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func channelsHandler(t *testing.T, db *store.DB, p *auth.Principal) http.Handler {
	t.Helper()
	return newHandler(t, api.Options{DB: db, AlertChannels: newAlertChannels(t, db), Authenticator: &fakeAuth{principal: p}})
}

func TestAlertChannelEndpoints(t *testing.T) {
	ctx := context.Background()
	const slackURL = "https://hooks.slack.com/services/T000/B000/XXXXSECRET"
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := channelsHandler(t, db, nil)
		call := func(method, target, body string) (int, []byte) {
			t.Helper()
			w := do(t, h, request{authed: true, method: method, target: target, body: body})
			return w.Code, w.Body.Bytes()
		}

		// Create a webhook channel: the URL and signing secret go in, and
		// only a redacted form comes out.
		w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-channels",
			body: `{"name":"Slack","type":"webhook","min_severity":"warning","webhook":{"url":"` + slackURL + `","format":"slack","signing_secret":"s3cret"}}`})
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body)
		}
		slack := decodeJSON[gen.AlertChannel](t, w.Body.Bytes())
		if !id.Valid(slack.ID) || slack.Name != "Slack" || slack.Type != "webhook" || !slack.Enabled || slack.MinSeverity != "warning" ||
			slack.Email != nil || slack.Webhook == nil || slack.Webhook.URL != "https://hooks.slack.com/…" || slack.Webhook.Format != "slack" ||
			!slack.Webhook.HasSigningSecret || slack.RuleID != nil || slack.ClientGroupID != nil || slack.CreatedAt.IsZero() {
			t.Errorf("created = %s", w.Body)
		}
		if loc := w.Header().Get("Location"); loc != "/api/v1/alert-channels/"+slack.ID {
			t.Errorf("Location = %q", loc)
		}
		if code, body := call(http.MethodPost, "/api/v1/alert-channels", `{"name":"Slack","type":"webhook","webhook":{"url":"https://example.com/x"}}`); code != http.StatusConflict {
			t.Errorf("create duplicate: %d %s", code, body)
		}

		// An email channel with its own SMTP server; the platform relay
		// isn't configured here, so one without a host is refused.
		code, body := call(http.MethodPost, "/api/v1/alert-channels", `{"name":"Mail","type":"email","enabled":false,"email":{"host":"smtp.example.com",`+
			`"encryption":"tls","username":"u","password":"smtp-pass","from_email":"pf@example.com","from_name":"PressFleet","to":["ops@example.com"]}}`)
		if code != http.StatusCreated {
			t.Fatalf("create email: %d %s", code, body)
		}
		mail := decodeJSON[gen.AlertChannel](t, body)
		if mail.Enabled || mail.Webhook != nil || mail.Email == nil || mail.Email.Host != "smtp.example.com" || mail.Email.Encryption != "tls" ||
			!mail.Email.HasPassword || mail.Email.UsesPlatformRelay || mail.Email.Username != "u" || len(mail.Email.To) != 1 || mail.MinSeverity != "info" {
			t.Errorf("created email = %s", body)
		}
		code, body = call(http.MethodPost, "/api/v1/alert-channels", `{"name":"Relay","type":"email","email":{"to":["ops@example.com"]}}`)
		if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "/email/host") {
			t.Errorf("create email without a relay: %d %s", code, body)
		}

		// Validation reports every problem with JSON Pointers.
		w = do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-channels",
			body: `{"name":"Bad","type":"webhook","rule_id":"` + id.New() + `","client_group_id":"` + id.New() + `","webhook":{"url":"ftp://x"}}`})
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create invalid: %d %s", w.Code, w.Body)
		}
		fields := map[string]bool{}
		for _, e := range decodeProblem(t, w).Errors {
			fields[e.Field] = true
		}
		for _, f := range []string{"/rule_id", "/client_group_id", "/webhook/url"} {
			if !fields[f] {
				t.Errorf("no error for %s in %v", f, fields)
			}
		}
		for name, b := range map[string]string{
			"unknown field":    `{"name":"x","type":"webhook","webhook":{"url":"https://x.example"},"token":"y"}`,
			"unknown type":     `{"name":"x","type":"sms"}`,
			"no url":           `{"name":"x","type":"webhook","webhook":{}}`,
			"no recipients":    `{"name":"x","type":"email","email":{"to":[]}}`,
			"unknown format":   `{"name":"x","type":"webhook","webhook":{"url":"https://x.example","format":"discord"}}`,
			"unknown severity": `{"name":"x","type":"webhook","min_severity":"urgent","webhook":{"url":"https://x.example"}}`,
		} {
			if code, body := call(http.MethodPost, "/api/v1/alert-channels", b); code != http.StatusUnprocessableEntity {
				t.Errorf("create with %s: %d %s", name, code, body)
			}
		}

		// Get and list never return a credential.
		code, body = call(http.MethodGet, "/api/v1/alert-channels/"+slack.ID, "")
		if got := decodeJSON[gen.AlertChannel](t, body); code != http.StatusOK || got.ID != slack.ID {
			t.Errorf("get: %d %s", code, body)
		}
		if code, _ := call(http.MethodGet, "/api/v1/alert-channels/"+id.New(), ""); code != http.StatusNotFound {
			t.Errorf("get missing: %d", code)
		}
		code, body = call(http.MethodGet, "/api/v1/alert-channels", "")
		list := decodeJSON[gen.AlertChannelList](t, body)
		if code != http.StatusOK || len(list.Items) != 2 || list.Items[0].Name != "Mail" || list.Items[1].Name != "Slack" || list.NextCursor != nil {
			t.Errorf("list: %d %s", code, body)
		}
		for _, b := range [][]byte{body, w.Body.Bytes()} {
			for _, secret := range []string{"XXXXSECRET", "s3cret", "smtp-pass", "services/T000"} {
				if strings.Contains(string(b), secret) {
					t.Errorf("a response contains %q: %s", secret, b)
				}
			}
		}
		// Paging, and the type and enabled filters.
		code, body = call(http.MethodGet, "/api/v1/alert-channels?limit=1", "")
		page := decodeJSON[gen.AlertChannelList](t, body)
		if code != http.StatusOK || len(page.Items) != 1 || page.NextCursor == nil {
			t.Fatalf("list page 1: %d %s", code, body)
		}
		code, body = call(http.MethodGet, "/api/v1/alert-channels?limit=1&cursor="+*page.NextCursor, "")
		if page2 := decodeJSON[gen.AlertChannelList](t, body); code != http.StatusOK || len(page2.Items) != 1 || page2.Items[0].Name != "Slack" || page2.NextCursor != nil {
			t.Errorf("list page 2: %d %s", code, body)
		}
		if code, body = call(http.MethodGet, "/api/v1/alert-channels?type=email&enabled=false", ""); len(decodeJSON[gen.AlertChannelList](t, body).Items) != 1 {
			t.Errorf("list filters: %d %s", code, body)
		}
		if code, _ = call(http.MethodGet, "/api/v1/alert-channels?cursor=nonsense", ""); code != http.StatusBadRequest {
			t.Errorf("list with a bad cursor: %d", code)
		}

		// Update: fields present change, credentials stay unless sent.
		code, body = call(http.MethodPatch, "/api/v1/alert-channels/"+slack.ID, `{"name":"Slack #ops","min_severity":"critical","webhook":{"format":"generic"}}`)
		got := decodeJSON[gen.AlertChannel](t, body)
		if code != http.StatusOK || got.Name != "Slack #ops" || got.MinSeverity != "critical" || got.Webhook.Format != "generic" ||
			got.Webhook.URL != "https://hooks.slack.com/…" || !got.Webhook.HasSigningSecret {
			t.Errorf("update: %d %s", code, body)
		}
		code, body = call(http.MethodPatch, "/api/v1/alert-channels/"+slack.ID, `{"webhook":{"signing_secret":""}}`)
		if got := decodeJSON[gen.AlertChannel](t, body); code != http.StatusOK || got.Webhook.HasSigningSecret || got.Webhook.URL != "https://hooks.slack.com/…" {
			t.Errorf("remove the signing secret: %d %s", code, body)
		}
		code, body = call(http.MethodPatch, "/api/v1/alert-channels/"+mail.ID, `{"email":{"to":["a@example.com","b@example.com"],"password":"new"}}`)
		if got := decodeJSON[gen.AlertChannel](t, body); code != http.StatusOK || len(got.Email.To) != 2 || got.Email.Host != "smtp.example.com" || !got.Email.HasPassword {
			t.Errorf("update recipients: %d %s", code, body)
		}
		for name, c := range map[string][2]string{
			"taken name":            {slack.ID, `{"name":"Mail"}`},
			"email on a webhook":    {slack.ID, `{"email":{"to":["a@example.com"]}}`},
			"webhook on an email":   {mail.ID, `{"webhook":{"format":"slack"}}`},
			"unknown format":        {slack.ID, `{"webhook":{"format":"discord"}}`},
			"empty update":          {slack.ID, `{}`},
			"missing rule":          {slack.ID, `{"rule_id":"` + id.New() + `"}`},
			"recipient with a name": {mail.ID, `{"email":{"to":["Ops <ops@example.com>"]}}`},
		} {
			want := http.StatusUnprocessableEntity
			if name == "taken name" {
				want = http.StatusConflict
			}
			if code, body := call(http.MethodPatch, "/api/v1/alert-channels/"+c[0], c[1]); code != want {
				t.Errorf("update with %s: %d %s", name, code, body)
			}
		}
		if code, _ := call(http.MethodPatch, "/api/v1/alert-channels/"+id.New(), `{"name":"x"}`); code != http.StatusNotFound {
			t.Errorf("update missing: %d", code)
		}

		// Enable and disable.
		code, body = call(http.MethodPost, "/api/v1/alert-channels/"+mail.ID+"/enable", "")
		if got := decodeJSON[gen.AlertChannel](t, body); code != http.StatusOK || !got.Enabled || !got.Email.HasPassword {
			t.Errorf("enable: %d %s", code, body)
		}
		code, body = call(http.MethodPost, "/api/v1/alert-channels/"+mail.ID+"/disable", "")
		if got := decodeJSON[gen.AlertChannel](t, body); code != http.StatusOK || got.Enabled {
			t.Errorf("disable: %d %s", code, body)
		}
		if code, _ := call(http.MethodPost, "/api/v1/alert-channels/"+id.New()+"/enable", ""); code != http.StatusNotFound {
			t.Errorf("enable missing: %d", code)
		}

		// Delete takes the credentials along.
		if code, body := call(http.MethodDelete, "/api/v1/alert-channels/"+mail.ID, ""); code != http.StatusNoContent {
			t.Errorf("delete: %d %s", code, body)
		}
		if code, _ := call(http.MethodDelete, "/api/v1/alert-channels/"+mail.ID, ""); code != http.StatusNotFound {
			t.Errorf("delete twice: %d", code)
		}
		var n int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM secrets WHERE kind = 'alert-channel-credentials'").Scan(&n); err != nil || n != 1 {
			t.Errorf("channel secrets left = %d, %v; want 1", n, err)
		}
	})
}

func TestAlertChannelTestEndpoint(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := channelsHandler(t, db, nil)
		hook := alertchannelstest.NewWebhook(t)
		w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-channels", body: `{"name":"Hook","type":"webhook","webhook":{"url":"` + hook.URL + `"}}`})
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body)
		}
		ch := decodeJSON[gen.AlertChannel](t, w.Body.Bytes())

		w = do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-channels/" + ch.ID + "/test"})
		res := decodeJSON[gen.AlertChannelTestResult](t, w.Body.Bytes())
		if w.Code != http.StatusOK || !res.Success || res.Error != nil || len(hook.Requests()) != 1 {
			t.Errorf("test: %d %s", w.Code, w.Body)
		}

		hook.SetStatus(500)
		w = do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-channels/" + ch.ID + "/test"})
		res = decodeJSON[gen.AlertChannelTestResult](t, w.Body.Bytes())
		if w.Code != http.StatusOK || res.Success || res.Error == nil || !strings.Contains(*res.Error, "HTTP 500") || strings.Contains(w.Body.String(), "secret-token") {
			t.Errorf("failing test: %d %s", w.Code, w.Body)
		}
		if w = do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-channels/" + id.New() + "/test"}); w.Code != http.StatusNotFound {
			t.Errorf("test of a missing channel: %d", w.Code)
		}
	})
}

func TestAlertDeliveryEndpoint(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := channelsHandler(t, db, nil)
		rule := store.AlertRule{Name: "Down", Scope: store.AlertScopeSite, Kind: store.AlertKindExpression, Expr: "x", Severity: store.SeverityInfo, Enabled: true}
		if err := store.NewAlertRules(db).Create(ctx, &rule); err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		alert := store.Alert{RuleID: rule.ID, Fingerprint: "f", State: store.AlertFiring, Severity: store.SeverityInfo, StartedAt: now, FiredAt: &now}
		if err := store.NewAlerts(db).Insert(ctx, &alert); err != nil {
			t.Fatal(err)
		}
		ch := store.AlertChannel{Name: "Hook", Type: store.ChannelWebhook, MinSeverity: store.SeverityInfo, Config: "{}"}
		if err := store.NewAlertChannels(db).Create(ctx, &ch); err != nil {
			t.Fatal(err)
		}
		d := store.NewAlertDeliveries(db)
		for _, e := range []store.AlertEventType{store.AlertEventFiring, store.AlertEventResolved} {
			if _, err := d.Enqueue(ctx, alert.ID, e, ch.ID, "{}"); err != nil {
				t.Fatal(err)
			}
		}
		list, _ := d.List(ctx, store.AlertDeliveryFilter{})
		if err := d.MarkAttemptFailed(ctx, list[0].ID, 8, "HTTP 500", time.Time{}, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := d.MarkDelivered(ctx, list[1].ID, 1, time.Now()); err != nil {
			t.Fatal(err)
		}

		get := func(query string) gen.AlertDeliveryList {
			t.Helper()
			w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/alert-deliveries" + query})
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s: %d %s", query, w.Code, w.Body)
			}
			return decodeJSON[gen.AlertDeliveryList](t, w.Body.Bytes())
		}
		all := get("?alert_id=" + alert.ID)
		if len(all.Items) != 2 || all.Items[0].Event != "resolved" || all.Items[1].Event != "firing" {
			t.Fatalf("deliveries = %+v", all.Items)
		}
		resolved, firing := all.Items[0], all.Items[1]
		if resolved.State != "failed" || resolved.Attempts != 8 || resolved.LastError == nil || *resolved.LastError != "HTTP 500" || resolved.NextAttemptAt != nil || resolved.DeliveredAt != nil ||
			resolved.ChannelName != "Hook" || resolved.ChannelID != ch.ID || resolved.AlertID != alert.ID {
			t.Errorf("failed delivery = %+v", resolved)
		}
		if firing.State != "delivered" || firing.DeliveredAt == nil || firing.LastError != nil || firing.Attempts != 1 {
			t.Errorf("delivered delivery = %+v", firing)
		}
		if got := get("?state=failed"); len(got.Items) != 1 || got.Items[0].ID != resolved.ID {
			t.Errorf("state filter = %+v", got.Items)
		}
		if got := get("?channel_id=" + id.New()); len(got.Items) != 0 {
			t.Errorf("channel filter = %+v", got.Items)
		}
		if page := get("?limit=1"); len(page.Items) != 1 || page.NextCursor == nil {
			t.Errorf("page 1 = %+v", page)
		} else if next := get("?limit=1&cursor=" + *page.NextCursor); len(next.Items) != 1 || next.Items[0].ID != firing.ID || next.NextCursor != nil {
			t.Errorf("page 2 = %+v", next)
		}
		if w := do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/alert-deliveries?cursor=zzz"}); w.Code != http.StatusBadRequest {
			t.Errorf("bad cursor: %d", w.Code)
		}
	})
}

func TestAlertChannelPermissions(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ch := store.AlertChannel{Name: "Existing", Type: store.ChannelWebhook, MinSeverity: store.SeverityInfo, Config: `{"format":"generic"}`}
		if err := store.NewAlertChannels(db).Create(ctx, &ch); err != nil {
			t.Fatal(err)
		}
		principal := func(role store.Role, method auth.Method, scope store.TokenScope) *auth.Principal {
			return &auth.Principal{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: role, Method: method, Scope: scope, SessionID: "s", TokenID: "t"}
		}
		// Channels are platform settings: only Admins write (design doc §4.6).
		callers := []struct {
			name              string
			p                 *auth.Principal
			canRead, canWrite bool
		}{
			{"viewer", principal(store.RoleViewer, auth.MethodSession, ""), true, false},
			{"operator", principal(store.RoleOperator, auth.MethodSession, ""), true, false},
			{"admin", principal(store.RoleAdmin, auth.MethodSession, ""), true, true},
			{"admin with a read-only token", principal(store.RoleAdmin, auth.MethodToken, store.ScopeReadOnly), true, false},
			{"admin with a full token", principal(store.RoleAdmin, auth.MethodToken, store.ScopeFull), true, true},
		}
		for _, c := range callers {
			t.Run(c.name, func(t *testing.T) {
				h := channelsHandler(t, db, c.p)
				status := func(method, target, body string) int {
					return do(t, h, request{authed: true, method: method, target: target, body: body}).Code
				}
				for _, target := range []string{"/api/v1/alert-channels", "/api/v1/alert-channels/" + ch.ID, "/api/v1/alert-deliveries"} {
					if got := status(http.MethodGet, target, ""); (got == http.StatusOK) != c.canRead {
						t.Errorf("GET %s = %d, canRead = %v", target, got, c.canRead)
					}
				}
				writes := []struct{ method, target, body string }{
					{http.MethodPost, "/api/v1/alert-channels", `{"name":"` + id.New() + `","type":"webhook","webhook":{"url":"https://example.com/x"}}`},
					{http.MethodPatch, "/api/v1/alert-channels/" + ch.ID, `{"name":"renamed"}`},
					{http.MethodPost, "/api/v1/alert-channels/" + ch.ID + "/disable", ""},
					{http.MethodPost, "/api/v1/alert-channels/" + ch.ID + "/enable", ""},
					{http.MethodPost, "/api/v1/alert-channels/" + ch.ID + "/test", ""},
					{http.MethodDelete, "/api/v1/alert-channels/" + id.New(), ""},
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

func TestAlertChannelChangesAreAudited(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		h := channelsHandler(t, db, nil)
		w := do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-channels",
			body: `{"name":"Hook","type":"webhook","webhook":{"url":"https://hooks.example.com/SECRETPATH"}}`})
		if w.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", w.Code, w.Body)
		}
		ch := decodeJSON[gen.AlertChannel](t, w.Body.Bytes())
		do(t, h, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-channels/" + ch.ID + "/disable"})
		do(t, h, request{authed: true, method: http.MethodDelete, target: "/api/v1/alert-channels/" + ch.ID})
		do(t, h, request{authed: true, method: http.MethodGet, target: "/api/v1/alert-channels"})

		events := allEvents(t, db)
		if len(events) != 3 {
			t.Fatalf("audit events = %+v, want one per change and none for the read", events)
		}
		for _, e := range events {
			if e.Action != "platform.settings.manage" || e.TargetType != "alert_channel" || e.TargetID != ch.ID || e.Result != store.AuditSuccess {
				t.Errorf("audit event = %+v", e)
			}
			if strings.Contains(e.Action+e.TargetID+e.TargetType, "SECRETPATH") {
				t.Errorf("audit event holds the URL: %+v", e)
			}
		}

		operator := channelsHandler(t, db, &auth.Principal{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleOperator, Method: auth.MethodSession, SessionID: "s"})
		do(t, operator, request{authed: true, method: http.MethodPost, target: "/api/v1/alert-channels",
			body: `{"name":"Hook","type":"webhook","webhook":{"url":"https://hooks.example.com/x"}}`})
		if events = allEvents(t, db); len(events) != 4 || events[0].Result != store.AuditDenied || events[0].Action != "platform.settings.manage" {
			t.Errorf("audit events after a denial = %+v", events)
		}
	})
}
