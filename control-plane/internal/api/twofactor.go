package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
)

// GetTwoFactorStatus returns the current user's second factors, so the
// settings page learns them from one read without starting a WebAuthn
// challenge (design doc §6.2). It is ViewSelf: a session that must enroll
// needs it to show the setup page, and it holds no secrets, so tokens may
// read it as they read the credential list.
func (h *Handlers) GetTwoFactorStatus(ctx context.Context, _ gen.GetTwoFactorStatusRequestObject) (gen.GetTwoFactorStatusResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.ViewSelf, self(ctx))
	if err != nil {
		return nil, err
	}
	st, err := h.auth.TwoFactorStatus(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	body := gen.TwoFactorStatus{
		Enabled: st.Enabled,
		Totp: gen.TotpStatus{
			Confirmed:        st.TOTPConfirmedAt != nil,
			ConfirmedAt:      st.TOTPConfirmedAt,
			PendingExpiresAt: st.TOTPPendingExpiresAt,
		},
		RecoveryCodesRemaining: st.RecoveryCodesRemaining,
	}
	if st.RPID != "" {
		body.RpID = &st.RPID
	}
	body.Webauthn.Credentials = make([]gen.WebAuthnCredential, len(st.WebAuthnCredentials))
	for i, c := range st.WebAuthnCredentials {
		body.Webauthn.Credentials[i] = webAuthnCredential(c)
	}
	body.Webauthn.Usable = st.UsableWebAuthnCredentials
	return gen.GetTwoFactorStatus200JSONResponse(body), nil
}
