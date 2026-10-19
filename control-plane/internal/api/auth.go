package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
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

// SecondFactorRequiredProblemType is the problem type of the 401 that
// asks for the second step of a login (design doc §6.2, where it is called
// second-factor-required).
const SecondFactorRequiredProblemType = "https://github.com/rchouinard/pressfleet/blob/main/docs/configuration.md#logging-in-with-a-second-factor"

// auditSecondFactor is the details key that login audit events use for
// the second step: "required" when the password was right and a factor is
// needed, otherwise the factor method checked.
const auditSecondFactor = "second_factor"

// Login verifies credentials and starts a session. It is public, so it
// needs no authorization check.
func (h *Handlers) Login(ctx context.Context, req gen.LoginRequestObject) (gen.LoginResponseObject, error) {
	client := clientFromContext(ctx)
	res, err := h.auth.Login(ctx, auth.LoginRequest{
		Email:     string(req.Body.Email),
		Password:  redact.Secret(deref(req.Body.Password)),
		ClientIP:  client.IP,
		UserAgent: client.UserAgent,
	})
	var pending *auth.SecondFactorRequiredError
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		// Failed logins are audited without an actor (the email may not
		// belong to anyone, and isn't recorded). Throttled attempts cost
		// the attacker nothing, so they aren't recorded, keeping an
		// unauthenticated client from filling the audit log.
		noteLogin(ctx, "")
		return nil, httpserver.NewProblem(http.StatusUnauthorized, "Invalid email or password.")
	case errors.As(err, &pending):
		// The password was right, so the user is the actor: an event the
		// user didn't cause means someone else knows their password.
		noteLogin(ctx, pending.UserID)
		setAuditDetails(ctx, map[string]string{auditSecondFactor: "required"})
		return nil, h.secondFactorRequired(ctx, pending)
	case err != nil:
		return nil, err
	}
	noteLogin(ctx, res.User.ID)
	return h.loginResponse(ctx, res)
}

// secondFactorRequired returns the 401 that starts the second step of a
// login, with the pending-login cookie.
func (h *Handlers) secondFactorRequired(ctx context.Context, pending *auth.SecondFactorRequiredError) error {
	methods := make([]string, 0, len(pending.Methods))
	for _, m := range pending.Methods {
		methods = append(methods, string(m))
	}
	p := &httpserver.Problem{
		Type:       SecondFactorRequiredProblemType,
		Title:      "Second factor required",
		Status:     http.StatusUnauthorized,
		Detail:     "Pass a second factor to finish logging in.",
		Extensions: map[string]any{"methods": methods},
	}
	// Methods that need a challenge (WebAuthn request options, #152) get
	// a member named after the method.
	for m, c := range pending.Challenges {
		p.Extensions[string(m)] = c
	}
	return &cookieError{err: p, cookie: h.loginCookie(ctx, pending.Token.Reveal(), pending.ExpiresAt)}
}

// LoginSecondFactor checks the second factor of a pending login and starts
// the session. It is public: the pressfleet_login cookie, set by Login
// after a right password, identifies the login.
func (h *Handlers) LoginSecondFactor(ctx context.Context, req gen.LoginSecondFactorRequestObject) (gen.LoginSecondFactorResponseObject, error) {
	expired := httpserver.NewProblem(http.StatusUnauthorized, "The login has expired or was already used. Log in again.")
	token := deref(req.Params.PressfleetLogin)
	if token == "" {
		return nil, expired
	}
	method := auth.FactorMethod(req.Body.Method)
	codeField := "/code"
	switch method {
	case auth.FactorTOTP, auth.FactorRecoveryCode:
		if deref(req.Body.Code) == "" {
			return nil, fieldError(codeField, "is required for "+string(method))
		}
	case auth.FactorWebAuthn:
		codeField = "/credential"
	}
	client := clientFromContext(ctx)
	res, err := h.auth.LoginSecondFactor(ctx, auth.SecondFactorRequest{
		PendingToken: redact.Secret(token),
		Proof:        auth.FactorProof{Method: method, Code: redact.Secret(deref(req.Body.Code))},
		ClientIP:     client.IP,
		UserAgent:    client.UserAgent,
	})
	var failed *auth.SecondFactorError
	switch {
	case errors.Is(err, auth.ErrLoginExpired):
		// Not audited: without a pending login there is no user to name,
		// and an unknown token costs the client nothing to send.
		return nil, expired
	case errors.Is(err, auth.ErrFactorUnavailable):
		return nil, fieldError("/method", "isn't available for this account")
	case errors.As(err, &failed):
		// Audited like a failed login, but with the user as the actor,
		// since their password was right.
		noteLogin(ctx, failed.UserID)
		setAuditDetails(ctx, map[string]string{auditSecondFactor: string(method)})
		return nil, fieldError(codeField, "is incorrect")
	case err != nil:
		return nil, err
	}
	noteLogin(ctx, res.User.ID)
	details := map[string]string{auditSecondFactor: string(method)}
	if method == auth.FactorRecoveryCode {
		n, err := h.auth.RecoveryCodesRemaining(ctx, res.User.ID)
		if err != nil {
			return nil, err
		}
		details["recovery_codes_remaining"] = strconv.Itoa(n)
	}
	setAuditDetails(ctx, details)
	body, cookie, err := h.loginBody(ctx, res)
	if err != nil {
		return nil, err
	}
	return gen.LoginSecondFactor200JSONResponse{Body: body, Headers: gen.LoginSecondFactor200ResponseHeaders{SetCookie: &cookie}}, nil
}

