// Package sftpgw is the SFTP gateway (design doc §5.8): one SSH server on
// one host port through which developers reach a site's wp-content.
//
// Developers connect with `sftp -P <port> <site-slug>@<dashboard domain>`.
// The SSH username names the site and the public key names the platform
// user (package auth); only key authentication is offered. After the
// client proves it holds a registered key, the gateway authorizes the user
// for the site (authz.ConnectSFTP: Operators and Admins) and records the
// session in the audit log.
//
// For each SFTP subsystem request, the gateway has the driver start a
// short-lived worker for the site (provisioner.SessionRunner), which runs
// `pressfleet sftp-server` as the site's UID with only its wp-content
// mounted, and relays the channel to the worker's standard streams. The
// gateway itself never touches site files, and the worker has no network.
//
// # Channels and requests
//
// The gateway accepts "session" channels, and on them only the "sftp"
// subsystem: shells, commands, terminals, agent and X11 forwarding, and
// environment variables are refused. Remote forwarding (tcpip-forward) and
// every other global request are refused.
//
// Local forwarding ("direct-tcpip" channels, `ssh -L`) is decided by a
// ForwardingPolicy. The default refuses everything. Temporary database
// sessions (package dbsession) plug in a policy that dials the site's database for a
// user with an active session on it; the gateway relays the connection,
// counts its bytes, and ends it with the SSH connection. A policy that
// implements ForwardRechecker also has its open forwards closed when the user
// loses them. Each connection may have maxSFTPChannels SFTP sessions and
// maxForwardChannels forwards open at once.
//
// # Ending sessions
//
// A connection ends when the client leaves, after IdleTimeout without
// traffic, after MaxSessionLength, when the control plane stops, and when
// the user loses access: every RecheckInterval the gateway checks that the
// key is still registered, its user still enabled, and their role still
// allows SFTP, so removing a key, disabling a user, or demoting them to
// Viewer ends their open sessions within seconds. The same check closes
// open port forwards whose ForwardingPolicy (a ForwardRechecker) no longer
// allows them, without ending the connection's SFTP sessions.
//
// # Limits
//
// Connections still authenticating come from their own pool, with a
// per-address share, so idle sockets can't lock signed-in users out. Each
// connection may have a few SFTP sessions and port forwards open at once.
package sftpgw

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Defaults for Options.
const (
	DefaultIdleTimeout      = 15 * time.Minute
	DefaultMaxSessionLength = 8 * time.Hour
	DefaultRecheckInterval  = 10 * time.Second
)

// Limits that protect the control plane; not configurable.
const (
	// handshakeTimeout bounds key exchange and authentication.
	handshakeTimeout = 30 * time.Second
	// maxConnections bounds concurrent authenticated connections.
	maxConnections = 256
	// maxPreAuth bounds connections still shaking hands, in a pool apart
	// from maxConnections so idle sockets can't starve signed-in users;
	// maxPreAuthPerIP bounds one address's share of it.
	maxPreAuth      = 64
	maxPreAuthPerIP = 8
	// maxForwardChannels bounds the port forwards one connection may have
	// open at once; each dials the database.
	maxForwardChannels = 16
	// maxSFTPChannels bounds the SFTP sessions (workers) one connection
	// may run at once. Clients use one; some open a second for transfers.
	maxSFTPChannels = 4
	// callbackTimeout bounds the database work of one authentication step.
	callbackTimeout = 10 * time.Second
	// auditTimeout bounds writing an audit event.
	auditTimeout = 10 * time.Second
)

// Audit actions of SFTP sessions. A session's start, and a refused
// connection, is recorded under the authorization action; its end under
// ActionDisconnect.
const (
	ActionConnect    = string(authz.ConnectSFTP)
	ActionDisconnect = "site.sftp.disconnect"
)

// Reasons a session ended, recorded in the end event's details.
const (
	ReasonClosed    = "client_closed"
	ReasonIdle      = "idle_timeout"
	ReasonMaxLength = "max_session_length"
	ReasonRevoked   = "access_revoked"
	ReasonShutdown  = "shutdown"
)

