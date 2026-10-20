package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth/authtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

const twoFactorStatusPath = "/api/v1/users/me/two-factor"

type twoFactorStatusBody struct {
	Enabled bool    `json:"enabled"`
	RPID    *string `json:"rp_id"`
	TOTP    struct {
		Confirmed        bool       `json:"confirmed"`
		ConfirmedAt      *time.Time `json:"confirmed_at"`
		PendingExpiresAt *time.Time `json:"pending_expires_at"`
	} `json:"totp"`
	WebAuthn struct {
		Credentials []struct {
			Name string `json:"name"`
			RPID string `json:"rp_id"`
		} `json:"credentials"`
		Usable int `json:"usable"`
	} `json:"webauthn"`
	RecoveryCodesRemaining int `json:"recovery_codes_remaining"`
}

// getTwoFactorStatus reads the status with c, which must be allowed.
func (e *authEnv) getTwoFactorStatus(c creds, what string) twoFactorStatusBody {
	e.t.Helper()
	w := e.do(http.MethodGet, twoFactorStatusPath, "", c)
	expectStatus(e.t, w, http.StatusOK, what)
	expectNoSecrets(e.t, w, what)
	var st twoFactorStatusBody
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		e.t.Fatalf("%s: %v", what, err)
	}
	return st
}

// expectNoSecrets checks that the status names no secret, key, or
// challenge, whatever the user has.
func expectNoSecrets(t *testing.T, w *httptest.ResponseRecorder, what string) {
	t.Helper()
	for _, key := range []string{`"secret"`, `"uri"`, `"public_key"`, `"credential_id"`, `"challenge"`, `"codes"`} {
		if strings.Contains(w.Body.String(), key) {
			t.Errorf("%s: response has %s: %s", what, key, w.Body)
		}
	}
}

// addCredential stores a WebAuthn credential for userID bound to rpID.
func (e *authEnv) addCredential(userID, name, rpID string) {
	e.t.Helper()
	c := store.WebAuthnCredential{UserID: userID, CredentialID: []byte(name), PublicKey: []byte("key"), Algorithm: -7, RPID: rpID, Name: name}
	if err := store.NewWebAuthnCredentials(e.db).Create(context.Background(), &c); err != nil {
		e.t.Fatal(err)
	}
}

