package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth/authtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const totpPath = "/api/v1/users/me/two-factor/totp"

func TestTOTPOverHTTP(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newAuthEnv(t, api.Options{DB: db})
		s := e.mustLogin()

		// API tokens can't enroll, even with full scope.
		_, tok := e.createToken(s, `{"name":"ci","scope":"full","expires_at":null}`)
		expectStatus(t, e.do(http.MethodPost, totpPath, "", creds{bearer: tok}), http.StatusForbidden, "token caller")

		expectStatus(t, e.do(http.MethodPost, totpPath+"/confirm", `{"code":"123456"}`, s.creds()), http.StatusNotFound, "confirm before enrolling")
		expectStatus(t, e.do(http.MethodDelete, totpPath, "", s.creds()), http.StatusNotFound, "remove before enrolling")

		w := e.do(http.MethodPost, totpPath, "", s.creds())
		expectStatus(t, w, http.StatusOK, "start enrollment")
		var enrollment struct {
			Secret    string    `json:"secret"`
			URI       string    `json:"uri"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &enrollment); err != nil {
			t.Fatal(err)
		}
		if w.Header().Get("Cache-Control") != "no-store" || len(enrollment.Secret) != 32 ||
			!strings.HasPrefix(enrollment.URI, "otpauth://totp/PressFleet:admin@example.com%20%28dash.example.com%29?") ||
			!strings.Contains(enrollment.URI, "issuer=PressFleet") || enrollment.ExpiresAt.IsZero() {
			t.Fatalf("enrollment: %s, Cache-Control %q", w.Body, w.Header().Get("Cache-Control"))
		}
		code := func() string { return authtest.TOTPCode(enrollment.Secret, e.clock.now()) }

		// Unconfirmed, it isn't a factor.
		if me := decodeMe(t, e.do(http.MethodGet, "/api/v1/auth/me", "", s.creds())); me.TwoFactorEnabled {
			t.Error("unconfirmed TOTP reported as two-factor")
		}

		wrong := []byte(code())
		wrong[5] = '0' + (wrong[5]-'0'+1)%10
		w = e.do(http.MethodPost, totpPath+"/confirm", `{"code":"`+string(wrong)+`"}`, s.creds())
		expectStatus(t, w, http.StatusUnprocessableEntity, "wrong confirming code")
		if p := decodeProblem(t, w); len(p.Errors) != 1 || p.Errors[0].Field != "/code" {
			t.Errorf("wrong confirming code: %s", w.Body)
		}

		// Confirming returns the first factor's recovery codes.
		w = e.do(http.MethodPost, totpPath+"/confirm", `{"code":"`+code()+`"}`, s.creds())
		expectStatus(t, w, http.StatusOK, "confirm")
		var confirmed struct {
			RecoveryCodes []string `json:"recovery_codes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &confirmed); err != nil {
			t.Fatal(err)
		}
		if len(confirmed.RecoveryCodes) != 10 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("confirm: %s", w.Body)
		}
		expectStatus(t, e.do(http.MethodPost, totpPath, "", s.creds()), http.StatusConflict, "enrolling again")
		if me := decodeMe(t, e.do(http.MethodGet, "/api/v1/auth/me", "", s.creds())); !me.TwoFactorEnabled || me.RecoveryCodesRemaining != 10 {
			t.Errorf("me after confirming: %+v", me)
		}

		// Log out; the password alone no longer logs in.
		expectStatus(t, e.do(http.MethodPost, "/api/v1/auth/logout", "", s.creds()), http.StatusNoContent, "logout")
		lc := e.startTwoStepLogin()
		e.clock.add(30 * time.Second)
		c := code()
		w = e.secondStep(lc, `{"method":"totp","code":"`+c+`"}`)
		expectStatus(t, w, http.StatusOK, "login with TOTP")
		var sc string
		for _, ck := range w.Result().Cookies() {
			if ck.Name == api.SessionCookieName {
				sc = ck.Value
			}
		}
		me := decodeMe(t, w)
		s2 := session{cookie: &http.Cookie{Value: sc}, csrf: *me.CSRFToken}
		if ev := lastLogin(t, db); ev.Result != store.AuditSuccess || ev.ActorUserID != e.user.ID || ev.Details["second_factor"] != "totp" {
			t.Errorf("TOTP login event = %+v", ev)
		}

		// The same code can't log in again, and the refusal is audited.
		w = e.secondStep(e.startTwoStepLogin(), `{"method":"totp","code":"`+c+`"}`)
		expectStatus(t, w, http.StatusUnprocessableEntity, "reused code")
		if ev := lastLogin(t, db); ev.Result != store.AuditFailure || ev.ActorUserID != e.user.ID || ev.Details["second_factor"] != "totp" {
			t.Errorf("reused TOTP code event = %+v", ev)
		}

		// A second session, then removing the TOTP from the first signs the
		// second out and keeps the API token.
		e.clock.add(30 * time.Second)
		w = e.secondStep(e.startTwoStepLogin(), `{"method":"totp","code":"`+code()+`"}`)
		expectStatus(t, w, http.StatusOK, "second session")
		var other string
		for _, ck := range w.Result().Cookies() {
			if ck.Name == api.SessionCookieName {
				other = ck.Value
			}
		}
		expectStatus(t, e.do(http.MethodDelete, totpPath, "", creds{bearer: tok}), http.StatusForbidden, "token removes")
		expectStatus(t, e.do(http.MethodDelete, totpPath, "", s2.creds()), http.StatusNoContent, "remove")
		expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{cookie: other}), http.StatusUnauthorized, "other session")
		expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", s2.creds()), http.StatusOK, "current session")
		expectStatus(t, e.do(http.MethodGet, "/api/v1/auth/me", "", creds{bearer: tok}), http.StatusOK, "API token")
		e.mustLogin()

		// Audit: each step under its action, without secrets or codes.
		type summary struct{ action, result, stage string }
		var got []summary
		for _, ev := range allEvents(t, e.db) {
			if strings.HasPrefix(ev.Action, "self.two_factor.totp.") {
				got = append(got, summary{ev.Action, string(ev.Result), ev.Details["stage"]})
			}
			for k, v := range ev.Details {
				if strings.Contains(v, enrollment.Secret) || v == c {
					t.Errorf("audit event %s details %s hold a secret or code", ev.Action, k)
				}
			}
		}
		// Newest first.
		want := []summary{
			{"self.two_factor.totp.remove", "success", ""},
			{"self.two_factor.totp.remove", "denied", ""},
			{"self.two_factor.totp.enroll", "failure", "start"},
			{"self.two_factor.totp.enroll", "success", "confirm"},
			{"self.two_factor.totp.enroll", "failure", "confirm"},
			{"self.two_factor.totp.enroll", "success", "start"},
			{"self.two_factor.totp.remove", "failure", ""},
			{"self.two_factor.totp.enroll", "failure", "confirm"},
			{"self.two_factor.totp.enroll", "denied", ""},
		}
		if len(got) != len(want) {
			t.Fatalf("TOTP events = %+v, want %+v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("TOTP event %d = %+v, want %+v", i, got[i], want[i])
			}
		}
	})
}