// DefaultWorkerCommand is the worker's argv: the control plane image's own
// SFTP server, confined to the site's wp-content.
var DefaultWorkerCommand = []string{"pressfleet", "sftp-server", "--root", provisioner.TaskWPContentPath}

// ErrForwardingDenied is returned by a ForwardingPolicy that refuses a
// forwarding request; the gateway rejects the channel as prohibited.
var ErrForwardingDenied = errors.New("sftpgw: port forwarding is not allowed")

// Identity is an authenticated connection: who it is, with which key, to
// which site. Everything a ForwardingPolicy needs to decide.
type Identity struct {
	// SessionID identifies the connection in the audit log and logs.
	SessionID string
	UserID    string
	SSHKeyID  string
	// KeyFingerprint is the key's SHA-256 fingerprint.
	KeyFingerprint string
	SiteID         string
	SiteSlug       string
	// SourceIP is the client's address.
	SourceIP string
}

// ForwardingPolicy decides local port forwarding requests ("direct-tcpip"
// channels, ssh -L) and dials the ones it allows. It is the extension
// point for temporary database sessions (package dbsession), which allow forwarding to
// the site's database only, for a user with an active session on it.
type ForwardingPolicy interface {
	// DialForward returns a connection to host:port for the connection
	// id, or an error wrapping ErrForwardingDenied if it isn't allowed.
	// ctx ends with the SSH connection.
	DialForward(ctx context.Context, id Identity, host string, port uint32) (net.Conn, error)
}

// DenyForwarding refuses every forwarding request: the gateway's policy
// when none is given.
type DenyForwarding struct{}

// DialForward refuses.
func (DenyForwarding) DialForward(context.Context, Identity, string, uint32) (net.Conn, error) {
	return nil, ErrForwardingDenied
}

// ForwardRechecker is an optional extension of a ForwardingPolicy. Every
// RecheckInterval the gateway asks it whether the connection's open forwards
// may continue, and closes them (not the SFTP sessions) if it says no, so a
// user who loses the role a tunnel needs doesn't keep it until it expires.
type ForwardRechecker interface {
	// ForwardsAllowed reports whether id may keep its forwards. An error
	// keeps them: the next check tries again.
	ForwardsAllowed(ctx context.Context, id Identity) (bool, error)
}

// Options configures a Server.
type Options struct {
	// DB holds sites and receives audit events. Required.
	DB *store.DB
	// Auth authenticates SSH keys. Required.
	Auth *auth.Service
	// Sessions starts the per-session workers. Required.
	Sessions provisioner.SessionRunner
	// HostKey is the gateway's SSH host key (LoadHostKey). Required.
	HostKey ssh.Signer
	// WorkerCommand is the worker's argv; nil means DefaultWorkerCommand.
	WorkerCommand []string
	// IdleTimeout ends connections without traffic for this long;
	// MaxSessionLength ends every connection this long after it started;
	// RecheckInterval is how often access is checked again. Zero means
	// the defaults.
	IdleTimeout      time.Duration
	MaxSessionLength time.Duration
	RecheckInterval  time.Duration
	// Forwarding decides port forwarding; nil means DenyForwarding.
	Forwarding ForwardingPolicy
	// Logger is required.
	Logger *slog.Logger
}

// Server is the SFTP gateway.
type Server struct {
	opts   Options
	config *ssh.ServerConfig
	log    *slog.Logger
	// base is the context of Serve, for authentication callbacks, which
	// get none from package ssh. Keeping a context in a struct goes against
	// the usual rule (pass it, don't store it), but the callbacks have no
	// other way to see Serve's cancellation; it is atomic because Serve
	// sets it while earlier connections' callbacks may still read it.
	base atomic.Pointer[context.Context]
}

