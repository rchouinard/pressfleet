package redis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/objectcache"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/resp"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Sentinel errors.
var (
	// ErrInvalidSiteID means a site ID is not a well-formed ID, so no user
	// name or key prefix can be built from it.
	ErrInvalidSiteID = errors.New("redis: invalid site id")
	// ErrNoFreeDatabase means every logical database already belongs to a
	// site.
	ErrNoFreeDatabase = errors.New("redis: no free object cache database")
)

// Defaults for Config.
const (
	DefaultPollInterval   = 10 * time.Second
	DefaultResyncInterval = 10 * time.Minute
)

// Config locates the cache server and its admin user.
type Config struct {
	// Addr is the server's host:port.
	Addr string
	// User is the admin ACL user.
	User string
	// PasswordFile holds the admin password. It is read for every new
	// connection; a trailing newline is ignored.
	PasswordFile string
	// Databases is the server's number of logical databases
	// (platform.RedisDatabases). Sites get databases 1 to Databases-1.
	Databases int
	// PollInterval is how often Watch checks whether the server restarted;
	// zero means DefaultPollInterval.
	PollInterval time.Duration
	// ResyncInterval is how often Watch reconciles even without a restart;
	// zero means DefaultResyncInterval.
	ResyncInterval time.Duration
}

// Manager implements objectcache.Manager on the shared cache server.
type Manager struct {
	server    server
	state     *store.DB
	secrets   secrets.Store
	databases int
	log       *slog.Logger

	pollInterval   time.Duration
	resyncInterval time.Duration
	now            func() time.Time
}

var _ objectcache.Manager = (*Manager)(nil)

// dialTimeout bounds connecting to the server and each command.
const dialTimeout = 5 * time.Second

// New returns a Manager for the server described by cfg that records
// credentials in state, encrypted with secretStore. It doesn't connect;
// connections are opened when needed, so New succeeds while the server is
// down.
func New(cfg Config, state *store.DB, secretStore secrets.Store, log *slog.Logger) (*Manager, error) {
	if cfg.Addr == "" || cfg.User == "" || cfg.PasswordFile == "" {
		return nil, errors.New("redis: address, admin user, and admin password file are required")
	}
	if cfg.Databases < 2 {
		return nil, fmt.Errorf("redis: need at least 2 databases, got %d", cfg.Databases)
	}
	if cfg.PollInterval < 0 || cfg.ResyncInterval < 0 {
		return nil, errors.New("redis: intervals must not be negative")
	}
	srv := respServer{dial: func(ctx context.Context) (*resp.Conn, error) {
		pw, err := readPasswordFile(cfg.PasswordFile)
		if err != nil {
			return nil, err
		}
		return resp.Dial(ctx, resp.Options{Addr: cfg.Addr, Username: cfg.User, Password: pw, Timeout: dialTimeout})
	}}
	m := newManager(srv, state, secretStore, cfg.Databases, log)
	if cfg.PollInterval > 0 {
		m.pollInterval = cfg.PollInterval
	}
	if cfg.ResyncInterval > 0 {
		m.resyncInterval = cfg.ResyncInterval
	}
	return m, nil
}

func newManager(srv server, state *store.DB, secretStore secrets.Store, databases int, log *slog.Logger) *Manager {
	return &Manager{
		server: srv, state: state, secrets: secretStore, databases: databases, log: log,
		pollInterval: DefaultPollInterval, resyncInterval: DefaultResyncInterval, now: time.Now,
	}
}

// maxPasswordFileSize bounds how much of the password file is read.
const maxPasswordFileSize = 4096

