package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth/authtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// Two-factor flows over HTTP with real factors (#160): codes computed from
// the enrolled secret and assertions from the software authenticator, on
// both dialects. The tests of each endpoint are in totp_test.go,
// webauthn_test.go, twofactor_test.go, reauth_test.go, and
// enrollment_test.go.

// enrollTOTPOverHTTP starts and confirms an authenticator app with c and
// returns its secret and the confirmation's recovery codes, if any.
func (e *authEnv) enrollTOTPOverHTTP(c creds) (string, []string) {
	e.t.Helper()
	w := e.do(http.MethodPost, totpPath, "", c)
	expectStatus(e.t, w, http.StatusOK, "start TOTP enrollment")
	var enrollment struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &enrollment); err != nil {
		e.t.Fatal(err)
	}
	w = e.do(http.MethodPost, totpPath+"/confirm", `{"code":"`+authtest.TOTPCode(enrollment.Secret, e.clock.now())+`"}`, c)
	expectStatus(e.t, w, http.StatusOK, "confirm TOTP")
	var confirmed struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &confirmed); err != nil {
		e.t.Fatal(err)
	}
	return enrollment.Secret, confirmed.RecoveryCodes
}

// sessionFrom returns the session a successful login or second step set.
func sessionFrom(t *testing.T, w *httptest.ResponseRecorder) session {
	t.Helper()
	var body struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == api.SessionCookieName {
			return session{cookie: c, csrf: body.CSRFToken}
		}
	}
	t.Fatalf("no session cookie: %v", w.Result().Cookies())
	return session{}
}

