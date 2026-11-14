package mariadb

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/sitedb"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// ErrClientNotFound means the mariadb-dump or mariadb client program that
// Dump or Import runs is not installed.
var ErrClientNotFound = errors.New("mariadb: client program not found")

// Default client programs, looked up in PATH.
const (
	DefaultDumpProgram   = "mariadb-dump"
	DefaultClientProgram = "mariadb"
)

// dumpArgs are mariadb-dump's options, after --defaults-file.
//
// The database is named as a plain argument rather than with --databases,
// so the dump has no CREATE DATABASE or USE statement and imports into
// whatever database the importer selects: a restore can target a different
// site (§5.4). Routines and events are left out (the defaults): WordPress
// doesn't use them, and a site user can't recreate one owned by another
// account. Triggers and views are kept, with their DEFINER clauses removed
// by stripDefiners.
var dumpArgs = []string{
	// A consistent InnoDB snapshot without locking the site's tables, so
	// the site keeps serving while it is backed up. It needs no privileges
	// beyond SELECT.
	"--single-transaction",
	// Binary columns as hex literals, so no byte sequence depends on the
	// connection character set or survives only by escaping.
	"--hex-blob",
	"--default-character-set=utf8mb4",
	// Tablespace information needs the PROCESS privilege, which site users
	// don't have, and InnoDB file-per-table databases don't need it.
	"--no-tablespaces",
	// Identical data gives an identical dump, which restic deduplicates.
	"--skip-dump-date",
}

// importArgs are the mariadb client's options, after --defaults-file. The
// dump file is untrusted input: it comes from a backup the site's own code
// could have shaped. The client runs in the control plane container, so it
// must not do anything but send SQL.
var importArgs = []string{
	"--default-character-set=utf8mb4",
	// No shell (\!) or other client commands that touch the local system.
	"--sandbox",
	// LOAD DATA LOCAL INFILE would read a file on the control plane (its
	// secrets included) into the site's database.
	"--local-infile=0",
	// Binary mode stops the client from interpreting sequences inside the
	// data as client commands, which a dump of binary content can contain.
	"--binary-mode",
	// The client's own packet limit; the server's max_allowed_packet still
	// applies. mariadb-dump keeps multi-row INSERTs near net_buffer_length
	// (1 MiB), so only single rows bigger than that need this.
	"--max-allowed-packet=1G",
}

// Dump implements sitedb.Manager. It runs mariadb-dump as the site's own
// user and writes the dump to destPath, replacing any file there only once
// the dump is complete. See dumpArgs for the options.
//
// The site user rather than the admin: its grants confine the dump to the
// site's database, it has every privilege a dump of a WordPress database
// needs, and the admin password never leaves the control plane's own
// connection pool.
func (m *Manager) Dump(ctx context.Context, siteID, destPath string) error {
	name, err := newSiteName(siteID)
	if err != nil {
		return fmt.Errorf("dump database: %w", err)
	}
	prog, err := lookClient(m.dumpProgram)
	if err != nil {
		return fmt.Errorf("dump database for site %s: %w", siteID, err)
	}
	creds, err := m.existingCredentials(ctx, siteID, name)
	if err != nil {
		return fmt.Errorf("dump database for site %s: %w", siteID, err)
	}
	if err := m.dump(ctx, prog, creds, destPath); err != nil {
		return fmt.Errorf("dump database for site %s: %w", siteID, err)
	}
	m.log.Info("site database dumped", "site_id", siteID, "database", name.String(), "path", destPath)
	return nil
}

func (m *Manager) dump(ctx context.Context, prog string, creds sitedb.Credentials, destPath string) (err error) {
	// Write next to destPath and rename, so destPath only ever holds a
	// complete dump, and a failed or retried dump leaves no partial file.
	out, err := os.CreateTemp(filepath.Dir(destPath), "."+filepath.Base(destPath)+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating dump file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = out.Close()
			_ = os.Remove(out.Name())
		}
	}()

	cmd, stderr, cleanup, err := m.command(ctx, prog, creds, dumpArgs)
	if err != nil {
		return err
	}
	defer cleanup()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("starting %s: %w", filepath.Base(prog), err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", filepath.Base(prog), err)
	}
	copyErr := stripDefiners(out, stdout)
	if copyErr != nil {
		// Unblock the client if it is still writing.
		_, _ = io.Copy(io.Discard, stdout)
	}
	if err := cmd.Wait(); err != nil {
		return clientError(filepath.Base(prog), err, stderr, creds.Password)
	}
	if copyErr != nil {
		return fmt.Errorf("writing dump file: %w", copyErr)
	}
	if err := out.Sync(); err != nil {
		return fmt.Errorf("writing dump file: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("writing dump file: %w", err)
	}
	if err := os.Rename(out.Name(), destPath); err != nil {
		return fmt.Errorf("writing dump file: %w", err)
	}
	return nil
}

