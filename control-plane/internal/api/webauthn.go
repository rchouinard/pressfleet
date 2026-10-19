package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Security keys and passkeys (WebAuthn) as a second factor (#152, design doc
// §6.2), under /users/me/two-factor/webauthn. The relying party is the
// auth service's, from configuration; nothing here reads the Host header.

// BeginWebAuthnRegistration returns the options for registering a security
// key or passkey.
func (h *Handlers) BeginWebAuthnRegistration(ctx context.Context, _ gen.BeginWebAuthnRegistrationRequestObject) (gen.BeginWebAuthnRegistrationResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.RegisterWebAuthnCredential, self(ctx))
	if err != nil {
		return nil, err
	}
	// The event shares the registration's action; this tells them apart.
	setAuditDetails(ctx, map[string]string{"step": "options"})
	opts, err := h.auth.BeginWebAuthnRegistration(ctx, p.UserID)
	if err != nil {
		return nil, webAuthnError(err)
	}
	var body gen.WebAuthnCreationOptions
	if err := json.Unmarshal(opts, &body); err != nil {
		return nil, fmt.Errorf("decoding WebAuthn options: %w", err)
	}
	return gen.BeginWebAuthnRegistration200JSONResponse(body), nil
}

// RegisterWebAuthnCredential checks a registration response and stores the
// credential, returning the first recovery codes with the first factor.
func (h *Handlers) RegisterWebAuthnCredential(ctx context.Context, req gen.RegisterWebAuthnCredentialRequestObject) (gen.RegisterWebAuthnCredentialResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.RegisterWebAuthnCredential, self(ctx))
	if err != nil {
		return nil, err
	}
	response, err := json.Marshal(req.Body.Credential)
	if err != nil {
		return nil, fmt.Errorf("encoding WebAuthn credential: %w", err)
	}
	reg, err := h.auth.FinishWebAuthnRegistration(ctx, p.UserID, req.Body.Name, response)
	if err != nil {
		return nil, webAuthnError(err)
	}
	setAuditTarget(ctx, authz.WebAuthnCredential(reg.Credential.ID))
	setAuditDetails(ctx, map[string]string{"backup_eligible": fmt.Sprint(reg.Credential.BackupEligible)})
	body := gen.WebAuthnCredentialRegistered{Credential: webAuthnCredential(reg.Credential)}
	if reg.RecoveryCodes != nil {
		codes := make([]string, len(reg.RecoveryCodes))
		for i, c := range reg.RecoveryCodes {
			codes[i] = c.Reveal()
		}
		body.RecoveryCodes = &codes
	}
	location := "/api/v1/users/me/two-factor/webauthn/" + reg.Credential.ID
	return gen.RegisterWebAuthnCredential201JSONResponse{
		Body: body, Headers: gen.RegisterWebAuthnCredential201ResponseHeaders{Location: &location},
	}, nil
}

// ListWebAuthnCredentials lists the current user's security keys and
// passkeys.
func (h *Handlers) ListWebAuthnCredentials(ctx context.Context, _ gen.ListWebAuthnCredentialsRequestObject) (gen.ListWebAuthnCredentialsResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.ViewSelf, self(ctx))
	if err != nil {
		return nil, err
	}
	creds, err := h.auth.ListWebAuthnCredentials(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	body := gen.WebAuthnCredentialList{Items: make([]gen.WebAuthnCredential, len(creds))}
	for i, c := range creds {
		body.Items[i] = webAuthnCredential(c)
	}
	return gen.ListWebAuthnCredentials200JSONResponse(body), nil
}

// GetWebAuthnCredential returns one of the current user's credentials.
func (h *Handlers) GetWebAuthnCredential(ctx context.Context, req gen.GetWebAuthnCredentialRequestObject) (gen.GetWebAuthnCredentialResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.ViewSelf, authz.WebAuthnCredential(req.WebauthnCredentialID))
	if err != nil {
		return nil, err
	}
	c, err := h.auth.GetWebAuthnCredential(ctx, p.UserID, req.WebauthnCredentialID)
	if err != nil {
		return nil, webAuthnError(err)
	}
	return gen.GetWebAuthnCredential200JSONResponse(webAuthnCredential(c)), nil
}

// RenameWebAuthnCredential renames one of the current user's credentials.
func (h *Handlers) RenameWebAuthnCredential(ctx context.Context, req gen.RenameWebAuthnCredentialRequestObject) (gen.RenameWebAuthnCredentialResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.RenameWebAuthnCredential, authz.WebAuthnCredential(req.WebauthnCredentialID))
	if err != nil {
		return nil, err
	}
	c, err := h.auth.RenameWebAuthnCredential(ctx, p.UserID, req.WebauthnCredentialID, req.Body.Name)
	if err != nil {
		return nil, webAuthnError(err)
	}
	return gen.RenameWebAuthnCredential200JSONResponse(webAuthnCredential(c)), nil
}

// RemoveWebAuthnCredential removes one of the current user's credentials,
// signing them out of their other sessions.
func (h *Handlers) RemoveWebAuthnCredential(ctx context.Context, req gen.RemoveWebAuthnCredentialRequestObject) (gen.RemoveWebAuthnCredentialResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.RemoveWebAuthnCredential, authz.WebAuthnCredential(req.WebauthnCredentialID))
	if err != nil {
		return nil, err
	}
	if err := h.auth.RemoveWebAuthnCredential(ctx, p, req.WebauthnCredentialID); err != nil {
		return nil, webAuthnError(err)
	}
	return gen.RemoveWebAuthnCredential204Response{}, nil
}

// webAuthnError maps WebAuthn errors to API errors.
func webAuthnError(err error) error {
	switch {
	case errors.Is(err, auth.ErrWebAuthnCredentialNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, auth.ErrWebAuthnCredentialExists):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	case errors.Is(err, auth.ErrInvalidWebAuthnResponse):
		return fieldError("/credential", "doesn't verify: it is for another site, its challenge is unknown, expired, or used, or the key didn't confirm your presence")
	case errors.Is(err, auth.ErrInvalidWebAuthnCredentialName):
		return fieldError("/name", err.Error())
	case errors.Is(err, auth.ErrFactorUnavailable):
		return httpserver.NewProblem(http.StatusServiceUnavailable, "Security keys and passkeys aren't available: the dashboard domain isn't a valid relying party ID.")
	}
	return authError(err, "")
}

func webAuthnCredential(c store.WebAuthnCredential) gen.WebAuthnCredential {
	transports := c.Transports
	if transports == nil {
		transports = []string{}
	}
	return gen.WebAuthnCredential{
		ID:             c.ID,
		Name:           c.Name,
		RpID:           c.RPID,
		Aaguid:         formatAAGUID(c.AAGUID),
		Transports:     transports,
		UserVerified:   c.UserVerified,
		BackupEligible: c.BackupEligible,
		BackupState:    c.BackupState,
		SignCount:      int64(c.SignCount),
		CreatedAt:      c.CreatedAt,
		LastUsedAt:     c.LastUsedAt,
	}
}

// formatAAGUID formats an AAGUID like a UUID; a missing one is all zeros.
func formatAAGUID(b []byte) string {
	if len(b) != 16 {
		b = make([]byte, 16)
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
