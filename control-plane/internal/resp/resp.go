// Package resp is a minimal client for the Redis serialization protocol
// (RESP2), enough for the control plane to administer the shared object
// cache (Valkey) and for tests to act as a site.
//
// It exists instead of a Redis client module because the control plane
// sends a handful of administrative commands (ACL, SELECT, FLUSHDB, INFO)
// over short-lived connections: no pooling, pipelining, cluster support,
// or typed command API is needed, and the whole protocol subset fits in one
// small, fully tested file (CLAUDE.md: prefer the standard library).
//
// A [Conn] is not safe for concurrent use. Command arguments are never
// included in errors, so a password sent with AUTH or ACL SETUSER can't
// leak through one.
package resp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// Limits on what a reply may contain. The control plane's replies are
// small (ACL rules, INFO sections); the limits only stop a broken or
// hostile peer from making the client allocate without bound.
const (
	maxBulkLen  = 64 << 20
	maxArrayLen = 1 << 20
	maxDepth    = 8
	maxLineLen  = 64 << 10
)

// ErrProtocol means the server sent something that isn't valid RESP2.
var ErrProtocol = errors.New("resp: protocol error")

// Error is an error reply from the server, such as
// "NOPERM User x has no permissions to run the 'keys' command".
type Error struct {
	// Message is the reply without its leading '-'.
	Message string
}

func (e *Error) Error() string { return "resp: server error: " + e.Message }

// Code returns the error's first word, e.g. "NOPERM", "WRONGPASS", "ERR".
func (e *Error) Code() string {
	code, _, _ := strings.Cut(e.Message, " ")
	return code
}

// IsCode reports whether err is a server error reply with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code() == code
}

// Options configure Dial.
type Options struct {
	// Addr is the server's host:port.
	Addr string
	// Username and Password authenticate the connection with AUTH; an empty
	// Username sends no AUTH.
	Username string
	Password redact.Secret
	// DB, if not zero, is selected after authenticating.
	DB int
	// Timeout bounds dialing and, when the context has no earlier
	// deadline, each command; zero means 5 seconds.
	Timeout time.Duration
}

// Conn is a connection to a server.
type Conn struct {
	c       net.Conn
	r       *bufio.Reader
	timeout time.Duration
}

// Dial connects to the server, authenticates, and selects the database.
func Dial(ctx context.Context, opts Options) (*Conn, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	nc, err := d.DialContext(ctx, "tcp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("resp: connecting to %s: %w", opts.Addr, err)
	}
	c := &Conn{c: nc, r: bufio.NewReader(nc), timeout: timeout}
	if opts.Username != "" {
		if _, err := c.Do(ctx, "AUTH", opts.Username, opts.Password.Reveal()); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("resp: authenticating as %s: %w", opts.Username, err)
		}
	}
	if opts.DB != 0 {
		if _, err := c.Do(ctx, "SELECT", strconv.Itoa(opts.DB)); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("resp: selecting database %d: %w", opts.DB, err)
		}
	}
	return c, nil
}

// NewConn wraps an established connection, for tests with an in-memory
// server.
func NewConn(nc net.Conn, timeout time.Duration) *Conn {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Conn{c: nc, r: bufio.NewReader(nc), timeout: timeout}
}

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Close() }

// Do sends a command and returns its reply: a string for simple strings
// and bulk strings, an int64 for integers, a []any for arrays, and nil for
// null replies. An error reply is returned as an *Error; the connection
// stays usable. Any other error (I/O, protocol) leaves the connection in an
// unknown state, and it should be closed.
func (c *Conn) Do(ctx context.Context, args ...string) (any, error) {
	if len(args) == 0 {
		return nil, errors.New("resp: empty command")
	}
	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.c.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("resp: %w", err)
	}
	// Abort a blocked read or write when ctx is canceled.
	stop := context.AfterFunc(ctx, func() { _ = c.c.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if _, err := c.c.Write(encode(args)); err != nil {
		return nil, c.ioError(ctx, args[0], err)
	}
	v, err := c.read(0)
	if err != nil {
		var serverErr *Error
		if errors.As(err, &serverErr) {
			return nil, err
		}
		return nil, c.ioError(ctx, args[0], err)
	}
	return v, nil
}

func (c *Conn) ioError(ctx context.Context, cmd string, err error) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return fmt.Errorf("resp: %s: %w", strings.ToUpper(cmd), err)
}

// encode renders a command as an array of bulk strings.
func encode(args []string) []byte {
	n := 16
	for _, a := range args {
		n += len(a) + 16
	}
	b := make([]byte, 0, n)
	b = append(b, '*')
	b = strconv.AppendInt(b, int64(len(args)), 10)
	b = append(b, '\r', '\n')
	for _, a := range args {
		b = append(b, '$')
		b = strconv.AppendInt(b, int64(len(a)), 10)
		b = append(b, '\r', '\n')
		b = append(b, a...)
		b = append(b, '\r', '\n')
	}
	return b
}

// read reads one reply. An error reply nested in an array (EXEC, scripts)
// is returned as an *Error value inside the array rather than failing it.
func (c *Conn) read(depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: replies nested too deeply", ErrProtocol)
	}
	line, err := c.line()
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		return nil, fmt.Errorf("%w: empty reply line", ErrProtocol)
	}
	body := string(line[1:])
	switch line[0] {
	case '+':
		return body, nil
	case '-':
		return nil, &Error{Message: body}
	case ':':
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid integer", ErrProtocol)
		}
		return n, nil
	case '$':
		n, err := strconv.Atoi(body)
		switch {
		case err != nil || n < -1 || n > maxBulkLen:
			return nil, fmt.Errorf("%w: invalid bulk length", ErrProtocol)
		case n == -1:
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return nil, err
		}
		if buf[n] != '\r' || buf[n+1] != '\n' {
			return nil, fmt.Errorf("%w: bulk string not terminated", ErrProtocol)
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		switch {
		case err != nil || n < -1 || n > maxArrayLen:
			return nil, fmt.Errorf("%w: invalid array length", ErrProtocol)
		case n == -1:
			return nil, nil
		}
		out := make([]any, n)
		for i := range out {
			v, err := c.read(depth + 1)
			var serverErr *Error
			switch {
			case errors.As(err, &serverErr):
				out[i] = serverErr
			case err != nil:
				return nil, err
			default:
				out[i] = v
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: unknown reply type %q", ErrProtocol, line[0])
	}
}

// line reads a CRLF-terminated line without the CRLF.
func (c *Conn) line() ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := c.r.ReadLine()
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > maxLineLen {
			return nil, fmt.Errorf("%w: line too long", ErrProtocol)
		}
		if !isPrefix {
			return line, nil
		}
	}
}

// String returns v as a string if it is one.
func String(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// Strings returns v as a []string if it is an array of strings.
func Strings(v any) ([]string, bool) {
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out[i] = s
	}
	return out, true
}
