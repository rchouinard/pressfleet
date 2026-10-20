package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// newPolicyAuthEnv is newAuthEnv on db with a two-factor policy.
func newPolicyAuthEnv(t *testing.T, db *store.DB, required auth.TwoFactorRequirement) *authEnv {
	t.Helper()
	clk := &testClock{t: time.Now()}
	svc := auth.NewService(db, auth.Options{
		Logger:         storetest.Logger(),
		Now:            clk.now,
		PasswordParams: auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32},
		TwoFactor:      auth.TwoFactorPolicy{Required: required},
	})
	u, err := svc.CreateUser(context.Background(), auth.NewUser{Email: "admin@example.com", Password: testPassword, Role: store.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	opts := api.Options{DB: db, Sites: newSiteEnv(t, db, testPreviewBase).svc, Auth: svc, Authenticator: api.NewAuthenticator(svc)}
	return &authEnv{t: t, h: newHandler(t, opts), db: db, svc: svc, clock: clk, user: u}
}

type enrollmentMe struct {
	TwoFactorRequired  bool `json:"two_factor_required"`
	EnrollmentRequired bool `json:"enrollment_required"`
}

func decodeEnrollmentMe(t *testing.T, w interface{ Result() *http.Response }, what string) enrollmentMe {
	t.Helper()
	var me enrollmentMe
	if err := json.NewDecoder(w.Result().Body).Decode(&me); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	return me
}

// expectEnrollmentRequired checks that w is the 403 of a session that
// must enroll.
func expectEnrollmentRequired(t *testing.T, w interface {
	Result() *http.Response
}, what string,
) {
	t.Helper()
	res := w.Result()
	var p problem
	if err := json.NewDecoder(res.Body).Decode(&p); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if res.StatusCode != http.StatusForbidden || p.Type != api.TwoFactorEnrollmentRequiredProblemType || p.Status != http.StatusForbidden {
		t.Errorf("%s: %d %+v, want 403 with type %s", what, res.StatusCode, p, api.TwoFactorEnrollmentRequiredProblemType)
	}
}

// TestForcedEnrollmentOverHTTP is the acceptance criterion: with
// required: admins, an Admin without a second factor can only enroll until
// they do, then has full access; Operators and the Admin's API tokens are
// unaffected.
func TestForcedEnrollmentOverHTTP(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		e := newPolicyAuthEnv(t, db, auth.RequireAdmins)
		_, tok, err := e.svc.CreateAPIToken(ctx, e.user.ID, auth.NewAPIToken{Name: "ci", Scope: store.ScopeFull})
		if err != nil {
			t.Fatal(err)
		}
		bearer := creds{bearer: tok.Reveal()}

		// The password still logs in, into a restricted session.
		s, w := e.login("admin@example.com", testPassword, nil)
		if s.cookie == nil {
			t.Fatalf("login: %d %s", w.Code, w.Body)
		}
		if me := decodeEnrollmentMe(t, w, "login"); !me.TwoFactorRequired || !me.EnrollmentRequired {
			t.Errorf("login body: %s", w.Body)
		}
		w = e.do(http.MethodGet, "/api/v1/auth/me", "", s.creds())
		expectStatus(t, w, http.StatusOK, "me")
		if me := decodeEnrollmentMe(t, w, "me"); !me.TwoFactorRequired || !me.EnrollmentRequired {
			t.Errorf("me: %s", w.Body)
		}
		expectEnrollmentRequired(t, e.do(http.MethodGet, "/api/v1/sites", "", s.creds()), "list sites")
		expectEnrollmentRequired(t, e.do(http.MethodGet, "/api/v1/users", "", s.creds()), "list users")
		expectEnrollmentRequired(t, e.do(http.MethodPost, "/api/v1/auth/tokens", `{"name":"x","scope":"full"}`, s.creds()), "create token")
		// Enrolling is allowed: regenerating recovery codes reaches the
		// handler, which wants a factor first.
		w = e.do(http.MethodPost, "/api/v1/users/me/two-factor/recovery-codes", "", s.creds())
		expectStatus(t, w, http.StatusConflict, "regenerate recovery codes")

		// The Admin's token isn't restricted.
		expectStatus(t, e.do(http.MethodGet, "/api/v1/sites", "", bearer), http.StatusOK, "token lists sites")
		expectStatus(t, e.do(http.MethodGet, "/api/v1/users", "", bearer), http.StatusOK, "token lists users")
		w = e.do(http.MethodGet, "/api/v1/auth/me", "", bearer)
		expectStatus(t, w, http.StatusOK, "token me")
		if me := decodeEnrollmentMe(t, w, "token me"); !me.TwoFactorRequired || !me.EnrollmentRequired {
			t.Errorf("token me: %s", w.Body)
		}

		// Operators aren't covered.
		if _, err := e.svc.CreateUser(ctx, auth.NewUser{Email: "op@example.com", Password: testPassword, Role: store.RoleOperator}); err != nil {
			t.Fatal(err)
		}
		op := e.mustLoginAs("op@example.com")
		expectStatus(t, e.do(http.MethodGet, "/api/v1/sites", "", op.creds()), http.StatusOK, "operator lists sites")
		w = e.do(http.MethodGet, "/api/v1/auth/me", "", op.creds())
		if me := decodeEnrollmentMe(t, w, "operator me"); me.TwoFactorRequired || me.EnrollmentRequired {
			t.Errorf("operator me: %s", w.Body)
		}

		// Once enrolled, the same session has full access.
		e.enrollAdmin()
		expectStatus(t, e.do(http.MethodGet, "/api/v1/sites", "", s.creds()), http.StatusOK, "enrolled lists sites")
		expectStatus(t, e.do(http.MethodGet, "/api/v1/users", "", s.creds()), http.StatusOK, "enrolled lists users")
		w = e.do(http.MethodGet, "/api/v1/auth/me", "", s.creds())
		if me := decodeEnrollmentMe(t, w, "enrolled me"); !me.TwoFactorRequired || me.EnrollmentRequired {
			t.Errorf("enrolled me: %s", w.Body)
		}
		expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/logout", "", s.creds()), http.StatusNoContent, "logout")
	})
}

