package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Backup writes a consistent copy of an SQLite database into dir and returns
// its path. label goes into the file name. For PostgreSQL it returns
// ErrUnsupported (see the package documentation).
func (db *DB) Backup(ctx context.Context, dir, label string) (string, error) {
	if db.dialect != SQLite {
		return "", fmt.Errorf("backup: %w", ErrUnsupported)
	}
	return backupSQLite(ctx, db.querier, dir, label)
}

// backupSQLite runs VACUUM INTO, which writes a compact, transactionally
// consistent copy (including any WAL contents) while other connections keep
// working.
func backupSQLite(ctx context.Context, q querier, dir, label string) (string, error) {
	if dir == "" {
		return "", errors.New("backup directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating backup directory: %w", err)
	}
	name := fmt.Sprintf("pressfleet-%s-%s.db", time.Now().UTC().Format("20060102T150405.000Z"), label)
	p := filepath.Join(dir, name)
	if _, err := os.Lstat(p); err == nil {
		return "", fmt.Errorf("backup %s already exists", p)
	}
	if _, err := q.ExecContext(ctx, "VACUUM INTO ?", p); err != nil {
		_ = os.Remove(p)
		return "", fmt.Errorf("writing backup %s: %w", p, err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		return "", fmt.Errorf("writing backup %s: %w", p, err)
	}
	return p, nil
}

// RestoreResult describes what Restore did.
type RestoreResult struct {
	// SchemaVersion is the schema version of the restored database.
	SchemaVersion int
	// PreRestorePath is a backup of the database that was replaced, or empty
	// if there was none.
	PreRestorePath string
}

// Restore replaces an SQLite database with a backup made by Migrate or
// Backup. It is used to roll back a failed upgrade, running the previous
// pressfleet version, so it refuses backups with a schema newer than this
// binary supports. The current database, if any, is first backed up into
// backupDir so the restore can itself be undone.
//
// No other process may have the database open: Restore returns ErrInUse if
// one does. For PostgreSQL it returns ErrUnsupported.
func Restore(ctx context.Context, cfg Config, backupPath, backupDir string, log *slog.Logger) (RestoreResult, error) {
	var res RestoreResult
	if cfg.Driver != SQLite {
		return res, fmt.Errorf("restore: %w; restore PostgreSQL from your own backups", ErrUnsupported)
	}
	dbPath := cfg.SQLitePath
	unlock, err := lockSQLite(dbPath, true)
	if err != nil {
		if errors.Is(err, ErrInUse) {
			return res, fmt.Errorf("%w: stop the control plane before restoring", err)
		}
		return res, err
	}
	defer func() { _ = unlock() }()

	if res.SchemaVersion, err = checkBackup(ctx, backupPath); err != nil {
		return res, err
	}

	if _, err := os.Stat(dbPath); err == nil {
		if res.PreRestorePath, err = backupCurrent(ctx, dbPath, backupDir); err != nil {
			return res, fmt.Errorf("backing up the current database before restoring (move it aside by hand if it is unreadable): %w", err)
		}
		log.Info("backed up current database before restoring", "path", res.PreRestorePath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}

	if err := replaceFile(backupPath, dbPath); err != nil {
		return res, fmt.Errorf("restoring %s: %w", backupPath, err)
	}
	log.Info("restored database", "from", backupPath, "path", dbPath, "schema_version", res.SchemaVersion)
	return res, nil
}

// checkBackup verifies that path is an intact PressFleet database this
// binary can run, and returns its schema version.
func checkBackup(ctx context.Context, path string) (int, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, fmt.Errorf("backup: %w", err)
	}
	sqldb, err := openSQLite(path, true)
	if err != nil {
		return 0, err
	}
	defer func() { _ = sqldb.Close() }()
	q := querier{ex: sqldb, dialect: SQLite}

	var check string
	if err := q.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&check); err != nil {
		return 0, fmt.Errorf("backup %s is not a readable SQLite database: %w", path, err)
	}
	if check != "ok" {
		return 0, fmt.Errorf("backup %s is corrupt: %s", path, check)
	}
	version, err := schemaVersion(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("backup %s: %w", path, err)
	}
	if version == 0 {
		return 0, fmt.Errorf("backup %s is not a PressFleet state database", path)
	}
	latest, err := LatestVersion(SQLite)
	if err != nil {
		return 0, err
	}
	if version > latest {
		return 0, fmt.Errorf("%w: backup %s is at schema version %d, this binary supports up to %d",
			ErrSchemaTooNew, path, version, latest)
	}
	return version, nil
}

func backupCurrent(ctx context.Context, dbPath, backupDir string) (string, error) {
	sqldb, err := openSQLite(dbPath, false)
	if err != nil {
		return "", err
	}
	defer func() { _ = sqldb.Close() }()
	return backupSQLite(ctx, querier{ex: sqldb, dialect: SQLite}, backupDir, "pre-restore")
}

// replaceFile atomically replaces dst with a copy of src, removing dst's WAL
// and shared-memory files, which belong to the database being replaced.
func replaceFile(src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // G304: the operator names the backup to restore
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".restore-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(dst + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dst))
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: the database directory
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