// loginResponse is the 200 of a one-step login.
func (h *Handlers) loginResponse(ctx context.Context, res auth.LoginResult) (gen.LoginResponseObject, error) {
	body, cookie, err := h.loginBody(ctx, res)
	if err != nil {
		return nil, err
	}
	return gen.Login200JSONResponse{Body: body, Headers: gen.Login200ResponseHeaders{SetCookie: &cookie}}, nil
}

// loginBody returns the body and session cookie of a successful login,
// whichever step finished it.
func (h *Handlers) loginBody(ctx context.Context, res auth.LoginResult) (gen.CurrentUser, string, error) {
	cookie := h.sessionCookie(ctx, res.Token.Reveal(), res.Session.ExpiresAt).String()
	csrf := res.CSRFToken.Reveal()
	verified := res.Session.VerifiedAt
	body, err := h.currentUser(ctx, res.User, gen.AuthMethodSession, &csrf, &verified)
	return body, cookie, err
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
	method, csrf, verified := gen.AuthMethodToken, (*string)(nil), (*time.Time)(nil)
	if p.Method == auth.MethodSession {
		method, verified = gen.AuthMethodSession, &p.VerifiedAt
		if t, ok := csrfFromContext(ctx); ok {
			s := t.Reveal()
			csrf = &s
		}
	}
	body, err := h.currentUser(ctx, u, method, csrf, verified)
	if err != nil {
		return nil, err
	}
	return gen.GetCurrentUser200JSONResponse(body), nil
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
	g, err := h.userWithTwoFactor(ctx, u)
	if err != nil {
		return nil, err
	}
	return gen.UpdateCurrentUser200JSONResponse(g), nil
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
	tok, plain, err := h.auth.CreateAPIToken(ctx, p.UserID, auth.NewAPIToken{
		Name:      req.Body.Name,
		Scope:     store.TokenScope(req.Body.Scope),
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

// RegenerateRecoveryCodes replaces the current user's recovery codes and
// returns the new ones, once.
func (h *Handlers) RegenerateRecoveryCodes(ctx context.Context, _ gen.RegenerateRecoveryCodesRequestObject) (gen.RegenerateRecoveryCodesResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.RegenerateRecoveryCodes, self(ctx))
	if err != nil {
		return nil, err
	}
	codes, err := h.auth.RegenerateRecoveryCodes(ctx, p.UserID)
	if err != nil {
		return nil, authError(err, "")
	}
	body := gen.RecoveryCodes{Codes: make([]string, len(codes))}
	for i, c := range codes {
		body.Codes[i] = c.Reveal()
	}
	return gen.RegenerateRecoveryCodes200JSONResponse(body), nil
}

// authorizeAuth is authorize for handlers that need the auth service.
func (h *Handlers) authorizeAuth(ctx context.Context, action authz.Action, target authz.Target) (auth.Principal, error) {
	p, err := h.authorize(ctx, action, target)
	if err != nil {
		return auth.Principal{}, err
	}
	return p, nil
}

// authError maps auth errors to API errors. passwordField names the body
// member that holds a new password, for policy violations.
func authError(err error, passwordField string) error {
	switch {
	case errors.Is(err, auth.ErrUserNotFound), errors.Is(err, auth.ErrTokenNotFound), errors.Is(err, auth.ErrSSHKeyNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, auth.ErrEmailTaken), errors.Is(err, auth.ErrLastAdmin), errors.Is(err, auth.ErrNoSecondFactor):
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

// currentUser describes the caller. verified is when the session was last
// verified, nil for API tokens.
func (h *Handlers) currentUser(ctx context.Context, u store.User, method gen.CurrentUserAuthMethod, csrf *string, verified *time.Time) (gen.CurrentUser, error) {
	g, err := h.userWithTwoFactor(ctx, u)
	if err != nil {
		return gen.CurrentUser{}, err
	}
	remaining, err := h.auth.RecoveryCodesRemaining(ctx, u.ID)
	if err != nil {
		return gen.CurrentUser{}, err
	}
	body := gen.CurrentUser{
		ID: g.ID, Email: g.Email, DisplayName: g.DisplayName, Role: g.Role, Disabled: g.Disabled,
		TwoFactorEnabled: g.TwoFactorEnabled,
		CreatedAt:        g.CreatedAt, UpdatedAt: g.UpdatedAt, LastLoginAt: g.LastLoginAt,
		AuthMethod: method, CsrfToken: csrf, VerifiedAt: verified, RecoveryCodesRemaining: remaining,
	}
	body.ReauthenticationMaxAgeSeconds = h.reauthenticationMaxAge(method)
	return body, nil
}

// userWithTwoFactor is user for one user, looking up their second factors.
func (h *Handlers) userWithTwoFactor(ctx context.Context, u store.User) (gen.User, error) {
	twoFactor, err := h.auth.TwoFactorEnabled(ctx, u.ID)
	if err != nil {
		return gen.User{}, err
	}
	return user(u, twoFactor), nil
}

func user(u store.User, twoFactor bool) gen.User {
	return gen.User{
		ID:               u.ID,
		Email:            openapi_types.Email(u.Email),
		DisplayName:      u.DisplayName,
		Role:             gen.Role(u.Role),
		Disabled:         u.Disabled(),
		TwoFactorEnabled: twoFactor,
		CreatedAt:        u.CreatedAt,
		UpdatedAt:        u.UpdatedAt,
		LastLoginAt:      u.LastLoginAt,
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
