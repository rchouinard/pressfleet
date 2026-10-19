package auth_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth/authtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// newTOTPService returns a service with the TOTP verifier, under policy,
// and its clock, set 10 seconds into a TOTP step.
func newTOTPService(t *testing.T, db *store.DB, policy auth.TwoFactorPolicy) (*auth.Service, *clock) {
	t.Helper()
	c := &clock{t: time.Now().Truncate(30 * time.Second).Add(10 * time.Second)}
	return auth.NewService(db, auth.Options{
		Logger: storetest.Logger(), Now: c.Now, PasswordParams: cheap, TwoFactor: policy,
		Factors: []auth.FactorVerifier{auth.NewTOTPVerifier(secretstest.Keyring(t, 1), "dash.example.com"), fakeFactor{db: db}},
	}), c
}

// enrollTOTP enrolls and confirms a TOTP for the user and returns its
// secret. The confirming code's step is used up, so the clock moves on a
// step.
func enrollTOTP(t *testing.T, svc *auth.Service, clk *clock, userID string) redact.Secret {
	t.Helper()
	ctx := context.Background()
	e, err := svc.StartTOTPEnrollment(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmTOTP(ctx, userID, totpCode(e.Secret, clk)); err != nil {
		t.Fatal(err)
	}
	clk.Add(30 * time.Second)
	return e.Secret
}

func totpCode(secret redact.Secret, clk *clock) redact.Secret {
	return redact.Secret(authtest.TOTPCode(secret.Reveal(), clk.Now()))
}

// wrongCode returns a code that isn't code, nor any other step's code
// near it, as far as tests go: it differs in the last digit.
func wrongCode(code redact.Secret) redact.Secret {
	c := []byte(code.Reveal())
	c[len(c)-1] = '0' + (c[len(c)-1]-'0'+1)%10
	return redact.Secret(c)
}

func TestTOTPEnrollmentAndLogin(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newTOTPService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)

		e, err := svc.StartTOTPEnrollment(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(e.Secret.Reveal()) != 32 || !strings.HasPrefix(e.URI.Reveal(), "otpauth://totp/PressFleet:a@example.com%20%28dash.example.com%29?") ||
			!strings.Contains(e.URI.Reveal(), "secret="+e.Secret.Reveal()) || !e.ExpiresAt.Equal(clk.Now().UTC().Truncate(time.Microsecond).Add(15*time.Minute)) {
			t.Errorf("enrollment: secret %d chars, URI %s, expires %v", len(e.Secret.Reveal()), e.URI.Reveal(), e.ExpiresAt)
		}
		// The secret is sealed, never stored in the clear.
		var (
			kind       string
			ciphertext []byte
		)
		if err := db.QueryRowContext(ctx, "SELECT s.kind, s.ciphertext FROM secrets s JOIN user_totp t ON t.secret_id = s.id WHERE t.user_id = ?", u.ID).Scan(&kind, &ciphertext); err != nil {
			t.Fatal(err)
		}
		if kind != "totp-secret" || bytes.Contains(ciphertext, []byte(e.Secret.Reveal())) {
			t.Errorf("stored secret: kind %q, plaintext present %v", kind, bytes.Contains(ciphertext, []byte(e.Secret.Reveal())))
		}

		// Unconfirmed, it isn't a factor: login is still one step.
		if on, _ := svc.TwoFactorEnabled(ctx, u.ID); on {
			t.Error("unconfirmed TOTP counts as a factor")
		}
		login(t, svc, "a@example.com")

		// A wrong code doesn't confirm it.
		code := totpCode(e.Secret, clk)
		if _, err := svc.ConfirmTOTP(ctx, u.ID, wrongCode(code)); !errors.Is(err, auth.ErrInvalidSecondFactor) {
			t.Fatalf("wrong code: %v", err)
		}
		codes, err := svc.ConfirmTOTP(ctx, u.ID, code)
		if err != nil || len(codes) != auth.RecoveryCodeCount {
			t.Fatalf("ConfirmTOTP = %d codes, %v; want the first factor's recovery codes", len(codes), err)
		}
		if _, err := svc.ConfirmTOTP(ctx, u.ID, code); !errors.Is(err, auth.ErrTOTPEnrolled) {
			t.Errorf("second confirm: %v", err)
		}
		if _, err := svc.StartTOTPEnrollment(ctx, u.ID); !errors.Is(err, auth.ErrTOTPEnrolled) {
			t.Errorf("enrolling again: %v", err)
		}

		// Now the password alone isn't enough, and the confirming code
		// can't log in: its step is used.
		pending := startLogin(t, svc, "a@example.com")
		if len(pending.Methods) != 2 || pending.Methods[0] != auth.FactorTOTP || pending.Methods[1] != auth.FactorRecoveryCode {
			t.Errorf("methods: %v", pending.Methods)
		}
		if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, code.Reveal()); !errors.Is(err, auth.ErrInvalidSecondFactor) {
			t.Errorf("confirming code at login: %v", err)
		}

		// The next step's code logs in once.
		clk.Add(30 * time.Second)
		code = totpCode(e.Secret, clk)
		res, err := secondStep(svc, pending.Token, auth.FactorTOTP, code.Reveal())
		if err != nil || res.SecondFactor != auth.FactorTOTP {
			t.Fatalf("login with TOTP = %+v, %v", res, err)
		}
		pending = startLogin(t, svc, "a@example.com")
		if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, code.Reveal()); !errors.Is(err, auth.ErrInvalidSecondFactor) {
			t.Errorf("replayed code: %v", err)
		}
	})
}

