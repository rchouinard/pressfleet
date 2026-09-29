package sftpgw_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/sftpgw"
	"github.com/rchouinard/pressfleet/control-plane/internal/sftpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/store/storetest"
)

// dirRunner stands in for the driver: each site's "worker" serves SFTP
// from its own directory, in the test process.
type dirRunner struct {
	mu    sync.Mutex
	dirs  map[string]string
	calls []string
}

func (r *dirRunner) RunSession(_ context.Context, siteID string, s provisioner.Session) error {
	r.mu.Lock()
	dir, ok := r.dirs[siteID]
	r.calls = append(r.calls, siteID+" "+strings.Join(s.Command, " "))
	r.mu.Unlock()
	if !ok {
		return provisioner.ErrSiteNotFound
	}
	return sftpserver.Serve(s.Stdin, s.Stdout, dir)
}

type env struct {
	t        *testing.T
	db       *store.DB
	auth     *auth.Service
	addr     string
	hostKey  ssh.PublicKey
	runner   *dirRunner
	siteA    store.Site
	siteB    store.Site
	dirA     string
	dirB     string
	operator store.User
	viewer   store.User
}

type options struct {
	idle       time.Duration
	forwarding sftpgw.ForwardingPolicy
}

func newEnv(t *testing.T, o options) *env {
	t.Helper()
	ctx := context.Background()
	db := storetest.Open(t, store.SQLite)
	svc := auth.NewService(db, auth.Options{Logger: storetest.Logger(),
		PasswordParams: auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}})
	e := &env{t: t, db: db, auth: svc, runner: &dirRunner{dirs: map[string]string{}}}

	g := &store.ClientGroup{Name: "Acme"}
	if err := store.NewClientGroups(db).Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	for i, slug := range []string{"alpha", "bravo"} {
		s := &store.Site{ClientGroupID: g.ID, Name: slug, Slug: slug, Status: store.SiteRunning,
			PreviewDomain: slug + ".preview.example", PHPVersion: "8.4", WPCoreVersion: "7.1.2", CPULimit: "1.0",
			MemoryReservationMB: 128, MemoryLimitMB: 512, UID: 20000 + i, WPSaltsSecretRef: "s" + slug}
		if err := store.NewSites(db).Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		e.runner.dirs[s.ID] = dir
		if i == 0 {
			e.siteA, e.dirA = *s, dir
		} else {
			e.siteB, e.dirB = *s, dir
		}
	}
	if err := os.WriteFile(filepath.Join(e.dirB, "secret.txt"), []byte("bravo only"), 0o644); err != nil {
		t.Fatal(err)
	}
	var err error
	if e.operator, err = svc.CreateUser(ctx, auth.NewUser{Email: "op@example.com", Password: "correct horse battery", Role: store.RoleOperator}); err != nil {
		t.Fatal(err)
	}
	if e.viewer, err = svc.CreateUser(ctx, auth.NewUser{Email: "viewer@example.com", Password: "correct horse battery", Role: store.RoleViewer}); err != nil {
		t.Fatal(err)
	}

	pem, err := sftpgw.GenerateHostKey()
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "host_key")
	if err := os.WriteFile(keyFile, pem, 0o600); err != nil {
		t.Fatal(err)
	}
	host, err := sftpgw.LoadHostKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	e.hostKey = host.PublicKey()
	gw, err := sftpgw.New(sftpgw.Options{
		DB: db, Auth: svc, Sessions: e.runner, HostKey: host, Logger: storetest.Logger(),
		IdleTimeout: o.idle, RecheckInterval: 50 * time.Millisecond, Forwarding: o.forwarding,
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.addr = ln.Addr().String()
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- gw.Serve(sctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve = %v", err)
		}
	})
	return e
}

// newKey registers a new key for user and returns its signer.
func (e *env) newKey(user store.User) (ssh.Signer, store.SSHKey) {
	e.t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		e.t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		e.t.Fatal(err)
	}
	k, err := e.auth.AddSSHKey(context.Background(), user.ID, auth.NewSSHKey{PublicKey: string(ssh.MarshalAuthorizedKey(signer.PublicKey()))})
	if err != nil {
		e.t.Fatal(err)
	}
	return signer, k
}

func (e *env) dial(slug string, signer ssh.Signer) (*ssh.Client, error) {
	return ssh.Dial("tcp", e.addr, &ssh.ClientConfig{
		User:            slug,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.FixedHostKey(e.hostKey),
		Timeout:         10 * time.Second,
	})
}

func (e *env) sftp(slug string, signer ssh.Signer) (*ssh.Client, *sftp.Client) {
	e.t.Helper()
	c, err := e.dial(slug, signer)
	if err != nil {
		e.t.Fatalf("dial %s: %v", slug, err)
	}
	s, err := sftp.NewClient(c)
	if err != nil {
		_ = c.Close()
		e.t.Fatalf("sftp %s: %v", slug, err)
	}
	return c, s
}

