package resp_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/resp"
)

// server answers each command read from its end of a pipe with the next
// canned reply, and records the commands.
func server(t *testing.T, replies ...string) (*resp.Conn, <-chan []string) {
	t.Helper()
	client, srv := net.Pipe()
	cmds := make(chan []string, len(replies)+1)
	go func() {
		defer func() { _ = srv.Close() }()
		r := bufio.NewReader(srv)
		for _, reply := range replies {
			cmd, err := readCommand(r)
			if err != nil {
				return
			}
			cmds <- cmd
			if _, err := io.WriteString(srv, reply); err != nil {
				return
			}
		}
	}()
	c := resp.NewConn(client, time.Second)
	t.Cleanup(func() { _ = c.Close() })
	return c, cmds
}

// readCommand parses an array of bulk strings, as clients send.
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	out := make([]string, n)
	for i := range out {
		if _, err := r.ReadString('\n'); err != nil { // $<len>
			return nil, err
		}
		arg, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		out[i] = strings.TrimSuffix(arg, "\r\n")
	}
	return out, nil
}

func TestReplies(t *testing.T) {
	tests := []struct {
		name  string
		reply string
		want  any
	}{
		{"simple string", "+OK\r\n", "OK"},
		{"integer", ":42\r\n", int64(42)},
		{"negative integer", ":-1\r\n", int64(-1)},
		{"bulk string", "$5\r\nhe\r\no\r\n", "he\r\no"},
		{"empty bulk string", "$0\r\n\r\n", ""},
		{"null bulk string", "$-1\r\n", nil},
		{"null array", "*-1\r\n", nil},
		{"empty array", "*0\r\n", []any{}},
		{"nested array", "*2\r\n$1\r\n0\r\n*2\r\n$3\r\na:1\r\n:7\r\n", []any{"0", []any{"a:1", int64(7)}}},
		{"error inside an array", "*2\r\n+OK\r\n-ERR no\r\n", []any{"OK", &resp.Error{Message: "ERR no"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cmds := server(t, tt.reply)
			got, err := c.Do(context.Background(), "GET", "k")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("reply = %#v, want %#v", got, tt.want)
			}
			if cmd := <-cmds; !reflect.DeepEqual(cmd, []string{"GET", "k"}) {
				t.Errorf("command = %q", cmd)
			}
		})
	}
}

func TestErrorReply(t *testing.T) {
	c, _ := server(t, "-NOPERM User pf_x has no permissions to run the 'keys' command\r\n", "+PONG\r\n")
	_, err := c.Do(context.Background(), "KEYS", "*")
	var e *resp.Error
	if !errors.As(err, &e) || e.Code() != "NOPERM" || !resp.IsCode(err, "NOPERM") || resp.IsCode(err, "ERR") {
		t.Fatalf("err = %v", err)
	}
	// The connection is still usable after an error reply.
	if got, err := c.Do(context.Background(), "PING"); err != nil || got != "PONG" {
		t.Errorf("PING after error = %v, %v", got, err)
	}
}

func TestProtocolErrors(t *testing.T) {
	for _, reply := range []string{
		"?what\r\n",
		":notanumber\r\n",
		"$-2\r\n",
		"$99999999999\r\n",
		"$3\r\nabcde\r\n",
		"*-5\r\n",
		"\r\n",
		strings.Repeat("*1\r\n", 20) + ":1\r\n",
	} {
		c, _ := server(t, reply)
		if _, err := c.Do(context.Background(), "PING"); !errors.Is(err, resp.ErrProtocol) {
			t.Errorf("reply %q: err = %v, want ErrProtocol", reply, err)
		}
	}
}

// Arguments, which can hold passwords, never appear in errors.
func TestErrorsOmitArguments(t *testing.T) {
	c, _ := server(t) // closes without answering
	_, err := c.Do(context.Background(), "AUTH", "user", "hunter2")
	if err == nil {
		t.Fatal("no error from a closed server")
	}
	if strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "AUTH") {
		t.Errorf("err = %v", err)
	}
}

func TestContextCancelUnblocks(t *testing.T) {
	client, srv := net.Pipe()
	defer func() { _ = srv.Close() }()
	go func() { _, _ = io.Copy(io.Discard, srv) }() // reads, never answers
	c := resp.NewConn(client, time.Minute)
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if _, err := c.Do(ctx, "PING"); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("Do didn't return promptly after cancel")
	}
}

func TestDialAuthenticatesAndSelects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	cmds := make(chan []string, 3)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = nc.Close() }()
		r := bufio.NewReader(nc)
		for _, reply := range []string{"+OK\r\n", "+OK\r\n", "+PONG\r\n"} {
			cmd, err := readCommand(r)
			if err != nil {
				return
			}
			cmds <- cmd
			_, _ = io.WriteString(nc, reply)
		}
	}()
	c, err := resp.Dial(context.Background(), resp.Options{Addr: ln.Addr().String(), Username: "u", Password: "p", DB: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Do(context.Background(), "PING"); err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]string{{"AUTH", "u", "p"}, {"SELECT", "7"}, {"PING"}} {
		if got := <-cmds; !reflect.DeepEqual(got, want) {
			t.Errorf("command = %q, want %q", got, want)
		}
	}
}

func TestDialRejectedAuth(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = nc.Close() }()
		if _, err := readCommand(bufio.NewReader(nc)); err == nil {
			_, _ = io.WriteString(nc, "-WRONGPASS invalid username-password pair or user is disabled.\r\n")
		}
	}()
	_, err = resp.Dial(context.Background(), resp.Options{Addr: ln.Addr().String(), Username: "u", Password: "secret-pw"})
	if !resp.IsCode(err, "WRONGPASS") || strings.Contains(err.Error(), "secret-pw") {
		t.Errorf("err = %v", err)
	}
}

func TestHelpers(t *testing.T) {
	if s, ok := resp.String("x"); !ok || s != "x" {
		t.Error("String")
	}
	if _, ok := resp.String(int64(1)); ok {
		t.Error("String accepted an integer")
	}
	if s, ok := resp.Strings([]any{"a", "b"}); !ok || !reflect.DeepEqual(s, []string{"a", "b"}) {
		t.Error("Strings")
	}
	if _, ok := resp.Strings([]any{"a", int64(1)}); ok {
		t.Error("Strings accepted a mixed array")
	}
}
