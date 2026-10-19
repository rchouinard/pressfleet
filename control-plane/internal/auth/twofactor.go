package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// FactorMethod names a way to pass the second step of a login.
type FactorMethod string

// Second-factor methods, in the order logins list them.
const (
	FactorTOTP     FactorMethod = "totp"
	FactorWebAuthn FactorMethod = "webauthn"
	// FactorRecoveryCode stands in for a second factor; it isn't one.
	FactorRecoveryCode FactorMethod = "recovery_code"
)

var factorOrder = []FactorMethod{FactorTOTP, FactorWebAuthn, FactorRecoveryCode}

// pendingLoginTTL is how long the second step of a login may take.
const pendingLoginTTL = 5 * time.Minute

// Two-factor sentinel errors.
var (
	// ErrSecondFactorRequired means the password was right but the user
	// has a second factor. Errors that wrap it are *SecondFactorRequiredError.
	ErrSecondFactorRequired = errors.New("a second factor is required")
	// ErrLoginExpired means a pending login is unknown, expired, or was
	// already used: the user must enter their password again.
	ErrLoginExpired = errors.New("the login has expired; log in again")
	// ErrInvalidSecondFactor means a second-factor check failed. Errors
	// that wrap it are *SecondFactorError.
	ErrInvalidSecondFactor = errors.New("the second factor is incorrect")
	// ErrFactorUnavailable means the user can't use the requested method:
	// they haven't enrolled it, or this control plane doesn't support it.
	ErrFactorUnavailable = errors.New("this second-factor method isn't available for the account")
	// ErrNoSecondFactor means the user has no second factor, so the
	// operation (such as regenerating recovery codes) makes no sense.
	ErrNoSecondFactor = errors.New("the account has no second factor")
)

// SecondFactorRequiredError is a pending login: the password was right,
// and the client must now pass a second factor with Token.
type SecondFactorRequiredError struct {
	UserID string
	// Token identifies the pending login. It goes in the pressfleet_login
	// cookie and is never stored; the store keeps only its hash.
	Token     redact.Secret
	ExpiresAt time.Time
	// Methods are the methods the user can use, in factorOrder.
	Methods []FactorMethod
	// Challenges holds what a method needs the client to sign, keyed by
	// method, such as WebAuthn request options (FactorChallenger).
	Challenges map[FactorMethod]json.RawMessage
}

func (e *SecondFactorRequiredError) Error() string { return ErrSecondFactorRequired.Error() }

// Is makes errors.Is(err, ErrSecondFactorRequired) true.
func (e *SecondFactorRequiredError) Is(target error) bool { return target == ErrSecondFactorRequired }

// SecondFactorError is a failed second-factor check. It names the user,
// whose password was right, so the failure can be audited with them as
// the actor.
type SecondFactorError struct {
	UserID string
	Method FactorMethod
}

func (e *SecondFactorError) Error() string {
	return fmt.Sprintf("%v (%s)", ErrInvalidSecondFactor, e.Method)
}

// Is makes errors.Is(err, ErrInvalidSecondFactor) true.
func (e *SecondFactorError) Is(target error) bool { return target == ErrInvalidSecondFactor }

// FactorProof is what the client sends to pass the second step.
type FactorProof struct {
	Method FactorMethod
	// Code is a TOTP or recovery code.
	Code redact.Secret
	// Credential is a WebAuthn assertion (PublicKeyCredential as JSON).
	Credential json.RawMessage
}

// FactorVerifier checks one second-factor method at login. The login step
// dispatches to the verifier registered for the method the client chose
// (Options.Factors); recovery codes are built in. TOTP (#151) and WebAuthn
// (#152) plug in here.
type FactorVerifier interface {
	// Method is the method the verifier checks.
	Method() FactorMethod
	// Enrolled reports whether the user can log in with the method.
	Enrolled(ctx context.Context, userID string) (bool, error)
	// Verify checks proof for user. A wrong proof returns an error
	// wrapping ErrInvalidSecondFactor, which counts against the throttle;
	// any other error is unexpected. A successful check must consume
	// whatever made it single use (a TOTP step, a recovery code, a
	// WebAuthn challenge) before returning.
	Verify(ctx context.Context, user store.User, proof FactorProof) error
}

// FactorChallenger is implemented by verifiers whose method needs a
// server challenge before the client can answer, such as WebAuthn. Login
// calls it when a pending login starts and passes the result to the client.
type FactorChallenger interface {
	Challenge(ctx context.Context, user store.User) (json.RawMessage, error)
}

// pendingLogin is a login waiting for its second step.
type pendingLogin struct {
	userID string
	// email is the normalized email the password was given for, the
	// per-account throttle key.
	email string
}

