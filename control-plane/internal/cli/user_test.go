package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/cli"
	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// runUser runs a user subcommand against dataDir with stdin.
func runUser(t *testing.T, dataDir, stdin string, args ...string) result {
	t.Helper()
	emptyConfig := filepath.Join(t.TempDir(), "pressfleet.yaml")
	if err := os.WriteFile(emptyConfig, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{config.EnvConfigPath: emptyConfig}
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
