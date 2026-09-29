// Package dbsession runs temporary database sessions (design doc §5.8,
// decision #61): developers reach a site's database with their own tools
// (TablePlus, DBeaver, the mariadb client) through the SFTP gateway,
// without ever seeing the site's database password.
//
// # Opening a session
//
// The API (POST /sites/{id}/database/sessions) calls [Service.Open]. It
// records the session (store.DatabaseSession, status active) and then has
// the site database manager create its temporary MariaDB user,
// pft_<session-id>, limited to the site's database: read-only by default,
// read-write when an Admin asks. The caller gets the user's credentials
// once, with the `ssh -L` command that opens the tunnel. The password is
// held in a redact.Secret and never stored, logged, or audited. If the
// user can't be created, the session is marked failed and whatever was
// created is dropped.
//
// # The tunnel
//
// The session's user connects to the SFTP gateway with one of their
// registered SSH keys, as for SFTP (the SSH username names the site), and
// asks it to forward a local port to the database's address, which is the
// only destination allowed. [Service.Forwarding] is the gateway's
// ForwardingPolicy: it allows the connection only if the user has an
// active, unexpired session on that site that their current role still
// allows (authz.TunnelDatabase, or authz.TunnelDatabaseReadWrite for a
// read-write session), and dials the database from the control plane,
// which is on the platform network. A session the user's role no longer
// allows is ended on the spot. Each tunneled connection gets a deadline at
// the latest expiry of the user's sessions, so it stops at expiry even
// between sweeps.
//
// # Ending a session
//
// A session ends when a user ends it through the API ([Service.End]), or
// when it expires. Either way the control plane drops its MariaDB user
// and kills its connections. The [SweepJobType] job, every minute on a
// schedule ([EnsureSweepSchedule]) and once when the control plane starts
// ([Service.Sweep]):
//
//  1. expires active sessions whose expiry has passed, recording each in
//     the audit log ([ActionExpire]);
//  2. drops the users of ended sessions whose drop hasn't succeeded yet
//     (MariaDB was down, the control plane stopped in between);
//  3. drops temporary users that no active session owns: those left behind
//     by a crash between creating a user and recording a failure, or by a
//     state database restored from a backup.
//
// Because every session is recorded before its user is created, step 3
// never drops the user of a session being opened.
package dbsession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Session lengths (§5.8).
const (
	DefaultTTL = time.Hour
	MaxTTL     = 8 * time.Hour
	// MinTTL keeps sessions long enough to be useful.
	MinTTL = time.Minute
)

// LocalPort is the local port in the suggested ssh command. Not 3306, so
// the tunnel doesn't collide with a database server on the developer's
// own machine.
const LocalPort = 3307

// The sweep job and its schedule.
const (
	SweepJobType      = "database_session.sweep"
	SweepScheduleName = "database-session-sweep"
	// sweepInterval bounds how long an expired session's user outlives
	// its expiry. Tunnels stop at expiry regardless.
	sweepInterval = time.Minute
	// The sweep targets the platform; job targets need a type and an ID.
	sweepTargetType = "platform"
	sweepTargetID   = "database_sessions"
)

// Audit actions the control plane records on its own. Opening and ending
// sessions through the API are recorded by the API under
// authz.OpenDatabaseSession (or OpenReadWriteDatabaseSession) and
// authz.EndDatabaseSession.
const (
	// ActionExpire is a session reaching its expiry.
	ActionExpire = "site.database.session.expire"
	// ActionRevoke is a session ended because its user's role no longer
	// allows it.
	ActionRevoke = "site.database.session.revoke"
)

// Reasons a session ended, in its record and audit events.
const (
	ReasonEnded   = "ended"
	ReasonExpired = "expired"
	ReasonRevoked = "access_revoked"
	ReasonFailed  = "create_failed"
)

