package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
)

// User management (#13; roles and last-Admin protection in #103).

// ListUsers lists users.
func (h *Handlers) ListUsers(context.Context, gen.ListUsersRequestObject) (gen.ListUsersResponseObject, error) {
	return nil, notImplemented(13)
}

// CreateUser creates a user.
func (h *Handlers) CreateUser(context.Context, gen.CreateUserRequestObject) (gen.CreateUserResponseObject, error) {
	return nil, notImplemented(13)
}

// GetUser returns a user.
func (h *Handlers) GetUser(context.Context, gen.GetUserRequestObject) (gen.GetUserResponseObject, error) {
	return nil, notImplemented(13)
}

// UpdateUser changes a user's email, password, role, or disabled flag.
func (h *Handlers) UpdateUser(context.Context, gen.UpdateUserRequestObject) (gen.UpdateUserResponseObject, error) {
	return nil, notImplemented(13)
}

// DeleteUser deletes a user and revokes their sessions and tokens.
func (h *Handlers) DeleteUser(context.Context, gen.DeleteUserRequestObject) (gen.DeleteUserResponseObject, error) {
	return nil, notImplemented(13)
}
