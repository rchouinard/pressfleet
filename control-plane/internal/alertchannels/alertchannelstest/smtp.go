// Package alertchannelstest provides fakes for testing alert channels
// without a network: an SMTP server that records what it receives, and a
// webhook receiver.
package alertchannelstest

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Mail is one message an [SMTPServer] accepted.
type Mail struct {
	From string
	To   []string
	// Data is the message as received, with dot-stuffing undone.
	Data string
	// Username and Password are what the client authenticated with (SMTP
	// PLAIN), empty if it didn't.
	Username, Password string
	// Encrypted is whether the connection was encrypted when the message
	// was sent.
	Encrypted bool
}

// SMTPMode is how an [SMTPServer] handles encryption.
type SMTPMode int

// SMTP server modes.
const (
	// SMTPPlain offers no encryption.
	SMTPPlain SMTPMode = iota
	// SMTPSTARTTLS offers the STARTTLS extension.
	SMTPSTARTTLS
	// SMTPImplicitTLS wraps the connection in TLS from the start.
	SMTPImplicitTLS
)

// SMTPServer is a minimal SMTP server on a loopback port.
type SMTPServer struct {
	// Host and Port are where the server listens.
	Host string
	Port int
	// RootCAs trusts the server's certificate, which is valid for 127.0.0.1.
	RootCAs *x509.CertPool

	mode    SMTPMode
	tlsConf *tls.Config
	ln      net.Listener

	mu       sync.Mutex
	mails    []Mail
	failData string
}

// NewSMTPServer starts a server in the given mode, closed when the test
// ends.
func NewSMTPServer(t testing.TB, mode SMTPMode) *SMTPServer {
	t.Helper()
	// httptest's certificate is valid for 127.0.0.1; borrow it.
	ts := httptest.NewUnstartedServer(http.NotFoundHandler())
	ts.StartTLS()
	t.Cleanup(ts.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &SMTPServer{
		Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, RootCAs: pool, mode: mode, ln: ln,
		tlsConf: &tls.Config{Certificates: ts.TLS.Certificates, MinVersion: tls.VersionTLS12},
	}
	t.Cleanup(func() { _ = ln.Close() })
	go s.serve()
	return s
}

// FailData makes the server answer DATA with the given reply line, such as
// "451 try again later", until called with the empty string.
func (s *SMTPServer) FailData(reply string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failData = reply
}

// Mails returns the messages accepted so far.
func (s *SMTPServer) Mails() []Mail {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Mail(nil), s.mails...)
}

func (s *SMTPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.session(conn)
	}
}

func (s *SMTPServer) session(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	encrypted := false
	if s.mode == SMTPImplicitTLS {
		conn, encrypted = tls.Server(conn, s.tlsConf), true
	}
	r := bufio.NewReader(conn)
	reply := func(format string, args ...any) { _, _ = fmt.Fprintf(conn, format+"\r\n", args...) }
	reply("220 fake ESMTP")
	var cur Mail
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			reply("250-fake")
			if s.mode == SMTPSTARTTLS && !encrypted {
				reply("250-STARTTLS")
			}
			reply("250-AUTH PLAIN")
			reply("250 OK")
		case "STARTTLS":
			reply("220 go ahead")
			conn, encrypted = tls.Server(conn, s.tlsConf), true
			r = bufio.NewReader(conn)
		case "AUTH":
			_, payload, _ := strings.Cut(arg, " ")
			raw, err := base64.StdEncoding.DecodeString(payload)
			parts := strings.Split(string(raw), "\x00")
			if err != nil || len(parts) != 3 {
				reply("501 bad auth")
				continue
			}
			cur.Username, cur.Password = parts[1], parts[2]
			reply("235 ok")
		case "MAIL":
			_, from, _ := strings.Cut(arg, ":")
			cur.From = strings.Trim(from, "<> ")
			reply("250 ok")
		case "RCPT":
			_, to, _ := strings.Cut(arg, ":")
			cur.To = append(cur.To, strings.Trim(to, "<> "))
			reply("250 ok")
		case "DATA":
			s.mu.Lock()
			fail := s.failData
			s.mu.Unlock()
			if fail != "" {
				reply("%s", fail)
				continue
			}
			reply("354 go")
			var data strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil && err != io.EOF {
					return
				}
				if l == ".\r\n" {
					break
				}
				data.WriteString(strings.TrimPrefix(l, "."))
				if err != nil {
					return
				}
			}
			cur.Data, cur.Encrypted = data.String(), encrypted
			s.mu.Lock()
			s.mails = append(s.mails, cur)
			s.mu.Unlock()
			cur = Mail{Username: cur.Username, Password: cur.Password}
			reply("250 queued")
		case "RSET":
			cur = Mail{Username: cur.Username, Password: cur.Password}
			reply("250 ok")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("502 not implemented")
		}
	}
}
