package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var cheap = auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

const pw = "correct horse battery"

func newService(t *testing.T, db *store.DB) (*auth.Service, *clock) {
	t.Helper()
	c := &clock{t: time.Now()}
	return auth.NewService(db, auth.Options{Logger: storetest.Logger(), Now: c.Now, PasswordParams: cheap}), c
}

func mustCreate(t *testing.T, svc *auth.Service, email string, role store.Role) store.User {
	t.Helper()
	u, err := svc.CreateUser(context.Background(), auth.NewUser{Email: email, Password: pw, Role: role})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func login(t *testing.T, svc *auth.Service, email string) auth.LoginResult {
	t.Helper()
	res, err := svc.Login(context.Background(), auth.LoginRequest{Email: email, Password: pw, ClientIP: "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestLogin(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, _ := newService(t, db)
		u := mustCreate(t, svc, " Admin@Example.com ", store.RoleAdmin)
		if u.Email != "admin@example.com" {
			t.Errorf("email not normalized: %q", u.Email)
		}

		res := login(t, svc, "ADMIN@example.com")
		if res.User.ID != u.ID || res.User.LastLoginAt == nil || res.Token == "" || res.CSRFToken != auth.CSRFToken(res.Token) {
			t.Errorf("login result: %+v", res)
		}
		p, err := svc.AuthenticateSession(ctx, res.Token)
		if err != nil || p.UserID != u.ID || p.Role != store.RoleAdmin || p.Method != auth.MethodSession || p.SessionID != res.Session.ID || p.TokenID != "" {
			t.Errorf("AuthenticateSession = %+v, %v", p, err)
		}
		if res.Session.TokenHash == res.Token.Reveal() {
			t.Error("session stores the plaintext token")
		}

		// Unknown email, wrong password, and disabled user all fail the same way.
		disabled := mustCreate(t, svc, "off@example.com", store.RoleViewer)
		yes := true
		if _, err := svc.UpdateUser(ctx, disabled.ID, auth.UserChanges{Disabled: &yes}); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct{ email, pw string }{
			{"nobody@example.com", pw}, {"admin@example.com", "wrong password"}, {"off@example.com", pw},
		} {
			_, err := svc.Login(ctx, auth.LoginRequest{Email: c.email, Password: redact.Secret(c.pw), ClientIP: "198.51.100." + c.email[:1]})
			if err != auth.ErrInvalidCredentials { //nolint:errorlint // must be exactly the generic error
				t.Errorf("login %s: %v", c.email, err)
			}
		}
	})
}

func TestLoginThrottle(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newService(t, db)
		mustCreate(t, svc, "a@example.com", store.RoleAdmin)
		bad := auth.LoginRequest{Email: "a@example.com", Password: "wrong password", ClientIP: "192.0.2.1"}
		for range 5 {
			if _, err := svc.Login(ctx, bad); !errors.Is(err, auth.ErrInvalidCredentials) {
				t.Fatal(err)
			}
		}
		// Even the right password is refused while throttled.
		good := bad
		good.Password = pw
		if _, err := svc.Login(ctx, good); !errors.Is(err, auth.ErrThrottled) {
			t.Fatalf("throttled login: %v", err)
		}
		// Unknown accounts are throttled the same way, so throttling
		// doesn't reveal which emails exist.
		ghost := auth.LoginRequest{Email: "ghost@example.com", Password: "x", ClientIP: "192.0.2.2"}
		for range 5 {
			_, _ = svc.Login(ctx, ghost)
		}
		if _, err := svc.Login(ctx, ghost); !errors.Is(err, auth.ErrThrottled) {
			t.Errorf("unknown account not throttled: %v", err)
		}
		clk.Add(16 * time.Minute)
		if _, err := svc.Login(ctx, good); err != nil {
			t.Errorf("login after window: %v", err)
		}
	})
}

