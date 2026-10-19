package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// The current user's authenticator app (TOTP, design doc §6.2, #151).

// auditTOTPStage is the details key that tells the two steps of TOTP
// enrollment apart in the audit log; both are self.two_factor.totp.enroll.
const auditTOTPStage = "stage"

// StartTotpEnrollment creates an unconfirmed TOTP secret for the current
// user and returns it, once.
func (h *Handlers) StartTotpEnrollment(ctx context.Context, _ gen.StartTotpEnrollmentRequestObject) (gen.StartTotpEnrollmentResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.EnrollTOTP, self(ctx))
	if err != nil {
		return nil, err
	}
	setAuditDetails(ctx, map[string]string{auditTOTPStage: "start"})
	e, err := h.auth.StartTOTPEnrollment(ctx, p.UserID)
	if err != nil {
		return nil, totpError(err)
	}
	return gen.StartTotpEnrollment200JSONResponse{
		Body:    gen.TotpEnrollment{Secret: e.Secret.Reveal(), URI: e.URI.Reveal(), ExpiresAt: e.ExpiresAt},
		Headers: gen.StartTotpEnrollment200ResponseHeaders{CacheControl: new("no-store")},
	}, nil
}

// ConfirmTotpEnrollment activates the current user's TOTP with a code,
// returning their recovery codes if it is their first second factor.
func (h *Handlers) ConfirmTotpEnrollment(ctx context.Context, req gen.ConfirmTotpEnrollmentRequestObject) (gen.ConfirmTotpEnrollmentResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.EnrollTOTP, self(ctx))
	if err != nil {
		return nil, err
	}
	setAuditDetails(ctx, map[string]string{auditTOTPStage: "confirm"})
	codes, err := h.auth.ConfirmTOTP(ctx, p.UserID, redact.Secret(deref(req.Body.Code)))
	if err != nil {
		return nil, totpError(err)
	}
	var body gen.TotpConfirmed
	if codes != nil {
		setAuditDetails(ctx, map[string]string{"recovery_codes_issued": "true"})
		plain := make([]string, len(codes))
		for i, c := range codes {
			plain[i] = c.Reveal()
		}
		body.RecoveryCodes = &plain
	}
	return gen.ConfirmTotpEnrollment200JSONResponse{
		Body:    body,
		Headers: gen.ConfirmTotpEnrollment200ResponseHeaders{CacheControl: new("no-store")},
	}, nil
}

// RemoveTotp removes the current user's TOTP and signs out their other
// sessions.
func (h *Handlers) RemoveTotp(ctx context.Context, _ gen.RemoveTotpRequestObject) (gen.RemoveTotpResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.RemoveTOTP, self(ctx))
	if err != nil {
		return nil, err
	}
	if err := h.auth.RemoveTOTP(ctx, p); err != nil {
		return nil, totpError(err)
	}
	return gen.RemoveTotp204Response{}, nil
}

// totpError maps the TOTP errors of auth to API errors, and the rest
// through authError.
func totpError(err error) error {
	switch {
	case errors.Is(err, auth.ErrInvalidSecondFactor):
		return fieldError("/code", "is incorrect")
	case errors.Is(err, auth.ErrNoTOTPEnrollment), errors.Is(err, auth.ErrNoTOTP):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, auth.ErrTOTPEnrolled), errors.Is(err, auth.ErrLastSecondFactor),
		errors.Is(err, auth.ErrFactorUnavailable):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return authError(err, "")
}
