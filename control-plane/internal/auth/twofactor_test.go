package auth_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// fakeFactor is a FactorVerifier for WebAuthn that accepts the code "good",
// standing in for the real verifiers of #151 and #152.
type fakeFactor struct{ db *store.DB }

func (f fakeFactor) Method() auth.FactorMethod { return auth.FactorWebAuthn }

func (f fakeFactor) Enrolled(ctx context.Context, userID string) (bool, error) {
	creds, err := store.NewWebAuthnCredentials(f.db).ListForUser(ctx, userID)
	return len(creds) > 0, err
}

func (f fakeFactor) Verify(_ context.Context, _ store.User, proof auth.FactorProof) error {
	if proof.Code.Reveal() != "good" {
		return auth.ErrInvalidSecondFactor
	}
	return nil
}

func newTwoFactorService(t *testing.T, db *store.DB) (*auth.Service, *clock) {
	t.Helper()
	c := &clock{t: time.Now()}
	return auth.NewService(db, auth.Options{
		Logger: storetest.Logger(), Now: c.Now, PasswordParams: cheap,
		Factors: []auth.FactorVerifier{fakeFactor{db: db}},
	}), c
}

// enroll gives the user a second factor: a WebAuthn credential, which the
// store counts and fakeFactor accepts.
func enroll(t *testing.T, db *store.DB, userID string) {
	t.Helper()
	c := store.WebAuthnCredential{
		UserID: userID, CredentialID: []byte("cred-" + userID), PublicKey: []byte("key"),
		Algorithm: -7, RPID: "localhost", Name: "key",
	}
	if err := store.NewWebAuthnCredentials(db).Create(context.Background(), &c); err != nil {
		t.Fatal(err)
	}
}

// startLogin enters the password of a user with a second factor and
// returns the pending login.
func startLogin(t *testing.T, svc *auth.Service, email string) *auth.SecondFactorRequiredError {
	t.Helper()
	_, err := svc.Login(context.Background(), auth.LoginRequest{Email: email, Password: pw, ClientIP: "192.0.2.1"})
	var pending *auth.SecondFactorRequiredError
	if !errors.As(err, &pending) || !errors.Is(err, auth.ErrSecondFactorRequired) {
		t.Fatalf("Login = %v, want a pending login", err)
	}
	return pending
}

func secondStep(svc *auth.Service, token redact.Secret, method auth.FactorMethod, code string) (auth.LoginResult, error) {
	return svc.LoginSecondFactor(context.Background(), auth.SecondFactorRequest{
		PendingToken: token, Proof: auth.FactorProof{Method: method, Code: redact.Secret(code)}, ClientIP: "192.0.2.1",
	})
}

