package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// SSH keys for the SFTP gateway (#62, design doc §5.8). Every user manages
// their own keys under /users/me/ssh-keys; Admins manage anyone's under
// /users/{user_id}/ssh-keys (§4.6).

// ListOwnSSHKeys lists the current user's SSH keys.
func (h *Handlers) ListOwnSSHKeys(ctx context.Context, req gen.ListOwnSSHKeysRequestObject) (gen.ListOwnSSHKeysResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.ViewOwnSSHKeys, self(ctx))
	if err != nil {
		return nil, err
	}
	body, err := h.listSSHKeys(ctx, p.UserID, req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	return gen.ListOwnSSHKeys200JSONResponse(body), nil
}

// AddOwnSSHKey registers an SSH key for the current user.
func (h *Handlers) AddOwnSSHKey(ctx context.Context, req gen.AddOwnSSHKeyRequestObject) (gen.AddOwnSSHKeyResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.AddOwnSSHKey, self(ctx))
	if err != nil {
		return nil, err
	}
	k, err := h.addSSHKey(ctx, p.UserID, *req.Body)
	if err != nil {
		return nil, err
	}
	location := "/api/v1/users/me/ssh-keys/" + k.ID
	return gen.AddOwnSSHKey201JSONResponse{Body: k, Headers: gen.AddOwnSSHKey201ResponseHeaders{Location: &location}}, nil
}

// DeleteOwnSSHKey removes one of the current user's SSH keys.
func (h *Handlers) DeleteOwnSSHKey(ctx context.Context, req gen.DeleteOwnSSHKeyRequestObject) (gen.DeleteOwnSSHKeyResponseObject, error) {
	p, err := h.authorizeAuth(ctx, authz.DeleteOwnSSHKey, authz.SSHKey(req.SSHKeyID))
	if err != nil {
		return nil, err
	}
	if err := h.auth.DeleteSSHKey(ctx, p.UserID, req.SSHKeyID); err != nil {
		return nil, authError(err, "")
	}
	return gen.DeleteOwnSSHKey204Response{}, nil
}

// ListUserSSHKeys lists a user's SSH keys.
func (h *Handlers) ListUserSSHKeys(ctx context.Context, req gen.ListUserSSHKeysRequestObject) (gen.ListUserSSHKeysResponseObject, error) {
	if _, err := h.authorizeAuth(ctx, authz.ViewOthersSSHKeys, authz.User(req.UserID)); err != nil {
		return nil, err
	}
	// An unknown user is a 404, not an empty list.
	if _, err := h.auth.GetUser(ctx, req.UserID); err != nil {
		return nil, authError(err, "")
	}
	body, err := h.listSSHKeys(ctx, req.UserID, req.Params.Cursor, req.Params.Limit)
	if err != nil {
		return nil, err
	}
	return gen.ListUserSSHKeys200JSONResponse(body), nil
}

// AddUserSSHKey registers an SSH key for a user.
func (h *Handlers) AddUserSSHKey(ctx context.Context, req gen.AddUserSSHKeyRequestObject) (gen.AddUserSSHKeyResponseObject, error) {
	if _, err := h.authorizeAuth(ctx, authz.ManageOthersSSHKeys, authz.User(req.UserID)); err != nil {
		return nil, err
	}
	k, err := h.addSSHKey(ctx, req.UserID, *req.Body)
	if err != nil {
		return nil, err
	}
	location := "/api/v1/users/" + req.UserID + "/ssh-keys/" + k.ID
	return gen.AddUserSSHKey201JSONResponse{Body: k, Headers: gen.AddUserSSHKey201ResponseHeaders{Location: &location}}, nil
}

// DeleteUserSSHKey removes one of a user's SSH keys.
func (h *Handlers) DeleteUserSSHKey(ctx context.Context, req gen.DeleteUserSSHKeyRequestObject) (gen.DeleteUserSSHKeyResponseObject, error) {
	if _, err := h.authorizeAuth(ctx, authz.ManageOthersSSHKeys, authz.SSHKey(req.SSHKeyID)); err != nil {
		return nil, err
	}
	if err := h.auth.DeleteSSHKey(ctx, req.UserID, req.SSHKeyID); err != nil {
		return nil, authError(err, "")
	}
	return gen.DeleteUserSSHKey204Response{}, nil
}

func (h *Handlers) listSSHKeys(ctx context.Context, userID string, cursor *string, limitParam *int) (gen.SSHKeyList, error) {
	limit := pageLimit(limitParam)
	keys, err := h.auth.ListSSHKeys(ctx, userID, deref(cursor), limit+1)
	if err != nil {
		return gen.SSHKeyList{}, err
	}
	keys, next := page(keys, limit, func(k store.SSHKey) string { return k.ID })
	body := gen.SSHKeyList{Items: make([]gen.SSHKey, 0, len(keys)), NextCursor: next}
	for _, k := range keys {
		body.Items = append(body.Items, sshKey(k))
	}
	return body, nil
}

func (h *Handlers) addSSHKey(ctx context.Context, userID string, body gen.SSHKeyCreate) (gen.SSHKey, error) {
	k, err := h.auth.AddSSHKey(ctx, userID, auth.NewSSHKey{Name: deref(body.Name), PublicKey: body.PublicKey})
	if err != nil {
		return gen.SSHKey{}, sshKeyError(err)
	}
	setAuditTarget(ctx, authz.SSHKey(k.ID))
	return sshKey(k), nil
}

// sshKeyError maps the errors of registering a key.
func sshKeyError(err error) error {
	switch {
	case errors.Is(err, auth.ErrInvalidSSHKey):
		// The message says what is wrong with the key; it never echoes it.
		return fieldError("/public_key", strings.TrimPrefix(err.Error(), auth.ErrInvalidSSHKey.Error()+": "))
	case errors.Is(err, auth.ErrInvalidSSHKeyName):
		return fieldError("/name", err.Error())
	case errors.Is(err, auth.ErrSSHKeyTaken):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return authError(err, "")
}

func sshKey(k store.SSHKey) gen.SSHKey {
	typ, _, _ := strings.Cut(k.PublicKey, " ")
	return gen.SSHKey{
		ID:          k.ID,
		Name:        k.Name,
		Type:        typ,
		Fingerprint: k.Fingerprint,
		PublicKey:   k.PublicKey,
		CreatedAt:   k.CreatedAt,
		LastUsedAt:  k.LastUsedAt,
	}
}
