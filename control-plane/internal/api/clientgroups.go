package api

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Client groups (#16).

// ListClientGroups lists client groups by name. The cursor encodes the name
// and ID of the last group on the previous page.
func (h *Handlers) ListClientGroups(ctx context.Context, req gen.ListClientGroupsRequestObject) (gen.ListClientGroupsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewClientGroups, authz.Platform()); err != nil {
		return nil, err
	}
	if h.db == nil {
		return nil, errNoDatabase
	}
	limit := pageLimit(req.Params.Limit)
	f := store.ClientGroupFilter{Limit: limit + 1}
	if c := req.Params.Cursor; c != nil {
		var ok bool
		if f.AfterName, f.AfterID, ok = decodeGroupCursor(*c); !ok {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "The cursor is invalid; pass the next_cursor of a previous page.")
		}
	}
	groups, err := store.NewClientGroups(h.db).List(ctx, f)
	if err != nil {
		return nil, err
	}
	groups, next := page(groups, limit, encodeGroupCursor)
	body := gen.ClientGroupList{Items: make([]gen.ClientGroup, 0, len(groups)), NextCursor: next}
	for _, g := range groups {
		body.Items = append(body.Items, clientGroup(g))
	}
	return gen.ListClientGroups200JSONResponse(body), nil
}

// encodeGroupCursor makes the opaque cursor for the page after g.
func encodeGroupCursor(g store.ClientGroup) string {
	return base64.RawURLEncoding.EncodeToString([]byte(g.ID + ":" + g.Name))
}

func decodeGroupCursor(c string) (name, groupID string, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", "", false
	}
	groupID, name, ok = strings.Cut(string(b), ":")
	return name, groupID, ok && id.Valid(groupID)
}

// CreateClientGroup creates a client group.
func (h *Handlers) CreateClientGroup(ctx context.Context, req gen.CreateClientGroupRequestObject) (gen.CreateClientGroupResponseObject, error) {
	if _, err := h.authorize(ctx, authz.CreateClientGroup, authz.Platform()); err != nil {
		return nil, err
	}
	if h.db == nil {
		return nil, errNoDatabase
	}
	g := store.ClientGroup{Name: req.Body.Name, Notes: deref(req.Body.Notes)}
	if err := store.NewClientGroups(h.db).Create(ctx, &g); err != nil {
		return nil, clientGroupError(err, g.Name)
	}
	setAuditTarget(ctx, authz.ClientGroup(g.ID))
	location := "/api/v1/client-groups/" + g.ID
	return gen.CreateClientGroup201JSONResponse{
		Body:    clientGroup(g),
		Headers: gen.CreateClientGroup201ResponseHeaders{Location: &location},
	}, nil
}

// GetClientGroup returns a client group.
func (h *Handlers) GetClientGroup(ctx context.Context, req gen.GetClientGroupRequestObject) (gen.GetClientGroupResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewClientGroups, authz.ClientGroup(req.ClientGroupID)); err != nil {
		return nil, err
	}
	if h.db == nil {
		return nil, errNoDatabase
	}
	g, err := store.NewClientGroups(h.db).Get(ctx, req.ClientGroupID)
	if err != nil {
		return nil, clientGroupError(err, "")
	}
	return gen.GetClientGroup200JSONResponse(clientGroup(g)), nil
}

// UpdateClientGroup changes a client group's name or notes.
func (h *Handlers) UpdateClientGroup(ctx context.Context, req gen.UpdateClientGroupRequestObject) (gen.UpdateClientGroupResponseObject, error) {
	if _, err := h.authorize(ctx, authz.UpdateClientGroup, authz.ClientGroup(req.ClientGroupID)); err != nil {
		return nil, err
	}
	if h.db == nil {
		return nil, errNoDatabase
	}
	var g store.ClientGroup
	err := h.db.InTx(ctx, func(q store.Querier) error {
		groups := store.NewClientGroups(q)
		var err error
		if g, err = groups.Get(ctx, req.ClientGroupID); err != nil {
			return err
		}
		if n := req.Body.Name; n != nil {
			g.Name = *n
		}
		if n := req.Body.Notes; n != nil {
			g.Notes = *n
		}
		return groups.Update(ctx, &g)
	})
	if err != nil {
		return nil, clientGroupError(err, g.Name)
	}
	return gen.UpdateClientGroup200JSONResponse(clientGroup(g)), nil
}

// DeleteClientGroup deletes a client group that has no sites.
//
// The site check runs in the transaction that deletes the group, and the
// sites table's foreign key backs it up: a site created concurrently either
// commits first, and the delete fails with a conflict, or finds its group
// gone and fails itself.
func (h *Handlers) DeleteClientGroup(ctx context.Context, req gen.DeleteClientGroupRequestObject) (gen.DeleteClientGroupResponseObject, error) {
	if _, err := h.authorize(ctx, authz.DeleteClientGroup, authz.ClientGroup(req.ClientGroupID)); err != nil {
		return nil, err
	}
	if h.db == nil {
		return nil, errNoDatabase
	}
	err := h.db.InTx(ctx, func(q store.Querier) error {
		groups := store.NewClientGroups(q)
		g, err := groups.Get(ctx, req.ClientGroupID)
		if err != nil {
			return err
		}
		if g.SiteCount > 0 {
			return errGroupHasSites(g.SiteCount)
		}
		if g.BackupRepository != "" {
			return errGroupHasRepository
		}
		return groups.Delete(ctx, g.ID)
	})
	if errors.Is(err, store.ErrReference) {
		// A site or the backup repository was created concurrently.
		err = fmt.Errorf("%w: the client group still has sites or a backup repository", ErrConflict)
	}
	if err != nil {
		return nil, clientGroupError(err, "")
	}
	return gen.DeleteClientGroup204Response{}, nil
}

func errGroupHasSites(n int) error {
	return fmt.Errorf("%w: the client group still has %d site(s); delete them first", ErrConflict, n)
}

// errGroupHasRepository refuses to delete a group whose backups still
// exist: offboarding exports or deletes them deliberately first.
var errGroupHasRepository = fmt.Errorf("%w: the client group still has a backup repository; export or delete it first "+
	"(pressfleet repository export / delete in the control plane container)", ErrConflict)

// clientGroupError maps store errors to API errors.
func clientGroupError(err error, name string) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("client group: %w", ErrNotFound)
	case errors.Is(err, store.ErrConflict):
		return fmt.Errorf("%w: a client group named %q already exists", ErrConflict, name)
	}
	return err
}

func clientGroup(g store.ClientGroup) gen.ClientGroup {
	out := gen.ClientGroup{
		ID:        g.ID,
		Name:      g.Name,
		Notes:     g.Notes,
		SiteCount: g.SiteCount,
		CreatedAt: g.CreatedAt,
		UpdatedAt: g.UpdatedAt,
	}
	if g.BackupRepository != "" {
		out.BackupRepository = &gen.BackupRepository{Location: g.BackupRepository}
	}
	return out
}
