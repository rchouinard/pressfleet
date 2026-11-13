package sftpgw_test

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/sftpgw"
)

// recheckPolicy is echoPolicy that can be told to take its forwards away.
type recheckPolicy struct {
	echoPolicy
	revoked atomic.Bool
}

func (p *recheckPolicy) ForwardsAllowed(context.Context, sftpgw.Identity) (bool, error) {
	return !p.revoked.Load(), nil
}

func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func TestForwardChannelLimit(t *testing.T) {
	addr := echoServer(t)
	policy := &echoPolicy{addr: addr}
	e := newEnv(t, options{forwarding: policy})
	policy.siteID = e.siteA.ID
	signer, _ := e.newKey(e.operator)
	c, err := e.dial("alpha", signer)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	const limit = 16 // maxForwardChannels
	var open []net.Conn
	for i := range limit {
		fc, err := c.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("forward %d: %v", i, err)
		}
		open = append(open, fc)
	}
	if _, err := c.Dial("tcp", addr); err == nil {
		t.Fatal("a forward beyond the limit was accepted")
	}
	_ = open[0].Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		fc, err := c.Dial("tcp", addr)
		if err == nil {
			_ = fc.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("closing a forward didn't free a slot: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestForwardsClosedWhenNoLongerAllowed(t *testing.T) {
	addr := echoServer(t)
	policy := &recheckPolicy{echoPolicy: echoPolicy{addr: addr}}
	e := newEnv(t, options{forwarding: policy})
	policy.siteID = e.siteA.ID
	signer, _ := e.newKey(e.operator)
	conn, sc := e.sftp("alpha", signer)
	defer func() { _ = conn.Close() }()
	fc, err := conn.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(fc, buf); err != nil {
		t.Fatal(err)
	}
	policy.revoked.Store(true)
	_ = fc.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := fc.Read(buf); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the tunnel stayed open after the policy revoked it: %v", err)
	}
	// Only the forwards go; the SFTP session carries on.
	if _, err := sc.ReadDir("/"); err != nil {
		t.Errorf("SFTP after the forwards were closed: %v", err)
	}
}

func TestPreAuthConnectionsPerIPLimited(t *testing.T) {
	e := newEnv(t, options{})
	const limit = 8 // maxPreAuthPerIP
	for i := range limit {
		nc, err := net.Dial("tcp", e.addr)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		defer func() { _ = nc.Close() }()
	}
	// The gateway may not have accepted them all yet, so probe until a
	// connection is dropped: an accepted one is sent the SSH banner, a
	// refused one is closed.
	for range 100 {
		nc, err := net.Dial("tcp", e.addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = nc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, err = nc.Read(make([]byte, 1))
		_ = nc.Close()
		if errors.Is(err, io.EOF) {
			return
		}
	}
	t.Fatal("a connection beyond the per-address limit was not refused")
}
