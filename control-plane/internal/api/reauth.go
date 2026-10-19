package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Reauthentication for sensitive actions (#154, design doc §6.2). The
// refusal itself is in authorize; these are how a session gets past it.

// auditReauthMethod is the details key that reauthentication events use
// for the method the user chose.
const auditReauthMethod = "method"

// StartReauthentication lists how the caller can reauthenticate and starts
// the challenges of the methods that need one, such as WebAuthn.
func (h *Handlers) StartReauthentication(ctx context.Context, _ gen.StartReauthenticationRequestObject) (gen.StartReauthenticationResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.StartReauthentication, self(ctx))
	if err != nil {
		return nil, err
	}
	opts, err := h.auth.StartReauthentication(ctx, p)
	if err != nil {
		return nil, authError(err, "")
	}
	body := gen.ReauthenticationOptions{Methods: make([]gen.ReauthenticationMethod, 0, len(opts.Methods))}
	for _, m := range opts.Methods {
		body.Methods = append(body.Methods, gen.ReauthenticationMethod(m))
	}
	// The spec names a member for each method that has a challenge, as the
	// login's second-factor problem does.
	if c, ok := opts.Challenges[auth.FactorWebAuthn]; ok {
		var o gen.WebAuthnRequestOptions
		if err := json.Unmarshal(c, &o); err != nil {
			return nil, fmt.Errorf("start reauthentication: decoding the WebAuthn options: %w", err)
		}
		body.Webauthn = &o
	}
	return gen.StartReauthentication200JSONResponse(body), nil
}

// Reauthenticate checks the caller again, with their second factor or, for
// users without one, their password, and marks the session as just
// verified.
func (h *Handlers) Reauthenticate(ctx context.Context, req gen.ReauthenticateRequestObject) (gen.ReauthenticateResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.Reauthenticate, self(ctx))
	if err != nil {
		return nil, err
	}
	method := auth.FactorMethod(req.Body.Method)
	r := auth.ReauthenticationRequest{
		Proof:    auth.FactorProof{Method: method, Code: redact.Secret(deref(req.Body.Code))},
		Password: redact.Secret(deref(req.Body.Password)),
		ClientIP: clientFromContext(ctx).IP,
	}
	field := "/code"
	switch method {
	case auth.FactorTOTP, auth.FactorRecoveryCode:
		if r.Proof.Code == "" {
			return nil, fieldError(field, "is required for "+string(method))
		}
	case auth.FactorWebAuthn:
		field = "/credential"
		if req.Body.Credential == nil {
			return nil, fieldError(field, "is required for "+string(method))
		}
		cred, err := json.Marshal(*req.Body.Credential)
		if err != nil {
			return nil, fmt.Errorf("reauthenticate: encoding the credential: %w", err)
		}
		r.Proof.Credential = cred
	case auth.ReauthenticatePassword:
		field = "/password"
		if r.Password == "" {
			return nil, fieldError(field, "is required for "+string(method))
		}
	}
	setAuditDetails(ctx, map[string]string{auditReauthMethod: string(method)})
	verified, err := h.auth.Reauthenticate(ctx, p, r)
	switch {
	case errors.Is(err, auth.ErrFactorUnavailable):
		return nil, fieldError("/method", "isn't available for this account")
	case errors.Is(err, auth.ErrInvalidSecondFactor), errors.Is(err, auth.ErrWrongPassword):
		return nil, fieldError(field, "is incorrect")
	case err != nil:
		return nil, authError(err, "")
	}
	u, err := h.auth.GetUser(ctx, p.UserID)
	if err != nil {
		return nil, authError(err, "")
	}
	var csrf *string
	if t, ok := csrfFromContext(ctx); ok {
		s := t.Reveal()
		csrf = &s
	}
	body, err := h.currentUser(ctx, u, gen.AuthMethodSession, csrf, &verified)
	if err != nil {
		return nil, err
	}
	return gen.Reauthenticate200JSONResponse(body), nil
}

// reauthenticationMaxAge is the current user's reauthentication_max_age_seconds:
// how long a session's check stays fresh, or nil when nothing asks (API
// tokens, or reauthentication off).
func (h *Handlers) reauthenticationMaxAge(method gen.CurrentUserAuthMethod) *int {
	policy := h.auth.TwoFactorPolicy()
	if method != gen.AuthMethodSession || !policy.Reauthentication {
		return nil
	}
	s := int(policy.ReauthenticationMaxAge.Seconds())
	return &s
}
