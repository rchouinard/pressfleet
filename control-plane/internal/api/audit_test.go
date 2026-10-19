package api_test

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// httptest requests come from this address.
const testClientIP = "192.0.2.1"

// allEvents returns every audit event, newest first.
func allEvents(t *testing.T, db *store.DB) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	f := store.AuditFilter{Limit: 200}
	for {
		page, err := store.NewAuditEvents(db).List(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, page.Events...)
		if page.NextCursor == "" {
			return out
		}
		f.Before = page.NextCursor
	}
}

// sessionOnlyOperations are the operations only dashboard sessions may
// call: creating API tokens and managing second factors.
var sessionOnlyOperations = map[string]bool{
	"createApiToken": true, "regenerateRecoveryCodes": true,
	"beginWebAuthnRegistration": true, "registerWebAuthnCredential": true,
	"renameWebAuthnCredential": true, "removeWebAuthnCredential": true,
	"startTotpEnrollment": true, "confirmTotpEnrollment": true, "removeTotp": true,
}

// auditedReads are the reads recorded like changes (auditRead).
var auditedReads = map[string]bool{"downloadSiteDatabaseExport": true, "downloadExportFile": true}

func isUnsafe(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// TestEveryStateChangingOperationIsAudited sends every operation in the
// OpenAPI document as an authenticated admin token and expects exactly one
// audit event for each state-changing one, whether it succeeded or failed,
// and none for reads, except the reads that hand out sensitive data
// (auditedReads).
func TestEveryStateChangingOperationIsAudited(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, _ := newAuthServiceOn(db)
		u, err := svc.CreateUser(context.Background(), auth.NewUser{Email: "admin@example.com", Password: testPassword, Role: store.RoleAdmin})
		if err != nil {
			t.Fatal(err)
		}
		const tokenID = "01j9zq3v6t8k4m2n5p7r9s1x3z"
		fa := &fakeAuth{principal: &auth.Principal{UserID: u.ID, Email: u.Email, Role: u.Role, Method: auth.MethodToken, TokenID: tokenID, Scope: store.ScopeFull}}
		h := newHandler(t, api.Options{Authenticator: fa, Auth: svc, DB: db})
		for _, op := range ops {
			t.Run(op.ID, func(t *testing.T) {
				before := len(allEvents(t, db))
				w := do(t, h, sampleRequest(op, true))
				events := allEvents(t, db)
				if op.ID == "loginSecondFactor" {
					// Public, and without a pending login there is no user
					// to attribute it to, so nothing is recorded.
					if len(events) != before {
						t.Errorf("second step without a pending login recorded %d audit events", len(events)-before)
					}
					return
				}
				if !isUnsafe(op.Method) && !auditedReads[op.ID] {
					if len(events) != before {
						t.Errorf("read recorded %d audit events: %+v", len(events)-before, events[0])
					}
					return
				}
				if len(events) != before+1 {
					t.Fatalf("status %d produced %d audit events, want 1", w.Code, len(events)-before)
				}
				e := events[0]
				want := store.AuditEvent{
					ID: e.ID, OccurredAt: e.OccurredAt, ActorUserID: u.ID, ActorTokenID: tokenID, Action: string(op.Action),
					TargetType: e.TargetType, TargetID: e.TargetID, Result: store.AuditSuccess, SourceIP: testClientIP,
					// Details depend on how far the operation got; this
					// test is about attribution.
					Details: e.Details,
				}
				if w.Code >= 400 {
					want.Result = store.AuditFailure
				}
				if sessionOnlyOperations[op.ID] {
					// Only sessions may create tokens and manage second
					// factors, so the token caller is denied.
					want.Result = store.AuditDenied
				}
				if op.ID == "login" {
					// Public: the sample credentials are wrong, so there is
					// no actor.
					want.Action, want.ActorUserID, want.ActorTokenID = "self.login", "", ""
				}
				if !reflect.DeepEqual(e, want) {
					t.Errorf("event = %+v, want %+v (status %d)", e, want, w.Code)
				}
			})
		}
	})
}

// TestDenialsAreAudited calls every protected operation as a caller whose
// role allows nothing, and expects one denied event each, reads included.
func TestDenialsAreAudited(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t, store.SQLite)
	svc, _ := newAuthServiceOn(db)
	const userID = "01j9zq3v6t8k4m2n5p7r9s1x3y"
	nobody := &auth.Principal{UserID: userID, Role: "nobody", Method: auth.MethodSession, SessionID: "s"}
	h := newHandler(t, api.Options{Authenticator: &fakeAuth{principal: nobody}, Auth: svc, DB: db})
	for _, op := range ops {
		if op.Public {
			continue
		}
		t.Run(op.ID, func(t *testing.T) {
			before := len(allEvents(t, db))
			w := do(t, h, sampleRequest(op, true))
			expectStatus(t, w, http.StatusForbidden, op.ID)
			events := allEvents(t, db)
			if len(events) != before+1 {
				t.Fatalf("%d audit events, want 1", len(events)-before)
			}
			if e := events[0]; e.Result != store.AuditDenied || e.Action != string(op.Action) || e.ActorUserID != userID || e.ActorTokenID != "" {
				t.Errorf("event = %+v", e)
			}
		})
	}
}