func TestTwoFactorStatusOverHTTP(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		ctx := context.Background()
		e := newAuthEnv(t, api.Options{DB: db})
		s := e.mustLogin()

		// No factors.
		st := e.getTwoFactorStatus(s.creds(), "no factors")
		if st.Enabled || st.RPID == nil || *st.RPID != auth.DefaultRelyingPartyID || st.TOTP.Confirmed ||
			st.TOTP.ConfirmedAt != nil || st.TOTP.PendingExpiresAt != nil || st.WebAuthn.Credentials == nil ||
			len(st.WebAuthn.Credentials) != 0 || st.WebAuthn.Usable != 0 || st.RecoveryCodesRemaining != 0 {
			t.Errorf("no factors: %+v", st)
		}

		// An unconfirmed authenticator app is pending until it expires, and
		// isn't a factor.
		w := e.do(http.MethodPost, totpPath, "", s.creds())
		expectStatus(t, w, http.StatusOK, "start TOTP")
		var enrollment struct {
			Secret    string    `json:"secret"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &enrollment); err != nil {
			t.Fatal(err)
		}
		st = e.getTwoFactorStatus(s.creds(), "TOTP pending")
		if st.Enabled || st.TOTP.Confirmed || st.TOTP.ConfirmedAt != nil || st.TOTP.PendingExpiresAt == nil ||
			!st.TOTP.PendingExpiresAt.Equal(enrollment.ExpiresAt) {
			t.Errorf("TOTP pending: %+v, want expiry %v", st.TOTP, enrollment.ExpiresAt)
		}
		e.clock.add(auth.TOTPEnrollmentTTL)
		if st = e.getTwoFactorStatus(s.creds(), "TOTP expired"); st.TOTP.PendingExpiresAt != nil || st.TOTP.Confirmed {
			t.Errorf("TOTP expired: %+v", st.TOTP)
		}

		// Confirmed, with the first recovery codes.
		w = e.do(http.MethodPost, totpPath, "", s.creds())
		expectStatus(t, w, http.StatusOK, "start TOTP again")
		if err := json.Unmarshal(w.Body.Bytes(), &enrollment); err != nil {
			t.Fatal(err)
		}
		code := authtest.TOTPCode(enrollment.Secret, e.clock.now())
		expectStatus(t, e.do(http.MethodPost, totpPath+"/confirm", `{"code":"`+code+`"}`, s.creds()), http.StatusOK, "confirm TOTP")
		st = e.getTwoFactorStatus(s.creds(), "TOTP confirmed")
		if !st.Enabled || !st.TOTP.Confirmed || st.TOTP.ConfirmedAt == nil || st.TOTP.PendingExpiresAt != nil || st.RecoveryCodesRemaining != 10 {
			t.Errorf("TOTP confirmed: %+v", st)
		}

		// Keys for the current relying party ID count; those from an old
		// dashboard domain are listed but not usable.
		e.addCredential(e.user.ID, "current", auth.DefaultRelyingPartyID)
		e.addCredential(e.user.ID, "stale", "old.example.com")
		st = e.getTwoFactorStatus(s.creds(), "keys")
		if len(st.WebAuthn.Credentials) != 2 || st.WebAuthn.Usable != 1 ||
			st.WebAuthn.Credentials[0].Name != "current" || st.WebAuthn.Credentials[1].RPID != "old.example.com" {
			t.Errorf("keys: %+v", st.WebAuthn)
		}

		// A used recovery code is gone from the count.
		codes, err := e.svc.RegenerateRecoveryCodes(ctx, e.user.ID)
		if err != nil {
			t.Fatal(err)
		}
		w = e.secondStep(e.startTwoStepLogin(), `{"method":"recovery_code","code":"`+codes[0].Reveal()+`"}`)
		expectStatus(t, w, http.StatusOK, "log in with a recovery code")
		if st = e.getTwoFactorStatus(s.creds(), "a code used"); st.RecoveryCodesRemaining != 9 {
			t.Errorf("a code used: %d remaining, want 9", st.RecoveryCodesRemaining)
		}

		// Tokens may read it, read-only ones too.
		for _, scope := range []store.TokenScope{store.ScopeFull, store.ScopeReadOnly} {
			_, tok, err := e.svc.CreateAPIToken(ctx, e.user.ID, auth.NewAPIToken{Name: string(scope), Scope: scope})
			if err != nil {
				t.Fatal(err)
			}
			if got := e.getTwoFactorStatus(creds{bearer: tok.Reveal()}, string(scope)+" token"); got.WebAuthn.Usable != 1 || !got.TOTP.Confirmed {
				t.Errorf("%s token: %+v", scope, got)
			}
		}

		// Another user sees only their own factors.
		other := e.createUser(s, "operator@example.com", store.RoleOperator)
		e.addCredential(other, "theirs", "old.example.com")
		st = e.getTwoFactorStatus(e.mustLoginAs("operator@example.com").creds(), "other user")
		if st.Enabled || st.TOTP.Confirmed || len(st.WebAuthn.Credentials) != 1 || st.WebAuthn.Credentials[0].Name != "theirs" ||
			st.WebAuthn.Usable != 0 || st.RecoveryCodesRemaining != 0 {
			t.Errorf("other user: %+v", st)
		}

		expectStatus(t, e.do(http.MethodGet, twoFactorStatusPath, "", creds{}), http.StatusUnauthorized, "anonymous")
	})
}

// TestTwoFactorStatusRestricted: a session that must enroll reads the
// status, so the setup page can show it.
func TestTwoFactorStatusRestricted(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newPolicyAuthEnv(t, db, auth.RequireAdmins)
		s := e.mustLogin()
		if me := decodeEnrollmentMe(t, e.do(http.MethodGet, "/api/v1/auth/me", "", s.creds()), "me"); !me.EnrollmentRequired {
			t.Fatal("the session isn't restricted")
		}
		expectEnrollmentRequired(t, e.do(http.MethodGet, "/api/v1/sites", "", s.creds()), "sites")

		// A key from an old domain doesn't lift the restriction.
		e.addCredential(e.user.ID, "stale", "old.example.com")
		st := e.getTwoFactorStatus(s.creds(), "restricted")
		if st.Enabled || len(st.WebAuthn.Credentials) != 1 || st.WebAuthn.Usable != 0 {
			t.Errorf("restricted: %+v", st)
		}
	})
}