// events returns the audit events for action, newest first.
func (e *env) events(action string) []store.AuditEvent {
	e.t.Helper()
	page, err := store.NewAuditEvents(e.db).List(context.Background(), store.AuditFilter{Action: action})
	if err != nil {
		e.t.Fatal(err)
	}
	return page.Events
}

// waitEvents waits until action has n events.
func (e *env) waitEvents(action string, n int) []store.AuditEvent {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ev := e.events(action)
		if len(ev) >= n {
			return ev
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%d %s events, want %d", len(ev), action, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSFTPSession(t *testing.T) {
	e := newEnv(t, options{})
	signer, key := e.newKey(e.operator)
	conn, c := e.sftp("alpha", signer)

	f, err := c.Create("/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("hi from sftp")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if b, err := os.ReadFile(filepath.Join(e.dirA, "hello.txt")); err != nil || string(b) != "hi from sftp" {
		t.Errorf("file in site A = %q, %v", b, err)
	}
	// Site B's files are out of reach.
	for _, p := range []string{"/secret.txt", "../secret.txt", "/../" + filepath.Base(e.dirB) + "/secret.txt"} {
		if _, err := c.Stat(p); err == nil {
			t.Errorf("Stat(%q) reached another site's file", p)
		}
	}
	_ = c.Close()
	_ = conn.Close()

	ends := e.waitEvents(sftpgw.ActionDisconnect, 1)
	starts := e.events(sftpgw.ActionConnect)
	if len(starts) != 1 || starts[0].Result != store.AuditSuccess || starts[0].ActorUserID != e.operator.ID ||
		starts[0].TargetID != e.siteA.ID || starts[0].SourceIP != "127.0.0.1" ||
		starts[0].Details["key_fingerprint"] != key.Fingerprint || starts[0].Details["session_id"] == "" {
		t.Errorf("start events = %+v", starts)
	}
	end := ends[0]
	in, _ := strconv.Atoi(end.Details["bytes_received"])
	out, _ := strconv.Atoi(end.Details["bytes_sent"])
	if end.Details["session_id"] != starts[0].Details["session_id"] || in < len("hi from sftp") || out == 0 ||
		end.Details["reason"] != sftpgw.ReasonClosed || end.TargetID != e.siteA.ID || end.ActorUserID != e.operator.ID {
		t.Errorf("end event = %+v", end)
	}
	if got, _ := store.NewSSHKeys(e.db).Get(context.Background(), key.ID); got.LastUsedAt == nil {
		t.Error("the key's use wasn't recorded")
	}
	e.runner.mu.Lock()
	calls := e.runner.calls
	e.runner.mu.Unlock()
	if len(calls) != 1 || calls[0] != e.siteA.ID+" pressfleet sftp-server --root /site/wp-content" {
		t.Errorf("worker calls = %q", calls)
	}
}

func TestSFTPRefused(t *testing.T) {
	e := newEnv(t, options{})
	ctx := context.Background()
	opKey, opRecord := e.newKey(e.operator)
	viewerKey, _ := e.newKey(e.viewer)
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	unknown, _ := ssh.NewSignerFromKey(priv)

	if _, err := e.dial("alpha", unknown); err == nil {
		t.Error("a key nobody registered connected")
	}
	if _, err := e.dial("alpha", viewerKey); err == nil {
		t.Error("a Viewer connected")
	}
	if _, err := e.dial("nosuchsite", opKey); err == nil {
		t.Error("connected to a site that doesn't exist")
	}
	denied := e.events(sftpgw.ActionConnect)
	if len(denied) != 2 || denied[1].Result != store.AuditDenied || denied[1].ActorUserID != e.viewer.ID ||
		denied[1].TargetID != e.siteA.ID || denied[0].Result != store.AuditFailure || denied[0].Details["site_slug"] != "nosuchsite" {
		t.Errorf("refusal events = %+v", denied)
	}

	// A site being deleted is refused.
	if _, err := store.NewSites(e.db).SetStatus(ctx, e.siteB.ID, store.SiteDeleting, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.dial("bravo", opKey); err == nil {
		t.Error("connected to a site being deleted")
	}

	// Disabled users and removed keys can't connect.
	disabled := true
	if _, err := e.auth.UpdateUser(ctx, e.operator.ID, auth.UserChanges{Disabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.dial("alpha", opKey); err == nil {
		t.Error("a disabled user connected")
	}
	enabled := false
	if _, err := e.auth.UpdateUser(ctx, e.operator.ID, auth.UserChanges{Disabled: &enabled}); err != nil {
		t.Fatal(err)
	}
	c, err := e.dial("alpha", opKey)
	if err != nil {
		t.Fatalf("re-enabled user: %v", err)
	}
	_ = c.Close()
	if err := e.auth.DeleteSSHKey(ctx, e.operator.ID, opRecord.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.dial("alpha", opKey); err == nil {
		t.Error("a removed key connected")
	}
}

func TestShellAndForwardingRefused(t *testing.T) {
	e := newEnv(t, options{})
	signer, _ := e.newKey(e.operator)
	c, err := e.dial("alpha", signer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	for _, run := range []func(*ssh.Session) error{
		func(s *ssh.Session) error { return s.Run("id") },
		func(s *ssh.Session) error { return s.Shell() },
		func(s *ssh.Session) error { return s.RequestSubsystem("netconf") },
	} {
		s, err := c.NewSession()
		if err != nil {
			t.Fatal(err)
		}
		if err := run(s); err == nil {
			t.Error("a shell, command, or other subsystem was allowed")
		}
		_ = s.Close()
	}
	if _, err := c.Dial("tcp", "127.0.0.1:22"); err == nil {
		t.Error("port forwarding was allowed")
	} else if oc := (*ssh.OpenChannelError)(nil); !errors.As(err, &oc) || oc.Reason != ssh.Prohibited {
		t.Errorf("port forwarding: %v, want a prohibited channel", err)
	}
	if _, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Error("remote forwarding was allowed")
	}
}

// echoPolicy allows forwarding to one address, for connections to one site,
// the way #115's database policy will.
type echoPolicy struct {
	addr, siteID string
}

func (p echoPolicy) DialForward(ctx context.Context, id sftpgw.Identity, host string, port uint32) (net.Conn, error) {
	if id.SiteID != p.siteID || net.JoinHostPort(host, strconv.Itoa(int(port))) != p.addr {
		return nil, sftpgw.ErrForwardingDenied
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", p.addr)
}

func TestForwardingPolicy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	var e *env
	policy := &echoPolicy{addr: ln.Addr().String()}
	e = newEnv(t, options{forwarding: policy})
	policy.siteID = e.siteA.ID
	signer, _ := e.newKey(e.operator)

	c, err := e.dial("alpha", signer)
	if err != nil {
		t.Fatal(err)
	}
	fc, err := c.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("allowed forwarding: %v", err)
	}
	if _, err := fc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(fc, buf); err != nil || string(buf) != "ping" {
		t.Errorf("echo = %q, %v", buf, err)
	}
	_ = fc.Close()
	if _, err := c.Dial("tcp", "127.0.0.1:1"); err == nil {
		t.Error("forwarding to another address was allowed")
	}
	_ = c.Close()

	c, err = e.dial("bravo", signer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Dial("tcp", ln.Addr().String()); err == nil {
		t.Error("forwarding was allowed for another site")
	}
}

// waitClosed waits for the server to end the connection.
func waitClosed(t *testing.T, c *ssh.Client) {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the gateway didn't end the connection")
	}
}

func TestAccessRevokedEndsSession(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		revoke func(e *env, key store.SSHKey)
	}{
		{"key removed", func(e *env, key store.SSHKey) {
			if err := e.auth.DeleteSSHKey(ctx, e.operator.ID, key.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"user disabled", func(e *env, _ store.SSHKey) {
			disabled := true
			if _, err := e.auth.UpdateUser(ctx, e.operator.ID, auth.UserChanges{Disabled: &disabled}); err != nil {
				t.Fatal(err)
			}
		}},
		{"demoted to viewer", func(e *env, _ store.SSHKey) {
			viewer := store.RoleViewer
			if _, err := e.auth.UpdateUser(ctx, e.operator.ID, auth.UserChanges{Role: &viewer}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, options{})
			signer, key := e.newKey(e.operator)
			conn, c := e.sftp("alpha", signer)
			defer func() { _ = c.Close() }()
			if _, err := c.ReadDir("/"); err != nil {
				t.Fatal(err)
			}
			tt.revoke(e, key)
			waitClosed(t, conn)
			ends := e.waitEvents(sftpgw.ActionDisconnect, 1)
			if ends[0].Details["reason"] != sftpgw.ReasonRevoked {
				t.Errorf("end reason = %q", ends[0].Details["reason"])
			}
		})
	}
}

func TestIdleTimeout(t *testing.T) {
	e := newEnv(t, options{idle: 300 * time.Millisecond})
	signer, _ := e.newKey(e.operator)
	conn, c := e.sftp("alpha", signer)
	defer func() { _ = c.Close() }()
	waitClosed(t, conn)
	if ends := e.waitEvents(sftpgw.ActionDisconnect, 1); ends[0].Details["reason"] != sftpgw.ReasonIdle {
		t.Errorf("end reason = %q", ends[0].Details["reason"])
	}
}