func TestSessionExpiry(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newService(t, db)
		mustCreate(t, svc, "a@example.com", store.RoleAdmin)

		// Idle timeout, refreshed by use.
		res := login(t, svc, "a@example.com")
		for range 3 {
			clk.Add(auth.DefaultSessionIdleTimeout - time.Hour)
			if _, err := svc.AuthenticateSession(ctx, res.Token); err != nil {
				t.Fatalf("active session rejected: %v", err)
			}
		}
		clk.Add(auth.DefaultSessionIdleTimeout)
		if _, err := svc.AuthenticateSession(ctx, res.Token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("idle session: %v", err)
		}

		// Absolute lifetime, however active.
		res = login(t, svc, "a@example.com")
		for elapsed := time.Duration(0); elapsed < auth.DefaultSessionMaxAge-12*time.Hour; elapsed += 6 * time.Hour {
			clk.Add(6 * time.Hour)
			if _, err := svc.AuthenticateSession(ctx, res.Token); err != nil {
				t.Fatalf("session rejected after %s: %v", elapsed, err)
			}
		}
		clk.Add(12 * time.Hour)
		if _, err := svc.AuthenticateSession(ctx, res.Token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("session past max age: %v", err)
		}

		// Logout.
		res = login(t, svc, "a@example.com")
		if err := svc.Logout(ctx, res.Session.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AuthenticateSession(ctx, res.Token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("session after logout: %v", err)
		}
		if _, err := svc.AuthenticateSession(ctx, "garbage"); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("garbage token: %v", err)
		}
	})
}

func TestAPITokens(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, clk := newService(t, db)
		u := mustCreate(t, svc, "a@example.com", store.RoleOperator)

		tok, plain, err := svc.CreateAPIToken(ctx, u.ID, auth.NewAPIToken{Name: " ci ", Scope: store.ScopeReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		if tok.Name != "ci" || tok.Prefix != plain.Reveal()[:8] || tok.TokenHash == plain.Reveal() {
			t.Errorf("created token: %+v", tok)
		}
		p, err := svc.AuthenticateToken(ctx, plain)
		if err != nil || p.UserID != u.ID || p.Method != auth.MethodToken || p.TokenID != tok.ID || p.Scope != store.ScopeReadOnly || p.Role != store.RoleOperator {
			t.Errorf("AuthenticateToken = %+v, %v", p, err)
		}
		list, _ := svc.ListAPITokens(ctx, u.ID, "", 0)
		if len(list) != 1 || list[0].LastUsedAt == nil {
			t.Errorf("last used not recorded: %+v", list)
		}

		// Expiry.
		exp := clk.Now().Add(time.Hour)
		_, expiring, err := svc.CreateAPIToken(ctx, u.ID, auth.NewAPIToken{Name: "x", Scope: store.ScopeFull, ExpiresAt: &exp})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AuthenticateToken(ctx, expiring); err != nil {
			t.Fatal(err)
		}
		clk.Add(time.Hour)
		if _, err := svc.AuthenticateToken(ctx, expiring); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("expired token: %v", err)
		}

		// Revocation.
		if err := svc.RevokeAPIToken(ctx, u.ID, tok.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AuthenticateToken(ctx, plain); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("revoked token: %v", err)
		}
		if err := svc.RevokeAPIToken(ctx, u.ID, tok.ID); !errors.Is(err, auth.ErrTokenNotFound) {
			t.Errorf("revoke twice: %v", err)
		}

		// Validation.
		past := clk.Now().Add(-time.Second)
		for _, c := range []struct {
			nt   auth.NewAPIToken
			want error
		}{
			{auth.NewAPIToken{Name: "", Scope: store.ScopeFull}, auth.ErrInvalidTokenName},
			{auth.NewAPIToken{Name: "x", Scope: "admin"}, auth.ErrInvalidScope},
			{auth.NewAPIToken{Name: "x", Scope: store.ScopeFull, ExpiresAt: &past}, auth.ErrInvalidExpiry},
		} {
			if _, _, err := svc.CreateAPIToken(ctx, u.ID, c.nt); !errors.Is(err, c.want) {
				t.Errorf("CreateAPIToken(%+v) = %v, want %v", c.nt, err, c.want)
			}
		}
		if _, err := svc.AuthenticateToken(ctx, "pft_nope"); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("malformed token: %v", err)
		}
	})
}