// Username is the name of a session's temporary MariaDB user, as the site
// database manager (package mariadb) names it, for listing sessions
// without asking the server.
func Username(sessionID string) string { return "pft_" + sessionID }

// Database is the name of a site's database, as the site database manager
// names it.
func Database(siteID string) string { return "pf_" + siteID }

// Sentinel errors.
var (
	// ErrSiteNotFound means the site doesn't exist.
	ErrSiteNotFound = errors.New("site not found")
	// ErrUnavailable means the site has no database to open a session on
	// right now: it is being provisioned or deleted, or its provisioning
	// failed.
	ErrUnavailable = errors.New("the site's database can't be opened now")
	// ErrNoGateway means the SFTP gateway is off, so there is no tunnel.
	ErrNoGateway = errors.New("the SFTP gateway is off, so database sessions can't be reached")
	// ErrInvalidRequest means an access or session length the service
	// doesn't accept.
	ErrInvalidRequest = errors.New("invalid database session request")
	// ErrSessionNotFound means there is no active session to end: it
	// doesn't exist, belongs to another site, or has ended.
	ErrSessionNotFound = errors.New("database session not found")
)

// Users manages the sessions' temporary MariaDB users; sitedb.Manager
// implements it.
type Users interface {
	CreateTemporaryUser(ctx context.Context, siteID, sessionID string, access sitedb.Access) (sitedb.Credentials, error)
	DropTemporaryUser(ctx context.Context, sessionID string) error
	ListTemporaryUsers(ctx context.Context) ([]string, error)
}

// Tunnel is where the tunnel goes.
type Tunnel struct {
	// SSHHost and SSHPort are where developers reach the SFTP gateway: the
	// dashboard domain and sftp.port. An empty SSHHost means the gateway
	// is off.
	SSHHost string
	SSHPort int
	// DatabaseAddr is the database server's host:port as the control plane
	// reaches it (mariadb.address). It is the tunnel's only destination.
	DatabaseAddr string
}

// Options configure a Service.
type Options struct {
	// DB is the state database. Required.
	DB *store.DB
	// Users creates and drops the temporary users. Required.
	Users Users
	// Tunnel is where the tunnel goes. DatabaseAddr is required.
	Tunnel Tunnel
	// Dial connects to the database for a tunnel; nil means a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Logger defaults to discarding output.
	Logger *slog.Logger
}

// Service opens, ends, and expires database sessions, and decides the SFTP
// gateway's port forwarding.
type Service struct {
	db     *store.DB
	users  Users
	tunnel Tunnel
	dbHost string
	dbPort uint32
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	now    func() time.Time
	log    *slog.Logger
}

