package dbsession_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/dbsession"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/sftpgw"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb/sitedbtest"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	db     *store.DB
	users  *sitedbtest.Fake
	svc    *dbsession.Service
	clock  *clock
	group  string
	uids   int
	dialed []string
}

func newEnv(t *testing.T, db *store.DB, tunnel dbsession.Tunnel) *env {
	t.Helper()
	e := &env{db: db, users: sitedbtest.New(), clock: &clock{t: time.Now().UTC().Truncate(time.Second)}}
	if tunnel.DatabaseAddr == "" {
		tunnel.DatabaseAddr = "mariadb:3306"
	}
	var err error
	e.svc, err = dbsession.New(dbsession.Options{
		DB: db, Users: e.users, Tunnel: tunnel, Now: e.clock.now, Logger: storetest.Logger(),
		Dial: func(_ context.Context, network, addr string) (net.Conn, error) {
			e.dialed = append(e.dialed, network+" "+addr)
			c, other := net.Pipe()
			t.Cleanup(func() { _ = other.Close() })
			return c, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	g := &store.ClientGroup{Name: "Acme " + id.New()}
	if err := store.NewClientGroups(db).Create(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	e.group = g.ID
	return e
}

var gateway = dbsession.Tunnel{SSHHost: "fleet.example", SSHPort: 2222, DatabaseAddr: "mariadb:3306"}

// site creates a site with the given status; provisioned says whether it
// finished provisioning, and so has a database.
func (e *env) site(t *testing.T, status store.SiteStatus, provisioned bool) store.Site {
	t.Helper()
	s := &store.Site{
		ClientGroupID: e.group, Name: "Site", Slug: "site-" + id.New()[20:], Status: status,
		PHPVersion: "8.4", WPCoreVersion: "6.9.1", PreviewDomain: id.New()[20:] + ".preview.example",
		MemoryReservationMB: 256, MemoryLimitMB: 512, CPULimit: "1",
		UID: 70000 + e.uids,
	}
	e.uids++
	if provisioned {
		s.WPSaltsSecretRef = "salts"
	}
	if err := store.NewSites(e.db).Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if provisioned {
		e.users.SetData(s.ID, nil)
	}
	return *s
}

func (e *env) session(t *testing.T, sessionID string) store.DatabaseSession {
	t.Helper()
	d, err := store.NewDatabaseSessions(e.db).Get(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) auditEvents(t *testing.T, action string) []store.AuditEvent {
	t.Helper()
	page, err := store.NewAuditEvents(e.db).List(context.Background(), store.AuditFilter{Action: action})
	if err != nil {
		t.Fatal(err)
	}
	return page.Events
}

func TestNewValidates(t *testing.T) {
	db := storetest.Open(t, store.SQLite)
	for _, opts := range []dbsession.Options{
		{Users: sitedbtest.New(), Tunnel: gateway},
		{DB: db, Tunnel: gateway},
		{DB: db, Users: sitedbtest.New(), Tunnel: dbsession.Tunnel{DatabaseAddr: "mariadb"}},
		{DB: db, Users: sitedbtest.New(), Tunnel: dbsession.Tunnel{DatabaseAddr: "mariadb:0"}},
	} {
		if _, err := dbsession.New(opts); err == nil {
			t.Errorf("New(%+v) succeeded", opts)
		}
	}
}

func TestOpen(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, gateway)
		site := e.site(t, store.SiteRunning, true)
		userID := id.New()

		o, err := e.svc.Open(ctx, site.ID, userID, sitedb.AccessReadOnly, 0)
		if err != nil {
			t.Fatal(err)
		}
		d := o.Session
		if d.Status != store.DatabaseSessionActive || d.SiteID != site.ID || d.UserID != userID || d.Access != "read_only" ||
			!d.ExpiresAt.Equal(e.clock.now().Add(dbsession.DefaultTTL)) {
			t.Errorf("session = %+v", d)
		}
		if got := e.session(t, d.ID); got != d {
			t.Errorf("stored = %+v, want %+v", got, d)
		}
		if o.Credentials.Username != "pft_"+d.ID || o.Credentials.Password.Reveal() == "" {
			t.Errorf("credentials = %s", o.Credentials.Username)
		}
		if want := "ssh -N -L 3307:mariadb:3306 -p 2222 " + site.Slug + "@fleet.example"; o.SSHCommand != want {
			t.Errorf("ssh command = %q, want %q", o.SSHCommand, want)
		}
		if o.LocalHost != "127.0.0.1" || o.LocalPort != 3307 {
			t.Errorf("local address = %s:%d", o.LocalHost, o.LocalPort)
		}
		if u, ok := e.users.TemporaryUsers()[d.ID]; !ok || u.Access != sitedb.AccessReadOnly || u.SiteID != site.ID {
			t.Errorf("temporary user = %+v, %v", u, ok)
		}

		rw, err := e.svc.Open(ctx, site.ID, userID, sitedb.AccessReadWrite, dbsession.MaxTTL)
		if err != nil {
			t.Fatal(err)
		}
		if u := e.users.TemporaryUsers()[rw.Session.ID]; u.Access != sitedb.AccessReadWrite {
			t.Errorf("read-write user = %+v", u)
		}
		list, err := e.svc.List(ctx, site.ID)
		if err != nil || len(list) != 2 {
			t.Errorf("List = %+v, %v", list, err)
		}
		e.clock.add(dbsession.DefaultTTL)
		if list, err := e.svc.List(ctx, site.ID); err != nil || len(list) != 1 || list[0].ID != rw.Session.ID {
			t.Errorf("List after the first expired = %+v, %v", list, err)
		}
	})
}

func TestOpenRefuses(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, gateway)
		running := e.site(t, store.SiteRunning, true)
		cases := []struct {
			name   string
			siteID string
			access sitedb.Access
			ttl    time.Duration
			want   error
		}{
			{"unknown access", running.ID, "admin", 0, dbsession.ErrInvalidRequest},
			{"too short", running.ID, sitedb.AccessReadOnly, 30 * time.Second, dbsession.ErrInvalidRequest},
			{"too long", running.ID, sitedb.AccessReadOnly, 9 * time.Hour, dbsession.ErrInvalidRequest},
			{"no site", id.New(), sitedb.AccessReadOnly, 0, dbsession.ErrSiteNotFound},
			{"provisioning", e.site(t, store.SiteProvisioning, false).ID, sitedb.AccessReadOnly, 0, dbsession.ErrUnavailable},
			{"deleting", e.site(t, store.SiteDeleting, true).ID, sitedb.AccessReadOnly, 0, dbsession.ErrUnavailable},
			{"never provisioned", e.site(t, store.SiteFailed, false).ID, sitedb.AccessReadOnly, 0, dbsession.ErrUnavailable},
		}
		for _, c := range cases {
			if _, err := e.svc.Open(ctx, c.siteID, id.New(), c.access, c.ttl); !errors.Is(err, c.want) {
				t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
			}
		}
		if n := len(e.users.TemporaryUsers()); n != 0 {
			t.Errorf("%d temporary users created", n)
		}

		off := newEnv(t, db, dbsession.Tunnel{DatabaseAddr: "mariadb:3306"})
		if _, err := off.svc.Open(ctx, running.ID, id.New(), sitedb.AccessReadOnly, 0); !errors.Is(err, dbsession.ErrNoGateway) {
			t.Errorf("without the gateway: err = %v, want ErrNoGateway", err)
		}
	})
}

