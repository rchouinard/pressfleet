package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
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
	cmd.AddCommand(a.userCreateCommand(), a.userResetPasswordCommand())
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

// withAuth opens (and migrates) the state database and runs fn with an
// auth service on it: the same code the API uses.
func (a *app) withAuth(ctx context.Context, fn func(*auth.Service) error) error {
	db, err := a.openStateDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	// The user commands check no second factors, so they need no master
	// key.
	return fn(auth.NewService(db, a.authOptions(nil)))
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
// two-factor policy. keys seal TOTP secrets; without them (nil) the
// service has no TOTP verifier, which only callers that check no second
// factors can do without.
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
	}
}