func countSessions(t *testing.T, db *store.DB, userID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM sessions WHERE user_id = ?", userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLoginWithoutSecondFactorIsOneStep(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, _ := newTwoFactorService(t, db)
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		// Recovery codes left over without a factor don't make it two steps.
		if err := db.InTx(ctx, func(q store.Querier) error {
			_, err := svc.IssueRecoveryCodes(ctx, q, u.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		res := login(t, svc, "a@example.com")
		if res.SecondFactor != "" || !res.Session.VerifiedAt.Equal(res.Session.CreatedAt) {
			t.Errorf("login result: %+v", res)
		}
		p, err := svc.AuthenticateSession(ctx, res.Token)
		if err != nil || !p.VerifiedAt.Equal(res.Session.VerifiedAt) {
			t.Errorf("AuthenticateSession = %+v, %v; want VerifiedAt %v", p, err, res.Session.VerifiedAt)
		}
	})
}

func TestTwoStepLogin(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newTwoFactorService(t, db)
		u := mustCreate(t, svc, "a@example.com", store.RoleAdmin)
		enroll(t, db, u.ID)

		// The password alone starts no session.
		pending := startLogin(t, svc, "A@example.com")
		if pending.UserID != u.ID || len(pending.Methods) != 1 || pending.Methods[0] != auth.FactorWebAuthn ||
			len(pending.Token.Reveal()) < 43 || !pending.ExpiresAt.Equal(clk.Now().UTC().Truncate(time.Microsecond).Add(5*time.Minute)) {
			t.Errorf("pending login: %+v", pending)
		}
		if n := countSessions(t, db, u.ID); n != 0 {
			t.Fatalf("%d sessions after the password step", n)
		}
		if got, _ := svc.GetUser(ctx, u.ID); got.LastLoginAt != nil {
			t.Error("password step recorded a login")
		}

		// A wrong factor fails, naming the user, and leaves the login
		// pending.
		_, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "bad")
		var failed *auth.SecondFactorError
		if !errors.As(err, &failed) || failed.UserID != u.ID || failed.Method != auth.FactorWebAuthn || !errors.Is(err, auth.ErrInvalidSecondFactor) {
			t.Fatalf("wrong factor: %v", err)
		}
		// A method the user hasn't enrolled is unavailable, not a failure.
		if _, err := secondStep(svc, pending.Token, auth.FactorTOTP, "123456"); !errors.Is(err, auth.ErrFactorUnavailable) {
			t.Errorf("unenrolled method: %v, want ErrFactorUnavailable", err)
		}
		if _, err := secondStep(svc, pending.Token, auth.FactorRecoveryCode, "AAAA-AAAA-AAAA-AAAA"); !errors.Is(err, auth.ErrFactorUnavailable) {
			t.Errorf("recovery code without codes: %v, want ErrFactorUnavailable", err)
		}

		res, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "good")
		if err != nil {
			t.Fatal(err)
		}
		if res.User.ID != u.ID || res.SecondFactor != auth.FactorWebAuthn || res.User.LastLoginAt == nil ||
			!res.Session.VerifiedAt.Equal(res.Session.CreatedAt) || res.CSRFToken != auth.CSRFToken(res.Token) {
			t.Errorf("login result: %+v", res)
		}
		if p, err := svc.AuthenticateSession(ctx, res.Token); err != nil || p.UserID != u.ID || p.SessionID != res.Session.ID {
			t.Errorf("AuthenticateSession = %+v, %v", p, err)
		}

		// The pending login works once.
		if _, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "good"); !errors.Is(err, auth.ErrLoginExpired) {
			t.Errorf("reused pending login: %v, want ErrLoginExpired", err)
		}
		if _, err := secondStep(svc, "made-up", auth.FactorWebAuthn, "good"); !errors.Is(err, auth.ErrLoginExpired) {
			t.Errorf("unknown pending login: %v, want ErrLoginExpired", err)
		}
		if n := countSessions(t, db, u.ID); n != 1 {
			t.Errorf("%d sessions, want 1", n)
		}
	})
}

func TestPendingLoginExpires(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newTwoFactorService(t, db)
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		enroll(t, db, u.ID)

		pending := startLogin(t, svc, "a@example.com")
		clk.Add(5*time.Minute - time.Second)
		if _, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "bad"); !errors.Is(err, auth.ErrInvalidSecondFactor) {
			t.Fatalf("before expiry: %v", err)
		}
		clk.Add(time.Second)
		if _, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "good"); !errors.Is(err, auth.ErrLoginExpired) {
			t.Errorf("after 5 minutes: %v, want ErrLoginExpired", err)
		}

		// A user disabled between the steps can't finish.
		pending = startLogin(t, svc, "a@example.com")
		disabled := true
		if _, err := svc.UpdateUser(context.Background(), u.ID, auth.UserChanges{Disabled: &disabled}); err != nil {
			t.Fatal(err)
		}
		if _, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "good"); !errors.Is(err, auth.ErrLoginExpired) {
			t.Errorf("disabled user: %v, want ErrLoginExpired", err)
		}
	})
}

func TestSecondFactorThrottled(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newTwoFactorService(t, db)
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		enroll(t, db, u.ID)

		// Three wrong codes, then the password again: a right password
		// doesn't reset the count of guesses at the second factor.
		pending := startLogin(t, svc, "a@example.com")
		for range 3 {
			if _, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "bad"); !errors.Is(err, auth.ErrInvalidSecondFactor) {
				t.Fatal(err)
			}
		}
		pending = startLogin(t, svc, "a@example.com")
		for range 2 {
			if _, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "bad"); !errors.Is(err, auth.ErrInvalidSecondFactor) {
				t.Fatal(err)
			}
		}
		// Five failures: even the right factor and the right password are
		// refused until the window ends.
		var te *auth.ThrottledError
		if _, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "good"); !errors.As(err, &te) {
			t.Fatalf("sixth attempt: %v, want throttled", err)
		}
		if _, err := svc.Login(context.Background(), auth.LoginRequest{Email: "a@example.com", Password: pw, ClientIP: "192.0.2.9"}); !errors.Is(err, auth.ErrThrottled) {
			t.Errorf("password login while throttled: %v", err)
		}

		// After the window, the pending login has expired but a new login
		// works, and success resets the count.
		clk.Add(15 * time.Minute)
		pending = startLogin(t, svc, "a@example.com")
		for range 4 {
			_, _ = secondStep(svc, pending.Token, auth.FactorWebAuthn, "bad")
		}
		if _, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "good"); err != nil {
			t.Fatalf("after the window: %v", err)
		}
		pending = startLogin(t, svc, "a@example.com")
		for range 4 {
			_, _ = secondStep(svc, pending.Token, auth.FactorWebAuthn, "bad")
		}
		if _, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "good"); err != nil {
			t.Errorf("count not reset by a successful login: %v", err)
		}
	})
}