// readPasswordFile reads the admin password. Errors never include the
// file's contents.
func readPasswordFile(path string) (redact.Secret, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the path comes from operator configuration
	if err != nil {
		return "", fmt.Errorf("reading Redis admin password: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxPasswordFileSize+1))
	if err != nil {
		return "", fmt.Errorf("reading Redis admin password: %w", err)
	}
	if len(b) > maxPasswordFileSize {
		return "", fmt.Errorf("reading Redis admin password: %s is larger than %d bytes", path, maxPasswordFileSize)
	}
	b = bytes.TrimRight(b, "\r\n")
	if len(b) == 0 {
		return "", fmt.Errorf("reading Redis admin password: %s is empty", path)
	}
	return redact.Secret(b), nil
}

// Ping checks that the server is reachable with the admin credentials.
func (m *Manager) Ping(ctx context.Context) error {
	_, err := m.server.runID(ctx)
	return err
}

// CreateUser implements objectcache.Manager. It is idempotent; see the
// package documentation for the steps and why each retry is safe.
func (m *Manager) CreateUser(ctx context.Context, siteID string) (string, error) {
	u, err := newSiteUser(siteID)
	if err != nil {
		return "", fmt.Errorf("create object cache user: %w", err)
	}
	rec, creds, created, err := m.credentials(ctx, u)
	if err != nil {
		return "", fmt.Errorf("create object cache user for site %s: %w", siteID, err)
	}
	if err := m.server.ensureUser(ctx, u, rec.DB, passwordHash(creds.Password)); err != nil {
		return "", fmt.Errorf("create object cache user for site %s: %w", siteID, err)
	}
	// Converging an existing user is routine (every update job, every
	// reconciliation), so only a new one is worth an info line.
	level := slog.LevelDebug
	if created {
		level = slog.LevelInfo
	}
	m.log.Log(ctx, level, "site object cache user ready", "site_id", siteID, "user", u.String(), "database", rec.DB)
	return string(rec.SecretRef), nil
}

// credentials returns the site's record and stored credentials, creating
// them if the site has none.
func (m *Manager) credentials(ctx context.Context, u siteUser) (rec record, creds objectcache.Credentials, created bool, err error) {
	rec, err = getRecord(ctx, m.state, u.siteID)
	if errors.Is(err, store.ErrNotFound) {
		rec, err = m.createRecord(ctx, u)
		created = true
	}
	if err != nil {
		return record{}, objectcache.Credentials{}, false, err
	}
	creds, err = m.recordCredentials(ctx, u, rec)
	return rec, creds, created, err
}

// recordCredentials reads and checks the credentials a record points to.
func (m *Manager) recordCredentials(ctx context.Context, u siteUser, rec record) (objectcache.Credentials, error) {
	if rec.Username != u.name || rec.DB <= 0 || rec.DB >= m.databases {
		return objectcache.Credentials{}, fmt.Errorf("stored user %q and database %d don't fit %s", rec.Username, rec.DB, u)
	}
	plain, err := m.secrets.Get(ctx, rec.SecretRef)
	if err != nil {
		return objectcache.Credentials{}, fmt.Errorf("reading credentials: %w", err)
	}
	creds, err := objectcache.UnmarshalCredentials(plain)
	if err != nil {
		return objectcache.Credentials{}, err
	}
	if creds.Username != u.name || creds.Database != rec.DB || creds.KeyPrefix != objectcache.KeyPrefix(u.siteID) {
		return objectcache.Credentials{}, fmt.Errorf("stored credentials are for user %q, database %d, and prefix %q, not %s", creds.Username, creds.Database, creds.KeyPrefix, u)
	}
	return creds, nil
}

// allocAttempts bounds retries when concurrent creations pick the same
// free database.
const allocAttempts = 5

// createRecord allocates the lowest free database, generates credentials,
// and stores them with the site's row in one transaction.
func (m *Manager) createRecord(ctx context.Context, u siteUser) (record, error) {
	for attempt := 1; ; attempt++ {
		rec, err := m.tryCreateRecord(ctx, u)
		if !errors.Is(err, store.ErrConflict) {
			return rec, err
		}
		// Either a concurrent CreateUser for this site committed first
		// (use its record; our secret rolled back with the transaction), or
		// another site took the database we picked (pick again).
		if rec, err := getRecord(ctx, m.state, u.siteID); err == nil {
			return rec, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return record{}, err
		}
		if attempt == allocAttempts {
			return record{}, fmt.Errorf("allocating a database: %w", err)
		}
	}
}

func (m *Manager) tryCreateRecord(ctx context.Context, u siteUser) (record, error) {
	var rec record
	err := m.state.InTx(ctx, func(q store.Querier) error {
		used, err := usedDatabases(ctx, q)
		if err != nil {
			return err
		}
		db := lowestFree(used, m.databases)
		if db == 0 {
			return fmt.Errorf("%w (all %d are in use)", ErrNoFreeDatabase, m.databases-1)
		}
		plain, err := objectcache.MarshalCredentials(objectcache.Credentials{
			Username: u.name, Password: newPassword(), Database: db, KeyPrefix: objectcache.KeyPrefix(u.siteID),
		})
		if err != nil {
			return err
		}
		ref, err := m.secrets.With(q).Put(ctx, secrets.KindSiteRedisPassword, plain)
		if err != nil {
			return err
		}
		rec = record{SiteID: u.siteID, Username: u.name, DB: db, SecretRef: ref, CreatedAt: m.now()}
		return insertRecord(ctx, q, rec)
	})
	if err != nil {
		return record{}, fmt.Errorf("storing credentials: %w", err)
	}
	return rec, nil
}

// lowestFree returns the lowest database in [1, databases) not in used
// (sorted ascending), or 0 if there is none.
func lowestFree(used []int, databases int) int {
	next := 1
	for _, db := range used {
		if db > next {
			break
		}
		if db == next {
			next++
		}
	}
	if next >= databases {
		return 0
	}
	return next
}

// DeleteUser implements objectcache.Manager. It deletes the stored row and
// credentials first, then the ACL user, then empties the site's database.
// It succeeds if none of them exist.
func (m *Manager) DeleteUser(ctx context.Context, siteID string) error {
	u, err := newSiteUser(siteID)
	if err != nil {
		return fmt.Errorf("delete object cache user: %w", err)
	}
	// The database number goes with the row, so find it first: from the
	// row, or, on a retry after the row is gone, from the user on the
	// server.
	db := 0
	err = m.state.InTx(ctx, func(q store.Querier) error {
		rec, err := getRecord(ctx, q, siteID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		db = rec.DB
		if err := deleteRecord(ctx, q, siteID); err != nil {
			return err
		}
		return m.secrets.With(q).Delete(ctx, rec.SecretRef)
	})
	if err != nil {
		return fmt.Errorf("delete object cache user for site %s: deleting credentials: %w", siteID, err)
	}
	if db == 0 {
		found, exists, err := m.server.userDB(ctx, u.name)
		if err != nil {
			return fmt.Errorf("delete object cache user for site %s: %w", siteID, err)
		}
		if exists {
			db = found
		}
	}
	if err := m.server.deleteUser(ctx, u.name); err != nil {
		return fmt.Errorf("delete object cache user for site %s: %w", siteID, err)
	}
	if db > 0 && db < m.databases {
		if err := m.server.flushDB(ctx, db); err != nil {
			return fmt.Errorf("delete object cache user for site %s: %w", siteID, err)
		}
	}
	m.log.Info("site object cache user deleted", "site_id", siteID, "user", u.String(), "database", db)
	return nil
}

// Reconcile makes the server's site users match the stored records: it
// (re)creates the user of every record with the stored password, which
// restores them after the server restarts (it keeps users only in memory),
// and deletes site users that have no record, such as one left by a
// deletion that failed halfway. See the package documentation for why
// running it alongside jobs is safe.
func (m *Manager) Reconcile(ctx context.Context) error {
	// List the server's users before reading the records: a job's
	// CreateUser commits its record before creating the user, so every
	// user listed here that a job is creating already has a record below.
	names, err := m.server.users(ctx)
	if err != nil {
		return fmt.Errorf("reconcile object cache users: %w", err)
	}
	recs, err := listRecords(ctx, m.state)
	if err != nil {
		return fmt.Errorf("reconcile object cache users: %w", err)
	}
	var errs []error
	ensured := 0
	candidates := map[string]bool{}
	for _, n := range names {
		if _, ok := siteIDFromUser(n); ok {
			candidates[n] = true
		}
	}
	for _, rec := range recs {
		candidates[rec.Username] = true
		u, err := newSiteUser(rec.SiteID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		creds, err := m.recordCredentials(ctx, u, rec)
		if err == nil {
			err = m.server.ensureUser(ctx, u, rec.DB, passwordHash(creds.Password))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("site %s: %w", rec.SiteID, err))
			continue
		}
		ensured++
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	// Read the records again: a deletion that removed its record while the
	// loop above ran may have had its user recreated by it. DeleteUser
	// deletes the record before the user, so such a user's record is gone
	// by now, and it is deleted here.
	recs, err = listRecords(ctx, m.state)
	if err != nil {
		return fmt.Errorf("reconcile object cache users: %w", err)
	}
	for _, rec := range recs {
		delete(candidates, rec.Username)
	}
	orphans := make([]string, 0, len(candidates))
	for n := range candidates {
		orphans = append(orphans, n)
	}
	slices.Sort(orphans)
	for _, n := range orphans {
		if err := m.server.deleteUser(ctx, n); err != nil {
			errs = append(errs, err)
			continue
		}
		m.log.Info("orphaned object cache user deleted", "user", n)
	}
	m.log.Debug("object cache users reconciled", "users", ensured, "orphans", len(orphans))
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("reconcile object cache users: %w", err)
	}
	return nil
}

// Watch reconciles when it starts, whenever the server has restarted, and
// every ResyncInterval, until ctx is canceled.
//
// The server keeps site users only in memory, so a restart (a crash, an
// OOM kill, a platform upgrade) leaves every site unable to authenticate
// until they are recreated. Watch notices a restart from the server's
// run_id, polled every PollInterval, so sites are back within seconds; the
// Redis Object Cache plugin falls back to no caching in the meantime. The
// resync is a safety net for anything else that changes users behind the
// control plane's back.
func (m *Manager) Watch(ctx context.Context) {
	var (
		lastRunID   string
		lastSync    time.Time
		unreachable bool
	)
	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()
	for {
		runID, err := m.server.runID(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			// Log once per outage rather than every poll.
			if !unreachable {
				m.log.Warn("object cache unreachable; will retry", "error", err)
				unreachable = true
			}
		case runID != lastRunID || m.now().Sub(lastSync) >= m.resyncInterval:
			if unreachable {
				m.log.Info("object cache reachable again")
				unreachable = false
			}
			if runID != lastRunID {
				m.log.Info("object cache server started; restoring site users", "run_id", runID)
			}
			if err := m.Reconcile(ctx); err != nil {
				m.log.Warn("reconciling object cache users failed; will retry", "error", err)
				break
			}
			lastRunID, lastSync = runID, m.now()
		default:
			if unreachable {
				m.log.Info("object cache reachable again")
				unreachable = false
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