func TestTOTPSkewAndReplay(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newTOTPService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		secret := enrollTOTP(t, svc, clk, u.ID)
		// Move past the step after the confirming one, so the step behind
		// hasn't been used.
		clk.Add(30 * time.Second)
		at := func(d time.Duration) string {
			return authtest.TOTPCode(secret.Reveal(), clk.Now().Add(d))
		}

		pending := startLogin(t, svc, "a@example.com")
		// Two steps off either way is too far.
		for _, d := range []time.Duration{-60 * time.Second, 60 * time.Second} {
			if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, at(d)); !errors.Is(err, auth.ErrInvalidSecondFactor) {
				t.Errorf("code %v away: %v", d, err)
			}
		}
		// A phone a step behind works.
		if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, at(-30*time.Second)); err != nil {
			t.Errorf("code a step behind: %v", err)
		}
		// A phone a step ahead works, and then the current step's code is
		// refused: it is earlier than the one used.
		pending = startLogin(t, svc, "a@example.com")
		ahead := at(30 * time.Second)
		if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, ahead); err != nil {
			t.Errorf("code a step ahead: %v", err)
		}
		pending = startLogin(t, svc, "a@example.com")
		if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, at(0)); !errors.Is(err, auth.ErrInvalidSecondFactor) {
			t.Errorf("code of an earlier step than the last used: %v", err)
		}
		// Even when its step comes round, the used code stays used.
		clk.Add(30 * time.Second)
		if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, ahead); !errors.Is(err, auth.ErrInvalidSecondFactor) {
			t.Errorf("replayed code in its own step: %v", err)
		}
		clk.Add(30 * time.Second)
		if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, at(0)); err != nil {
			t.Errorf("next code: %v", err)
		}
	})
}

func TestTOTPEnrollmentExpires(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newTOTPService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		countSecrets := func() int {
			t.Helper()
			var n int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM secrets WHERE kind = 'totp-secret'").Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}

		e, err := svc.StartTOTPEnrollment(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Starting again replaces the unconfirmed secret.
		if e, err = svc.StartTOTPEnrollment(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
		if n := countSecrets(); n != 1 {
			t.Errorf("%d TOTP secrets after starting twice, want 1", n)
		}
		clk.Add(auth.TOTPEnrollmentTTL)
		if _, err := svc.ConfirmTOTP(ctx, u.ID, totpCode(e.Secret, clk)); !errors.Is(err, auth.ErrNoTOTPEnrollment) {
			t.Errorf("confirming an expired enrollment: %v", err)
		}
		if on, _ := svc.TwoFactorEnabled(ctx, u.ID); on {
			t.Error("expired enrollment counts as a factor")
		}
		// Another user's enrollment cleans up the expired one.
		other := mustCreate(t, svc, "b@example.com", store.RoleViewer)
		clk.Add(time.Second)
		if _, err := svc.StartTOTPEnrollment(ctx, other.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.NewUserTOTPs(db).Get(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expired enrollment kept: %v", err)
		}
		if n := countSecrets(); n != 1 {
			t.Errorf("%d TOTP secrets, want only the new enrollment's", n)
		}
		if _, err := svc.ConfirmTOTP(ctx, u.ID, "123456"); !errors.Is(err, auth.ErrNoTOTPEnrollment) {
			t.Errorf("confirming without an enrollment: %v", err)
		}
	})
}

func TestTOTPNotFirstFactorIssuesNoCodes(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newTOTPService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		enroll(t, db, u.ID)
		e, err := svc.StartTOTPEnrollment(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		codes, err := svc.ConfirmTOTP(ctx, u.ID, totpCode(e.Secret, clk))
		if err != nil || codes != nil {
			t.Errorf("ConfirmTOTP = %v, %v; want no new recovery codes", codes, err)
		}
	})
}

func TestTOTPWrongCodeThrottled(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newTOTPService(t, db, auth.TwoFactorPolicy{})
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		secret := enrollTOTP(t, svc, clk, u.ID)
		right := totpCode(secret, clk)
		wrong := wrongCode(right).Reveal()
		pending := startLogin(t, svc, "a@example.com")
		for range 5 {
			if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, wrong); !errors.Is(err, auth.ErrInvalidSecondFactor) {
				t.Fatal(err)
			}
		}
		var te *auth.ThrottledError
		if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, right.Reveal()); !errors.As(err, &te) {
			t.Errorf("right code after five wrong ones: %v, want throttled", err)
		}
	})
}

