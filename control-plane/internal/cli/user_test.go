package cli_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth/authtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets/secretstest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// runUser runs a user subcommand against dataDir with stdin.
func runUser(t *testing.T, dataDir, stdin string, args ...string) result {
	t.Helper()
	return runUserEnv(t, dataDir, nil, stdin, args...)
}

// runUserOn runs a user subcommand against the state database cfg
// describes, SQLite or PostgreSQL.
func runUserOn(t *testing.T, cfg store.Config, stdin string, args ...string) result {
	t.Helper()
	vars := map[string]string{"PRESSFLEET_DATABASE_DRIVER": string(cfg.Driver)}
	if cfg.Driver == store.Postgres {
		vars["PRESSFLEET_DATABASE_POSTGRES_DSN"] = cfg.PostgresDSN.Reveal()
	} else {
		vars["PRESSFLEET_DATABASE_SQLITE_PATH"] = cfg.SQLitePath
	}
	return runUserEnv(t, t.TempDir(), vars, stdin, args...)
}

// runUserEnv runs a user subcommand against dataDir with stdin and the
// environment variables vars.
func runUserEnv(t *testing.T, dataDir string, vars map[string]string, stdin string, args ...string) result {
	t.Helper()
	emptyConfig := filepath.Join(t.TempDir(), "pressfleet.yaml")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	vars = maps.Clone(vars)
	if vars == nil {
		vars = map[string]string{}
	}
	vars[config.EnvConfigPath] = emptyConfig
	var stdout, stderr syncBuffer
	code := cli.Run(context.Background(), append(append([]string{"user"}, args...), "--data-dir", dataDir), cli.Options{
		Version: "test",
		Stdin:   strings.NewReader(stdin),
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env:     func(k string) (string, bool) { v, ok := vars[k]; return v, ok },
	})
	return result{code, stdout.String(), stderr.String()}
}

// tryLogin logs in against the state database in dataDir.
func tryLogin(t *testing.T, dataDir, email, pw string) (store.User, error) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, store.Config{Driver: store.SQLite, SQLitePath: filepath.Join(dataDir, "state", "pressfleet.db")}, storetest.Logger())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	res, err := auth.NewService(db, auth.Options{Logger: storetest.Logger()}).Login(ctx, auth.LoginRequest{Email: email, Password: redact.Secret(pw)})
	return res.User, err
}

var generatedPassword = regexp.MustCompile(`(?m)^Password: ([a-z2-7]{26})$`)

func TestUserCreate(t *testing.T) {
	dataDir := t.TempDir()

	// Generated password, default role admin.
	r := runUser(t, dataDir, "", "create", "--email", "Admin@Example.com")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr: %s", r.code, r.stderr)
	}
	m := generatedPassword.FindStringSubmatch(r.stdout)
	if m == nil || !strings.Contains(r.stdout, "Created admin user admin@example.com") {
		t.Fatalf("stdout: %s", r.stdout)
	}
	if strings.Contains(r.stderr, m[1]) {
		t.Error("password was logged")
	}
	if u, err := tryLogin(t, dataDir, "admin@example.com", m[1]); err != nil || u.Role != store.RoleAdmin {
		t.Errorf("login with generated password: %+v, %v", u, err)
	}

	// Password from stdin, explicit role; nothing secret printed.
	r = runUser(t, dataDir, "viewer password 1\n", "create", "--email", "v@example.com", "--role", "viewer", "--password-stdin")
	if r.code != 0 || strings.Contains(r.stdout, "Password:") || !strings.Contains(r.stdout, "Created viewer user v@example.com") {
		t.Fatalf("exit %d, stdout: %s, stderr: %s", r.code, r.stdout, r.stderr)
	}
	if u, err := tryLogin(t, dataDir, "v@example.com", "viewer password 1"); err != nil || u.Role != store.RoleViewer {
		t.Errorf("login with stdin password: %+v, %v", u, err)
	}

	for name, tt := range map[string]struct {
		stdin string
		args  []string
		want  string
	}{
		"duplicate":      {"", []string{"--email", "admin@example.com"}, "already exists"},
		"bad role":       {"", []string{"--email", "x@example.com", "--role", "root"}, "role must be"},
		"short password": {"short\n", []string{"--email", "x@example.com", "--password-stdin"}, "between 12 and 256"},
		"bad email":      {"", []string{"--email", "nope"}, "invalid email"},
		"missing email":  {"", nil, `"email" not set`},
	} {
		t.Run(name, func(t *testing.T) {
			r := runUser(t, dataDir, tt.stdin, append([]string{"create"}, tt.args...)...)
			if r.code != 1 || !strings.Contains(r.stderr, tt.want) {
				t.Errorf("exit %d, stderr: %s", r.code, r.stderr)
			}
		})
	}
}

