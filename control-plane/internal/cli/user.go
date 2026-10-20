package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// User commands operate on the state database, so they run where it is:
// inside the control plane container (design doc §4.7), e.g.
//
//	docker compose exec control-plane pressfleet user create --email admin@example.com

func (a *app) userCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Manage control plane users (run inside the control plane container)",
	}
	cmd.AddCommand(a.userCreateCommand(), a.userResetPasswordCommand(), a.userResetTwoFactorCommand())
	return cmd
}

const passwordStdinHelp = "read the password from the first line of standard input instead of generating one"

func (a *app) userCreateCommand() *cobra.Command {
	var (
		email, role   string
		passwordStdin bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a user",
		Long: `Create a user in the state database.

Without --password-stdin, a random password is generated and printed once.
The default role is admin, for bootstrapping the first user ("pressfleet
init") and for recovery.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pw, generated, err := a.newPassword(passwordStdin)
			if err != nil {
				return err
			}
			return a.withAuth(cmd.Context(), func(svc *auth.Service) error {
				u, err := svc.CreateUser(cmd.Context(), auth.NewUser{Email: email, Password: pw, Role: store.Role(role)})
				if err != nil {
					return fmt.Errorf("creating user: %w", err)
				}
				return printUser(cmd.OutOrStdout(), "Created", u, pw, generated)
			})
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "email address to log in with (required)")
	cmd.Flags().StringVar(&role, "role", string(store.RoleAdmin), "role: admin, operator, or viewer")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, passwordStdinHelp)
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

func (a *app) userResetPasswordCommand() *cobra.Command {
	var (
		email         string
		passwordStdin bool
	)
	cmd := &cobra.Command{
		Use:   "reset-password",
		Short: "Set a new password for a user",
		Long: `Set a new password for a user, signing them out everywhere and revoking
their API tokens.

Without --password-stdin, a random password is generated and printed once.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pw, generated, err := a.newPassword(passwordStdin)
			if err != nil {
				return err
			}
			return a.withAuth(cmd.Context(), func(svc *auth.Service) error {
				u, err := svc.ResetPassword(cmd.Context(), email, pw)
				if err != nil {
					return fmt.Errorf("resetting password: %w", err)
				}
				return printUser(cmd.OutOrStdout(), "Reset the password of", u, pw, generated)
			})
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "email address of the user (required)")
	cmd.Flags().BoolVar(&passwordStdin, "password-stdin", false, passwordStdinHelp)
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

func (a *app) userResetTwoFactorCommand() *cobra.Command {
	var (
		email string
		yes   bool
	)
	cmd := &cobra.Command{
		Use:   "reset-two-factor",
		Short: "Remove all of a user's second factors",
		Long: `Remove all of a user's second factors, for a user who lost theirs: their
authenticator app, every security key and passkey, and every recovery code.
It signs them out everywhere; their API tokens keep working. They can then
log in with their password alone and, if two_factor.required covers their
role, must set up a new second factor first.

This is how an Admin locked out of their own account is recovered: the
dashboard doesn't let Admins reset their own second factors. The reset is
in the audit log as user.two_factor.reset, with no user as the actor.

It asks for confirmation; --yes gives it. A user with no second factors
is left as they are, and the command still succeeds.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.withAuthDB(cmd.Context(), func(db *store.DB, svc *auth.Service) error {
				return a.resetTwoFactor(cmd.Context(), cmd.OutOrStdout(), db, svc, email, yes)
			})
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "email address of the user (required)")
	cmd.Flags().BoolVar(&yes, "yes", false, "don't ask for confirmation")
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

// resetTwoFactor confirms and runs a two-factor reset, and audits it. The
// prompt can't give counts: they come from the reset itself, which is the
// only code that knows what a reset removes, so they are printed after.
func (a *app) resetTwoFactor(ctx context.Context, w io.Writer, db *store.DB, svc *auth.Service, email string, yes bool) error {
	u, err := svc.GetUserByEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("resetting second factors: %w", err)
	}
	if !yes {
		if _, err := fmt.Fprintf(w, "This removes the authenticator app, every security key and passkey, and every recovery code\n"+
			"of %s user %s (id %s), and signs them out everywhere. Their API tokens keep working.\n", u.Role, u.Email, u.ID); err != nil {
			return err
		}
		if err := a.confirm(w, "Reset their second factors?", "the reset"); err != nil {
			return err
		}
	}
	reset, err := svc.ResetTwoFactor(ctx, u.ID)
	if errors.Is(err, auth.ErrNoSecondFactor) {
		// Not an error: the user is already in the state the command
		// leads to, so a script recovering an account can run it again.
		_, err := fmt.Fprintf(w, "Nothing to reset: %s user %s (id %s) has no second factors.\n", u.Role, u.Email, u.ID)
		return err
	}
	if err != nil {
		return fmt.Errorf("resetting second factors: %w", err)
	}
	// The same event and details as the API's reset (package api), with no
	// actor user or token: nobody logged in made it.
	if err := store.NewAuditEvents(db).Insert(ctx, &store.AuditEvent{
		Action:     string(authz.ResetUserTwoFactor),
		TargetType: string(authz.TargetUser),
		TargetID:   u.ID,
		Result:     store.AuditSuccess,
		Details: map[string]string{
			"actor":                        "cli",
			"totp_removed":                 strconv.FormatBool(reset.TOTP),
			"webauthn_credentials_removed": strconv.Itoa(reset.WebAuthnCredentials),
			"recovery_codes_removed":       strconv.Itoa(reset.RecoveryCodes),
			"sessions_ended":               strconv.Itoa(reset.Sessions),
		},
	}); err != nil {
		return fmt.Errorf("the second factors of %s were reset, but recording it in the audit log failed: %w", u.Email, err)
	}
	app := "no authenticator app"
	if reset.TOTP {
		app = "the authenticator app"
	}
	next := "They can now log in with their password alone."
	if svc.TwoFactorPolicy().Covers(u.Role) {
		next = "They can now log in with their password, and must then set up a new second factor (two_factor.required)."
	}
	_, err = fmt.Fprintf(w, "Reset the second factors of %s user %s (id %s)\nRemoved %s, %d %s, and %d %s, and ended %d %s.\n%s\n",
		u.Role, u.Email, u.ID, app,
		reset.WebAuthnCredentials, plural(reset.WebAuthnCredentials, "security key or passkey", "security keys and passkeys"),
		reset.RecoveryCodes, plural(reset.RecoveryCodes, "recovery code", "recovery codes"),
		reset.Sessions, plural(reset.Sessions, "session", "sessions"), next)
	return err
}

// confirm asks a yes/no question on w and reads the answer from standard
// input. Any answer but yes cancels the action, named by what in errors,
// before it has changed anything.
func (a *app) confirm(w io.Writer, question, what string) error {
	in := bufio.NewReader(a.opts.Stdin)
	for {
		if _, err := fmt.Fprintf(w, "%s [y/N]: ", question); err != nil {
			return err
		}
		line, err := in.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			_, _ = fmt.Fprintln(w)
			return fmt.Errorf("%s needs confirmation: answer the prompt, or pass --yes", what)
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return nil
		case "", "n", "no":
			return fmt.Errorf("%s was canceled; nothing was changed", what)
		}
	}
}

// withAuth opens (and migrates) the state database and runs fn with an
// auth service on it: the same code the API uses.
func (a *app) withAuth(ctx context.Context, fn func(*auth.Service) error) error {
	return a.withAuthDB(ctx, func(_ *store.DB, svc *auth.Service) error { return fn(svc) })
}

// withAuthDB is withAuth for commands that also write to the database
// themselves, such as an audit event.
func (a *app) withAuthDB(ctx context.Context, fn func(*store.DB, *auth.Service) error) error {
	db, err := a.openStateDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	// The user commands check no second factors, so they need no master
	// key: removing a TOTP deletes its sealed secret without opening it.
	return fn(db, auth.NewService(db, a.authOptions(nil)))
}

// newPassword reads a password from stdin or generates one.
func (a *app) newPassword(fromStdin bool) (pw redact.Secret, generated bool, err error) {
	if !fromStdin {
		return generatePassword(), true, nil
	}
	line, err := bufio.NewReader(a.opts.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, fmt.Errorf("reading password from standard input: %w", err)
	}
	pw = redact.Secret(strings.TrimRight(line, "\r\n"))
	if err := auth.CheckPasswordPolicy(pw); err != nil {
		return "", false, err
	}
	return pw, false, nil
}

func printUser(w io.Writer, verb string, u store.User, pw redact.Secret, generated bool) error {
	if _, err := fmt.Fprintf(w, "%s %s user %s (id %s)\n", verb, u.Role, u.Email, u.ID); err != nil {
		return err
	}
	if !generated {
		return nil
	}
	_, err := fmt.Fprintf(w, "Password: %s\nStore it now; it can't be shown again.\n", pw.Reveal())
	return err
}

// authOptions are the auth service's options from the configuration, for
// the API and the in-container user commands alike, so both apply the same
// two-factor policy and WebAuthn relying party. keys seal TOTP secrets;
// without them (nil) the service has no TOTP verifier, which only callers
// that check no second factors can do without.
func (a *app) authOptions(keys *secrets.Keyring) auth.Options {
	tf := a.cfg.TwoFactor
	var factors []auth.FactorVerifier
	if keys != nil {
		factors = append(factors, auth.NewTOTPVerifier(keys, a.cfg.DashboardDomain))
	}
	return auth.Options{
		Logger:  a.log,
		Factors: factors,
		TwoFactor: auth.TwoFactorPolicy{
			Required:               auth.TwoFactorRequirement(tf.Required),
			Reauthentication:       tf.ReauthenticationEnabled(),
			ReauthenticationMaxAge: tf.ReauthenticationMaxAge,
		},
		// From the configured domain, never a request's Host header (§6.2).
		RelyingParty: auth.RelyingPartyFor(a.cfg.DashboardDomain),
	}
}
