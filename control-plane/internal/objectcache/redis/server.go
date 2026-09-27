package redis

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/resp"
)

// server is the cache server side of the manager. Every method must be
// idempotent: the manager calls them again on every retry. Tests substitute
// an in-memory fake to exercise the state database side and crash points.
type server interface {
	// ensureUser makes the user exist with siteRules(u, db, hash). If the
	// user didn't exist, db is flushed first: a database only changes hands
	// while it has no user, so whatever it holds belongs to nobody.
	ensureUser(ctx context.Context, u siteUser, db int, hash string) error
	// userDB returns the database a user is confined to, and whether the
	// user exists.
	userDB(ctx context.Context, name string) (db int, exists bool, err error)
	// deleteUser removes the user if it exists.
	deleteUser(ctx context.Context, name string) error
	// flushDB empties a database.
	flushDB(ctx context.Context, db int) error
	// users lists every ACL user.
	users(ctx context.Context) ([]string, error)
	// runID identifies the server process; it changes when the server
	// restarts, which is when it has forgotten every runtime user.
	runID(ctx context.Context) (string, error)
}

// respServer runs commands as the admin user, over a new connection per
// operation: operations are rare (job steps, reconciliation, a poll every
// few seconds), so a pool would only add state.
type respServer struct {
	dial func(ctx context.Context) (*resp.Conn, error)
}

// session runs fn on a new admin connection.
func (s respServer) session(ctx context.Context, fn func(c *resp.Conn) error) error {
	c, err := s.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

func (s respServer) ensureUser(ctx context.Context, u siteUser, db int, hash string) error {
	if !validHash(hash) {
		return fmt.Errorf("ensure user %s: invalid password hash", u)
	}
	return s.session(ctx, func(c *resp.Conn) error {
		v, err := c.Do(ctx, "ACL", "GETUSER", u.name)
		if err != nil {
			return fmt.Errorf("looking up user %s: %w", u, err)
		}
		if v == nil {
			if err := flush(ctx, c, db); err != nil {
				return err
			}
		}
		args := append([]string{"ACL", "SETUSER", u.name}, siteRules(u, db, hash)...)
		if _, err := c.Do(ctx, args...); err != nil {
			return fmt.Errorf("setting user %s: %w", u, err)
		}
		return nil
	})
}

func (s respServer) userDB(ctx context.Context, name string) (int, bool, error) {
	var (
		db     int
		exists bool
	)
	err := s.session(ctx, func(c *resp.Conn) error {
		v, err := c.Do(ctx, "ACL", "GETUSER", name)
		if err != nil {
			return fmt.Errorf("looking up user %s: %w", name, err)
		}
		if v == nil {
			return nil
		}
		exists = true
		db, err = parseUserDB(v)
		return err
	})
	return db, exists, err
}

func (s respServer) deleteUser(ctx context.Context, name string) error {
	return s.session(ctx, func(c *resp.Conn) error {
		if _, err := c.Do(ctx, "ACL", "DELUSER", name); err != nil {
			return fmt.Errorf("deleting user %s: %w", name, err)
		}
		return nil
	})
}

func (s respServer) flushDB(ctx context.Context, db int) error {
	return s.session(ctx, func(c *resp.Conn) error { return flush(ctx, c, db) })
}

// flush empties db. ASYNC frees the memory in the background, so a large
// site doesn't stall the server; the database is empty at once all the
// same.
func flush(ctx context.Context, c *resp.Conn, db int) error {
	if db <= 0 {
		return fmt.Errorf("flushing database %d: not a site database", db)
	}
	if _, err := c.Do(ctx, "SELECT", strconv.Itoa(db)); err != nil {
		return fmt.Errorf("selecting database %d: %w", db, err)
	}
	if _, err := c.Do(ctx, "FLUSHDB", "ASYNC"); err != nil {
		return fmt.Errorf("flushing database %d: %w", db, err)
	}
	return nil
}

func (s respServer) users(ctx context.Context) ([]string, error) {
	var names []string
	err := s.session(ctx, func(c *resp.Conn) error {
		v, err := c.Do(ctx, "ACL", "USERS")
		if err != nil {
			return fmt.Errorf("listing users: %w", err)
		}
		var ok bool
		if names, ok = resp.Strings(v); !ok {
			return fmt.Errorf("listing users: %w: unexpected reply", resp.ErrProtocol)
		}
		return nil
	})
	return names, err
}

func (s respServer) runID(ctx context.Context) (string, error) {
	var id string
	err := s.session(ctx, func(c *resp.Conn) error {
		v, err := c.Do(ctx, "INFO", "server")
		if err != nil {
			return fmt.Errorf("reading server info: %w", err)
		}
		info, _ := resp.String(v)
		id = infoField(info, "run_id")
		if id == "" {
			return fmt.Errorf("reading server info: %w: no run_id", resp.ErrProtocol)
		}
		return nil
	})
	return id, err
}

// parseUserDB finds the "databases" field of an ACL GETUSER reply (a flat
// list of field names and values) and returns the single database a site
// user is confined to, e.g. "db=5". Anything else ("alldbs", several
// databases) isn't a site user's and returns 0.
func parseUserDB(v any) (int, error) {
	fields, ok := v.([]any)
	if !ok {
		return 0, fmt.Errorf("parsing user: %w: unexpected reply", resp.ErrProtocol)
	}
	for i := 0; i+1 < len(fields); i += 2 {
		if name, _ := resp.String(fields[i]); name != "databases" {
			continue
		}
		val, _ := resp.String(fields[i+1])
		s, ok := strings.CutPrefix(val, "db=")
		if !ok {
			return 0, nil
		}
		db, err := strconv.Atoi(s)
		if err != nil {
			return 0, nil //nolint:nilerr // several databases: not a site user
		}
		return db, nil
	}
	return 0, errors.New("parsing user: no databases field (the server must be Valkey 9 or later)")
}

// infoField returns a field of an INFO reply ("name:value" lines).
func infoField(info, name string) string {
	sc := bufio.NewScanner(strings.NewReader(info))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), name+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