// Import implements sitedb.Manager. It empties the site's database and
// loads the dump at srcPath into it with the mariadb client, as the site's
// own user. It is destructive by design; the restore workflow takes a
// snapshot first (§5.4). A retry starts over from an empty database, so a
// failed import can be retried.
//
// The site user rather than the admin, because the dump is untrusted: the
// site's code shaped its contents. As the site user, every statement in it
// is confined to the site's database, and it can't create a view or
// trigger that runs as another account (DEFINER needs SET USER), so a
// crafted dump can't reach other sites.
func (m *Manager) Import(ctx context.Context, siteID, srcPath string) error {
	name, err := newSiteName(siteID)
	if err != nil {
		return fmt.Errorf("import database: %w", err)
	}
	prog, err := lookClient(m.clientProgram)
	if err != nil {
		return fmt.Errorf("import database for site %s: %w", siteID, err)
	}
	creds, err := m.existingCredentials(ctx, siteID, name)
	if err != nil {
		return fmt.Errorf("import database for site %s: %w", siteID, err)
	}
	// Open the dump before anything is dropped, so a missing file leaves
	// the database as it was.
	src, err := os.Open(srcPath) //nolint:gosec // G304: the path comes from the restore job
	if err != nil {
		return fmt.Errorf("import database for site %s: opening dump: %w", siteID, err)
	}
	defer func() { _ = src.Close() }()

	// Dropping and recreating the database removes tables the dump doesn't
	// have (say, from a plugin installed after the backup), along with
	// views and triggers. ensure recreates the database and repairs the
	// user; grants on the database survive the drop anyway.
	if err := m.server.empty(ctx, name); err != nil {
		return fmt.Errorf("import database for site %s: %w", siteID, err)
	}
	if err := m.server.ensure(ctx, name, nativePasswordHash(creds.Password), m.maxConns); err != nil {
		return fmt.Errorf("import database for site %s: %w", siteID, err)
	}

	cmd, stderr, cleanup, err := m.command(ctx, prog, creds, importArgs)
	if err != nil {
		return fmt.Errorf("import database for site %s: %w", siteID, err)
	}
	defer cleanup()
	cmd.Stdin = src
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("import database for site %s: %w", siteID, clientError(filepath.Base(prog), err, stderr, creds.Password))
	}
	m.log.Info("site database imported", "site_id", siteID, "database", name.String(), "path", srcPath)
	return nil
}

// existingCredentials returns the site's stored credentials, or
// sitedb.ErrDatabaseNotFound if the site has no database. Unlike
// credentials, it never creates any.
func (m *Manager) existingCredentials(ctx context.Context, siteID string, name siteName) (sitedb.Credentials, error) {
	rec, err := getRecord(ctx, m.state, siteID)
	if errors.Is(err, store.ErrNotFound) {
		return sitedb.Credentials{}, sitedb.ErrDatabaseNotFound
	}
	if err != nil {
		return sitedb.Credentials{}, err
	}
	_, creds, err := m.recordCredentials(ctx, rec, name)
	return creds, err
}

// lookClient resolves a client program, by name in PATH or by path.
func lookClient(prog string) (string, error) {
	p, err := exec.LookPath(prog)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrClientNotFound, prog, err)
	}
	return p, nil
}

// clientWaitDelay bounds how long a cancelled client's output is waited
// for after it is killed.
const clientWaitDelay = 10 * time.Second

// maxStderr bounds how much of a client's error output is kept for the
// error message. Import errors quote the statement near the error, which
// is site data, so only the start is useful and the rest is dropped.
const maxStderr = 1024

// command builds a client command that connects as creds' user to creds'
// database. The credentials go in an option file only the control plane
// can read, never in argv, where any process could see them, or in the
// environment, where child processes inherit them. cleanup removes the
// file.
func (m *Manager) command(ctx context.Context, prog string, creds sitedb.Credentials, args []string) (*exec.Cmd, *limitedBuffer, func(), error) {
	optFile, err := m.writeOptionFile(creds)
	if err != nil {
		return nil, nil, nil, err
	}
	cleanup := func() { _ = os.Remove(optFile) }
	// --defaults-file must come first, and it replaces the system and user
	// option files, so nothing on the host changes the connection.
	argv := append([]string{"--defaults-file=" + optFile}, args...)
	argv = append(argv, creds.Database)
	cmd := exec.CommandContext(ctx, prog, argv...) //nolint:gosec // G204: the program is configuration; the arguments are fixed options and a validated name
	cmd.WaitDelay = clientWaitDelay
	stderr := &limitedBuffer{max: maxStderr}
	cmd.Stderr = stderr
	return cmd, stderr, cleanup, nil
}