// New returns a Service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.Users == nil {
		return nil, errors.New("dbsession: DB and Users are required")
	}
	host, portStr, err := net.SplitHostPort(opts.Tunnel.DatabaseAddr)
	if err != nil || host == "" {
		return nil, fmt.Errorf("dbsession: the database address must be host:port, got %q", opts.Tunnel.DatabaseAddr)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("dbsession: invalid database port %q", portStr)
	}
	s := &Service{db: opts.DB, users: opts.Users, tunnel: opts.Tunnel, dbHost: host, dbPort: uint32(port),
		dial: opts.Dial, now: opts.Now, log: opts.Logger}
	if s.dial == nil {
		d := &net.Dialer{Timeout: 10 * time.Second}
		s.dial = d.DialContext
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// Opened is a session just opened, with the credentials that are shown
// only once.
type Opened struct {
	Session store.DatabaseSession
	// Credentials are the temporary user's; the password is shown once.
	Credentials sitedb.Credentials
	// SSHCommand opens the tunnel: `ssh -N -L 3307:<database>:3306 -p
	// <port> <site-slug>@<host>`.
	SSHCommand string
	// LocalHost and LocalPort are where tools connect once the tunnel is
	// open.
	LocalHost string
	LocalPort int
}

// Open opens a session on the site's database for the user, with the
// given access, for ttl (zero means DefaultTTL). The caller has checked
// that the user may open it. It returns ErrInvalidRequest, ErrNoGateway,
// ErrSiteNotFound, or ErrUnavailable for requests it refuses.
func (s *Service) Open(ctx context.Context, siteID, userID string, access sitedb.Access, ttl time.Duration) (Opened, error) {
	if ttl == 0 {
		ttl = DefaultTTL
	}
	switch {
	case !access.Valid():
		return Opened{}, fmt.Errorf("%w: unknown access %q", ErrInvalidRequest, access)
	case ttl < MinTTL || ttl > MaxTTL:
		return Opened{}, fmt.Errorf("%w: a session lasts from %s to %s, not %s", ErrInvalidRequest, MinTTL, MaxTTL, ttl)
	case s.tunnel.SSHHost == "":
		return Opened{}, ErrNoGateway
	}
	site, err := openableSite(ctx, s.db, siteID)
	if err != nil {
		return Opened{}, err
	}
	now := s.now()
	session := store.DatabaseSession{ID: id.New(), SiteID: siteID, UserID: userID, Access: string(access),
		CreatedAt: now, ExpiresAt: now.Add(ttl)}
	// Recorded before the user exists, so the sweep never mistakes the
	// user for one left behind.
	if err := store.NewDatabaseSessions(s.db).Create(ctx, &session); err != nil {
		return Opened{}, err
	}
	creds, err := s.users.CreateTemporaryUser(ctx, siteID, session.ID, access)
	if err != nil {
		s.fail(ctx, session, err)
		if errors.Is(err, sitedb.ErrDatabaseNotFound) {
			return Opened{}, fmt.Errorf("%w: it has no database", ErrUnavailable)
		}
		return Opened{}, fmt.Errorf("open database session on site %s: %w", siteID, err)
	}
	s.log.InfoContext(ctx, "database session opened", "session_id", session.ID, "site_id", siteID,
		"user_id", userID, "access", string(access), "expires_at", session.ExpiresAt)
	return Opened{
		Session:     session,
		Credentials: creds,
		SSHCommand: fmt.Sprintf("ssh -N -L %d:%s -p %d %s@%s", LocalPort,
			net.JoinHostPort(s.dbHost, strconv.FormatUint(uint64(s.dbPort), 10)), s.tunnel.SSHPort, site.Slug, s.tunnel.SSHHost),
		LocalHost: "127.0.0.1",
		LocalPort: LocalPort,
	}, nil
}

// fail marks a session whose user couldn't be created failed, and drops
// whatever part of the user was created. Errors are logged: the sweep
// retries the drop.
func (s *Service) fail(ctx context.Context, session store.DatabaseSession, cause error) {
	s.log.WarnContext(ctx, "opening a database session failed", "session_id", session.ID, "site_id", session.SiteID, "error", cause)
	ctx = context.WithoutCancel(ctx)
	if _, err := store.NewDatabaseSessions(s.db).End(ctx, session.ID, store.DatabaseSessionFailed, ReasonFailed, "", s.now()); err != nil {
		s.log.WarnContext(ctx, "recording a failed database session failed", "session_id", session.ID, "error", err)
		return
	}
	s.drop(ctx, session.ID)
}

// openableSite checks that the site has a database to open a session on:
// it exists, is neither being provisioned nor deleted, and was
// provisioned.
func openableSite(ctx context.Context, q store.Querier, siteID string) (store.Site, error) {
	site, err := store.NewSites(q).Get(ctx, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Site{}, fmt.Errorf("site %s: %w", siteID, ErrSiteNotFound)
	}
	if err != nil {
		return store.Site{}, err
	}
	switch site.Status {
	case store.SiteProvisioning, store.SiteDeleting:
		return store.Site{}, fmt.Errorf("%w: the site is %s", ErrUnavailable, site.Status)
	}
	if site.WPSaltsSecretRef == "" {
		return store.Site{}, fmt.Errorf("%w: it was never provisioned", ErrUnavailable)
	}
	return site, nil
}

// List returns the site's active, unexpired sessions, oldest first.
func (s *Service) List(ctx context.Context, siteID string) ([]store.DatabaseSession, error) {
	all, err := store.NewDatabaseSessions(s.db).ListActive(ctx, siteID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]store.DatabaseSession, 0, len(all))
	for _, d := range all {
		if d.Active(now) {
			out = append(out, d)
		}
	}
	return out, nil
}