// SecondFactorRequest is the second step of a login.
type SecondFactorRequest struct {
	// PendingToken is the token from SecondFactorRequiredError.
	PendingToken redact.Secret
	Proof        FactorProof
	ClientIP     string
	UserAgent    string
}

// startPendingLogin begins the second step for a user whose password was
// right and returns it as a *SecondFactorRequiredError.
func (s *Service) startPendingLogin(ctx context.Context, user store.User, email string) error {
	e := &SecondFactorRequiredError{UserID: user.ID, Challenges: map[FactorMethod]json.RawMessage{}}
	for _, m := range factorOrder {
		v, ok := s.factors[m]
		if !ok {
			continue
		}
		enrolled, err := v.Enrolled(ctx, user.ID)
		if err != nil {
			return fmt.Errorf("login: %w", err)
		}
		if !enrolled {
			continue
		}
		e.Methods = append(e.Methods, m)
		if c, ok := v.(FactorChallenger); ok {
			opts, err := c.Challenge(ctx, user)
			if err != nil {
				return fmt.Errorf("login: %s challenge: %w", m, err)
			}
			e.Challenges[m] = opts
		}
	}
	e.Token = newSessionToken()
	e.ExpiresAt = s.now().Add(pendingLoginTTL)
	s.pending.put(hashToken(e.Token), pendingLogin{userID: user.ID, email: email}, e.ExpiresAt)
	s.log.Info("login waiting for second factor", "user_id", user.ID)
	return e
}

// LoginSecondFactor finishes a pending login by checking a second factor,
// and starts the session. It returns an error wrapping ErrLoginExpired if
// the pending login is gone, ErrFactorUnavailable if the user can't use
// the method, a *SecondFactorError for a wrong proof, or a *ThrottledError
// after too many recent failures. The per-account and per-address
// counters are those of password logins, so a known password allows no
// more guesses at the second factor than at the password. The pending
// login survives a wrong proof until it expires, so a mistyped code
// doesn't mean entering the password again; a successful login uses it up.
func (s *Service) LoginSecondFactor(ctx context.Context, req SecondFactorRequest) (LoginResult, error) {
	key := hashToken(req.PendingToken)
	pl, expires, ok := s.pending.take(key)
	if !ok {
		return LoginResult{}, ErrLoginExpired
	}
	// Until the login succeeds, the pending login goes back for another
	// try; taking it first keeps two concurrent attempts from both
	// succeeding.
	done := false
	defer func() {
		if !done {
			s.pending.put(key, pl, expires)
		}
	}()

	keys := []throttleKey{accountKey(pl.email), addressKey(req.ClientIP)}
	if err := s.throttle.check(keys...); err != nil {
		return LoginResult{}, err
	}
	user, err := s.activeUser(ctx, pl.userID)
	if errors.Is(err, ErrUnauthenticated) {
		// Deleted or disabled since the password step.
		done = true
		return LoginResult{}, ErrLoginExpired
	} else if err != nil {
		return LoginResult{}, fmt.Errorf("login: %w", err)
	}
	v, ok := s.factors[req.Proof.Method]
	if !ok {
		return LoginResult{}, ErrFactorUnavailable
	}
	if enrolled, err := v.Enrolled(ctx, pl.userID); err != nil {
		return LoginResult{}, fmt.Errorf("login: %w", err)
	} else if !enrolled {
		return LoginResult{}, ErrFactorUnavailable
	}
	if err := v.Verify(ctx, user, req.Proof); errors.Is(err, ErrInvalidSecondFactor) {
		s.throttle.fail(keys...)
		s.log.Info("second factor check failed", "user_id", user.ID, "method", string(req.Proof.Method))
		return LoginResult{}, &SecondFactorError{UserID: user.ID, Method: req.Proof.Method}
	} else if err != nil {
		return LoginResult{}, fmt.Errorf("login: checking %s: %w", req.Proof.Method, err)
	}
	done = true
	s.throttle.reset(accountKey(pl.email))
	return s.finishLogin(ctx, user, req.ClientIP, req.UserAgent, req.Proof.Method)
}

// TwoFactorEnabled reports whether the user has a second factor.
func (s *Service) TwoFactorEnabled(ctx context.Context, userID string) (bool, error) {
	return store.NewUsers(s.db).HasSecondFactor(ctx, userID)
}

// UsersWithTwoFactor returns the IDs of every user with a second factor,
// for user lists.
func (s *Service) UsersWithTwoFactor(ctx context.Context) (map[string]bool, error) {
	return store.NewUsers(s.db).IDsWithSecondFactor(ctx)
}