// TestReauthenticateWithEachFactorOverHTTP covers reauthentication (#154)
// with a real authenticator app and security key: a fresh session isn't
// asked, a stale one is refused until it reauthenticates with either
// factor, a TOTP code or WebAuthn assertion can't be used twice, and each
// step is audited.
func TestReauthenticateWithEachFactorOverHTTP(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newWebAuthnEnv(t, db, reauthPolicy)
		s := e.mustLogin()
		secret, codes := e.enrollTOTPOverHTTP(s.creds())
		if len(codes) != auth.RecoveryCodeCount {
			t.Fatalf("first factor issued %d recovery codes", len(codes))
		}
		key := authtest.NewAuthenticator(webAuthnOrigin)
		expectStatus(t, e.registerOverHTTP(s.creds(), key, "YubiKey"), http.StatusCreated, "register")

		// Regenerating recovery codes is sensitive.
		const regenerate = "/api/v1/users/me/two-factor/recovery-codes"
		expectStatus(t, e.do(http.MethodPost, regenerate, "", s.creds()), http.StatusOK, "regenerate from a fresh session")

		options := func() optionsBody {
			t.Helper()
			w := e.do(http.MethodPost, "/api/v1/auth/reauthenticate/options", "", s.creds())
			expectStatus(t, w, http.StatusOK, "reauthentication options")
			o := decodeOptions(t, w)
			if !slices.Equal(o.Methods, []string{"totp", "webauthn", "recovery_code"}) || len(o.WebAuthn) == 0 {
				t.Fatalf("options = %s", w.Body)
			}
			return o
		}
		reauthenticate := func(body string, want int, what string) {
			t.Helper()
			w := e.do(http.MethodPost, "/api/v1/auth/reauthenticate", body, s.creds())
			expectStatus(t, w, want, what)
			if want == http.StatusOK {
				if me := decodeMe(t, w); me.VerifiedAt == nil {
					t.Errorf("%s: no verified_at: %s", what, w.Body)
				}
			}
		}
		stale := func(what string) {
			t.Helper()
			e.clock.add(11 * time.Minute)
			if w := e.do(http.MethodPost, regenerate, "", s.creds()); !isReauthProblem(t, w) {
				t.Fatalf("%s: %d %s, want reauthentication-required", what, w.Code, w.Body)
			}
		}

		// With the authenticator app. The password isn't enough for a user
		// with a second factor, and a code works once.
		stale("stale before TOTP")
		options()
		reauthenticate(`{"method":"password","password":"`+testPassword+`"}`, http.StatusUnprocessableEntity, "password with a factor")
		code := authtest.TOTPCode(secret, e.clock.now())
		reauthenticate(`{"method":"totp","code":"`+code+`"}`, http.StatusOK, "TOTP")
		expectStatus(t, e.do(http.MethodPost, regenerate, "", s.creds()), http.StatusOK, "regenerate after TOTP")
		reauthenticate(`{"method":"totp","code":"`+code+`"}`, http.StatusUnprocessableEntity, "reused TOTP code")

		// With the security key; the assertion answers one challenge only.
		stale("stale before WebAuthn")
		assertion, err := key.Assert(options().WebAuthn)
		if err != nil {
			t.Fatal(err)
		}
		reauthenticate(`{"method":"webauthn","credential":`+string(assertion)+`}`, http.StatusOK, "WebAuthn")
		expectStatus(t, e.do(http.MethodPost, regenerate, "", s.creds()), http.StatusOK, "regenerate after WebAuthn")
		reauthenticate(`{"method":"webauthn","credential":`+string(assertion)+`}`, http.StatusUnprocessableEntity, "replayed assertion")

		// The key, signing for another origin, can't stand in.
		key.Origin = "https://evil.example.com"
		assertion, err = key.Assert(options().WebAuthn)
		if err != nil {
			t.Fatal(err)
		}
		reauthenticate(`{"method":"webauthn","credential":`+string(assertion)+`}`, http.StatusUnprocessableEntity, "another origin")

		// Audit, newest first, with the user as actor.
		type summary struct{ action, result, method string }
		var got []summary
		for _, ev := range allEvents(t, db) {
			switch ev.Action {
			case "self.reauthenticate", "self.reauthenticate.start", "self.two_factor.recovery_codes.regenerate":
				if ev.ActorUserID != e.user.ID {
					t.Errorf("%s event actor = %q", ev.Action, ev.ActorUserID)
				}
				got = append(got, summary{ev.Action, string(ev.Result), ev.Details["method"]})
			}
		}
		want := []summary{
			{"self.reauthenticate", "failure", "webauthn"},
			{"self.reauthenticate.start", "success", ""},
			{"self.reauthenticate", "failure", "webauthn"},
			{"self.two_factor.recovery_codes.regenerate", "success", ""},
			{"self.reauthenticate", "success", "webauthn"},
			{"self.reauthenticate.start", "success", ""},
			{"self.two_factor.recovery_codes.regenerate", "denied", ""},
			{"self.reauthenticate", "failure", "totp"},
			{"self.two_factor.recovery_codes.regenerate", "success", ""},
			{"self.reauthenticate", "success", "totp"},
			{"self.reauthenticate", "failure", "password"},
			{"self.reauthenticate.start", "success", ""},
			{"self.two_factor.recovery_codes.regenerate", "denied", ""},
			{"self.two_factor.recovery_codes.regenerate", "success", ""},
		}
		if !slices.Equal(got, want) {
			t.Errorf("audit events:\n got %+v\nwant %+v", got, want)
		}
	})
}