// New returns a gateway.
func New(opts Options) (*Server, error) {
	var errs []error
	if opts.DB == nil || opts.Auth == nil || opts.Sessions == nil || opts.Logger == nil {
		errs = append(errs, errors.New("a database, auth service, session runner, and logger are required"))
	}
	if opts.HostKey == nil {
		errs = append(errs, errors.New("a host key is required"))
	}
	if opts.IdleTimeout < 0 || opts.MaxSessionLength < 0 || opts.RecheckInterval < 0 {
		errs = append(errs, errors.New("timeouts must not be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("sftp gateway: %w", err)
	}
	if opts.WorkerCommand == nil {
		opts.WorkerCommand = DefaultWorkerCommand
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.MaxSessionLength == 0 {
		opts.MaxSessionLength = DefaultMaxSessionLength
	}
	if opts.RecheckInterval == 0 {
		opts.RecheckInterval = DefaultRecheckInterval
	}
	if opts.Forwarding == nil {
		opts.Forwarding = DenyForwarding{}
	}
	s := &Server{opts: opts, log: opts.Logger}
	s.config = &ssh.ServerConfig{
		ServerVersion:             "SSH-2.0-PressFleet",
		PublicKeyCallback:         s.offeredKey,
		VerifiedPublicKeyCallback: s.provenKey,
		// Only public keys: no passwords, keyboard-interactive, or none.
	}
	s.config.AddHostKey(opts.HostKey)
	ctx := context.Background()
	s.base.Store(&ctx)
	return s, nil
}

// Extensions of ssh.Permissions that carry the Identity from
// authentication to the connection.
const (
	extUserID      = "pressfleet-user-id"
	extKeyID       = "pressfleet-ssh-key-id"
	extFingerprint = "pressfleet-key-fingerprint"
	extSiteID      = "pressfleet-site-id"
	extSiteSlug    = "pressfleet-site-slug"
)

func (s *Server) callbackContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(*s.base.Load(), callbackTimeout)
}

// offeredKey accepts a key a client offers if a user has registered it and
// is enabled. Clients offer keys before proving they hold them, so nothing
// else happens here: authorization and the audit log wait for provenKey.
func (s *Server) offeredKey(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	ctx, cancel := s.callbackContext()
	defer cancel()
	if _, _, err := s.opts.Auth.AuthenticateSSHKey(ctx, key); err != nil {
		if !errors.Is(err, auth.ErrUnauthenticated) {
			s.log.Error("looking up an SSH key failed", "error", err)
		}
		return nil, errors.New("unknown key")
	}
	return &ssh.Permissions{}, nil
}

// provenKey authorizes a client that has proven it holds a registered key
// for the site its username names, and records refusals in the audit log.
func (s *Server) provenKey(meta ssh.ConnMetadata, key ssh.PublicKey, _ *ssh.Permissions, _ string) (*ssh.Permissions, error) {
	ctx, cancel := s.callbackContext()
	defer cancel()
	p, k, err := s.opts.Auth.AuthenticateSSHKey(ctx, key)
	if err != nil {
		return nil, errors.New("unknown key")
	}
	slug := strings.ToLower(meta.User())
	details := map[string]string{"site_slug": slug, "ssh_key_id": k.ID, "key_fingerprint": k.Fingerprint}
	ip := hostOf(meta.RemoteAddr())
	refuse := func(result store.AuditResult, target authz.Target, reason string) (*ssh.Permissions, error) {
		details["reason"] = reason
		s.audit(ctx, store.AuditEvent{ActorUserID: p.UserID, Action: ActionConnect, TargetType: string(target.Type),
			TargetID: target.ID, Result: result, SourceIP: ip, Details: details})
		s.log.Info("SFTP connection refused", "user_id", p.UserID, "site_slug", slug, "source_ip", ip, "reason", reason)
		return nil, errors.New(reason)
	}
	site, err := store.NewSites(s.opts.DB).GetBySlug(ctx, slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return refuse(store.AuditFailure, authz.Target{}, "no such site")
	case err != nil:
		s.log.Error("looking up a site for SFTP failed", "site_slug", slug, "error", err)
		return nil, errors.New("internal error")
	}
	target, err := authz.NewResolver(s.opts.DB).Resolve(ctx, authz.Site(site.ID))
	if err != nil {
		s.log.Error("resolving a site for SFTP failed", "site_id", site.ID, "error", err)
		return nil, errors.New("internal error")
	}
	if err := authz.Check(p, authz.ConnectSFTP, target); err != nil {
		return refuse(store.AuditDenied, target, err.Error())
	}
	if site.Status != store.SiteRunning && site.Status != store.SiteStopped {
		return refuse(store.AuditFailure, target, "the site is "+string(site.Status))
	}
	s.opts.Auth.NoteSSHKeyUsed(ctx, k)
	return &ssh.Permissions{Extensions: map[string]string{
		extUserID: p.UserID, extKeyID: k.ID, extFingerprint: k.Fingerprint,
		extSiteID: site.ID, extSiteSlug: site.Slug,
	}}, nil
}

// ListenAndServe listens on addr and serves until ctx ends.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("sftp gateway: %w", err)
	}
	return s.Serve(ctx, ln)
}

