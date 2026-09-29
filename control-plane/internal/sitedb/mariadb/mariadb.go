package mariadb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// DefaultMaxUserConnections is each site user's MAX_USER_CONNECTIONS. See
// the package documentation for how it was chosen.
const DefaultMaxUserConnections = 20

// ErrInvalidSiteID means a site ID is not a well-formed ID, so no database
// or user name can be built from it.
var ErrInvalidSiteID = errors.New("mariadb: invalid site id")

// ErrInvalidSessionID means a database session ID is not a well-formed ID,
// so no temporary user name can be built from it.
var ErrInvalidSessionID = errors.New("mariadb: invalid database session id")

// Config locates the MariaDB server and its admin account.
type Config struct {
	// Addr is the server's host:port.
	Addr string
	// User is the admin user.
	User string
	// PasswordFile holds the admin password. It is read for every new
	// connection; a trailing newline is ignored.
	PasswordFile string
	// MaxUserConnections is each site user's connection limit; zero means
	// DefaultMaxUserConnections.
	MaxUserConnections int
	// DumpProgram and ClientProgram are the mariadb-dump and mariadb
	// programs that Dump and Import run, as paths or names looked up in
	// PATH. Empty means DefaultDumpProgram and DefaultClientProgram.
	DumpProgram   string
	ClientProgram string
}

// Manager implements sitedb.Manager on the shared MariaDB server.
type Manager struct {
	server   server
	conn     *sql.DB // nil in tests with a fake server
	state    *store.DB
	secrets  secrets.Store
	maxConns int
	log      *slog.Logger

	// Dump and Import run client programs that connect to addr.
	addr          string
	dumpProgram   string
	clientProgram string
	tempDir       string // for client option files; empty means os.TempDir
}

var _ sitedb.Manager = (*Manager)(nil)

// Timeouts and pool size for the admin connection.
const (
	dialTimeout  = 5 * time.Second
	maxOpenConns = 2
	connIdleTime = time.Minute
)

// New returns a Manager that administers the server described by cfg and
// records credentials in state, encrypted with secretStore. It doesn't
// connect; connections are opened when needed, so New succeeds while MariaDB
// is down. Close releases the connections.
func New(cfg Config, state *store.DB, secretStore secrets.Store, log *slog.Logger) (*Manager, error) {
	if cfg.Addr == "" || cfg.User == "" || cfg.PasswordFile == "" {
		return nil, errors.New("mariadb: address, admin user, and admin password file are required")
	}
	if cfg.MaxUserConnections < 0 {
		return nil, fmt.Errorf("mariadb: max user connections must not be negative, got %d", cfg.MaxUserConnections)
	}
	mc := mysql.NewConfig()
	mc.Net = "tcp"
	mc.Addr = cfg.Addr
	mc.User = cfg.User
	mc.Timeout = dialTimeout
	mc.Logger = driverLogger{log}
	passwordFile := cfg.PasswordFile
	if err := mc.Apply(mysql.BeforeConnect(func(_ context.Context, c *mysql.Config) error {
		pw, err := readPasswordFile(passwordFile)
		if err != nil {
			return err
		}
		c.Passwd = pw.Reveal()
		return nil
	})); err != nil {
		return nil, fmt.Errorf("mariadb: %w", err)
	}
	connector, err := mysql.NewConnector(mc)
	if err != nil {
		return nil, fmt.Errorf("mariadb: %w", err)
	}
	conn := sql.OpenDB(connector)
	conn.SetMaxOpenConns(maxOpenConns)
	conn.SetMaxIdleConns(maxOpenConns)
	conn.SetConnMaxIdleTime(connIdleTime)
	m := newManager(sqlServer{db: conn}, state, secretStore, cfg.MaxUserConnections, log)
	m.conn = conn
	m.addr = cfg.Addr
	if cfg.DumpProgram != "" {
		m.dumpProgram = cfg.DumpProgram
	}
	if cfg.ClientProgram != "" {
		m.clientProgram = cfg.ClientProgram
	}
	return m, nil
}

func newManager(srv server, state *store.DB, secretStore secrets.Store, maxConns int, log *slog.Logger) *Manager {
	if maxConns == 0 {
		maxConns = DefaultMaxUserConnections
	}
	return &Manager{
		server: srv, state: state, secrets: secretStore, maxConns: maxConns, log: log,
		dumpProgram: DefaultDumpProgram, clientProgram: DefaultClientProgram,
	}
}

// maxPasswordFileSize bounds how much of the password file is read.
const maxPasswordFileSize = 4096

// readPasswordFile reads the admin password. Errors never include the
// file's contents.
func readPasswordFile(path string) (redact.Secret, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the path comes from operator configuration
	if err != nil {
		return "", fmt.Errorf("reading MariaDB admin password: %w", err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxPasswordFileSize+1))
	if err != nil {
		return "", fmt.Errorf("reading MariaDB admin password: %w", err)
	}
	if len(b) > maxPasswordFileSize {
		return "", fmt.Errorf("reading MariaDB admin password: %s is larger than %d bytes", path, maxPasswordFileSize)
	}
	// The MariaDB image reads its *_FILE variables with $(< file), which
	// drops trailing newlines; match it.
	b = bytes.TrimRight(b, "\r\n")
	if len(b) == 0 {
		return "", fmt.Errorf("reading MariaDB admin password: %s is empty", path)
	}
	return redact.Secret(b), nil
}