// A user that can't be created leaves a failed session and nothing on the
// server.
func TestOpenFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, gateway)
		site := e.site(t, store.SiteRunning, true)
		injected := errors.New("mariadb down")
		e.users.Err = func(op, _ string) error {
			if op == "CreateTemporaryUser" {
				return injected
			}
			return nil
		}
		if _, err := e.svc.Open(ctx, site.ID, id.New(), sitedb.AccessReadOnly, 0); !errors.Is(err, injected) {
			t.Fatalf("err = %v, want injected", err)
		}
		all, err := store.NewDatabaseSessions(db).ListUndropped(ctx, 0)
		if err != nil || len(all) != 0 {
			t.Errorf("undropped sessions = %+v, %v", all, err)
		}
		var dropped string
		for _, c := range e.users.Calls() {
			if c.Op == "DropTemporaryUser" {
				dropped = c.Path
			}
		}
		if dropped == "" {
			t.Fatal("the failed session's user wasn't dropped")
		}
		if d := e.session(t, dropped); d.Status != store.DatabaseSessionFailed || d.EndReason != dbsession.ReasonFailed || d.DroppedAt.IsZero() {
			t.Errorf("failed session = %+v", d)
		}

		// A site whose database is gone.
		e.users.Err = nil
		gone := e.site(t, store.SiteRunning, true)
		if err := e.users.DropDatabase(ctx, gone.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.svc.Open(ctx, gone.ID, id.New(), sitedb.AccessReadOnly, 0); !errors.Is(err, dbsession.ErrUnavailable) {
			t.Errorf("without a database: err = %v, want ErrUnavailable", err)
		}
	})
}

