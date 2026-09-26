package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// User management (#13). Only Admins may manage users (§4.6); that check,
// and last-Admin protection, are #103.

// ListUsers lists users.
func (h *Handlers) ListUsers(ctx context.Context, req gen.ListUsersRequestObject) (gen.ListUsersResponseObject, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, err
	}
	// TODO(#103): h.authorize(ctx, <manage users>, nil) before acting.
	limit := pageLimit(req.Params.Limit)
	users, err := h.auth.ListUsers(ctx, store.UserFilter{
		Role:  store.Role(deref(req.Params.Role)),
		After: deref(req.Params.Cursor),
		Limit: limit + 1,
	})
	if err != nil {
		return nil, authError(err, "")
	}
	users, next := page(users, limit, func(u store.User) string { return u.ID })
	body := gen.UserList{Items: make([]gen.User, 0, len(users)), NextCursor: next}
	for _, u := range users {
		body.Items = append(body.Items, user(u))
	}
	return gen.ListUsers200JSONResponse(body), nil
}

// CreateUser creates a user.
func (h *Handlers) CreateUser(ctx context.Context, req gen.CreateUserRequestObject) (gen.CreateUserResponseObject, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, err
	}
	// TODO(#103): h.authorize(ctx, <manage users>, nil) before acting.
	u, err := h.auth.CreateUser(ctx, auth.NewUser{
		Email:    string(req.Body.Email),
		Password: redact.Secret(deref(req.Body.Password)),
		Role:     store.Role(req.Body.Role),
	})
	if err != nil {
		return nil, authError(err, "/password")
	}
	location := "/api/v1/users/" + u.ID
	return gen.CreateUser201JSONResponse{Body: user(u), Headers: gen.CreateUser201ResponseHeaders{Location: &location}}, nil
}

// GetUser returns a user.
func (h *Handlers) GetUser(ctx context.Context, req gen.GetUserRequestObject) (gen.GetUserResponseObject, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, err
	}
	// TODO(#103): h.authorize(ctx, <manage users>, req.UserID) before acting.
	u, err := h.auth.GetUser(ctx, req.UserID)
	if err != nil {
		return nil, authError(err, "")
	}
	return gen.GetUser200JSONResponse(user(u)), nil
}

// UpdateUser changes a user's email, password, role, or disabled flag.
// Setting a password or disabling the user revokes their sessions and
// tokens.
func (h *Handlers) UpdateUser(ctx context.Context, req gen.UpdateUserRequestObject) (gen.UpdateUserResponseObject, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, err
	}
	// TODO(#103): h.authorize(ctx, <manage users>, req.UserID) before acting,
	// then refuse to demote or disable the last Admin (409).
	var c auth.UserChanges
	if e := req.Body.Email; e != nil {
		s := string(*e)
		c.Email = &s
	}
	if p := req.Body.Password; p != nil {
		s := redact.Secret(*p)
		c.Password = &s
	}
	if r := req.Body.Role; r != nil {
		role := store.Role(*r)
		c.Role = &role
	}
	c.Disabled = req.Body.Disabled
	u, err := h.auth.UpdateUser(ctx, req.UserID, c)
	if err != nil {
		return nil, authError(err, "/password")
	}
	return gen.UpdateUser200JSONResponse(user(u)), nil
}

// DeleteUser deletes a user and, with them, their sessions and tokens.
func (h *Handlers) DeleteUser(ctx context.Context, req gen.DeleteUserRequestObject) (gen.DeleteUserResponseObject, error) {
	if _, err := h.principal(ctx); err != nil {
		return nil, err
	}
	// TODO(#103): h.authorize(ctx, <manage users>, req.UserID) before acting,
	// then refuse to delete the last Admin (409).
	if err := h.auth.DeleteUser(ctx, req.UserID); err != nil {
		return nil, authError(err, "")
	}
	return gen.DeleteUser204Response{}, nil
}
