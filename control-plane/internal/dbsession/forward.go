package dbsession

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/sftpgw"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// KeyChecker rechecks an SSH key and returns its user's current
// principal; auth.Service implements it.
type KeyChecker interface {
	RecheckSSHKey(ctx context.Context, keyID string) (auth.Principal, error)
}

// Forwarding returns the SFTP gateway's port forwarding policy: forward to
// the site's database only, for a user with an active session on the site
// that their current role allows. keys rechecks the connection's key.
func (s *Service) Forwarding(keys KeyChecker) sftpgw.ForwardingPolicy {
	return forwarding{s: s, keys: keys}
}

type forwarding struct {
	s    *Service
	keys KeyChecker
}

// DialForward implements sftpgw.ForwardingPolicy.
func (f forwarding) DialForward(ctx context.Context, conn sftpgw.Identity, host string, port uint32) (net.Conn, error) {
	s := f.s
	addr := net.JoinHostPort(s.dbHost, strconv.FormatUint(uint64(s.dbPort), 10))
	refuse := func(reason string) (net.Conn, error) {
		s.log.InfoContext(ctx, "database tunnel refused", "ssh_session_id", conn.SessionID, "user_id", conn.UserID,
			"site_id", conn.SiteID, "source_ip", conn.SourceIP, "reason", reason)
		return nil, fmt.Errorf("%w: %s", sftpgw.ErrForwardingDenied, reason)
	}
	if !strings.EqualFold(host, s.dbHost) || port != s.dbPort {
		return refuse("only the site's database, " + addr + ", can be reached")
	}
	sessions, err := store.NewDatabaseSessions(s.db).ListActiveForUser(ctx, conn.SiteID, conn.UserID, s.now())
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return refuse("no active database session on the site")
	}
	p, err := f.keys.RecheckSSHKey(ctx, conn.SSHKeyID)
	if errors.Is(err, auth.ErrUnauthenticated) {
		return refuse("the key or its user is no longer valid")
	} else if err != nil {
		return nil, err
	}
	target, err := authz.NewResolver(s.db).Resolve(ctx, authz.Site(conn.SiteID))
	if err != nil {
		return nil, err
	}
	var (
		until   time.Time
		allowed []string
	)
	for _, d := range sessions {
		action := authz.TunnelDatabase
		if d.Access == string(sitedb.AccessReadWrite) {
			action = authz.TunnelDatabaseReadWrite
		}
		if err := authz.Check(p, action, target); err != nil {
			// The user has lost the role the session needs, say an Admin
			// demoted to Operator: end it rather than leave the database
			// user working until it expires.
			s.revoke(ctx, d, err)
			continue
		}
		allowed = append(allowed, d.ID)
		if d.ExpiresAt.After(until) {
			until = d.ExpiresAt
		}
	}
	if len(allowed) == 0 {
		return refuse("the user's role no longer allows their database sessions")
	}
	c, err := s.dial(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connecting to the database: %w", err)
	}
	// The connection stops at the latest expiry even if the sweep that
	// drops the user runs a little later.
	if err := c.SetDeadline(until); err != nil {
		_ = c.Close()
		return nil, err
	}
	s.log.InfoContext(ctx, "database tunnel opened", "ssh_session_id", conn.SessionID, "database_session_ids", allowed,
		"user_id", conn.UserID, "site_id", conn.SiteID, "source_ip", conn.SourceIP, "until", until)
	return c, nil
}

// revoke ends a session its user's role no longer allows, and records it
// in the audit log.
func (s *Service) revoke(ctx context.Context, d store.DatabaseSession, why error) {
	ctx = context.WithoutCancel(ctx)
	changed, err := store.NewDatabaseSessions(s.db).End(ctx, d.ID, store.DatabaseSessionEnded, ReasonRevoked, "", s.now())
	if err != nil {
		s.log.WarnContext(ctx, "revoking a database session failed", "session_id", d.ID, "error", err)
		return
	}
	if !changed {
		return
	}
	s.log.InfoContext(ctx, "database session revoked", "session_id", d.ID, "site_id", d.SiteID, "user_id", d.UserID, "reason", why.Error())
	s.audit(ctx, store.AuditEvent{Action: ActionRevoke, TargetType: string(authz.TargetSite), TargetID: d.SiteID,
		Result: store.AuditSuccess, Details: details(d, ReasonRevoked)})
	s.drop(ctx, d.ID)
}