func TestSecondStepUsedOnceConcurrently(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, _ := newTwoFactorService(t, db)
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		enroll(t, db, u.ID)
		pending := startLogin(t, svc, "a@example.com")

		var (
			wg sync.WaitGroup
			mu sync.Mutex
			ok int
		)
		for range 8 {
			wg.Go(func() {
				if _, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "good"); err == nil {
					mu.Lock()
					ok++
					mu.Unlock()
				} else if !errors.Is(err, auth.ErrLoginExpired) {
					t.Errorf("concurrent second step: %v", err)
				}
			})
		}
		wg.Wait()
		if ok != 1 {
			t.Errorf("%d logins from one pending login, want 1", ok)
		}
	})
}

var recoveryCodeFormat = regexp.MustCompile(`^[A-Z2-7]{4}-[A-Z2-7]{4}-[A-Z2-7]{4}-[A-Z2-7]{4}$`)

func TestRecoveryCodes(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, _ := newTwoFactorService(t, db)
		u := mustCreate(t, svc, "a@example.com", store.RoleViewer)

		// Regenerating needs a second factor.
		if _, err := svc.RegenerateRecoveryCodes(ctx, u.ID); !errors.Is(err, auth.ErrNoSecondFactor) {
			t.Errorf("regenerate without a factor: %v, want ErrNoSecondFactor", err)
		}

		// First enrollment issues the set, in the enrollment's transaction.
		var codes []redact.Secret
		if err := db.InTx(ctx, func(q store.Querier) error {
			var err error
			codes, err = svc.IssueRecoveryCodes(ctx, q, u.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		enroll(t, db, u.ID)
		seen := map[string]bool{}
		for _, c := range codes {
			if !recoveryCodeFormat.MatchString(c.Reveal()) || seen[c.Reveal()] {
				t.Errorf("code %q: wrong format or repeated", c.Reveal())
			}
			seen[c.Reveal()] = true
		}
		if len(codes) != auth.RecoveryCodeCount || auth.RecoveryCodeCount != 10 {
			t.Fatalf("%d codes, want 10", len(codes))
		}
		stored, err := store.NewRecoveryCodes(db).ListForUser(ctx, u.ID)
		if err != nil || len(stored) != 10 {
			t.Fatalf("stored codes: %d, %v", len(stored), err)
		}
		for _, s := range stored {
			if seen[s.CodeHash] || strings.Contains(s.CodeHash, "-") {
				t.Error("a code is stored in plaintext")
			}
		}
		if n, err := svc.RecoveryCodesRemaining(ctx, u.ID); err != nil || n != 10 {
			t.Errorf("RecoveryCodesRemaining = %d, %v", n, err)
		}

		pending := startLogin(t, svc, "a@example.com")
		if len(pending.Methods) != 2 || pending.Methods[0] != auth.FactorWebAuthn || pending.Methods[1] != auth.FactorRecoveryCode {
			t.Errorf("methods = %v", pending.Methods)
		}
		// Malformed codes are wrong codes.
		// (Each counts toward the throttle, so only a few are tried here.)
		for _, bad := range []string{"AAAA", "AAAA-AAAA-AAAA-AAA1"} {
			if _, err := secondStep(svc, pending.Token, auth.FactorRecoveryCode, bad); !errors.Is(err, auth.ErrInvalidSecondFactor) {
				t.Errorf("code %q: %v, want ErrInvalidSecondFactor", bad, err)
			}
		}
		// Codes are accepted in lowercase and without dashes.
		typed := strings.ToLower(strings.ReplaceAll(codes[0].Reveal(), "-", " "))
		res, err := secondStep(svc, pending.Token, auth.FactorRecoveryCode, typed)
		if err != nil || res.SecondFactor != auth.FactorRecoveryCode {
			t.Fatalf("recovery code login: %+v, %v", res, err)
		}
		if n, _ := svc.RecoveryCodesRemaining(ctx, u.ID); n != 9 {
			t.Errorf("%d codes left, want 9", n)
		}

		// The same code is refused the second time.
		pending = startLogin(t, svc, "a@example.com")
		if _, err := secondStep(svc, pending.Token, auth.FactorRecoveryCode, codes[0].Reveal()); !errors.Is(err, auth.ErrInvalidSecondFactor) {
			t.Errorf("reused code: %v, want ErrInvalidSecondFactor", err)
		}

		// Regenerating replaces the whole set.
		fresh, err := svc.RegenerateRecoveryCodes(ctx, u.ID)
		if err != nil || len(fresh) != 10 {
			t.Fatalf("regenerate: %d, %v", len(fresh), err)
		}
		if n, _ := svc.RecoveryCodesRemaining(ctx, u.ID); n != 10 {
			t.Errorf("%d codes after regenerating, want 10", n)
		}
		if _, err := secondStep(svc, pending.Token, auth.FactorRecoveryCode, codes[1].Reveal()); !errors.Is(err, auth.ErrInvalidSecondFactor) {
			t.Errorf("old code after regenerating: %v, want ErrInvalidSecondFactor", err)
		}
		if _, err := secondStep(svc, pending.Token, auth.FactorRecoveryCode, fresh[0].Reveal()); err != nil {
			t.Errorf("new code: %v", err)
		}
	})
}

// TestPendingLoginFloodKeepsOtherLogins is #360 (S5): a user who starts
// many pending logins with a right password must not evict another user's.
func TestPendingLoginFloodKeepsOtherLogins(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, _ := newTwoFactorService(t, db)
		svc.LimitPendingLogins(8)
		victim := mustCreate(t, svc, "victim@example.com", store.RoleViewer)
		attacker := mustCreate(t, svc, "attacker@example.com", store.RoleViewer)
		enroll(t, db, victim.ID)
		enroll(t, db, attacker.ID)

		pending := startLogin(t, svc, "victim@example.com")
		// More starts than the whole store holds, from within the throttle.
		for range 15 {
			_ = startLogin(t, svc, "attacker@example.com")
		}
		if _, err := secondStep(svc, pending.Token, auth.FactorWebAuthn, "good"); err != nil {
			t.Errorf("victim's second step after a flood = %v, want success", err)
		}
	})
}

