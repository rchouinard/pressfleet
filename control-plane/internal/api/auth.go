package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Sessions, the current user, and the current user's API tokens (#13).

// errAuthNotConfigured is a 500: the server was built without Options.Auth.
var errAuthNotConfigured = errors.New("authentication service is not configured")

// Login verifies credentials and starts a session. It is public, so it
// needs no authorization check.
func (h *Handlers) Login(ctx context.Context, req gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	if h.auth == nil {
		return nil, errAuthNotConfigured
	}
	client := clientFromContext(ctx)
	res, err := h.auth.Login(ctx, auth.LoginRequest{
		Email:     string(req.Body.Email),
		Password:  redact.Secret(deref(req.Body.Password)),
		ClientIP:  client.IP,
		UserAgent: client.UserAgent,
	})
	if errors.Is(err, auth.ErrInvalidCredentials) {
		// Failed logins are audited without an actor (the email may not
		// belong to anyone, and isn't recorded). Throttled attempts cost
		// the attacker nothing, so they aren't recorded, keeping an
		// unauthenticated client from filling the audit log.
		noteLogin(ctx, "")
		return nil, httpserver.NewProblem(http.StatusUnauthorized, "Invalid email or password.")
	} else if err != nil {
		return nil, err
	}
	noteLogin(ctx, res.User.ID)
	cookie := h.sessionCookie(ctx, res.Token.Reveal(), res.Session.ExpiresAt).String()
	csrf := res.CSRFToken.Reveal()
	return gen.Login200JSONResponse{
		Body:    currentUser(res.User, gen.AuthMethodSession, &csrf),
		Headers: gen.Login200ResponseHeaders{SetCookie: &cookie},
	}, nil
}

// Logout ends the current session and clears the cookie.
func (h *Handlers) Logout(ctx context.Context, _ gen.LogoutRequestObject) (gen.LogoutResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.Logout, self(ctx))
	if err != nil {
		return nil, err
	}
	if p.Method == auth.MethodSession {
		if err := h.auth.Logout(ctx, p.SessionID); err != nil {
			return nil, err
		}
	}
	cookie := h.sessionCookie(ctx, "", time.Time{}).String()
	return gen.Logout204Response{Headers: gen.Logout204ResponseHeaders{SetCookie: &cookie}}, nil
}

// GetCurrentUser returns the authenticated user and, for sessions, the CSRF
// token.
func (h *Handlers) GetCurrentUser(ctx context.Context, _ gen.GetCurrentUserRequestObject) (gen.GetCurrentUserResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.ViewSelf, self(ctx))
	if err != nil {
		return nil, err
	}
	u, err := h.auth.GetUser(ctx, p.UserID)
	if errors.Is(err, auth.ErrUserNotFound) {
		return nil, ErrUnauthenticated
	} else if err != nil {
		return nil, err
	}
	method, csrf := gen.AuthMethodToken, (*string)(nil)
	if p.Method == auth.MethodSession {
		method = gen.AuthMethodSession
		if t, ok := csrfFromContext(ctx); ok {
			s := t.Reveal()
			csrf = &s
		}
	}
	return gen.GetCurrentUser200JSONResponse(currentUser(u, method, csrf)), nil
}

// UpdateCurrentUser changes the caller's own display name.
func (h *Handlers) UpdateCurrentUser(ctx context.Context, req gen.UpdateCurrentUserRequestObject) (gen.UpdateCurrentUserResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.UpdateOwnProfile, self(ctx))
	if err != nil {
		return nil, err
	}
	u, err := h.auth.UpdateUser(ctx, p.UserID, auth.UserChanges{DisplayName: &req.Body.DisplayName})
	if errors.Is(err, auth.ErrUserNotFound) {
		return nil, ErrUnauthenticated
	} else if err != nil {
		return nil, authError(err, "")
	}
	return gen.UpdateCurrentUser200JSONResponse(user(u)), nil
}

// ChangePassword changes the caller's own password.
func (h *Handlers) ChangePassword(ctx context.Context, req gen.ChangePasswordRequestObject) (gen.ChangePasswordResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.ChangeOwnPassword, self(ctx))
	if err != nil {
		return nil, err
	}
	err = h.auth.ChangePassword(ctx, p,
		redact.Secret(deref(req.Body.CurrentPassword)), redact.Secret(deref(req.Body.NewPassword)))
	switch {
	case errors.Is(err, auth.ErrWrongPassword):
		return nil, fieldError("/current_password", "is incorrect")
	case err != nil:
		return nil, authError(err, "/new_password")
	}
	return gen.ChangePassword204Response{}, nil
}

// ListAPITokens lists the current user's API tokens.
func (h *Handlers) ListAPITokens(ctx context.Context, req gen.ListAPITokensRequestObject) (gen.ListAPITokensResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.ViewOwnTokens, self(ctx))
	if err != nil {
		return nil, err
	}
	limit := pageLimit(req.Params.Limit)
	tokens, err := h.auth.ListAPITokens(ctx, p.UserID, deref(req.Params.Cursor), limit+1)
	if err != nil {
		return nil, err
	}
	tokens, next := page(tokens, limit, func(t store.APIToken) string { return t.ID })
	body := gen.APITokenList{Items: make([]gen.APIToken, 0, len(tokens)), NextCursor: next}
	for _, t := range tokens {
		body.Items = append(body.Items, apiToken(t))
	}
	return gen.ListAPITokens200JSONResponse(body), nil
}

