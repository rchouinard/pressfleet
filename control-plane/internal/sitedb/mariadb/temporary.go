package mariadb

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// TemporaryUserConnections is each temporary user's MAX_USER_CONNECTIONS:
// enough for a database tool, which opens a few connections, and small
// against the server's max_connections (§4.1).
const TemporaryUserConnections = 10

// CreateTemporaryUser implements sitedb.Manager. The user is
// 'pft_<session-id>'@'%' with a new random password, TemporaryUserConnections,
// and the privileges of access on the site's database and nothing else. It
// returns sitedb.ErrDatabaseNotFound if the site has no database.
func (m *Manager) CreateTemporaryUser(ctx context.Context, siteID, sessionID string, access sitedb.Access) (sitedb.Credentials, error) {
	db, err := newSiteName(siteID)
	if err != nil {
		return sitedb.Credentials{}, fmt.Errorf("create temporary user: %w", err)
	}
	user, err := newTempName(sessionID)
	if err != nil {
		return sitedb.Credentials{}, fmt.Errorf("create temporary user: %w", err)
	}
	if !access.Valid() {
		return sitedb.Credentials{}, fmt.Errorf("create temporary user: unknown access %q", access)
	}
	// Grants don't need the database to exist, so check the record: a
	// site without one has nothing to grant.
	if _, err := getRecord(ctx, m.state, siteID); errors.Is(err, store.ErrNotFound) {
		return sitedb.Credentials{}, fmt.Errorf("create temporary user for site %s: %w", siteID, sitedb.ErrDatabaseNotFound)
	} else if err != nil {
		return sitedb.Credentials{}, fmt.Errorf("create temporary user for site %s: %w", siteID, err)
	}
	password := newPassword()
	if err := m.server.ensureTemporary(ctx, user, db, nativePasswordHash(password), access, TemporaryUserConnections); err != nil {
		return sitedb.Credentials{}, fmt.Errorf("create temporary user for site %s: %w", siteID, err)
	}
	m.log.Info("temporary database user created", "site_id", siteID, "session_id", sessionID,
		"username", user.String(), "access", string(access))
	return sitedb.Credentials{Database: db.String(), Username: user.String(), Password: password}, nil
}

// DropTemporaryUser implements sitedb.Manager.
func (m *Manager) DropTemporaryUser(ctx context.Context, sessionID string) error {
	user, err := newTempName(sessionID)
	if err != nil {
		return fmt.Errorf("drop temporary user: %w", err)
	}
	if err := m.server.dropTemporary(ctx, user); err != nil {
		return fmt.Errorf("drop temporary user: %w", err)
	}
	m.log.Info("temporary database user dropped", "session_id", sessionID, "username", user.String())
	return nil
}

// ListTemporaryUsers implements sitedb.Manager. Users with the temporary
// prefix whose names aren't "pft_" and a session ID are left out: they
// can't be PressFleet's.
func (m *Manager) ListTemporaryUsers(ctx context.Context) ([]string, error) {
	users, err := m.server.listTemporary(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, u := range users {
		if sessionID, ok := tempSessionID(u); ok {
			out = append(out, sessionID)
		}
	}
	return out, nil
}
