package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // G505: RFC 6238 HMAC-SHA1 is what authenticator apps implement
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// TOTP (RFC 6238, design doc §6.2) with the defaults every authenticator
// app supports: HMAC-SHA1, 6 digits, 30-second steps, and a 160-bit
// secret. One step of clock skew either way is accepted.
const (
	totpDigits      = 6
	totpPeriod      = 30 // seconds
	totpSkew        = 1  // steps
	totpSecretBytes = 20
	totpIssuer      = "PressFleet"

	// TOTPEnrollmentTTL is how long an unconfirmed TOTP waits for its
	// confirming code before it is discarded.
	TOTPEnrollmentTTL = 15 * time.Minute
)

// TOTP sentinel errors.
var (
	// ErrTOTPEnrolled means the user already has a confirmed TOTP; there
	// is one per user, so replacing it means removing it first.
	ErrTOTPEnrolled = errors.New("the account already has an authenticator app")
	// ErrNoTOTPEnrollment means there is no unconfirmed TOTP to confirm:
	// none was started, or it expired.
	ErrNoTOTPEnrollment = errors.New("no authenticator app enrollment is pending; start again")
	// ErrNoTOTP means the user has no TOTP to remove.
	ErrNoTOTP = errors.New("the account has no authenticator app")
)

// totpEncoding is how secrets are shown and stored: unpadded uppercase
// base32, as otpauth:// URIs carry them.
var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// hotp is RFC 4226 HOTP: HMAC-SHA1 of the counter, dynamically truncated
// to digits decimal digits.
func hotp(key []byte, counter uint64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	s := strconv.FormatUint(uint64(bin%mod), 10)
	return strings.Repeat("0", digits-len(s)) + s
}

// totpStep is the RFC 6238 time step of t.
func totpStep(t time.Time) int64 { return t.Unix() / totpPeriod }

// normalizeTOTPCode strips the spaces apps and users put in codes, and
// reports false unless what is left is totpDigits digits.
func normalizeTOTPCode(code redact.Secret) (string, bool) {
	s := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '-' {
			return -1
		}
		return r
	}, code.Reveal())
	if len(s) != totpDigits {
		return "", false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return s, true
}

// matchTOTP returns the time step, within the skew of now and after the
// step after, whose code is code. It reports false if there is none, which
// includes a code of step after or earlier: that is a replay. Every
// candidate is compared in constant time, so timing doesn't reveal which
// step, if any, matched.
func matchTOTP(key []byte, code redact.Secret, now time.Time, after int64) (int64, bool) {
	c, ok := normalizeTOTPCode(code)
	if !ok {
		return 0, false
	}
	cur := totpStep(now)
	var (
		step  int64
		found bool
	)
	for s := cur - totpSkew; s <= cur+totpSkew; s++ {
		eq := subtle.ConstantTimeCompare([]byte(hotp(key, uint64(s), totpDigits)), []byte(c)) == 1 //nolint:gosec // G115: steps are positive
		if eq && s > after {
			step, found = s, true
		}
	}
	return step, found
}

// TOTPVerifier is the FactorVerifier for authenticator apps. Register it
// in Options.Factors; the Service it is registered with also uses it for
// enrollment, so a Service without one can't enroll TOTPs.
type TOTPVerifier struct {
	keys   *secrets.Keyring
	domain string
	s      *Service
}

// NewTOTPVerifier returns a TOTPVerifier that seals secrets with keys.
// dashboardDomain names the installation in authenticator apps, so users
// of several can tell them apart; it may be empty. Register it with one
// Service only.
func NewTOTPVerifier(keys *secrets.Keyring, dashboardDomain string) *TOTPVerifier {
	return &TOTPVerifier{keys: keys, domain: dashboardDomain}
}

// bind gives the verifier the service's database and clock (NewService).
func (v *TOTPVerifier) bind(s *Service) { v.s = s }

// Method implements FactorVerifier.
func (v *TOTPVerifier) Method() FactorMethod { return FactorTOTP }

