package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
)

// Sessions, the current user, and the current user's API tokens (#13).

// Login verifies credentials and starts a session.
func (h *Handlers) Login(context.Context, gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	return nil, notImplemented(13)
}

// Logout ends the current session.
func (h *Handlers) Logout(context.Context, gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	return nil, notImplemented(13)
}

// GetCurrentUser returns the authenticated user.
func (h *Handlers) GetCurrentUser(context.Context, gen.GetCurrentUserRequestObject) (gen.GetCurrentUserResponseObject, error) {
	return nil, notImplemented(13)
}

// ListAPITokens lists the current user's API tokens.
func (h *Handlers) ListAPITokens(context.Context, gen.ListAPITokensRequestObject) (gen.ListAPITokensResponseObject, error) {
	return nil, notImplemented(13)
}

// CreateAPIToken creates an API token and returns its plaintext once.
func (h *Handlers) CreateAPIToken(context.Context, gen.CreateAPITokenRequestObject) (gen.CreateAPITokenResponseObject, error) {
	return nil, notImplemented(13)
}

// RevokeAPIToken revokes one of the current user's API tokens.
func (h *Handlers) RevokeAPIToken(context.Context, gen.RevokeAPITokenRequestObject) (gen.RevokeAPITokenResponseObject, error) {
	return nil, notImplemented(13)
}