func TestEnd(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, gateway)
		site, other := e.site(t, store.SiteRunning, true), e.site(t, store.SiteRunning, true)
		o, err := e.svc.Open(ctx, site.ID, id.New(), sitedb.AccessReadOnly, 0)
		if err != nil {
			t.Fatal(err)
		}
		sid := o.Session.ID
		if _, err := e.svc.End(ctx, other.ID, sid, "admin"); !errors.Is(err, dbsession.ErrSessionNotFound) {
			t.Errorf("ending through another site: err = %v", err)
		}
		if _, err := e.svc.End(ctx, site.ID, id.New(), "admin"); !errors.Is(err, dbsession.ErrSessionNotFound) {
			t.Errorf("ending a missing session: err = %v", err)
		}
		d, err := e.svc.End(ctx, site.ID, sid, "admin")
		if err != nil {
			t.Fatal(err)
		}
		if d.Status != store.DatabaseSessionEnded || d.EndedByUserID != "admin" || d.EndReason != dbsession.ReasonEnded || d.DroppedAt.IsZero() {
			t.Errorf("ended = %+v", d)
		}
		if _, ok := e.users.TemporaryUsers()[sid]; ok {
			t.Error("the ended session's user remains")
		}
		if _, err := e.svc.End(ctx, site.ID, sid, "admin"); !errors.Is(err, dbsession.ErrSessionNotFound) {
			t.Errorf("ending twice: err = %v", err)
		}

		// A failed drop ends the session anyway; the sweep drops the user.
		o2, err := e.svc.Open(ctx, site.ID, id.New(), sitedb.AccessReadOnly, 0)
		if err != nil {
			t.Fatal(err)
		}
		e.users.Err = func(op, _ string) error {
			if op == "DropTemporaryUser" {
				return errors.New("mariadb down")
			}
			return nil
		}
		if d, err := e.svc.End(ctx, site.ID, o2.Session.ID, "admin"); err != nil || d.Status != store.DatabaseSessionEnded || !d.DroppedAt.IsZero() {
			t.Fatalf("End with a failed drop = %+v, %v", d, err)
		}
		if _, err := e.svc.Sweep(ctx); err == nil {
			t.Error("Sweep with MariaDB down succeeded")
		}
		e.users.Err = nil
		res, err := e.svc.Sweep(ctx)
		if err != nil || res.Dropped != 1 {
			t.Errorf("Sweep = %+v, %v", res, err)
		}
		if _, ok := e.users.TemporaryUsers()[o2.Session.ID]; ok || e.session(t, o2.Session.ID).DroppedAt.IsZero() {
			t.Error("the sweep didn't drop the user")
		}
	})
}