// TestPendingLoginStartsAreThrottled checks that right-password logins are
// counted per user, and that the count doesn't lock out other users.
func TestPendingLoginStartsAreThrottled(t *testing.T) {
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newTwoFactorService(t, db)
		a := mustCreate(t, svc, "a@example.com", store.RoleViewer)
		b := mustCreate(t, svc, "b@example.com", store.RoleViewer)
		enroll(t, db, a.ID)
		enroll(t, db, b.ID)

		// Different addresses, so only the per-user count can trip.
		start := func(email, ip string) error {
			_, err := svc.Login(context.Background(), auth.LoginRequest{Email: email, Password: pw, ClientIP: ip})
			return err
		}
		for i := range 20 {
			if err := start("a@example.com", fmt.Sprintf("198.51.100.%d", i)); !errors.Is(err, auth.ErrSecondFactorRequired) {
				t.Fatalf("start %d = %v, want a pending login", i, err)
			}
		}
		if err := start("a@example.com", "203.0.113.1"); !errors.Is(err, auth.ErrThrottled) {
			t.Errorf("21st start = %v, want ErrThrottled", err)
		}
		if err := start("b@example.com", "203.0.113.2"); !errors.Is(err, auth.ErrSecondFactorRequired) {
			t.Errorf("other user's start = %v, want a pending login", err)
		}
		clk.Add(16 * time.Minute)
		if err := start("a@example.com", "203.0.113.1"); !errors.Is(err, auth.ErrSecondFactorRequired) {
			t.Errorf("start after the window = %v, want a pending login", err)
		}
	})
}