// Enrolled implements FactorVerifier. An unconfirmed TOTP doesn't count.
func (v *TOTPVerifier) Enrolled(ctx context.Context, userID string) (bool, error) {
	t, err := store.NewUserTOTPs(v.s.db).Get(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return t.Confirmed(), nil
}

// Verify implements FactorVerifier. It records the accepted step, so the
// code, and every code of an earlier step, can't be used again; UseStep
// checks and records in one statement, so of two requests with the same
// code only one passes.
func (v *TOTPVerifier) Verify(ctx context.Context, user store.User, proof FactorProof) error {
	t, err := store.NewUserTOTPs(v.s.db).Get(ctx, user.ID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrInvalidSecondFactor
	} else if err != nil {
		return err
	}
	if !t.Confirmed() {
		return ErrInvalidSecondFactor
	}
	key, err := v.key(ctx, v.s.db, t)
	if err != nil {
		return err
	}
	step, ok := matchTOTP(key, proof.Code, v.s.now(), t.LastUsedStep)
	if !ok {
		return ErrInvalidSecondFactor
	}
	used, err := store.NewUserTOTPs(v.s.db).UseStep(ctx, user.ID, step)
	if err != nil {
		return err
	}
	if !used {
		return ErrInvalidSecondFactor
	}
	return nil
}

// key decrypts the TOTP's secret.
func (v *TOTPVerifier) key(ctx context.Context, q store.Querier, t store.TOTP) ([]byte, error) {
	secret, err := secrets.NewStore(q, v.keys).Get(ctx, secrets.Ref(t.SecretID))
	if err != nil {
		return nil, fmt.Errorf("TOTP secret of user %s: %w", t.UserID, err)
	}
	key, err := totpEncoding.DecodeString(secret.Reveal())
	if err != nil {
		// Never include the secret, even malformed.
		return nil, fmt.Errorf("TOTP secret of user %s isn't base32", t.UserID)
	}
	return key, nil
}

// uri is the otpauth:// URI authenticator apps scan: issuer PressFleet,
// and an account name of the user's email and the dashboard domain.
func (v *TOTPVerifier) uri(email string, secret redact.Secret) redact.Secret {
	account := email
	if v.domain != "" {
		account += " (" + v.domain + ")"
	}
	q := url.Values{}
	q.Set("secret", secret.Reveal())
	q.Set("issuer", totpIssuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(totpDigits))
	q.Set("period", strconv.Itoa(totpPeriod))
	u := url.URL{Scheme: "otpauth", Host: "totp", Path: "/" + totpIssuer + ":" + account, RawQuery: q.Encode()}
	return redact.Secret(u.String())
}

// totp returns the registered TOTP verifier, or ErrFactorUnavailable.
func (s *Service) totp() (*TOTPVerifier, error) {
	v, ok := s.factors[FactorTOTP].(*TOTPVerifier)
	if !ok {
		return nil, ErrFactorUnavailable
	}
	return v, nil
}

// TOTPEnrollment is a TOTP waiting for its confirming code.
type TOTPEnrollment struct {
	// Secret is the base32 secret, for typing into an app by hand.
	Secret redact.Secret
	// URI is the otpauth:// URI, for a QR code. It contains Secret.
	URI       redact.Secret
	ExpiresAt time.Time
}

// StartTOTPEnrollment creates an unconfirmed TOTP for the user, replacing
// an unconfirmed one, and returns its secret to show once. The secret is
// sealed with the master key: checking codes needs it, so it can't be
// hashed. It returns ErrTOTPEnrolled if the user already has a confirmed
// TOTP. An unconfirmed TOTP isn't a factor, and expires after
// TOTPEnrollmentTTL.
func (s *Service) StartTOTPEnrollment(ctx context.Context, userID string) (TOTPEnrollment, error) {
	v, err := s.totp()
	if err != nil {
		return TOTPEnrollment{}, err
	}
	user, err := s.activeUser(ctx, userID)
	if err != nil {
		return TOTPEnrollment{}, err
	}
	raw := make([]byte, totpSecretBytes)
	_, _ = rand.Read(raw) // never fails (crypto/rand documentation)
	secret := redact.Secret(totpEncoding.EncodeToString(raw))
	now := s.now()
	err = s.db.InTx(ctx, func(q store.Querier) error {
		totps := store.NewUserTOTPs(q)
		// Expired enrollments are cleaned up here, where new ones begin,
		// so they need no background job.
		if _, err := totps.DeleteUnconfirmedBefore(ctx, now.Add(-TOTPEnrollmentTTL)); err != nil {
			return err
		}
		existing, err := totps.Get(ctx, user.ID)
		switch {
		case err == nil && existing.Confirmed():
			return ErrTOTPEnrolled
		case err == nil:
			// Starting again, say after losing the QR code.
			if err := totps.Delete(ctx, user.ID); err != nil {
				return err
			}
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		ref, err := secrets.NewStore(q, v.keys).Put(ctx, secrets.KindTOTPSecret, secret)
		if err != nil {
			return err
		}
		return totps.Create(ctx, &store.TOTP{UserID: user.ID, SecretID: string(ref), CreatedAt: now})
	})
	switch {
	case errors.Is(err, ErrTOTPEnrolled), errors.Is(err, store.ErrConflict):
		// A conflict is a concurrent enrollment that got there first.
		return TOTPEnrollment{}, ErrTOTPEnrolled
	case errors.Is(err, store.ErrReference):
		return TOTPEnrollment{}, ErrUserNotFound
	case err != nil:
		return TOTPEnrollment{}, fmt.Errorf("start TOTP enrollment: %w", err)
	}
	s.log.Info("TOTP enrollment started", "user_id", user.ID)
	return TOTPEnrollment{Secret: secret, URI: v.uri(user.Email, secret), ExpiresAt: now.Add(TOTPEnrollmentTTL)}, nil
}

// ConfirmTOTP activates the user's unconfirmed TOTP with a code from it.
// If it is the user's first second factor, it also issues their recovery
// codes and returns them, to show once; otherwise the codes are nil. It
// returns ErrNoTOTPEnrollment if there is no unexpired enrollment,
// ErrTOTPEnrolled if the TOTP is already confirmed, and an error wrapping
// ErrInvalidSecondFactor for a wrong code. The code's step counts as used,
// so it can't log in afterwards.
func (s *Service) ConfirmTOTP(ctx context.Context, userID string, code redact.Secret) ([]redact.Secret, error) {
	v, err := s.totp()
	if err != nil {
		return nil, err
	}
	now := s.now()
	var codes []redact.Secret
	err = s.db.InTx(ctx, func(q store.Querier) error {
		totps := store.NewUserTOTPs(q)
		t, err := totps.Get(ctx, userID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return ErrNoTOTPEnrollment
		case err != nil:
			return err
		case t.Confirmed():
			return ErrTOTPEnrolled
		case !now.Before(t.CreatedAt.Add(TOTPEnrollmentTTL)):
			return ErrNoTOTPEnrollment
		}
		key, err := v.key(ctx, q, t)
		if err != nil {
			return err
		}
		step, ok := matchTOTP(key, code, now, t.LastUsedStep)
		if !ok {
			return &SecondFactorError{UserID: userID, Method: FactorTOTP}
		}
		hadFactor, err := s.hasSecondFactor(ctx, q, userID)
		if err != nil {
			return err
		}
		if err := totps.Confirm(ctx, userID, step, now); errors.Is(err, store.ErrNotFound) {
			return ErrTOTPEnrolled // confirmed concurrently
		} else if err != nil {
			return err
		}
		if !hadFactor {
			codes, err = s.IssueRecoveryCodes(ctx, q, userID)
		}
		return err
	})
	switch {
	case errors.Is(err, ErrNoTOTPEnrollment), errors.Is(err, ErrTOTPEnrolled),
		errors.Is(err, ErrInvalidSecondFactor), errors.Is(err, ErrUserNotFound):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("confirm TOTP: %w", err)
	}
	s.log.Info("TOTP enrolled", "user_id", userID, "recovery_codes_issued", codes != nil)
	return codes, nil
}

// RemoveTOTP removes the caller's TOTP. Removing a confirmed one signs the
// user out of their other sessions, as a password change does; API tokens
// are kept. It returns ErrNoTOTP if there is none, and ErrLastSecondFactor
// if it is the user's last factor and the policy requires one. Removing an
// unconfirmed TOTP only cancels the enrollment.
func (s *Service) RemoveTOTP(ctx context.Context, p Principal) error {
	user, err := s.activeUser(ctx, p.UserID)
	if err != nil {
		return err
	}
	err = s.db.InTx(ctx, func(q store.Querier) error {
		totps := store.NewUserTOTPs(q)
		t, err := totps.Get(ctx, user.ID)
		if err != nil {
			return err
		}
		if err := totps.Delete(ctx, user.ID); err != nil {
			return err
		}
		if !t.Confirmed() {
			return nil
		}
		if err := s.ensureSecondFactorKept(ctx, q, user); err != nil {
			return err
		}
		_, err = store.NewSessions(q).DeleteForUser(ctx, user.ID, p.SessionID)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrNoTOTP
	case errors.Is(err, ErrLastSecondFactor):
		return ErrLastSecondFactor
	case err != nil:
		return fmt.Errorf("remove TOTP: %w", err)
	}
	s.log.Info("TOTP removed", "user_id", user.ID)
	return nil
}
