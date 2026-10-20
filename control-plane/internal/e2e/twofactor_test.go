package e2e_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/client"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth/authtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/e2e"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// TestTwoFactor (#160) checks two-factor authentication against a running
// control plane, where the API tests can't: that serve wires the master key
// into the TOTP verifier and dashboard_domain into the WebAuthn relying
// party, and that `pressfleet user reset-two-factor` works inside the
// control plane container. The flows themselves, their failures, and the
// policies are covered on both dialects by the api and auth packages.
func TestTwoFactor(t *testing.T) {
	p := e2e.Start(t)
	ctx := t.Context()

	const email = "mfa-admin@e2e.test"
	password := "e2e-password-" + strings.Repeat("m", 12)
	created, err := p.Client.CreateUserWithResponse(ctx, client.CreateUserJSONRequestBody{
		Email: client.Email(email), Password: &password, Role: client.Admin,
	})
	if err != nil || created.JSON201 == nil {
		t.Fatalf("creating %s: %v, %s", email, err, created.Body)
	}
	userID := created.JSON201.ID
	// Login's client reauthenticates with the password, which stops working
	// once the user has a factor; the test takes no sensitive action after
	// enrolling.
	c := p.Login(t, email, redact.Secret(password))

	// Enroll an authenticator app and a security key for the dashboard
	// domain.
	enrollment, err := c.StartTotpEnrollmentWithResponse(ctx)
	if err != nil || enrollment.JSON200 == nil {
		t.Fatalf("starting TOTP enrollment: %v, %s", err, enrollment.Body)
	}
	secret := enrollment.JSON200.Secret
	code := authtest.TOTPCode(secret, time.Now())
	confirmed, err := c.ConfirmTotpEnrollmentWithResponse(ctx, client.ConfirmTotpEnrollmentJSONRequestBody{Code: &code})
	if err != nil || confirmed.JSON200 == nil || confirmed.JSON200.RecoveryCodes == nil || len(*confirmed.JSON200.RecoveryCodes) != 10 {
		t.Fatalf("confirming TOTP: %v, %s", err, confirmed.Body)
	}
	key := authtest.NewAuthenticator("https://" + e2e.DashboardDomain)
	options, err := c.BeginWebAuthnRegistrationWithResponse(ctx)
	if err != nil || options.StatusCode() != http.StatusOK {
		t.Fatalf("registration options: %v, %s", err, options.Body)
	}
	attestation, err := key.Register(options.Body)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := c.RegisterWebAuthnCredentialWithBodyWithResponse(ctx, "application/json",
		bytes.NewReader([]byte(`{"name":"e2e key","credential":`+string(attestation)+`}`)))
	if err != nil || registered.JSON201 == nil {
		t.Fatalf("registering the key: %v, %s", err, registered.Body)
	}
	if rp := registered.JSON201.Credential.RpID; rp != e2e.DashboardDomain {
		t.Errorf("credential rp_id = %q, want %q", rp, e2e.DashboardDomain)
	}

	// login enters the password, finishes the second step with what
	// prove returns for the password step's problem, and returns the
	// second step's status.
	login := func(prove func(problem []byte) string) int {
		t.Helper()
		lc := p.NewClient(t)
		resp, err := lc.LoginWithResponse(ctx, client.LoginJSONRequestBody{Email: client.Email(email), Password: &password})
		if err != nil {
			t.Fatal(err)
		}
		want := []client.SecondFactorMethod{client.SecondFactorMethodTotp, client.SecondFactorMethodWebauthn, client.SecondFactorMethodRecoveryCode}
		if problem := resp.ApplicationProblemJSON401; problem == nil || problem.Methods == nil || !slices.Equal(*problem.Methods, want) {
			t.Fatalf("password step: status %d: %s", resp.StatusCode(), resp.Body)
		}
		// The pending-login cookie is in lc's jar.
		second, err := lc.LoginSecondFactorWithBodyWithResponse(ctx, &client.LoginSecondFactorParams{}, "application/json",
			strings.NewReader(prove(resp.Body)))
		if err != nil {
			t.Fatal(err)
		}
		if second.StatusCode() == http.StatusOK {
			me, err := lc.GetCurrentUserWithResponse(ctx)
			if err != nil || me.JSON200 == nil || me.JSON200.ID != userID {
				t.Errorf("me after the second step: %v, %s", err, me.Body)
			}
		}
		return second.StatusCode()
	}

	t.Run("login with TOTP", func(t *testing.T) {
		// The next step's code: the step of the confirmation was used, and
		// one step of skew is allowed.
		status := login(func([]byte) string {
			return `{"method":"totp","code":"` + authtest.TOTPCode(secret, time.Now().Add(30*time.Second)) + `"}`
		})
		if status != http.StatusOK {
			t.Errorf("second step with TOTP: %d", status)
		}
	})

	t.Run("login with a security key", func(t *testing.T) {
		status := login(func(problem []byte) string {
			var body struct {
				WebAuthn json.RawMessage `json:"webauthn"`
			}
			if err := json.Unmarshal(problem, &body); err != nil {
				t.Fatal(err)
			}
			assertion, err := key.Assert(body.WebAuthn)
			if err != nil {
				t.Fatal(err)
			}
			return `{"method":"webauthn","credential":` + string(assertion) + `}`
		})
		if status != http.StatusOK {
			t.Errorf("second step with WebAuthn: %d", status)
		}
	})

	t.Run("CLI reset", func(t *testing.T) {
		out, err := p.Project.Exec(ctx, e2e.Service, "pressfleet", "user", "reset-two-factor", "--email", email, "--yes")
		if err != nil {
			t.Fatalf("reset-two-factor: %v: %s", err, out)
		}
		if !strings.Contains(string(out), "Removed the authenticator app, 1 security key or passkey, and 10 recovery codes") {
			t.Errorf("reset-two-factor output: %s", out)
		}
		// The password alone logs in again.
		p.Login(t, email, redact.Secret(password))
	})

	t.Run("audit log", func(t *testing.T) {
		resp, err := p.Client.ListAuditEventsWithResponse(ctx, &client.ListAuditEventsParams{TargetID: &userID})
		if err != nil || resp.JSON200 == nil {
			t.Fatalf("audit log: %v, %s", err, resp.Body)
		}
		var reset, logins []string
		for _, ev := range resp.JSON200.Items {
			var details map[string]string
			if ev.Details != nil {
				details = *ev.Details
			}
			switch ev.Action {
			case "user.two_factor.reset":
				if ev.ActorUserID == nil && ev.Result == client.Success {
					reset = append(reset, details["actor"])
				}
			case "self.login":
				if ev.Result == client.Success {
					logins = append(logins, details["second_factor"])
				}
			}
		}
		// Newest first: the password alone after the reset, then the
		// security key and TOTP, then the login before enrolling.
		if want := []string{"", "webauthn", "totp", ""}; !slices.Equal(logins, want) {
			t.Errorf("successful logins' second factors = %q, want %q", logins, want)
		}
		if !slices.Equal(reset, []string{"cli"}) {
			t.Errorf("CLI resets in the audit log: %q, want one by the CLI", reset)
		}
		enrolled := map[string]bool{}
		all, err := p.Client.ListAuditEventsWithResponse(ctx, &client.ListAuditEventsParams{ActorUserID: &userID})
		if err != nil || all.JSON200 == nil {
			t.Fatalf("audit log: %v, %s", err, all.Body)
		}
		for _, ev := range all.JSON200.Items {
			if strings.HasPrefix(ev.Action, "self.two_factor.") && ev.Result == client.Success {
				enrolled[ev.Action] = true
			}
		}
		for _, action := range []string{"self.two_factor.totp.enroll", "self.two_factor.webauthn.register"} {
			if !enrolled[action] {
				t.Errorf("no successful %s event", action)
			}
		}
	})
}