// CreateAPIToken creates an API token and returns its plaintext once.
func (h *Handlers) CreateAPIToken(ctx context.Context, req gen.CreateAPITokenRequestObject) (gen.CreateAPITokenResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.CreateOwnToken, self(ctx))
	if err != nil {
		return nil, err
	}
	scope := store.TokenScope(req.Body.Scope)
	// A token must not mint one that can do more, or last longer, than
	// itself.
	if err := authz.CheckNewToken(p, scope, req.Body.ExpiresAt); err != nil {
		return nil, deny(ctx, err)
	}
	tok, plain, err := h.auth.CreateAPIToken(ctx, p.UserID, auth.NewAPIToken{
		Name:      req.Body.Name,
		Scope:     scope,
		ExpiresAt: req.Body.ExpiresAt,
	})
	if err != nil {
		return nil, authError(err, "")
	}
	setAuditTarget(ctx, authz.APIToken(tok.ID))
	t := apiToken(tok)
	location := "/api/v1/auth/tokens/" + tok.ID
	return gen.CreateAPIToken201JSONResponse{
		Body: gen.APITokenWithSecret{
			ID: t.ID, Name: t.Name, Scope: t.Scope, Prefix: t.Prefix,
			CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt,
			Token: plain.Reveal(),
		},
		Headers: gen.CreateAPIToken201ResponseHeaders{Location: &location},
	}, nil
}

// RevokeAPIToken revokes one of the current user's API tokens.
func (h *Handlers) RevokeAPIToken(ctx context.Context, req gen.RevokeAPITokenRequestObject) (gen.RevokeAPITokenResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.RevokeOwnToken, authz.APIToken(req.TokenID))
	if err != nil {
		return nil, err
	}
	if err := h.auth.RevokeAPIToken(ctx, p.UserID, req.TokenID); err != nil {
		return nil, authError(err, "")
	}
	return gen.RevokeAPIToken204Response{}, nil
}

// authorizeAuth is authorize for handlers that need the auth service.
func (h *Handlers) authorizeAuth(ctx context.Context, action authz.Action, target authz.Target) (auth.Principal, error) {
	p, err := h.authorize(ctx, action, target)
	if err != nil {
		return auth.Principal{}, err
	}
	if h.auth == nil {
		return auth.Principal{}, errAuthNotConfigured
	}
	return p, nil
}

// authError maps auth errors to API errors. passwordField names the body
// member that holds a new password, for policy violations.
func authError(err error, passwordField string) error {
	switch {
	case errors.Is(err, auth.ErrUserNotFound), errors.Is(err, auth.ErrTokenNotFound), errors.Is(err, auth.ErrSSHKeyNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, auth.ErrEmailTaken), errors.Is(err, auth.ErrLastAdmin):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	case errors.Is(err, auth.ErrUnauthenticated):
		return ErrUnauthenticated
	case errors.Is(err, auth.ErrPasswordPolicy):
		return fieldError(passwordField, err.Error())
	case errors.Is(err, auth.ErrInvalidEmail):
		return fieldError("/email", "must be an email address such as name@example.com")
	case errors.Is(err, auth.ErrInvalidDisplayName):
		return fieldError("/display_name", err.Error())
	case errors.Is(err, auth.ErrInvalidRole):
		return fieldError("/role", err.Error())
	case errors.Is(err, auth.ErrInvalidScope):
		return fieldError("/scope", err.Error())
	case errors.Is(err, auth.ErrInvalidTokenName):
		return fieldError("/name", err.Error())
	case errors.Is(err, auth.ErrInvalidExpiry):
		return fieldError("/expires_at", err.Error())
	}
	return err
}

func fieldError(field, detail string) error {
	return &ValidationError{Fields: []httpserver.FieldError{{Field: field, Detail: detail}}}
}

func currentUser(u store.User, method gen.CurrentUserAuthMethod, csrf *string) gen.CurrentUser {
	g := user(u)
	return gen.CurrentUser{
		ID: g.ID, Email: g.Email, DisplayName: g.DisplayName, Role: g.Role, Disabled: g.Disabled,
		CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt, LastLoginAt: g.LastLoginAt,
		AuthMethod: method, CsrfToken: csrf,
	}
}

func user(u store.User) gen.User {
	return gen.User{
		ID:          u.ID,
		Email:       openapi_types.Email(u.Email),
		DisplayName: u.DisplayName,
		Role:        gen.Role(u.Role),
		Disabled:    u.Disabled(),
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
		LastLoginAt: u.LastLoginAt,
	}
}

func apiToken(t store.APIToken) gen.APIToken {
	return gen.APIToken{
		ID:         t.ID,
		Name:       t.Name,
		Scope:      gen.TokenScope(t.Scope),
		Prefix:     t.Prefix,
		CreatedAt:  t.CreatedAt,
		ExpiresAt:  t.ExpiresAt,
		LastUsedAt: t.LastUsedAt,
	}
}

// defaultPageLimit matches the Limit parameter's default in the spec.
const defaultPageLimit = 50

func pageLimit(l *int) int {
	if l == nil || *l <= 0 {
		return defaultPageLimit
	}
	return *l
}

// page trims items fetched with limit+1 to limit, returning the cursor for
// the next page if there is one.
func page[T any](items []T, limit int, key func(T) string) ([]T, *string) {
	if len(items) <= limit {
		return items, nil
	}
	items = items[:limit]
	next := key(items[len(items)-1])
	return items, &next
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