// End ends an active session of the site on behalf of byUserID: it drops
// the session's user and kills its connections. It returns
// ErrSessionNotFound if the site has no such active session. A failed drop
// is logged and left to the sweep: the session has ended either way, and
// the tunnel refuses it at once.
func (s *Service) End(ctx context.Context, siteID, sessionID, byUserID string) (store.DatabaseSession, error) {
	sessions := store.NewDatabaseSessions(s.db)
	d, err := sessions.Get(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && d.SiteID != siteID) {
		return store.DatabaseSession{}, fmt.Errorf("%w: %s", ErrSessionNotFound, sessionID)
	}
	if err != nil {
		return store.DatabaseSession{}, err
	}
	changed, err := sessions.End(ctx, sessionID, store.DatabaseSessionEnded, ReasonEnded, byUserID, s.now())
	if err != nil {
		return store.DatabaseSession{}, err
	}
	if !changed {
		return store.DatabaseSession{}, fmt.Errorf("%w: %s has already ended", ErrSessionNotFound, sessionID)
	}
	s.log.InfoContext(ctx, "database session ended", "session_id", sessionID, "site_id", siteID, "by_user_id", byUserID)
	s.drop(context.WithoutCancel(ctx), sessionID)
	return sessions.Get(ctx, sessionID)
}

// drop drops an ended session's user and records it. Failures are logged;
// the sweep retries sessions that aren't recorded as dropped.
func (s *Service) drop(ctx context.Context, sessionID string) bool {
	if err := s.users.DropTemporaryUser(ctx, sessionID); err != nil {
		s.log.WarnContext(ctx, "dropping a database session's user failed; the sweep retries",
			"session_id", sessionID, "error", err)
		return false
	}
	if _, err := store.NewDatabaseSessions(s.db).MarkDropped(ctx, sessionID, s.now()); err != nil {
		s.log.WarnContext(ctx, "recording a dropped database session user failed", "session_id", sessionID, "error", err)
		return false
	}
	return true
}

// SweepResult counts what a sweep did.
type SweepResult struct {
	// Expired sessions, users Dropped for ended sessions, and Orphans
	// dropped that no active session owned.
	Expired, Dropped, Orphans int
}

// sweepBatch is how many sessions a sweep reads at a time.
const sweepBatch = 100

