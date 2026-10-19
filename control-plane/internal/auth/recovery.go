package auth

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Recovery codes (design doc §6.2): RecoveryCodeCount codes of 80 random
// bits, shown as 16 base32 characters in groups of four. At 80 bits a
// SHA-256 hash can't be brute-forced offline, so they are hashed like
// session tokens rather than like passwords.
const (
	// RecoveryCodeCount is how many codes a set has.
	RecoveryCodeCount = 10
	recoveryCodeBytes = 10
	recoveryCodeChars = 16
	recoveryCodeGroup = 4
)

// recoveryEncoding is uppercase RFC 4648 base32: no 0, 1, or 8, so codes
// copied by hand aren't confused with O, I, or B.
var recoveryEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newRecoveryCode returns a code in its display form, XXXX-XXXX-XXXX-XXXX.
func newRecoveryCode() redact.Secret {
	b := make([]byte, recoveryCodeBytes)
	_, _ = rand.Read(b) // never fails (crypto/rand documentation)
	s := recoveryEncoding.EncodeToString(b)
	groups := make([]string, 0, recoveryCodeChars/recoveryCodeGroup)
	for i := 0; i < len(s); i += recoveryCodeGroup {
		groups = append(groups, s[i:i+recoveryCodeGroup])
	}
	return redact.Secret(strings.Join(groups, "-"))
}

// normalizeRecoveryCode returns the canonical form of a code as typed:
// uppercase, without separators. It reports false if that isn't 16 base32
// characters.
func normalizeRecoveryCode(code redact.Secret) (redact.Secret, bool) {
	s := strings.Map(func(r rune) rune {
		switch r {
		case '-', ' ', '\t':
			return -1
		}
		return r
	}, strings.ToUpper(code.Reveal()))
	if len(s) != recoveryCodeChars {
		return "", false
	}
	if _, err := recoveryEncoding.DecodeString(s); err != nil {
		return "", false
	}
	return redact.Secret(s), true
}

// hashRecoveryCode returns the stored form of a normalized code.
func hashRecoveryCode(normalized redact.Secret) string {
	return hashToken(normalized)
}

// IssueRecoveryCodes replaces the user's recovery codes with a new set and
// returns the codes, which are not stored and must be shown to the user
// once. TOTP (#151) and WebAuthn (#152) enrollment call it in the
// transaction that adds a user's first second factor; regenerating calls
// it too. Run it in a transaction ([store.DB.InTx]), so the old set never
// outlives the new one.
func (s *Service) IssueRecoveryCodes(ctx context.Context, q store.Querier, userID string) ([]redact.Secret, error) {
	codes := make([]redact.Secret, RecoveryCodeCount)
	hashes := make([]string, RecoveryCodeCount)
	for i := range codes {
		codes[i] = newRecoveryCode()
		n, _ := normalizeRecoveryCode(codes[i])
		hashes[i] = hashRecoveryCode(n)
	}
	if _, err := store.NewRecoveryCodes(q).Replace(ctx, userID, hashes, s.now()); err != nil {
		if errors.Is(err, store.ErrReference) {
			return nil, ErrUserNotFound
		}
		return nil, fmt.Errorf("issue recovery codes: %w", err)
	}
	return codes, nil
}

// RegenerateRecoveryCodes replaces the user's recovery codes with a new
// set and returns it, to show once. It returns ErrNoSecondFactor if the
// user has no second factor: recovery codes stand in for one.
func (s *Service) RegenerateRecoveryCodes(ctx context.Context, userID string) ([]redact.Secret, error) {
	var codes []redact.Secret
	err := s.db.InTx(ctx, func(q store.Querier) error {
		has, err := s.hasSecondFactor(ctx, q, userID)
		if err != nil {
			return err
		}
		if !has {
			return ErrNoSecondFactor
		}
		codes, err = s.IssueRecoveryCodes(ctx, q, userID)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.log.Info("recovery codes regenerated", "user_id", userID)
	return codes, nil
}

// RecoveryCodesRemaining returns how many unused recovery codes the user
// has.
func (s *Service) RecoveryCodesRemaining(ctx context.Context, userID string) (int, error) {
	return store.NewRecoveryCodes(s.db).CountUnused(ctx, userID)
}

// recoveryCodeVerifier is the built-in FactorVerifier for recovery codes.
type recoveryCodeVerifier struct {
	s *Service
}

func (v recoveryCodeVerifier) Method() FactorMethod { return FactorRecoveryCode }

func (v recoveryCodeVerifier) Enrolled(ctx context.Context, userID string) (bool, error) {
	n, err := v.s.RecoveryCodesRemaining(ctx, userID)
	return n > 0, err
}

// Verify uses up the code. Use checks and marks it in one statement, so a
// code submitted twice at once works only once.
func (v recoveryCodeVerifier) Verify(ctx context.Context, user store.User, proof FactorProof) error {
	n, ok := normalizeRecoveryCode(proof.Code)
	if !ok {
		return ErrInvalidSecondFactor
	}
	err := store.NewRecoveryCodes(v.s.db).Use(ctx, user.ID, hashRecoveryCode(n), v.s.now())
	if errors.Is(err, store.ErrNotFound) {
		return ErrInvalidSecondFactor
	} else if err != nil {
		return err
	}
	v.s.log.Info("recovery code used", "user_id", user.ID)
	return nil
}
