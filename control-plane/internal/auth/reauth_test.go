package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// challengingFactor is fakeFactor with a challenge, as WebAuthn (#152)
// will have, so reauthentication is shown to get it from the verifier.
type challengingFactor struct{ fakeFactor }

func (challengingFactor) Challenge(_ context.Context, user store.User) (json.RawMessage, error) {
	return json.RawMessage(`{"challenge":"for-` + user.ID + `"}`), nil
}

func newReauthService(t *testing.T, db *store.DB) (*auth.Service, *clock) {
	t.Helper()
	c := &clock{t: time.Now()}
	return auth.NewService(db, auth.Options{
		Logger: storetest.Logger(), Now: c.Now, PasswordParams: cheap,
		Factors:   []auth.FactorVerifier{challengingFactor{fakeFactor{db: db}}},
		TwoFactor: auth.TwoFactorPolicy{Reauthentication: true, ReauthenticationMaxAge: 10 * time.Minute},
	}), c
}

// sessionOf logs the user in (with the fake factor if they have one) and
// returns the session's principal.
func sessionOf(t *testing.T, svc *auth.Service, email string, twoFactor bool) auth.Principal {
	t.Helper()
	p, _ := sessionAndToken(t, svc, email, twoFactor)
	return p
}

// sessionAndToken is sessionOf, also returning the session token.
func sessionAndToken(t *testing.T, svc *auth.Service, email string, twoFactor bool) (auth.Principal, redact.Secret) {
	t.Helper()
	var res auth.LoginResult
	if twoFactor {
		var err error
		if res, err = secondStep(svc, startLogin(t, svc, email).Token, auth.FactorWebAuthn, "good"); err != nil {
			t.Fatal(err)
		}
	} else {
		res = login(t, svc, email)
	}
	p, err := svc.AuthenticateSession(context.Background(), res.Token)
	if err != nil {
		t.Fatal(err)
	}
	return p, res.Token
}

func reauth(svc *auth.Service, p auth.Principal, method auth.FactorMethod, secret string) (time.Time, error) {
	req := auth.ReauthenticationRequest{Proof: auth.FactorProof{Method: method}, ClientIP: "192.0.2.1"}
	if method == auth.ReauthenticatePassword {
		req.Password = redact.Secret(secret)
	} else {
		req.Proof.Code = redact.Secret(secret)
	}
	return svc.Reauthenticate(context.Background(), p, req)
}

func TestReauthenticate(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newReauthService(t, db)
		plain := mustCreate(t, svc, "plain@example.com", store.RoleAdmin)
		secured := mustCreate(t, svc, "secured@example.com", store.RoleAdmin)
		enroll(t, db, secured.ID)
		var codes []redact.Secret
		if err := db.InTx(ctx, func(q store.Querier) error {
			var err error
			codes, err = svc.IssueRecoveryCodes(ctx, q, secured.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		tests := []struct {
			name      string
			twoFactor bool
			method    auth.FactorMethod
			secret    string
			want      error
		}{
			{"without a factor, the password", false, auth.ReauthenticatePassword, pw, nil},
			{"without a factor, a wrong password", false, auth.ReauthenticatePassword, "wrong password", auth.ErrWrongPassword},
			{"without a factor, a factor", false, auth.FactorWebAuthn, "good", auth.ErrFactorUnavailable},
			{"without a factor, a recovery code", false, auth.FactorRecoveryCode, "AAAA-BBBB-CCCC-DDDD", auth.ErrFactorUnavailable},
			{"with a factor, the factor", true, auth.FactorWebAuthn, "good", nil},
			{"with a factor, a wrong factor", true, auth.FactorWebAuthn, "bad", auth.ErrInvalidSecondFactor},
			{"with a factor, a recovery code", true, auth.FactorRecoveryCode, codes[0].Reveal(), nil},
			{"with a factor, a used recovery code", true, auth.FactorRecoveryCode, codes[0].Reveal(), auth.ErrInvalidSecondFactor},
			{"with a factor, the password", true, auth.ReauthenticatePassword, pw, auth.ErrFactorUnavailable},
			{"with a factor, a method without a verifier", true, auth.FactorTOTP, "123456", auth.ErrFactorUnavailable},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				email := plain.Email
				if tt.twoFactor {
					email = secured.Email
				}
				p, tok := sessionAndToken(t, svc, email, tt.twoFactor)
				clk.Add(11 * time.Minute)
				verified, err := reauth(svc, p, tt.method, tt.secret)
				if !errors.Is(err, tt.want) {
					t.Fatalf("Reauthenticate = %v, want %v", err, tt.want)
				}
				sess, aerr := svc.AuthenticateSession(ctx, tok)
				if aerr != nil {
					t.Fatal(aerr)
				}
				switch {
				case tt.want == nil && (!verified.Equal(clk.Now().UTC().Truncate(time.Microsecond)) || !sess.VerifiedAt.Equal(verified)):
					t.Errorf("verified at %v, session %v; want now, %v", verified, sess.VerifiedAt, clk.Now())
				case tt.want != nil && !sess.VerifiedAt.Equal(p.VerifiedAt):
					t.Errorf("a failed check moved verified_at from %v to %v", p.VerifiedAt, sess.VerifiedAt)
				}
				var sfe *auth.SecondFactorError
				if errors.Is(tt.want, auth.ErrInvalidSecondFactor) && (!errors.As(err, &sfe) || sfe.UserID != secured.ID) {
					t.Errorf("wrong factor: %#v, want a *SecondFactorError naming the user", err)
				}
			})
		}

		// Only sessions reauthenticate.
		token := auth.Principal{UserID: plain.ID, Email: plain.Email, Role: plain.Role, Method: auth.MethodToken, TokenID: "t", Scope: store.ScopeFull}
		if _, err := reauth(svc, token, auth.ReauthenticatePassword, pw); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("token caller: %v, want ErrUnauthenticated", err)
		}
		if _, err := svc.StartReauthentication(ctx, token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("token caller starting: %v, want ErrUnauthenticated", err)
		}
		// A session that has ended can't be refreshed.
		p := sessionOf(t, svc, plain.Email, false)
		if err := svc.Logout(ctx, p.SessionID); err != nil {
			t.Fatal(err)
		}
		if _, err := reauth(svc, p, auth.ReauthenticatePassword, pw); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("ended session: %v, want ErrUnauthenticated", err)
		}
	})
}