// Serve accepts connections on ln until ctx ends, then closes every open
// connection, ending their workers, and returns once they are done.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.base.Store(&ctx)
	s.log.Info("SFTP gateway listening", "address", ln.Addr().String())
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	var (
		wg    sync.WaitGroup
		slots = make(chan struct{}, maxConnections)
		pre   = newPreAuth()
		err   error
	)
	for {
		nc, aerr := ln.Accept()
		if aerr != nil {
			if ctx.Err() == nil {
				err = fmt.Errorf("sftp gateway: %w", aerr)
			}
			break
		}
		ip := hostOf(nc.RemoteAddr())
		if !pre.acquire(ip) {
			s.log.Warn("SFTP connection refused: too many connections still authenticating", "source_ip", ip)
			_ = nc.Close()
			continue
		}
		wg.Go(func() {
			s.handle(ctx, nc, pre, slots)
		})
	}
	_ = ln.Close()
	wg.Wait()
	s.log.Info("SFTP gateway stopped")
	return err
}

// preAuth limits connections that haven't finished authenticating: in
// total, and per source address.
type preAuth struct {
	mu    sync.Mutex
	total int
	byIP  map[string]int
}

func newPreAuth() *preAuth { return &preAuth{byIP: map[string]int{}} }

// acquire takes a slot for ip, reporting whether there was room.
func (p *preAuth) acquire(ip string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.total >= maxPreAuth || p.byIP[ip] >= maxPreAuthPerIP {
		return false
	}
	p.total++
	p.byIP[ip]++
	return true
}

func (p *preAuth) release(ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total--
	if p.byIP[ip]--; p.byIP[ip] <= 0 {
		delete(p.byIP, ip)
	}
}

// handle runs one connection. It holds a pre-authentication slot of pre
// until the handshake ends, then one of slots for the rest.
func (s *Server) handle(ctx context.Context, nc net.Conn, pre *preAuth, slots chan struct{}) {
	defer func() { _ = nc.Close() }()
	ip := hostOf(nc.RemoteAddr())
	_ = nc.SetDeadline(time.Now().Add(handshakeTimeout))
	sc, chans, reqs, err := ssh.NewServerConn(nc, s.config)
	pre.release(ip)
	if err != nil {
		s.log.Debug("SSH handshake failed", "source_ip", ip, "error", err)
		return
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		s.log.Warn("SFTP connection refused: too many connections", "source_ip", ip)
		_ = sc.Close()
		return
	}
	_ = nc.SetDeadline(time.Time{})
	ext := sc.Permissions.Extensions
	c := &conn{
		srv: s,
		sc:  sc,
		id: Identity{
			SessionID: id.New(), UserID: ext[extUserID], SSHKeyID: ext[extKeyID], KeyFingerprint: ext[extFingerprint],
			SiteID: ext[extSiteID], SiteSlug: ext[extSiteSlug], SourceIP: hostOf(sc.RemoteAddr()),
		},
		started: time.Now(),
	}
	c.touch()
	c.ctx, c.cancel = context.WithCancel(ctx)
	defer c.cancel()
	c.run(chans, reqs)
}

// audit writes an audit event; failures are logged, never returned.
func (s *Server) audit(ctx context.Context, e store.AuditEvent) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	defer cancel()
	if err := store.NewAuditEvents(s.opts.DB).Insert(ctx, &e); err != nil {
		s.log.Error("writing audit event failed", "action", e.Action, "error", err)
	}
}