func TestSweep(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, gateway)
		site := e.site(t, store.SiteRunning, true)
		userID := id.New()
		short, err := e.svc.Open(ctx, site.ID, userID, sitedb.AccessReadOnly, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		long, err := e.svc.Open(ctx, site.ID, userID, sitedb.AccessReadWrite, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		// A user a crash left behind.
		orphan := id.New()
		e.users.AddTemporaryUser(orphan, sitedbtest.TemporaryUser{SiteID: site.ID, Access: sitedb.AccessReadWrite})

		res, err := e.svc.Sweep(ctx)
		if err != nil || res != (dbsession.SweepResult{Orphans: 1}) {
			t.Errorf("first Sweep = %+v, %v", res, err)
		}
		users := e.users.TemporaryUsers()
		if _, ok := users[orphan]; ok || len(users) != 2 {
			t.Errorf("users after the first sweep = %v", users)
		}

		e.clock.add(time.Minute)
		res, err = e.svc.Sweep(ctx)
		if err != nil || res != (dbsession.SweepResult{Expired: 1, Dropped: 1}) {
			t.Errorf("Sweep at expiry = %+v, %v", res, err)
		}
		d := e.session(t, short.Session.ID)
		if d.Status != store.DatabaseSessionExpired || d.EndReason != dbsession.ReasonExpired || d.DroppedAt.IsZero() || d.EndedByUserID != "" {
			t.Errorf("expired = %+v", d)
		}
		if users := e.users.TemporaryUsers(); len(users) != 1 {
			t.Errorf("users after expiry = %v", users)
		} else if _, ok := users[long.Session.ID]; !ok {
			t.Error("the unexpired session's user was dropped")
		}
		events := e.auditEvents(t, dbsession.ActionExpire)
		if len(events) != 1 {
			t.Fatalf("expiry events = %+v", events)
		}
		ev := events[0]
		if ev.TargetID != site.ID || ev.Result != store.AuditSuccess || ev.Details["session_id"] != short.Session.ID ||
			ev.Details["user_id"] != userID || ev.Details["access"] != "read_only" || ev.Details["reason"] != "expired" {
			t.Errorf("expiry event = %+v", ev)
		}
		for k, v := range ev.Details {
			if strings.Contains(v, short.Credentials.Password.Reveal()) {
				t.Errorf("the password is in the audit event's %s", k)
			}
		}

		// Nothing more to do.
		if res, err := e.svc.Sweep(ctx); err != nil || res != (dbsession.SweepResult{}) {
			t.Errorf("idle Sweep = %+v, %v", res, err)
		}
	})
}

func TestSweepJob(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, gateway)
		for range 2 {
			if err := dbsession.EnsureSweepSchedule(ctx, db); err != nil {
				t.Fatal(err)
			}
		}
		pool := jobs.NewPool(db, jobs.Options{Logger: storetest.Logger()})
		for _, h := range e.svc.Handlers() {
			if err := pool.Register(h); err != nil {
				t.Fatal(err)
			}
		}
		orphan := id.New()
		e.users.AddTemporaryUser(orphan, sitedbtest.TemporaryUser{})
		if _, err := jobs.Enqueue(ctx, db, jobs.Spec{Type: dbsession.SweepJobType, TargetType: "platform", TargetID: "database_sessions"}); err != nil {
			t.Fatal(err)
		}
		if ran, err := pool.RunNext(ctx); err != nil || !ran {
			t.Fatalf("RunNext = %v, %v", ran, err)
		}
		if len(e.users.TemporaryUsers()) != 0 {
			t.Error("the sweep job didn't drop the leftover user")
		}
	})
}

// keys is a KeyChecker with fixed answers.
type keys map[string]auth.Principal