// Sweep expires sessions past their expiry, drops the users of ended
// sessions, and drops temporary users no active session owns (see the
// package documentation). It carries on past failures and returns them
// joined.
func (s *Service) Sweep(ctx context.Context) (SweepResult, error) {
	var (
		res  SweepResult
		errs []error
	)
	sessions := store.NewDatabaseSessions(s.db)
	for {
		due, err := sessions.ListDue(ctx, s.now(), sweepBatch)
		if err != nil {
			errs = append(errs, err)
			break
		}
		failed := 0
		for _, d := range due {
			changed, err := sessions.End(ctx, d.ID, store.DatabaseSessionExpired, ReasonExpired, "", s.now())
			if err != nil {
				errs = append(errs, err)
				failed++
				continue
			}
			if changed {
				res.Expired++
				s.log.InfoContext(ctx, "database session expired", "session_id", d.ID, "site_id", d.SiteID)
				s.audit(ctx, store.AuditEvent{Action: ActionExpire, TargetType: string(authz.TargetSite), TargetID: d.SiteID,
					Result: store.AuditSuccess, Details: details(d, ReasonExpired)})
			}
		}
		// Sessions that failed to expire come back in the next batch; stop
		// rather than read them forever.
		if len(due) < sweepBatch || failed > 0 || ctx.Err() != nil {
			break
		}
	}

	// Ended sessions whose users weren't dropped, including the ones just
	// expired. Failures stay in the list, so read it once.
	undropped, err := sessions.ListUndropped(ctx, 0)
	if err != nil {
		errs = append(errs, err)
	}
	for _, d := range undropped {
		if err := s.users.DropTemporaryUser(ctx, d.ID); err != nil {
			errs = append(errs, fmt.Errorf("dropping the user of database session %s: %w", d.ID, err))
			continue
		}
		if _, err := sessions.MarkDropped(ctx, d.ID, s.now()); err != nil {
			errs = append(errs, err)
			continue
		}
		res.Dropped++
	}

	// Users first, then sessions: a user created after its session was
	// recorded, so every user listed has its session visible below.
	users, err := s.users.ListTemporaryUsers(ctx)
	if err != nil {
		errs = append(errs, err)
		return res, errors.Join(errs...)
	}
	active, err := sessions.ActiveIDs(ctx)
	if err != nil {
		errs = append(errs, err)
		return res, errors.Join(errs...)
	}
	for _, sessionID := range users {
		if active[sessionID] {
			continue
		}
		if err := s.users.DropTemporaryUser(ctx, sessionID); err != nil {
			errs = append(errs, fmt.Errorf("dropping leftover temporary user of session %s: %w", sessionID, err))
			continue
		}
		res.Orphans++
		s.log.InfoContext(ctx, "dropped a leftover temporary database user", "session_id", sessionID)
	}
	return res, errors.Join(errs...)
}

// details are a session's audit event details. Never the password.
func details(d store.DatabaseSession, reason string) map[string]string {
	return map[string]string{
		"session_id": d.ID,
		"user_id":    d.UserID,
		"access":     d.Access,
		"expires_at": d.ExpiresAt.UTC().Format(time.RFC3339),
		"reason":     reason,
	}
}

// auditTimeout bounds writing an audit event.
const auditTimeout = 10 * time.Second

// audit writes an audit event; failures are logged, never returned.
func (s *Service) audit(ctx context.Context, e store.AuditEvent) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err := store.NewAuditEvents(s.db).Insert(ctx, &e); err != nil {
		s.log.ErrorContext(ctx, "writing audit event failed", "action", e.Action, "error", err)
	}
}

// Handlers returns the sweep job's handler, to register on the job pool.
func (s *Service) Handlers() []jobs.Handler {
	return []jobs.Handler{{Type: SweepJobType, Steps: []jobs.Step{
		{Name: "sweep_database_sessions", Do: func(ctx context.Context, r *jobs.Run) error {
			res, err := s.Sweep(ctx)
			if res.Expired+res.Dropped+res.Orphans > 0 {
				r.Logf("Expired %d database sessions, dropped %d users of ended sessions and %d left over.",
					res.Expired, res.Dropped, res.Orphans)
			}
			return err
		}},
	}}}
}

// EnsureSweepSchedule creates or updates the sweep job's schedule. It is
// safe to call on every start: an unchanged schedule keeps its next run.
func EnsureSweepSchedule(ctx context.Context, q store.Querier) error {
	_, err := jobs.UpsertSchedule(ctx, q, jobs.ScheduleSpec{
		Name:            SweepScheduleName,
		JobType:         SweepJobType,
		TargetType:      sweepTargetType,
		TargetID:        sweepTargetID,
		LockKey:         SweepJobType,
		Interval:        sweepInterval,
		MissedRunPolicy: store.MissedCoalesce,
	})
	if err != nil {
		return fmt.Errorf("database session sweep schedule: %w", err)
	}
	return nil
}
