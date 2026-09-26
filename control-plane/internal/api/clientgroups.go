package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
)

// Client groups (#16).

// ListClientGroups lists client groups.
func (h *Handlers) ListClientGroups(context.Context, gen.ListClientGroupsRequestObject) (gen.ListClientGroupsResponseObject, error) {
	return nil, notImplemented(16)
}

// CreateClientGroup creates a client group.
func (h *Handlers) CreateClientGroup(context.Context, gen.CreateClientGroupRequestObject) (gen.CreateClientGroupResponseObject, error) {
	return nil, notImplemented(16)
}

// GetClientGroup returns a client group.
func (h *Handlers) GetClientGroup(context.Context, gen.GetClientGroupRequestObject) (gen.GetClientGroupResponseObject, error) {
	return nil, notImplemented(16)
}

// UpdateClientGroup changes a client group's name or notes.
func (h *Handlers) UpdateClientGroup(context.Context, gen.UpdateClientGroupRequestObject) (gen.UpdateClientGroupResponseObject, error) {
	return nil, notImplemented(16)
}

// DeleteClientGroup deletes a client group that has no sites.
func (h *Handlers) DeleteClientGroup(context.Context, gen.DeleteClientGroupRequestObject) (gen.DeleteClientGroupResponseObject, error) {
	return nil, notImplemented(16)
}