func (k keys) RecheckSSHKey(_ context.Context, keyID string) (auth.Principal, error) {
	p, ok := k[keyID]
	if !ok {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	return p, nil
}

func TestForwarding(t *testing.T) {
	ctx := context.Background()
	storetest.Run(t, func(t *testing.T, db *store.DB) {
		e := newEnv(t, db, gateway)
		site, other := e.site(t, store.SiteRunning, true), e.site(t, store.SiteRunning, true)
		operator, admin := id.New(), id.New()
		k := keys{
			"op-key":    {UserID: operator, Role: store.RoleOperator, Method: auth.MethodSSHKey, SSHKeyID: "op-key"},
			"admin-key": {UserID: admin, Role: store.RoleAdmin, Method: auth.MethodSSHKey, SSHKeyID: "admin-key"},
		}
		policy := e.svc.Forwarding(k)
		conn := func(userID, keyID, siteID string) sftpgw.Identity {
			return sftpgw.Identity{SessionID: id.New(), UserID: userID, SSHKeyID: keyID, SiteID: siteID}
		}
		dial := func(c sftpgw.Identity, host string, port uint32) error {
			t.Helper()
			nc, err := policy.DialForward(ctx, c, host, port)
			if err == nil {
				_ = nc.Close()
			}
			return err
		}
		denied := func(what string, err error) {
			t.Helper()
			if !errors.Is(err, sftpgw.ErrForwardingDenied) {
				t.Errorf("%s: err = %v, want ErrForwardingDenied", what, err)
			}
		}

		opConn := conn(operator, "op-key", site.ID)
		denied("without a session", dial(opConn, "mariadb", 3306))
		ro, err := e.svc.Open(ctx, site.ID, operator, sitedb.AccessReadOnly, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if err := dial(opConn, "mariadb", 3306); err != nil {
			t.Fatalf("with a session: %v", err)
		}
		if err := dial(opConn, "MariaDB", 3306); err != nil {
			t.Errorf("host names ignore case: %v", err)
		}
		if len(e.dialed) != 2 || e.dialed[0] != "tcp mariadb:3306" {
			t.Errorf("dialed %v", e.dialed)
		}
		denied("another host", dial(opConn, "redis", 6379))
		denied("another port", dial(opConn, "mariadb", 3307))
		denied("an address", dial(opConn, "127.0.0.1", 3306))
		denied("another site", dial(conn(operator, "op-key", other.ID), "mariadb", 3306))
		denied("another user", dial(conn(admin, "admin-key", site.ID), "mariadb", 3306))
		denied("a removed key", dial(conn(operator, "gone-key", site.ID), "mariadb", 3306))

		// The connection's deadline is the session's expiry.
		nc, err := policy.DialForward(ctx, opConn, "mariadb", 3306)
		if err != nil {
			t.Fatal(err)
		}
		e.clock.add(time.Hour)
		denied("after expiry", dial(opConn, "mariadb", 3306))
		_ = nc.Close()

		// Ended sessions are refused.
		e.clock.add(-time.Hour)
		if _, err := e.svc.End(ctx, site.ID, ro.Session.ID, operator); err != nil {
			t.Fatal(err)
		}
		denied("after the session ended", dial(opConn, "mariadb", 3306))

		// A read-write session needs an Admin: demoting its user ends it.
		rw, err := e.svc.Open(ctx, site.ID, admin, sitedb.AccessReadWrite, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		adminConn := conn(admin, "admin-key", site.ID)
		if err := dial(adminConn, "mariadb", 3306); err != nil {
			t.Fatalf("admin with a read-write session: %v", err)
		}
		demoted := k["admin-key"]
		demoted.Role = store.RoleOperator
		k["admin-key"] = demoted
		denied("demoted admin", dial(adminConn, "mariadb", 3306))
		if d := e.session(t, rw.Session.ID); d.Status != store.DatabaseSessionEnded || d.EndReason != dbsession.ReasonRevoked || d.DroppedAt.IsZero() {
			t.Errorf("revoked session = %+v", d)
		}
		if _, ok := e.users.TemporaryUsers()[rw.Session.ID]; ok {
			t.Error("the revoked session's user remains")
		}
		if events := e.auditEvents(t, dbsession.ActionRevoke); len(events) != 1 || events[0].Details["session_id"] != rw.Session.ID {
			t.Errorf("revoke events = %+v", events)
		}
	})
}

func TestForwardingDeadline(t *testing.T) {
	db := storetest.Open(t, store.SQLite)
	e := newEnv(t, db, gateway)
	site := e.site(t, store.SiteRunning, true)
	user := id.New()
	o, err := e.svc.Open(context.Background(), site.ID, user, sitedb.AccessReadOnly, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var got time.Time
	svc, err := dbsession.New(dbsession.Options{
		DB: db, Users: e.users, Tunnel: gateway, Now: e.clock.now, Logger: storetest.Logger(),
		Dial: func(context.Context, string, string) (net.Conn, error) {
			c, _ := net.Pipe()
			return &deadlineConn{Conn: c, set: &got}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	k := keys{"k": {UserID: user, Role: store.RoleOperator, Method: auth.MethodSSHKey}}
	nc, err := svc.Forwarding(k).DialForward(context.Background(), sftpgw.Identity{UserID: user, SSHKeyID: "k", SiteID: site.ID}, "mariadb", 3306)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = nc.Close() }()
	if !got.Equal(o.Session.ExpiresAt) {
		t.Errorf("deadline = %v, want the expiry %v", got, o.Session.ExpiresAt)
	}
}

type deadlineConn struct {
	net.Conn
	set *time.Time
}

func (c *deadlineConn) SetDeadline(t time.Time) error {
	*c.set = t
	return c.Conn.SetDeadline(t)
}
