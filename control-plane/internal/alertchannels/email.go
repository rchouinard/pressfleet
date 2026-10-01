package alertchannels

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// SMTP encryption modes, the same values as the platform's smtp.encryption.
const (
	EncryptionSTARTTLS = "starttls"
	EncryptionTLS      = "tls"
	EncryptionNone     = "none"
)

// Relay is an SMTP server to send through.
type Relay struct {
	Host string
	// Port is the server's port; 0 means the standard one for Encryption.
	Port int
	// Encryption is EncryptionSTARTTLS (the default), EncryptionTLS, or
	// EncryptionNone.
	Encryption string
	Username   string
	Password   redact.Secret
	// FromEmail and FromName are the sender of the messages.
	FromEmail string
	FromName  string
}

// effectivePort returns the port to connect to.
func (r Relay) effectivePort() int {
	if r.Port != 0 {
		return r.Port
	}
	switch r.Encryption {
	case EncryptionTLS:
		return 465
	case EncryptionNone:
		return 25
	}
	return 587
}

// sendEmail delivers m to the recipients through relay. dialer decides which
// addresses may be connected to.
func (s *Service) sendEmail(ctx context.Context, relay Relay, to []string, m Message, dialer *net.Dialer) error {
	if relay.Host == "" || relay.FromEmail == "" || len(to) == 0 {
		return permanent(errors.New("the email channel has no SMTP server, sender, or recipients"))
	}
	body, err := buildEmail(relay, to, m)
	if err != nil {
		return permanent(fmt.Errorf("render email: %w", err))
	}
	addr := net.JoinHostPort(relay.Host, strconv.Itoa(relay.effectivePort()))
	tlsConfig := &tls.Config{ServerName: relay.Host, MinVersion: tls.VersionTLS12, RootCAs: s.rootCAs}

	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		if errors.Is(err, ErrBlockedTarget) {
			return permanent(fmt.Errorf("connect to %s: %w", addr, err))
		}
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if relay.Encryption == EncryptionTLS {
		tc := tls.Client(conn, tlsConfig)
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("TLS handshake with %s: %w", addr, err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, relay.Host)
	if err != nil {
		return fmt.Errorf("SMTP greeting from %s: %w", addr, err)
	}
	defer func() { _ = c.Close() }()
	if relay.Encryption == EncryptionSTARTTLS || relay.Encryption == "" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return permanent(fmt.Errorf("%s doesn't offer STARTTLS; set the channel's encryption to none to send without it", addr))
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("STARTTLS with %s: %w", addr, err)
		}
	}
	if relay.Username != "" {
		if err := c.Auth(plainAuth{username: relay.Username, password: relay.Password}); err != nil {
			return fmt.Errorf("SMTP authentication with %s: %w", addr, err)
		}
	}
	if err := c.Mail(relay.FromEmail); err != nil {
		return fmt.Errorf("SMTP MAIL FROM: %w", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("SMTP RCPT TO %s: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP DATA: %w", err)
	}
	_ = c.Quit()
	return nil
}

// plainAuth is SMTP PLAIN. net/smtp's own refuses to send credentials over
// a connection that isn't encrypted, which is what a channel with
// encryption "none" asks for explicitly (a relay on the same host or
// network).
type plainAuth struct {
	username string
	password redact.Secret
}

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password.Reveal()), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

// buildEmail renders the message: multipart/alternative with a plain-text
// and an HTML part, CRLF line endings, non-ASCII text encoded.
func buildEmail(relay Relay, to []string, m Message) ([]byte, error) {
	htmlBody, err := m.html()
	if err != nil {
		return nil, err
	}
	from := mail.Address{Name: relay.FromName, Address: relay.FromEmail}
	boundary, err := randomHex(12)
	if err != nil {
		return nil, err
	}
	msgID, err := randomHex(12)
	if err != nil {
		return nil, err
	}
	domain := "pressfleet.invalid"
	if _, d, ok := strings.Cut(relay.FromEmail, "@"); ok && d != "" {
		domain = d
	}
	at := m.SentAt
	if at.IsZero() {
		at = time.Now()
	}

	var b bytes.Buffer
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", from.String())
	header("To", strings.Join(to, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", m.subject()))
	header("Date", at.Format(time.RFC1123Z))
	header("Message-ID", "<"+msgID+"@"+domain+">")
	header("X-PressFleet-Event", m.Kind)
	header("Auto-Submitted", "auto-generated")
	header("MIME-Version", "1.0")
	header("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	for _, part := range []struct{ typ, content string }{{"text/plain", m.text()}, {"text/html", htmlBody}} {
		b.WriteString("--" + boundary + "\r\n")
		header("Content-Type", part.typ+`; charset="utf-8"`)
		header("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		qp := quotedprintable.NewWriter(&b)
		if _, err := qp.Write([]byte(strings.ReplaceAll(part.content, "\n", "\r\n"))); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
		b.WriteString("\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