func TestRemoveTOTP(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newTOTPService(t, db, auth.TwoFactorPolicy{Required: auth.RequireAdmins})
		viewer := mustCreate(t, svc, "v@example.com", store.RoleViewer)
		admin := mustCreate(t, svc, "a@example.com", store.RoleAdmin)

		if err := svc.RemoveTOTP(ctx, auth.Principal{UserID: viewer.ID}); !errors.Is(err, auth.ErrNoTOTP) {
			t.Errorf("removing no TOTP: %v", err)
		}

		// Removing signs out the other sessions and keeps API tokens.
		secret := enrollTOTP(t, svc, clk, viewer.ID)
		sessionWith := func(secret redact.Secret, email string) auth.LoginResult {
			t.Helper()
			clk.Add(30 * time.Second)
			res, err := secondStep(svc, startLogin(t, svc, email).Token, auth.FactorTOTP, totpCode(secret, clk).Reveal())
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
		keep, other := sessionWith(secret, "v@example.com"), sessionWith(secret, "v@example.com")
		_, tok, err := svc.CreateAPIToken(ctx, viewer.ID, auth.NewAPIToken{Name: "ci", Scope: store.ScopeFull})
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.RemoveTOTP(ctx, auth.Principal{UserID: viewer.ID, SessionID: keep.Session.ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AuthenticateSession(ctx, keep.Token); err != nil {
			t.Errorf("current session: %v", err)
		}
		if _, err := svc.AuthenticateSession(ctx, other.Token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("other session: %v", err)
		}
		if _, err := svc.AuthenticateToken(ctx, tok); err != nil {
			t.Errorf("API token: %v", err)
		}
		if on, _ := svc.TwoFactorEnabled(ctx, viewer.ID); on {
			t.Error("still two-factor after removing the only factor")
		}
		login(t, svc, "v@example.com")

		// An admin who must have a factor can't remove their last one.
		secret = enrollTOTP(t, svc, clk, admin.ID)
		sess := sessionWith(secret, "a@example.com")
		if err := svc.RemoveTOTP(ctx, auth.Principal{UserID: admin.ID, SessionID: sess.Session.ID}); !errors.Is(err, auth.ErrLastSecondFactor) {
			t.Errorf("removing the last required factor: %v", err)
		}
		if on, _ := svc.TwoFactorEnabled(ctx, admin.ID); !on {
			t.Error("refused removal removed the TOTP")
		}
		// With another factor, they can.
		enroll(t, db, admin.ID)
		if err := svc.RemoveTOTP(ctx, auth.Principal{UserID: admin.ID, SessionID: sess.Session.ID}); err != nil {
			t.Errorf("removing with another factor left: %v", err)
		}

		// Cancelling an unconfirmed enrollment is allowed even for an
		// admin without a factor, and signs no one out.
		if _, err := store.NewWebAuthnCredentials(db).DeleteAllForUser(ctx, admin.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.StartTOTPEnrollment(ctx, admin.ID); err != nil {
			t.Fatal(err)
		}
		if err := svc.RemoveTOTP(ctx, auth.Principal{UserID: admin.ID}); err != nil {
			t.Errorf("cancelling an enrollment: %v", err)
		}
		if _, err := svc.AuthenticateSession(ctx, sess.Token); err != nil {
			t.Errorf("session after cancelling an enrollment: %v", err)
		}
	})
}

func TestTOTPWithoutVerifier(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, _ := newService(t, db)
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		if _, err := svc.StartTOTPEnrollment(context.Background(), u.ID); !errors.Is(err, auth.ErrFactorUnavailable) {
			t.Errorf("StartTOTPEnrollment = %v", err)
		}
	})
}