func TestUserResetPassword(t *testing.T) {
	dataDir := t.TempDir()
	if r := runUser(t, dataDir, "original password\n", "create", "--email", "a@example.com", "--password-stdin"); r.code != 0 {
		t.Fatalf("create: %s", r.stderr)
	}

	r := runUser(t, dataDir, "", "reset-password", "--email", "A@example.com")
	m := generatedPassword.FindStringSubmatch(r.stdout)
	if r.code != 0 || m == nil || !strings.Contains(r.stdout, "Reset the password of admin user a@example.com") {
		t.Fatalf("exit %d, stdout: %s, stderr: %s", r.code, r.stdout, r.stderr)
	}
	if _, err := tryLogin(t, dataDir, "a@example.com", "original password"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("old password still works: %v", err)
	}
	if _, err := tryLogin(t, dataDir, "a@example.com", m[1]); err != nil {
		t.Errorf("new password: %v", err)
	}

	if r := runUser(t, dataDir, "chosen password 2\n", "reset-password", "--email", "a@example.com", "--password-stdin"); r.code != 0 {
		t.Fatalf("reset from stdin: %s", r.stderr)
	}
	if _, err := tryLogin(t, dataDir, "a@example.com", "chosen password 2"); err != nil {
		t.Errorf("stdin password: %v", err)
	}

	if r := runUser(t, dataDir, "", "reset-password", "--email", "nobody@example.com"); r.code != 1 || !strings.Contains(r.stderr, "user not found") {
		t.Errorf("unknown user: exit %d, stderr: %s", r.code, r.stderr)
	}
}

var userID = regexp.MustCompile(`\(id ([0-9a-z]+)\)`)

// TestUserAudit is the acceptance criterion of #180: creating a user and
// resetting a password from the CLI each leave one audit event, with no
// actor user, "actor": "cli", and no secrets; failed commands leave none.
func TestUserAudit(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			cfg := storetest.Config(t, d)
			db := storetest.OpenUnmigrated(t, cfg)
			if _, err := db.Migrate(ctx, filepath.Join(t.TempDir(), "backups")); err != nil {
				t.Fatal(err)
			}
			var secrets []string
			// run runs a command that must succeed, and returns the ID of
			// the user it names and any password it generated.
			run := func(stdin string, args ...string) string {
				t.Helper()
				r := runUserOn(t, cfg, stdin, args...)
				m := userID.FindStringSubmatch(r.stdout)
				if r.code != 0 || m == nil {
					t.Fatalf("%v: exit %d, stdout: %s, stderr: %s", args, r.code, r.stdout, r.stderr)
				}
				if pw := generatedPassword.FindStringSubmatch(r.stdout); pw != nil {
					secrets = append(secrets, pw[1])
				}
				return m[1]
			}
			// check wants exactly one event with action per user in want,
			// with the CLI as the actor and the user's details.
			check := func(action string, want map[string]map[string]string) {
				t.Helper()
				evs := auditEvents(t, db, action)
				got := map[string]map[string]string{}
				for _, ev := range evs {
					if ev.ActorUserID != "" || ev.ActorTokenID != "" || ev.TargetType != "user" || ev.Result != store.AuditSuccess {
						t.Errorf("%s event = %+v, want a successful one on a user with no actor", action, ev)
					}
					got[ev.TargetID] = ev.Details
				}
				if len(evs) != len(want) || !maps.EqualFunc(got, want, maps.Equal) {
					t.Errorf("%s events = %+v, want one per user with details %v", action, evs, want)
				}
			}

			secrets = append(secrets, "chosen password 1", "chosen password 2")
			admin := run("chosen password 1\n", "create", "--email", "a@example.com", "--password-stdin")
			viewer := run("", "create", "--email", "v@example.com", "--role", "viewer")
			created := map[string]map[string]string{admin: {"actor": "cli", "role": "admin"}, viewer: {"actor": "cli", "role": "viewer"}}
			check("user.create", created)

			run("", "reset-password", "--email", "a@example.com")
			run("chosen password 2\n", "reset-password", "--email", "v@example.com", "--password-stdin")
			// Generated and stdin passwords get the same details.
			reset := map[string]map[string]string{admin: {"actor": "cli"}, viewer: {"actor": "cli"}}
			check("user.update", reset)

			// Failed commands record nothing.
			for name, tt := range map[string]struct {
				stdin string
				args  []string
			}{
				"existing email":       {"", []string{"create", "--email", "A@example.com"}},
				"bad role":             {"", []string{"create", "--email", "x@example.com", "--role", "root"}},
				"unknown email":        {"", []string{"reset-password", "--email", "nobody@example.com"}},
				"unknown email, stdin": {"chosen password 3\n", []string{"reset-password", "--email", "nobody@example.com", "--password-stdin"}},
			} {
				if r := runUserOn(t, cfg, tt.stdin, tt.args...); r.code != 1 {
					t.Errorf("%s: exit %d, stdout: %s, stderr: %s", name, r.code, r.stdout, r.stderr)
				}
			}
			check("user.create", created)
			check("user.update", reset)

			all := auditEvents(t, db, "")
			if len(all) != 4 {
				t.Errorf("audit events = %+v, want 4", all)
			}
			if len(secrets) != 4 {
				t.Fatalf("passwords = %d, want 2 generated and 2 chosen", len(secrets))
			}
			for _, ev := range all {
				s := fmt.Sprintf("%+v", ev)
				for _, pw := range secrets {
					if strings.Contains(s, pw) {
						t.Errorf("audit event %s holds a password", s)
					}
				}
			}
		})
	}
}