func TestRevocationOnPasswordChangeAndDisable(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, _ := newService(t, db)
		u := mustCreate(t, svc, "a@example.com", store.RoleOperator) // not the last admin, so it can be disabled
		current := login(t, svc, "a@example.com")
		other := login(t, svc, "a@example.com")
		_, token, err := svc.CreateAPIToken(ctx, u.ID, auth.NewAPIToken{Name: "ci", Scope: store.ScopeFull})
		if err != nil {
			t.Fatal(err)
		}
		p, err := svc.AuthenticateSession(ctx, current.Token)
		if err != nil {
			t.Fatal(err)
		}

		if err := svc.ChangePassword(ctx, p, "wrong password", "new password 123"); !errors.Is(err, auth.ErrWrongPassword) {
			t.Errorf("wrong current password: %v", err)
		}
		if err := svc.ChangePassword(ctx, p, pw, "short"); !errors.Is(err, auth.ErrPasswordPolicy) {
			t.Errorf("short new password: %v", err)
		}
		if err := svc.ChangePassword(ctx, p, pw, "new password 123"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AuthenticateSession(ctx, current.Token); err != nil {
			t.Errorf("current session revoked: %v", err)
		}
		if _, err := svc.AuthenticateSession(ctx, other.Token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("other session survived password change: %v", err)
		}
		if _, err := svc.AuthenticateToken(ctx, token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("token survived password change: %v", err)
		}
		if _, err := svc.Login(ctx, auth.LoginRequest{Email: "a@example.com", Password: "new password 123"}); err != nil {
			t.Errorf("login with new password: %v", err)
		}

		// Disabling signs out every session and revokes tokens; re-enabling
		// doesn't bring them back.
		_, token, _ = svc.CreateAPIToken(ctx, u.ID, auth.NewAPIToken{Name: "ci2", Scope: store.ScopeFull})
		yes, no := true, false
		if _, err := svc.UpdateUser(ctx, u.ID, auth.UserChanges{Disabled: &yes}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.UpdateUser(ctx, u.ID, auth.UserChanges{Disabled: &no}); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AuthenticateSession(ctx, current.Token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("session survived disable: %v", err)
		}
		if _, err := svc.AuthenticateToken(ctx, token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("token survived disable: %v", err)
		}

		// Role changes apply to existing sessions immediately.
		res, err := svc.Login(ctx, auth.LoginRequest{Email: "a@example.com", Password: "new password 123"})
		if err != nil {
			t.Fatal(err)
		}
		viewer := store.RoleViewer
		if _, err := svc.UpdateUser(ctx, u.ID, auth.UserChanges{Role: &viewer}); err != nil {
			t.Fatal(err)
		}
		if p, err := svc.AuthenticateSession(ctx, res.Token); err != nil || p.Role != store.RoleViewer {
			t.Errorf("role after change: %+v, %v", p, err)
		}

		// Reset from the command line.
		if _, err := svc.ResetPassword(ctx, "A@example.com", "reset password 1"); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.AuthenticateSession(ctx, res.Token); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("session survived reset: %v", err)
		}
	})
}

func TestUserValidation(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		svc, _ := newService(t, db)
		u := mustCreate(t, svc, "a@example.com", store.RoleOperator)
		for _, c := range []struct {
			nu   auth.NewUser
			want error
		}{
			{auth.NewUser{Email: "not-an-email", Password: pw, Role: store.RoleAdmin}, auth.ErrInvalidEmail},
			{auth.NewUser{Email: "Name <x@example.com>", Password: pw, Role: store.RoleAdmin}, auth.ErrInvalidEmail},
			{auth.NewUser{Email: "x@example.com", Password: pw, Role: "root"}, auth.ErrInvalidRole},
			{auth.NewUser{Email: "x@example.com", Password: "short", Role: store.RoleAdmin}, auth.ErrPasswordPolicy},
			{auth.NewUser{Email: "A@example.com", Password: pw, Role: store.RoleAdmin}, auth.ErrEmailTaken},
		} {
			if _, err := svc.CreateUser(ctx, c.nu); !errors.Is(err, c.want) {
				t.Errorf("CreateUser(%s) = %v, want %v", c.nu.Email, err, c.want)
			}
		}
		root := store.Role("root")
		if _, err := svc.UpdateUser(ctx, u.ID, auth.UserChanges{Role: &root}); !errors.Is(err, auth.ErrInvalidRole) {
			t.Errorf("UpdateUser invalid role: %v", err)
		}
		if _, err := svc.UpdateUser(ctx, "01j9zq3v6t8k4m2n5p7r9s1x3y", auth.UserChanges{Role: &root}); err == nil {
			t.Error("UpdateUser of a missing user succeeded")
		}
		if err := svc.DeleteUser(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.GetUser(ctx, u.ID); !errors.Is(err, auth.ErrUserNotFound) {
			t.Errorf("GetUser after delete: %v", err)
		}
	})
}
