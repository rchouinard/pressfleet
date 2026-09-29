package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/dbsession"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Temporary database sessions (#115, design doc §5.8).

// errNoDatabaseSessions is a wiring bug: serve always passes the service.
var errNoDatabaseSessions = errors.New("api: no database session service configured")

// CreateSiteDatabaseSession opens a session. Read-only sessions need
// authz.OpenDatabaseSession, read-write ones
// authz.OpenReadWriteDatabaseSession (Admin); the audit event records the
// one checked, with the session's ID, access, and expiry, never the
// password.
func (h *Handlers) CreateSiteDatabaseSession(ctx context.Context, req gen.CreateSiteDatabaseSessionRequestObject) (gen.CreateSiteDatabaseSessionResponseObject, error) {
	access, ttl := sitedb.AccessReadOnly, time.Duration(0)
	if b := req.Body; b != nil {
		if b.Access != nil {
			access = sitedb.Access(*b.Access)
		}
		if b.TTLSeconds != nil {
			ttl = time.Duration(*b.TTLSeconds) * time.Second
		}
	}
	action := authz.OpenDatabaseSession
	if access == sitedb.AccessReadWrite {
		action = authz.OpenReadWriteDatabaseSession
	}
	p, err := h.authorize(ctx, action, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	if h.dbSessions == nil {
		return nil, errNoDatabaseSessions
	}
	o, err := h.dbSessions.Open(ctx, req.SiteID, p.UserID, access, ttl)
	if err != nil {
		return nil, databaseSessionError(err)
	}
	setAuditDetails(ctx, map[string]string{
		"session_id": o.Session.ID,
		"username":   o.Credentials.Username,
		"access":     o.Session.Access,
		"expires_at": o.Session.ExpiresAt.UTC().Format(time.RFC3339),
	})
	out := gen.CreateSiteDatabaseSession201JSONResponse{Session: databaseSession(o.Session)}
	out.Credentials.Username = o.Credentials.Username
	out.Credentials.Password = o.Credentials.Password.Reveal() // shown once, here
	out.Credentials.Database = o.Credentials.Database
	out.Tunnel.SSHCommand = o.SSHCommand
	out.Tunnel.LocalHost = o.LocalHost
	out.Tunnel.LocalPort = o.LocalPort
	return out, nil
}

// ListSiteDatabaseSessions lists the site's active sessions.
func (h *Handlers) ListSiteDatabaseSessions(ctx context.Context, req gen.ListSiteDatabaseSessionsRequestObject) (gen.ListSiteDatabaseSessionsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewSites, authz.Site(req.SiteID)); err != nil {
		return nil, err
	}
	if h.dbSessions == nil {
		return nil, errNoDatabaseSessions
	}
	if _, err := store.NewSites(h.db).Get(ctx, req.SiteID); errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("%w: %w", ErrNotFound, dbsession.ErrSiteNotFound)
	} else if err != nil {
		return nil, err
	}
	list, err := h.dbSessions.List(ctx, req.SiteID)
	if err != nil {
		return nil, err
	}
	out := gen.ListSiteDatabaseSessions200JSONResponse{Items: make([]gen.DatabaseSession, len(list))}
	for i, d := range list {
		out.Items[i] = databaseSession(d)
	}
	return out, nil
}

// EndSiteDatabaseSession ends a session at once.
func (h *Handlers) EndSiteDatabaseSession(ctx context.Context, req gen.EndSiteDatabaseSessionRequestObject) (gen.EndSiteDatabaseSessionResponseObject, error) {
	p, err := h.authorize(ctx, authz.EndDatabaseSession, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	if h.dbSessions == nil {
		return nil, errNoDatabaseSessions
	}
	setAuditDetails(ctx, map[string]string{"session_id": req.SessionID})
	d, err := h.dbSessions.End(ctx, req.SiteID, req.SessionID, p.UserID)
	if err != nil {
		return nil, databaseSessionError(err)
	}
	setAuditDetails(ctx, map[string]string{"user_id": d.UserID, "access": d.Access, "reason": d.EndReason})
	return gen.EndSiteDatabaseSession204Response{}, nil
}

func databaseSession(d store.DatabaseSession) gen.DatabaseSession {
	return gen.DatabaseSession{
		ID:        d.ID,
		SiteID:    d.SiteID,
		UserID:    d.UserID,
		Username:  dbsession.Username(d.ID),
		Database:  dbsession.Database(d.SiteID),
		Access:    gen.DatabaseSessionAccess(d.Access),
		Status:    gen.DatabaseSessionStatus(d.Status),
		CreatedAt: d.CreatedAt,
		ExpiresAt: d.ExpiresAt,
	}
}

// databaseSessionError maps database session errors to API errors.
func databaseSessionError(err error) error {
	switch {
	case errors.Is(err, dbsession.ErrSiteNotFound), errors.Is(err, dbsession.ErrSessionNotFound):
		// The wrapping says why, e.g. that the session has ended.
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, dbsession.ErrUnavailable), errors.Is(err, dbsession.ErrNoGateway):
		return fmt.Errorf("%w: %w", ErrConflict, err)
	case errors.Is(err, dbsession.ErrInvalidRequest):
		return fieldError("/ttl_seconds", err.Error())
	}
	return err
}