const testRPID = "pf.example.com"

// twoFactorService returns an auth service on db with TOTP and WebAuthn,
// under policy, as the control plane would run it.
func twoFactorService(t *testing.T, db *store.DB, policy auth.TwoFactorPolicy) *auth.Service {
	t.Helper()
	return auth.NewService(db, auth.Options{
		Logger:       storetest.Logger(),
		TwoFactor:    policy,
		RelyingParty: auth.RelyingPartyFor(testRPID),
		Factors:      []auth.FactorVerifier{auth.NewTOTPVerifier(secretstest.Keyring(t, 1), testRPID)},
	})
}

// enrollEverything gives the user with email a session, an authenticator
// app with its recovery codes, and a security key, and returns the user
// and the session's token.
func enrollEverything(t *testing.T, svc *auth.Service, email string) (store.User, redact.Secret) {
	t.Helper()
	ctx := context.Background()
	res, err := svc.Login(ctx, auth.LoginRequest{Email: email, Password: "original password"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := svc.StartTOTPEnrollment(ctx, res.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmTOTP(ctx, res.User.ID, redact.Secret(authtest.TOTPCode(e.Secret.Reveal(), time.Now()))); err != nil {
		t.Fatal(err)
	}
	opts, err := svc.BeginWebAuthnRegistration(ctx, res.User.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := authtest.NewAuthenticator("https://" + testRPID).Register(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FinishWebAuthnRegistration(ctx, res.User.ID, "key", resp); err != nil {
		t.Fatal(err)
	}
	return res.User, res.Token
}

// auditEvents returns the audit events with action, newest first.
func auditEvents(t *testing.T, db *store.DB, action string) []store.AuditEvent {
	t.Helper()
	page, err := store.NewAuditEvents(db).List(context.Background(), store.AuditFilter{Action: action})
	if err != nil {
		t.Fatal(err)
	}
	return page.Events
}

// TestUserResetTwoFactor is the acceptance criterion of #156: an Admin
// who lost every factor logs in again after the command, and the reset is
// in the audit log.
func TestUserResetTwoFactor(t *testing.T) {
	ctx := context.Background()
	for _, d := range storetest.Dialects {
		t.Run(string(d), func(t *testing.T) {
			cfg := storetest.Config(t, d)
			db := storetest.OpenUnmigrated(t, cfg)
			if _, err := db.Migrate(ctx, filepath.Join(t.TempDir(), "backups")); err != nil {
				t.Fatal(err)
			}
			for _, email := range []string{"a@example.com", "b@example.com"} {
				if r := runUserOn(t, cfg, "original password\n", "create", "--email", email, "--password-stdin"); r.code != 0 {
					t.Fatalf("create: %s", r.stderr)
				}
			}
			svc := twoFactorService(t, db, auth.TwoFactorPolicy{})
			admin, session := enrollEverything(t, svc, "a@example.com")
			other, _ := enrollEverything(t, svc, "b@example.com")

			if r := runUserOn(t, cfg, "", "reset-two-factor", "--email", "nobody@example.com", "--yes"); r.code != 1 || !strings.Contains(r.stderr, "user not found") {
				t.Errorf("unknown user: exit %d, stderr: %s", r.code, r.stderr)
			}

			// Declining, or not answering, changes nothing.
			for name, stdin := range map[string]string{"no": "n\n", "empty": "\n", "EOF": ""} {
				r := runUserOn(t, cfg, stdin, "reset-two-factor", "--email", "a@example.com")
				want := "the reset was canceled; nothing was changed"
				if stdin == "" {
					want = "the reset needs confirmation: answer the prompt, or pass --yes"
				}
				if r.code != 1 || !strings.Contains(r.stderr, want) ||
					!strings.Contains(r.stdout, "admin user a@example.com (id "+admin.ID+")") || !strings.Contains(r.stdout, "Reset their second factors? [y/N]") {
					t.Errorf("%s: exit %d, stdout: %s, stderr: %s", name, r.code, r.stdout, r.stderr)
				}
			}
			if on, err := svc.TwoFactorEnabled(ctx, admin.ID); err != nil || !on {
				t.Errorf("after declining, TwoFactorEnabled = %v, %v", on, err)
			}
			if _, err := svc.AuthenticateSession(ctx, session); err != nil {
				t.Errorf("after declining, session: %v", err)
			}
			if evs := auditEvents(t, db, "user.two_factor.reset"); len(evs) != 0 {
				t.Errorf("after declining, audit events: %+v", evs)
			}

			// Yes, after an answer the prompt doesn't understand.
			r := runUserOn(t, cfg, "maybe\ny\n", "reset-two-factor", "--email", "A@example.com")
			if r.code != 0 || !strings.Contains(r.stdout, "Reset the second factors of admin user a@example.com (id "+admin.ID+")\n"+
				"Removed the authenticator app, 1 security key or passkey, and 10 recovery codes, and ended 1 session.\n"+
				"They can now log in with their password alone.\n") {
				t.Fatalf("yes: exit %d, stdout: %s, stderr: %s", r.code, r.stdout, r.stderr)
			}
			if _, err := svc.AuthenticateSession(ctx, session); !errors.Is(err, auth.ErrUnauthenticated) {
				t.Errorf("session after reset: %v", err)
			}
			evs := auditEvents(t, db, "user.two_factor.reset")
			want := map[string]string{
				"actor": "cli", "totp_removed": "true", "webauthn_credentials_removed": "1",
				"recovery_codes_removed": "10", "sessions_ended": "1",
			}
			if len(evs) != 1 || evs[0].Action != "user.two_factor.reset" || evs[0].ActorUserID != "" || evs[0].ActorTokenID != "" ||
				evs[0].TargetType != "user" || evs[0].TargetID != admin.ID || evs[0].Result != store.AuditSuccess || !maps.Equal(evs[0].Details, want) {
				t.Errorf("audit events = %+v, want one reset of %s with details %v", evs, admin.ID, want)
			}

			// The Admin logs in with their password alone; under
			// two_factor.required: all, to a session restricted to enrolling.
			if res, err := svc.Login(ctx, auth.LoginRequest{Email: "a@example.com", Password: "original password"}); err != nil || res.SecondFactor != "" {
				t.Errorf("login after reset = %+v, %v", res, err)
			}
			strict := twoFactorService(t, db, auth.TwoFactorPolicy{Required: auth.RequireAll})
			res, err := strict.Login(ctx, auth.LoginRequest{Email: "a@example.com", Password: "original password"})
			if err != nil {
				t.Fatalf("login under required: all: %v", err)
			}
			if p, err := strict.AuthenticateSession(ctx, res.Token); err != nil || !p.EnrollmentRequired {
				t.Errorf("session under required: all = %+v, %v", p, err)
			}

			// Nothing left to reset: a message, success, and no event.
			r = runUserOn(t, cfg, "", "reset-two-factor", "--email", "a@example.com", "--yes")
			if r.code != 0 || strings.Contains(r.stdout, "[y/N]") ||
				!strings.Contains(r.stdout, "Nothing to reset: admin user a@example.com (id "+admin.ID+") has no second factors.") {
				t.Errorf("nothing to reset: exit %d, stdout: %s, stderr: %s", r.code, r.stdout, r.stderr)
			}
			if n := len(auditEvents(t, db, "user.two_factor.reset")); n != 1 {
				t.Errorf("audit events after a reset with nothing to reset: %d, want 1", n)
			}

			// The other user kept everything.
			if on, err := svc.TwoFactorEnabled(ctx, other.ID); err != nil || !on {
				t.Errorf("other user's TwoFactorEnabled = %v, %v", on, err)
			}
		})
	}
}