// writeOptionFile writes an option file with creds and the server address
// and returns its path. os.CreateTemp creates it with mode 0600.
func (m *Manager) writeOptionFile(creds sitedb.Credentials) (path string, err error) {
	host, port, err := net.SplitHostPort(m.addr)
	if err != nil {
		return "", fmt.Errorf("server address %q: %w", m.addr, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return "", fmt.Errorf("server address %q: invalid port", m.addr)
	}
	var b strings.Builder
	b.WriteString("[client]\n")
	for _, kv := range [][2]string{
		{"host", host},
		{"port", port},
		// A host of "localhost" would otherwise mean the Unix socket.
		{"protocol", "TCP"},
		{"user", creds.Username},
		{"password", creds.Password.Reveal()},
	} {
		v, ok := optionValue(kv[1])
		if !ok {
			return "", fmt.Errorf("writing client option file: %s can't be written to an option file", kv[0])
		}
		b.WriteString(kv[0] + "=" + v + "\n")
	}
	f, err := os.CreateTemp(m.tempDir, "pressfleet-mariadb-*.cnf")
	if err != nil {
		return "", fmt.Errorf("writing client option file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	_, werr := f.WriteString(b.String())
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", fmt.Errorf("writing client option file: %w", werr)
	}
	return f.Name(), nil
}

// optionValue quotes v for an option file, in which a double-quoted value
// has \" and \\ escapes. Values can't span lines, so v must have no line
// breaks.
func optionValue(v string) (string, bool) {
	if v == "" || strings.ContainsAny(v, "\r\n\x00") {
		return "", false
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`, true
}

// clientError describes a failed client run. The client's error output
// never contains the password, which it only reads from the option file;
// it is scrubbed anyway, in case a future client version echoes options.
func clientError(prog string, err error, stderr *limitedBuffer, password redact.Secret) error {
	msg := strings.TrimSpace(stderr.String())
	if pw := password.Reveal(); pw != "" {
		msg = strings.ReplaceAll(msg, pw, "[REDACTED]")
	}
	if msg == "" {
		return fmt.Errorf("%s: %w", prog, err)
	}
	return fmt.Errorf("%s: %w: %s", prog, err, msg)
}

// limitedBuffer keeps the first max bytes written to it and discards the
// rest, noting that it did.
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
		if len(p) > room {
			b.truncated = true
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	if b.truncated {
		return b.buf.String() + " [truncated]"
	}
	return b.buf.String()
}

// DEFINER clauses as mariadb-dump writes them, each at the start of a line:
// for views, on a line of its own between CREATE and VIEW, and for
// triggers, on the CREATE TRIGGER line. Accounts are backquoted, with
// backquotes in names doubled.
var (
	definerAccount = "`(?:[^`]|``)*`@`(?:[^`]|``)*`"
	viewDefiner    = regexp.MustCompile("^/\\*!50013 DEFINER=" + definerAccount + " ")
	triggerDefiner = regexp.MustCompile("^/\\*!50003 CREATE\\*/ /\\*!50017 DEFINER=" + definerAccount + "\\*/ ")
)

// stripDefiners copies a dump from r to w without the DEFINER clauses of
// views and triggers. They name the source site's user, which can't be
// created as by another site's user, so a dump that kept them could only be
// imported into the site it came from. Without them, the objects belong to
// whoever imports the dump.
//
// Only line starts are examined: mariadb-dump writes every row value on a
// line starting with "(" or "INSERT", with line breaks in values escaped,
// so no data can be mistaken for a clause. Lines longer than the buffer
// (large rows) pass through in pieces untouched.
func stripDefiners(w io.Writer, r io.Reader) error {
	br := bufio.NewReaderSize(r, 64<<10)
	atLineStart := true
	for {
		chunk, err := br.ReadSlice('\n')
		if atLineStart && len(chunk) > 0 && chunk[0] == '/' {
			chunk = viewDefiner.ReplaceAll(chunk, []byte("/*!50013 "))
			chunk = triggerDefiner.ReplaceAll(chunk, []byte("/*!50003 CREATE*/ "))
		}
		if _, werr := w.Write(chunk); werr != nil {
			return werr
		}
		switch {
		case err == nil:
			atLineStart = true
		case errors.Is(err, bufio.ErrBufferFull):
			atLineStart = false
		case errors.Is(err, io.EOF):
			return nil
		default:
			return err
		}
	}
}

// sweepOptionFiles removes the option files a killed process left in dir.
// They hold plaintext passwords, and nothing else clears them. The control
// plane is the only user of its TmpDir's option files, and one process runs
// per data directory, so none is in use at startup.
func sweepOptionFiles(dir string, log *slog.Logger) {
	matches, err := filepath.Glob(filepath.Join(dir, "pressfleet-mariadb-*.cnf"))
	if err != nil {
		return
	}
	for _, name := range matches {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Warn("removing a stale client option file failed", "error", err)
		}
	}
}