// hostOf returns the IP address of a network address.
func hostOf(a net.Addr) string {
	if a == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return host
}

// conn is one authenticated connection.
type conn struct {
	srv     *Server
	sc      *ssh.ServerConn
	id      Identity
	started time.Time
	ctx     context.Context
	cancel  context.CancelFunc

	bytesIn, bytesOut atomic.Int64
	lastActive        atomic.Int64 // unix nanoseconds
	sftpChannels      atomic.Int32
	forwardChannels   atomic.Int32

	// closers close the open forwards; see closeForwards.
	forwardMu  sync.Mutex
	forwardSeq int
	closers    map[int]func()

	reasonOnce sync.Once
	reason     string
}

// touch records traffic, for the idle timeout.
func (c *conn) touch() { c.lastActive.Store(time.Now().UnixNano()) }

// end closes the connection for reason; the first reason sticks.
func (c *conn) end(reason string) {
	c.reasonOnce.Do(func() { c.reason = reason })
	_ = c.sc.Close()
}

func (c *conn) details() map[string]string {
	return map[string]string{
		"session_id": c.id.SessionID, "site_slug": c.id.SiteSlug,
		"ssh_key_id": c.id.SSHKeyID, "key_fingerprint": c.id.KeyFingerprint,
	}
}

func (c *conn) run(chans <-chan ssh.NewChannel, reqs <-chan *ssh.Request) {
	s := c.srv
	s.audit(c.ctx, store.AuditEvent{ActorUserID: c.id.UserID, Action: ActionConnect, TargetType: string(authz.TargetSite),
		TargetID: c.id.SiteID, Result: store.AuditSuccess, SourceIP: c.id.SourceIP, Details: c.details()})
	s.log.Info("SFTP session started", "session_id", c.id.SessionID, "user_id", c.id.UserID,
		"site_id", c.id.SiteID, "source_ip", c.id.SourceIP)

	var wg sync.WaitGroup
	wg.Go(func() { ssh.DiscardRequests(reqs) }) // tcpip-forward and the like
	watchdogDone := make(chan struct{})
	wg.Go(func() { c.watchdog(watchdogDone) })
	for nch := range chans {
		c.touch()
		switch nch.ChannelType() {
		case "session":
			if c.sftpChannels.Add(1) > maxSFTPChannels {
				c.sftpChannels.Add(-1)
				_ = nch.Reject(ssh.ResourceShortage, "too many sessions on this connection")
				continue
			}
			wg.Go(func() {
				defer c.sftpChannels.Add(-1)
				c.session(nch)
			})
		case "direct-tcpip":
			if c.forwardChannels.Add(1) > maxForwardChannels {
				c.forwardChannels.Add(-1)
				_ = nch.Reject(ssh.ResourceShortage, "too many forwards on this connection")
				continue
			}
			wg.Go(func() {
				defer c.forwardChannels.Add(-1)
				c.forward(nch)
			})
		default:
			_ = nch.Reject(ssh.UnknownChannelType, "only SFTP is offered")
		}
	}
	// The client left, or end closed the connection.
	c.reasonOnce.Do(func() { c.reason = ReasonClosed })
	close(watchdogDone)
	c.cancel()
	wg.Wait()

	details := c.details()
	details["bytes_received"] = strconv.FormatInt(c.bytesIn.Load(), 10)
	details["bytes_sent"] = strconv.FormatInt(c.bytesOut.Load(), 10)
	details["duration_seconds"] = strconv.FormatInt(int64(time.Since(c.started).Seconds()), 10)
	details["reason"] = c.reason
	s.audit(c.ctx, store.AuditEvent{ActorUserID: c.id.UserID, Action: ActionDisconnect, TargetType: string(authz.TargetSite),
		TargetID: c.id.SiteID, Result: store.AuditSuccess, SourceIP: c.id.SourceIP, Details: details})
	s.log.Info("SFTP session ended", "session_id", c.id.SessionID, "reason", c.reason,
		"bytes_received", c.bytesIn.Load(), "bytes_sent", c.bytesOut.Load())
}