func TestStartReauthentication(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, _ := newReauthService(t, db)
		plain := mustCreate(t, svc, "plain@example.com", store.RoleViewer)
		secured := mustCreate(t, svc, "secured@example.com", store.RoleViewer)
		enroll(t, db, secured.ID)
		if err := db.InTx(ctx, func(q store.Querier) error {
			_, err := svc.IssueRecoveryCodes(ctx, q, secured.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}

		opts, err := svc.StartReauthentication(ctx, sessionOf(t, svc, plain.Email, false))
		if err != nil || !slices.Equal(opts.Methods, []auth.FactorMethod{auth.ReauthenticatePassword}) || len(opts.Challenges) != 0 {
			t.Errorf("without a factor: %+v, %v; want only the password", opts, err)
		}
		opts, err = svc.StartReauthentication(ctx, sessionOf(t, svc, secured.Email, true))
		if err != nil || !slices.Equal(opts.Methods, []auth.FactorMethod{auth.FactorWebAuthn, auth.FactorRecoveryCode}) {
			t.Errorf("with a factor: %+v, %v; want webauthn and recovery codes", opts, err)
		}
		if got := string(opts.Challenges[auth.FactorWebAuthn]); got != `{"challenge":"for-`+secured.ID+`"}` {
			t.Errorf("webauthn challenge = %s, want the verifier's", got)
		}
	})
}

// TestReauthenticateThrottled shows that wrong codes and passwords count
// against the login throttle, in both directions.
func TestReauthenticateThrottled(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newReauthService(t, db)
		plain := mustCreate(t, svc, "plain@example.com", store.RoleViewer)
		secured := mustCreate(t, svc, "secured@example.com", store.RoleViewer)
		enroll(t, db, secured.ID)

		// Two failed logins and three wrong passwords make five failures for
		// the account: even the right password is then refused, here and at
		// login.
		p := sessionOf(t, svc, plain.Email, false)
		for range 2 {
			if _, err := svc.Login(ctx, auth.LoginRequest{Email: plain.Email, Password: "wrong password", ClientIP: "192.0.2.7"}); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatal(err)
			}
		}
		for range 3 {
			if _, err := reauth(svc, p, auth.ReauthenticatePassword, "wrong password"); !errors.Is(err, auth.ErrWrongPassword) {
				t.Fatal(err)
			}
		}
		var te *auth.ThrottledError
		if _, err := reauth(svc, p, auth.ReauthenticatePassword, pw); !errors.As(err, &te) {
			t.Errorf("right password after five failures: %v, want throttled", err)
		}
		if _, err := svc.Login(ctx, auth.LoginRequest{Email: plain.Email, Password: pw, ClientIP: "192.0.2.8"}); !errors.Is(err, auth.ErrThrottled) {
			t.Errorf("login after failed reauthentications: %v, want throttled", err)
		}

		// Wrong factors count too, and a success resets the account.
		clk.Add(15 * time.Minute)
		s := sessionOf(t, svc, secured.Email, true)
		for range 4 {
			if _, err := reauth(svc, s, auth.FactorWebAuthn, "bad"); !errors.Is(err, auth.ErrInvalidSecondFactor) {
				t.Fatal(err)
			}
		}
		if _, err := reauth(svc, s, auth.FactorWebAuthn, "good"); err != nil {
			t.Fatalf("fifth try, right factor: %v", err)
		}
		for range 4 {
			_, _ = reauth(svc, s, auth.FactorWebAuthn, "bad")
		}
		if _, err := reauth(svc, s, auth.FactorWebAuthn, "good"); err != nil {
			t.Errorf("count not reset by a successful reauthentication: %v", err)
		}
	})
}
