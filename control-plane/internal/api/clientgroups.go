package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
)

// Client groups (#16).

// ListClientGroups lists client groups.
func (h *Handlers) ListClientGroups(ctx context.Context, _ gen.ListClientGroupsRequestObject) (gen.ListClientGroupsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewClientGroups, authz.Platform()); err != nil {
		return nil, err
	}
	return nil, notImplemented(16)
}

// CreateClientGroup creates a client group.
func (h *Handlers) CreateClientGroup(ctx context.Context, _ gen.CreateClientGroupRequestObject) (gen.CreateClientGroupResponseObject, error) {
	if _, err := h.authorize(ctx, authz.CreateClientGroup, authz.Platform()); err != nil {
		return nil, err
	}
	return nil, notImplemented(16)
}

// GetClientGroup returns a client group.
func (h *Handlers) GetClientGroup(ctx context.Context, req gen.GetClientGroupRequestObject) (gen.GetClientGroupResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewClientGroups, authz.ClientGroup(req.ClientGroupID)); err != nil {
		return nil, err
	}
	return nil, notImplemented(16)
}

// UpdateClientGroup changes a client group's name or notes.
func (h *Handlers) UpdateClientGroup(ctx context.Context, req gen.UpdateClientGroupRequestObject) (gen.UpdateClientGroupResponseObject, error) {
	if _, err := h.authorize(ctx, authz.UpdateClientGroup, authz.ClientGroup(req.ClientGroupID)); err != nil {
		return nil, err
	}
	return nil, notImplemented(16)
}

// DeleteClientGroup deletes a client group that has no sites.
func (h *Handlers) DeleteClientGroup(ctx context.Context, req gen.DeleteClientGroupRequestObject) (gen.DeleteClientGroupResponseObject, error) {
	if _, err := h.authorize(ctx, authz.DeleteClientGroup, authz.ClientGroup(req.ClientGroupID)); err != nil {
		return nil, err
	}
	return nil, notImplemented(16)
}