func TestAuditLogOverHTTP(t *testing.T) {
	e := newAuthEnv(t, api.Options{})
	start := time.Now().UTC().Add(-time.Second)
	admin := e.mustLogin()
	if _, w := e.login("admin@example.com", "wrong password", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("failed login: %d", w.Code)
	}
	viewerID := e.createUser(admin, "viewer@example.com", store.RoleViewer)
	viewer := e.mustLoginAs("viewer@example.com")
	tokenID, token := e.createToken(admin, `{"name":"ci","scope":"full"}`)
	newPassword := "a brand new password"
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/password",
		`{"current_password":"`+testPassword+`","new_password":"`+newPassword+`"}`, admin.creds()), http.StatusNoContent, "change password")
	expectStatus(t, e.do(http.MethodDelete, "/api/v1/sites/abc123", "", viewer.creds()), http.StatusForbidden, "viewer deletes site")
	expectStatus(t, e.do(http.MethodGet, "/api/v1/audit-events", "", viewer.creds()), http.StatusForbidden, "viewer reads audit log")
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/logout", "", viewer.creds()), http.StatusNoContent, "viewer logs out")

	list := func(query string) gen.AuditEventList {
		t.Helper()
		w := e.do(http.MethodGet, "/api/v1/audit-events?"+query, "", admin.creds())
		if w.Code != http.StatusOK {
			t.Fatalf("list %q: %d %s", query, w.Code, w.Body)
		}
		return decodeJSON[gen.AuditEventList](t, w.Body.Bytes())
	}
	actions := func(l gen.AuditEventList) string {
		var out []string
		for _, ev := range l.Items {
			out = append(out, ev.Action+":"+string(ev.Result))
		}
		return strings.Join(out, " ")
	}

	all := list("")
	want := "self.logout:success audit.view:denied site.delete:denied self.password.change:success self.tokens.create:success " +
		"self.login:success user.create:success self.login:failure self.login:success"
	if got := actions(all); got != want {
		t.Errorf("events:\n got %s\nwant %s", got, want)
	}
	for _, ev := range all.Items {
		if ev.SourceIP != testClientIP || ev.OccurredAt.Before(start) {
			t.Errorf("event %+v: source IP or time", ev)
		}
	}
	// The details of a few events.
	byAction := map[string]gen.AuditEvent{}
	for _, ev := range all.Items {
		byAction[ev.Action+":"+string(ev.Result)] = ev
	}
	if ev := byAction["self.login:failure"]; ev.ActorUserID != nil || ev.TargetID != nil {
		t.Errorf("failed login: %+v", ev)
	}
	if ev := byAction["user.create:success"]; ev.ActorUserID == nil || *ev.ActorUserID != e.user.ID || ev.TargetID == nil || *ev.TargetID != viewerID {
		t.Errorf("user.create: %+v", ev)
	}
	if ev := byAction["self.tokens.create:success"]; ev.TargetType == nil || *ev.TargetType != "api_token" || *ev.TargetID != tokenID || ev.ActorTokenID != nil {
		t.Errorf("token create: %+v", ev)
	}
	if ev := byAction["site.delete:denied"]; *ev.ActorUserID != viewerID || *ev.TargetType != "site" || *ev.TargetID != "abc123" {
		t.Errorf("denied site delete: %+v", ev)
	}

	// No secrets anywhere in the log.
	raw := e.do(http.MethodGet, "/api/v1/audit-events?limit=200", "", admin.creds()).Body.String()
	for _, secret := range []string{token, testPassword, newPassword, admin.cookie.Value, admin.csrf} {
		if strings.Contains(raw, secret) {
			t.Errorf("audit log contains a secret: %s", raw)
		}
	}

	// Filters and pages.
	tests := []struct {
		query, want string
	}{
		{"actor_user_id=" + viewerID, "self.logout:success audit.view:denied site.delete:denied self.login:success"},
		{"action=self.login", "self.login:success self.login:failure self.login:success"},
		{"target_type=site&target_id=abc123", "site.delete:denied"},
		{"until=" + url.QueryEscape(start.Format(time.RFC3339Nano)), ""},
		{"since=" + url.QueryEscape(start.Format(time.RFC3339Nano)) + "&action=user.create", "user.create:success"},
	}
	for _, tt := range tests {
		if got := actions(list(tt.query)); got != tt.want {
			t.Errorf("%s:\n got %s\nwant %s", tt.query, got, tt.want)
		}
	}
	var paged []string
	query := "limit=4"
	for range 5 {
		l := list(query)
		for _, ev := range l.Items {
			paged = append(paged, ev.Action+":"+string(ev.Result))
		}
		if l.NextCursor == nil {
			break
		}
		query = "limit=4&cursor=" + *l.NextCursor
	}
	if got := strings.Join(paged, " "); got != want {
		t.Errorf("paged:\n got %s\nwant %s", got, want)
	}
	expectStatus(t, e.do(http.MethodGet, "/api/v1/audit-events?cursor=bogus", "", admin.creds()), http.StatusBadRequest, "bad cursor")

	// Listing is a read: it isn't audited itself.
	if n := len(list("limit=200").Items); n != len(all.Items) {
		t.Errorf("listing added events: %d, then %d", len(all.Items), n)
	}
}

// A state-changing request whose handler panics is still audited, as a
// failure, and answered with a 500 by the recovery middleware.
func TestPanickingRequestIsAudited(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		// No site service: deleting a site panics past authorization.
		h := newHandler(t, api.Options{Authenticator: &fakeAuth{}, DB: db})
		w := do(t, h, request{authed: true, method: http.MethodDelete, target: "/api/v1/sites/01j9zq3v6t8k4m2n5p7r9s1x3y"})
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500", w.Code)
		}
		events := allEvents(t, db)
		if len(events) != 1 || events[0].Action != "site.delete" || events[0].Result != store.AuditFailure {
			t.Errorf("events = %+v, want one failed site.delete", events)
		}
	})
}