// Ping checks that the server is reachable with the admin credentials. It is
// used as a readiness check.
func (m *Manager) Ping(ctx context.Context) error {
	if m.conn == nil {
		return nil
	}
	return m.conn.PingContext(ctx)
}

// Close closes the admin connections.
func (m *Manager) Close() error {
	if m.conn == nil {
		return nil
	}
	return m.conn.Close()
}

// CreateDatabase implements sitedb.Manager. It is idempotent; see the
// package documentation for the steps and why each retry is safe.
func (m *Manager) CreateDatabase(ctx context.Context, siteID string) (string, error) {
	name, err := newSiteName(siteID)
	if err != nil {
		return "", fmt.Errorf("create database: %w", err)
	}
	ref, creds, err := m.credentials(ctx, siteID, name)
	if err != nil {
		return "", fmt.Errorf("create database for site %s: %w", siteID, err)
	}
	if err := m.server.ensure(ctx, name, nativePasswordHash(creds.Password), m.maxConns); err != nil {
		return "", fmt.Errorf("create database for site %s: %w", siteID, err)
	}
	m.log.Info("site database ready", "site_id", siteID, "database", name.String())
	return string(ref), nil
}

// credentials returns the site's stored credentials, creating them if the
// site has none.
func (m *Manager) credentials(ctx context.Context, siteID string, name siteName) (secrets.Ref, sitedb.Credentials, error) {
	rec, err := getRecord(ctx, m.state, siteID)
	if errors.Is(err, store.ErrNotFound) {
		rec, err = m.createRecord(ctx, siteID, name)
	}
	if err != nil {
		return "", sitedb.Credentials{}, err
	}
	return m.recordCredentials(ctx, rec, name)
}

// recordCredentials reads the credentials rec points to and checks that
// they are for name.
func (m *Manager) recordCredentials(ctx context.Context, rec record, name siteName) (secrets.Ref, sitedb.Credentials, error) {
	if rec.Database != name.String() || rec.Username != name.String() {
		return "", sitedb.Credentials{}, fmt.Errorf("stored database %q and user %q don't match %s", rec.Database, rec.Username, name)
	}
	plain, err := m.secrets.Get(ctx, rec.SecretRef)
	if err != nil {
		return "", sitedb.Credentials{}, fmt.Errorf("reading credentials: %w", err)
	}
	creds, err := sitedb.UnmarshalCredentials(plain)
	if err != nil {
		return "", sitedb.Credentials{}, err
	}
	if creds.Database != name.String() || creds.Username != name.String() {
		return "", sitedb.Credentials{}, fmt.Errorf("stored credentials are for database %q and user %q, not %s", creds.Database, creds.Username, name)
	}
	return rec.SecretRef, creds, nil
}

// createRecord generates credentials and stores them with the site's row in
// one transaction.
func (m *Manager) createRecord(ctx context.Context, siteID string, name siteName) (record, error) {
	plain, err := sitedb.MarshalCredentials(sitedb.Credentials{
		Database: name.String(),
		Username: name.String(),
		Password: newPassword(),
	})
	if err != nil {
		return record{}, err
	}
	rec := record{SiteID: siteID, Database: name.String(), Username: name.String(), CreatedAt: time.Now()}
	err = m.state.InTx(ctx, func(q store.Querier) error {
		ref, err := m.secrets.With(q).Put(ctx, secrets.KindSiteDBPassword, plain)
		if err != nil {
			return err
		}
		rec.SecretRef = ref
		return insertRecord(ctx, q, rec)
	})
	if errors.Is(err, store.ErrConflict) {
		// A concurrent CreateDatabase for this site committed first; our
		// secret rolled back with the transaction. Use its credentials.
		return getRecord(ctx, m.state, siteID)
	}
	if err != nil {
		return record{}, fmt.Errorf("storing credentials: %w", err)
	}
	return rec, nil
}

// DropDatabase implements sitedb.Manager. It drops the user and database,
// then deletes the stored row and credentials. It succeeds if none of them
// exist.
func (m *Manager) DropDatabase(ctx context.Context, siteID string) error {
	name, err := newSiteName(siteID)
	if err != nil {
		return fmt.Errorf("drop database: %w", err)
	}
	if err := m.server.drop(ctx, name); err != nil {
		return fmt.Errorf("drop database for site %s: %w", siteID, err)
	}
	err = m.state.InTx(ctx, func(q store.Querier) error {
		rec, err := getRecord(ctx, q, siteID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := deleteRecord(ctx, q, siteID); err != nil {
			return err
		}
		return m.secrets.With(q).Delete(ctx, rec.SecretRef)
	})
	if err != nil {
		return fmt.Errorf("drop database for site %s: deleting credentials: %w", siteID, err)
	}
	m.log.Info("site database dropped", "site_id", siteID, "database", name.String())
	return nil
}

// driverLogger sends the MySQL driver's messages (connection errors it
// otherwise prints to stderr) to the injected logger. They never contain
// the password.
type driverLogger struct{ log *slog.Logger }

func (l driverLogger) Print(v ...any) {
	l.log.Debug("mariadb driver", "detail", fmt.Sprint(v...))
}