// watchdog ends the connection on timeouts, shutdown, and lost access.
func (c *conn) watchdog(done <-chan struct{}) {
	o := c.srv.opts
	maxTimer := time.NewTimer(o.MaxSessionLength)
	defer maxTimer.Stop()
	tick := time.NewTicker(min(o.RecheckInterval, o.IdleTimeout))
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-c.ctx.Done():
			c.end(ReasonShutdown)
			return
		case <-maxTimer.C:
			c.end(ReasonMaxLength)
			return
		case <-tick.C:
			if time.Since(time.Unix(0, c.lastActive.Load())) >= o.IdleTimeout {
				c.end(ReasonIdle)
				return
			}
			if !c.stillAllowed() {
				c.end(ReasonRevoked)
				return
			}
			c.recheckForwards()
		}
	}
}

// recheckForwards closes the open forwards if the ForwardingPolicy says the
// user may no longer have them. Errors keep them open, as in stillAllowed.
func (c *conn) recheckForwards() {
	rc, ok := c.srv.opts.Forwarding.(ForwardRechecker)
	if !ok || c.forwardChannels.Load() == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, callbackTimeout)
	defer cancel()
	allowed, err := rc.ForwardsAllowed(ctx, c.id)
	switch {
	case err != nil:
		c.srv.log.Warn("rechecking port forwarding failed", "session_id", c.id.SessionID, "error", err)
	case !allowed:
		c.srv.log.Info("port forwards closed: access revoked", "session_id", c.id.SessionID, "user_id", c.id.UserID)
		c.closeForwards()
	}
}

// closeForwards closes every open forward.
func (c *conn) closeForwards() {
	c.forwardMu.Lock()
	fns := make([]func(), 0, len(c.closers))
	for _, fn := range c.closers {
		fns = append(fns, fn)
	}
	c.forwardMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// trackForward registers a closer for an open forward and returns the
// function that unregisters it.
func (c *conn) trackForward(closer func()) func() {
	c.forwardMu.Lock()
	defer c.forwardMu.Unlock()
	if c.closers == nil {
		c.closers = map[int]func(){}
	}
	c.forwardSeq++
	n := c.forwardSeq
	c.closers[n] = closer
	return func() {
		c.forwardMu.Lock()
		delete(c.closers, n)
		c.forwardMu.Unlock()
	}
}

// stillAllowed reports whether the key, its user, and their role still
// allow the connection. Database errors keep it open: an outage mustn't
// cut every developer off, and the next check tries again.
func (c *conn) stillAllowed() bool {
	ctx, cancel := context.WithTimeout(c.ctx, callbackTimeout)
	defer cancel()
	p, err := c.srv.opts.Auth.RecheckSSHKey(ctx, c.id.SSHKeyID)
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		return false
	case err != nil:
		c.srv.log.Warn("rechecking SFTP access failed", "session_id", c.id.SessionID, "error", err)
		return true
	}
	target, err := authz.NewResolver(c.srv.opts.DB).Resolve(ctx, authz.Site(c.id.SiteID))
	if err != nil {
		c.srv.log.Warn("rechecking SFTP access failed", "session_id", c.id.SessionID, "error", err)
		return true
	}
	return authz.Check(p, authz.ConnectSFTP, target) == nil
}