// TestForcedEnrollmentThroughTheAPI completes forced enrollment (#153)
// through the enrollment endpoints, with each factor and each policy that
// requires one: the restricted session gets full access as soon as the
// factor is confirmed, the next login asks for it, and the user can't
// remove it while it's their last.
func TestForcedEnrollmentThroughTheAPI(t *testing.T) {
	for _, policy := range []struct {
		required auth.TwoFactorRequirement
		email    string
		role     store.Role
	}{
		{auth.RequireAdmins, "admin@example.com", store.RoleAdmin},
		{auth.RequireAll, "viewer@example.com", store.RoleViewer},
	} {
		for _, factor := range []string{"totp", "webauthn"} {
			t.Run(string(policy.required)+"/"+factor, func(t *testing.T) {
				storetest.Run(t, func(t *testing.T, db *store.DB) {
					e := newWebAuthnEnv(t, db, auth.TwoFactorPolicy{Required: policy.required})
					if policy.email != "admin@example.com" {
						if _, err := e.svc.CreateUser(t.Context(), auth.NewUser{Email: policy.email, Password: testPassword, Role: policy.role}); err != nil {
							t.Fatal(err)
						}
					}

					s := e.mustLoginAs(policy.email)
					w := e.do(http.MethodGet, "/api/v1/auth/me", "", s.creds())
					if me := decodeEnrollmentMe(t, w, "me before enrolling"); !me.EnrollmentRequired {
						t.Fatalf("me before enrolling: %s", w.Body)
					}
					expectEnrollmentRequired(t, e.do(http.MethodGet, "/api/v1/sites", "", s.creds()), "list sites before enrolling")

					var secret, removePath string
					var codes []string
					key := authtest.NewAuthenticator(webAuthnOrigin)
					switch factor {
					case "totp":
						secret, codes = e.enrollTOTPOverHTTP(s.creds())
						removePath = totpPath
					case "webauthn":
						w := e.registerOverHTTP(s.creds(), key, "Key")
						expectStatus(t, w, http.StatusCreated, "register")
						var reg struct {
							Credential    webAuthnCredentialBody `json:"credential"`
							RecoveryCodes []string               `json:"recovery_codes"`
						}
						if err := json.Unmarshal(w.Body.Bytes(), &reg); err != nil {
							t.Fatal(err)
						}
						codes = reg.RecoveryCodes
						removePath = "/api/v1/users/me/two-factor/webauthn/" + reg.Credential.ID
					}
					if len(codes) != auth.RecoveryCodeCount {
						t.Errorf("enrolling issued %d recovery codes", len(codes))
					}

					// The same session now has full access.
					w = e.do(http.MethodGet, "/api/v1/auth/me", "", s.creds())
					if me := decodeEnrollmentMe(t, w, "me after enrolling"); !me.TwoFactorRequired || me.EnrollmentRequired {
						t.Errorf("me after enrolling: %s", w.Body)
					}
					expectStatus(t, e.do(http.MethodGet, "/api/v1/sites", "", s.creds()), http.StatusOK, "list sites after enrolling")
					expectStatus(t, e.do(http.MethodDelete, removePath, "", s.creds()), http.StatusConflict, "removing the only factor")

					// The next login asks for the factor, and isn't restricted.
					expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/logout", "", s.creds()), http.StatusNoContent, "logout")
					_, w = e.login(policy.email, testPassword, nil)
					expectStatus(t, w, http.StatusUnauthorized, "password step after enrolling")
					var problem optionsBody
					if err := json.Unmarshal(w.Body.Bytes(), &problem); err != nil {
						t.Fatal(err)
					}
					if !slices.Equal(problem.Methods, []string{factor, "recovery_code"}) {
						t.Errorf("second-factor methods = %v", problem.Methods)
					}
					var pending string
					for _, c := range w.Result().Cookies() {
						if c.Name == api.LoginCookieName {
							pending = c.Value
						}
					}
					var proof string
					switch factor {
					case "totp":
						e.clock.add(30 * time.Second)
						proof = `{"method":"totp","code":"` + authtest.TOTPCode(secret, e.clock.now()) + `"}`
					case "webauthn":
						assertion, err := key.Assert(problem.WebAuthn)
						if err != nil {
							t.Fatal(err)
						}
						proof = `{"method":"webauthn","credential":` + string(assertion) + `}`
					}
					w = e.secondStep(pending, proof)
					expectStatus(t, w, http.StatusOK, "second step")
					s = sessionFrom(t, w)
					expectStatus(t, e.do(http.MethodGet, "/api/v1/sites", "", s.creds()), http.StatusOK, "list sites after logging in")
					if ev := lastLogin(t, db); ev.Result != store.AuditSuccess || ev.Details["second_factor"] != factor {
						t.Errorf("login event = %+v", ev)
					}
				})
			})
		}
	}
}