// TestRestrictedSessionLogsOut checks logging out from a restricted
// session.
func TestRestrictedSessionLogsOut(t *testing.T) {
	e := newPolicyAuthEnv(t, storetest.Open(t, store.SQLite), auth.RequireAll)
	s := e.mustLogin()
	expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/logout", "", s.creds()), http.StatusNoContent, "logout")
	expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", s.creds()), http.StatusUnauthorized, "me after logout")
}

// TestPolicyOffReportsNothingRequired checks the /auth/me fields under the
// default policy.
func TestPolicyOffReportsNothingRequired(t *testing.T) {
	e := newPolicyAuthEnv(t, storetest.Open(t, store.SQLite), auth.RequireNone)
	s := e.mustLogin()
	w := e.do(http.MethodGet, "/api/v1/auth/me", "", s.creds())
	expectStatus(t, w, http.StatusOK, "me")
	if me := decodeEnrollmentMe(t, w, "me"); me.TwoFactorRequired || me.EnrollmentRequired {
		t.Errorf("me: %s", w.Body)
	}
	expectStatus(t, e.do(http.MethodGet, "/api/v1/sites", "", s.creds()), http.StatusOK, "list sites")
}

// TestRestrictedSessionRefusedEveryOtherOperation calls every protected
// operation as a restricted Admin session: everything but the enrollment
// actions is the enrollment 403.
func TestRestrictedSessionRefusedEveryOtherOperation(t *testing.T) {
	ops, err := api.Operations()
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := newAuthService(t)
	restricted := &auth.Principal{UserID: "01j9zq3v6t8k4m2n5p7r9s1x3y", Role: store.RoleAdmin, Method: auth.MethodSession, SessionID: "s", EnrollmentRequired: true}
	h := newHandler(t, api.Options{Authenticator: &fakeAuth{principal: restricted}, Auth: svc, DB: storetest.Open(t, store.SQLite)})
	allowed := map[authz.Action]bool{
		authz.ViewSelf: true, authz.Logout: true, authz.RegenerateRecoveryCodes: true,
		authz.EnrollTOTP: true, authz.RemoveTOTP: true, authz.RegisterWebAuthnCredential: true,
		authz.Reauthenticate: true, authz.StartReauthentication: true,
	}
	for _, op := range ops {
		if op.Public {
			continue
		}
		t.Run(op.ID, func(t *testing.T) {
			w := do(t, h, sampleRequest(op, true))
			if allowed[op.Action] {
				if w.Code == http.StatusForbidden {
					t.Errorf("status = 403, want the enrollment action allowed; body: %s", w.Body)
				}
				return
			}
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403; body: %s", w.Code, w.Body)
			}
			if p := decodeProblem(t, w); p.Type != api.TwoFactorEnrollmentRequiredProblemType || p.Title != "Two-factor enrollment required" {
				t.Errorf("problem = %+v", p)
			}
		})
	}
}

// TestRestrictedStaleSessionReauthenticatesAndEnrolls covers the two rules
// together (design doc §6.2): enrolling is sensitive, so a session that
// must enroll and is older than the reauthentication max age first
// reauthenticates with its password, then enrolls.
func TestRestrictedStaleSessionReauthenticatesAndEnrolls(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newAuthEnvWith(t, api.Options{DB: db}, auth.TwoFactorPolicy{
			Required: auth.RequireAdmins, Reauthentication: true, ReauthenticationMaxAge: 10 * time.Minute,
		})
		s := e.mustLogin()
		e.clock.add(11 * time.Minute)

		w := e.do(http.MethodPost, "/api/v1/users/me/two-factor/totp", "", s.creds())
		if w.Code != http.StatusForbidden || !isReauthProblem(t, w) {
			t.Fatalf("stale restricted session enrolls: status %d, want 403 reauthentication-required; body: %s", w.Code, w.Body)
		}
		expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/reauthenticate/options", "", s.creds()), http.StatusOK, "reauthentication options")
		body := `{"method":"password","password":"` + testPassword + `"}`
		expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/reauthenticate", body, s.creds()), http.StatusOK, "reauthenticate with the password")
		expectStatus(t, e.do(http.MethodPost, "/api/v1/users/me/two-factor/totp", "", s.creds()), http.StatusOK, "enroll after reauthenticating")
		// Everything else stays refused until the user has a factor.
		expectEnrollmentRequired(t, e.do(http.MethodGet, "/api/v1/sites", "", s.creds()), "list sites before confirming")
	})
}