// session serves a "session" channel: the sftp subsystem and nothing else.
func (c *conn) session(nch ssh.NewChannel) {
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	defer func() { _ = ch.Close() }()
	started := false
	var worker sync.WaitGroup
	for req := range reqs {
		switch req.Type {
		case "subsystem":
			var sub struct{ Name string }
			if err := ssh.Unmarshal(req.Payload, &sub); err == nil && sub.Name == "sftp" && !started {
				started = true
				_ = req.Reply(true, nil)
				worker.Go(func() { c.sftp(ch) })
				continue
			}
			_ = req.Reply(false, nil)
		case "shell", "exec":
			_, _ = fmt.Fprintf(ch.Stderr(), "This gateway only offers SFTP: sftp -P <port> %s@<host>\r\n", c.id.SiteSlug)
			_ = req.Reply(false, nil)
			if !started {
				return
			}
		default: // pty-req, env, x11-req, auth-agent-req@openssh.com, ...
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
	worker.Wait()
}

// sftp relays the channel to a worker for the site until either ends.
func (c *conn) sftp(ch ssh.Channel) {
	var stderr strings.Builder
	err := c.srv.opts.Sessions.RunSession(c.ctx, c.id.SiteID, provisioner.Session{
		Command: c.srv.opts.WorkerCommand,
		Stdin:   &counted{r: ch, n: &c.bytesIn, touch: c.touch},
		Stdout:  &counted{w: ch, n: &c.bytesOut, touch: c.touch},
		Stderr:  &limited{b: &stderr, max: 4096},
	})
	status := uint32(0)
	if err != nil {
		status = 1
		if c.ctx.Err() == nil {
			c.srv.log.Warn("SFTP worker failed", "session_id", c.id.SessionID, "site_id", c.id.SiteID,
				"error", err, "stderr", stderr.String())
		}
	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	_ = ch.CloseWrite()
	_ = ch.Close()
}

// forward serves a "direct-tcpip" channel if the ForwardingPolicy allows
// it.
func (c *conn) forward(nch ssh.NewChannel) {
	var req struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if err := ssh.Unmarshal(nch.ExtraData(), &req); err != nil {
		_ = nch.Reject(ssh.ConnectionFailed, "malformed forwarding request")
		return
	}
	target, err := c.srv.opts.Forwarding.DialForward(c.ctx, c.id, req.Host, req.Port)
	if err != nil {
		if errors.Is(err, ErrForwardingDenied) {
			_ = nch.Reject(ssh.Prohibited, "port forwarding is not allowed")
		} else {
			c.srv.log.Warn("port forwarding failed", "session_id", c.id.SessionID, "error", err)
			_ = nch.Reject(ssh.ConnectionFailed, "connection failed")
		}
		return
	}
	defer func() { _ = target.Close() }()
	ch, reqs, err := nch.Accept()
	if err != nil {
		return
	}
	defer func() { _ = ch.Close() }()
	go ssh.DiscardRequests(reqs)
	closeBoth := func() { _ = target.Close(); _ = ch.Close() }
	stop := context.AfterFunc(c.ctx, closeBoth)
	defer stop()
	defer c.trackForward(closeBoth)()
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = copyCounted(target, ch, &c.bytesIn, c.touch)
		if cw, ok := target.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	})
	_, _ = copyCounted(ch, target, &c.bytesOut, c.touch)
	_ = ch.CloseWrite()
	wg.Wait()
}

// counted counts the bytes through a reader or writer and records
// activity.
type counted struct {
	r     interface{ Read([]byte) (int, error) }
	w     interface{ Write([]byte) (int, error) }
	n     *atomic.Int64
	touch func()
}

func (c *counted) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.n.Add(int64(n))
		c.touch()
	}
	return n, err
}

func (c *counted) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 {
		c.n.Add(int64(n))
		c.touch()
	}
	return n, err
}

func copyCounted(dst interface{ Write([]byte) (int, error) }, src interface{ Read([]byte) (int, error) }, n *atomic.Int64, touch func()) (int64, error) {
	buf := make([]byte, 32<<10)
	var total int64
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			total += int64(nw)
			n.Add(int64(nw))
			touch()
			if werr != nil {
				return total, werr
			}
		}
		if rerr != nil {
			return total, rerr
		}
	}
}

// limited keeps the first max bytes written to it.
type limited struct {
	b   *strings.Builder
	max int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.max - l.b.Len(); room > 0 {
		l.b.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// GenerateHostKey returns a new Ed25519 host key in OpenSSH PEM format, as
// `pressfleet init` writes it.
func GenerateHostKey() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "pressfleet sftp gateway")
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}

// LoadHostKey reads a host key file (OpenSSH or PEM private key).
func LoadHostKey(path string) (ssh.Signer, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the configured host key file
	if err != nil {
		return nil, fmt.Errorf("reading the SFTP gateway host key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("parsing the SFTP gateway host key %s: %w", path, err)
	}
	return signer, nil
}
