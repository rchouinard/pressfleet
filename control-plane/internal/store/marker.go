package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// MigrationStatus is the state of a migration run recorded in a marker.
type MigrationStatus string

// Migration statuses.
const (
	// MigrationRunning means the run started and hasn't finished, or the
	// process stopped before it could say how it ended. Some migrations may
	// have been applied.
	MigrationRunning MigrationStatus = "running"
	// MigrationSucceeded means every pending migration was applied.
	MigrationSucceeded MigrationStatus = "succeeded"
	// MigrationFailed means the run stopped with an error. Migrations before
	// the failing one stay applied (see ReachedSchema).
	MigrationFailed MigrationStatus = "failed"
)

// MigrationMarker records one run of Migrate that had migrations to apply.
// Migrate writes it beside the pre-migration backups after the backup and
// before the first migration, and again when the run ends, so the host CLI
// can learn whether an upgrade migrated the state database, and from which
// backup to restore it, without opening the database (design doc §4.7).
// PostgreSQL runs get a marker too, without a backup.
type MigrationMarker struct {
	Status MigrationStatus `json:"status"`
	// StartedAt and FinishedAt bound the run; FinishedAt is zero while it
	// runs.
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	// Version is the release of the binary that migrated.
	Version string  `json:"version"`
	Dialect Dialect `json:"dialect"`
	// FromSchema is the schema version before the run, ToSchema the one it
	// set out to reach, and ReachedSchema the one it reached.
	FromSchema    int `json:"from_schema"`
	ToSchema      int `json:"to_schema"`
	ReachedSchema int `json:"reached_schema"`
	// Backup is the pre-migration backup's path, empty for PostgreSQL and
	// for a database that held nothing yet.
	Backup string `json:"backup,omitempty"`
	// Error is why a failed run failed.
	Error string `json:"error,omitempty"`
}

// Changed reports whether the run may have changed the schema: it applied
// a migration, or it never said how it ended.
func (m MigrationMarker) Changed() bool {
	return m.ReachedSchema > m.FromSchema || m.Status == MigrationRunning
}

// markerTimeLayout is the timestamp layout of markers' names, the same as
// backups'.
const markerTimeLayout = "20060102T150405.000Z"

// MarkerName matches the names of migration markers:
// migration-<timestamp>-schemaNNNN.json, where NNNN is the schema version
// before the run. The first group is the timestamp.
var MarkerName = regexp.MustCompile(`^migration-(\d{8}T\d{6}\.\d{3}Z)-schema\d{4}\.json$`)

// MarkerTime returns the time in a marker's name, which is its StartedAt to
// the millisecond.
func MarkerTime(name string) (time.Time, bool) {
	m := MarkerName.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(markerTimeLayout, m[1])
	return t, err == nil
}

// ParseMigrationMarker decodes a marker's contents.
func ParseMigrationMarker(data []byte) (MigrationMarker, error) {
	var m MigrationMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return m, fmt.Errorf("reading migration marker: %w", err)
	}
	switch m.Status {
	case MigrationRunning, MigrationSucceeded, MigrationFailed:
	default:
		return m, fmt.Errorf("reading migration marker: unknown status %q", m.Status)
	}
	return m, nil
}

// markerWriter keeps one run's marker up to date.
type markerWriter struct {
	path   string
	marker MigrationMarker
}

// newMarker writes the first version of a run's marker into dir.
func newMarker(dir string, m MigrationMarker) (*markerWriter, error) {
	if dir == "" {
		return nil, errors.New("writing migration marker: backup directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("writing migration marker: creating directory: %w", err)
	}
	name := fmt.Sprintf("migration-%s-schema%04d.json", m.StartedAt.UTC().Format(markerTimeLayout), m.FromSchema)
	w := &markerWriter{path: filepath.Join(dir, name), marker: m}
	if _, err := os.Lstat(w.path); err == nil {
		return nil, fmt.Errorf("writing migration marker: %s already exists", w.path)
	}
	if err := w.write(); err != nil {
		return nil, err
	}
	return w, nil
}

// write replaces the marker atomically, so a reader sees the old or the new
// version, never a torn one.
func (w *markerWriter) write() (err error) {
	data, err := json.MarshalIndent(w.marker, "", "  ")
	if err != nil {
		return fmt.Errorf("writing migration marker: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(w.path)
	tmp, err := os.CreateTemp(dir, ".migration-*.tmp")
	if err != nil {
		return fmt.Errorf("writing migration marker: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("writing migration marker: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing migration marker: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("writing migration marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing migration marker: %w", err)
	}
	if err := os.Rename(tmp.Name(), w.path); err != nil {
		return fmt.Errorf("writing migration marker: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("writing migration marker: %w", err)
	}
	return nil
}

// finish records how the run ended.
func (w *markerWriter) finish(reached int, runErr error) error {
	w.marker.FinishedAt = now()
	w.marker.ReachedSchema = reached
	w.marker.Status = MigrationSucceeded
	if runErr != nil {
		w.marker.Status = MigrationFailed
		w.marker.Error = runErr.Error()
	}
	return w.write()
}
