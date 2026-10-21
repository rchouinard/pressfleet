// Package statebackup backs up the control plane's state database for
// disaster recovery, and restores it on a new host (#96).
//
// The client group repositories (§5.3) and the master key aren't enough to
// rebuild a platform whose host is lost: the site records, their settings
// and domains, the users, and every secret (repository passwords included)
// are in the state database. This package keeps encrypted copies of it
// beside the client group repositories, and loads one into a new
// installation.
//
// # Format and location
//
// A backup is a dialect-neutral dump of the whole database (store.DB.Dump):
// one format for SQLite and PostgreSQL, needing no PostgreSQL client tools
// in the image, and loadable into either dialect. It is stored with
// `restic backup --stdin` in the state repository (backup/repository,
// state.go), encrypted by restic with a password derived from the master
// key, so the master key and the backup location are all a new host needs.
// The dump holds every table, including platform_keys and its secret (the
// login signing key, #128) and the master key's check value; secrets in it
// stay encrypted with the master key, inside restic's own encryption.
//
// # Schedule and retention
//
// The control plane backs up on a schedule (backup.state.interval_hours)
// with a [JobType] job, which takes a backup and then applies the
// retention policy (backup.state.retention) with restic forget --prune.
// `pressfleet state backup` queues one on demand. Snapshots are tagged
// with the dump's schema version and dialect and recorded under a fixed
// host name, so retention spans host changes.
//
// # Restore
//
// [Restore] streams a snapshot from restic into store.Load, which replaces
// the database in one transaction and lets the caller check the loaded
// state (the master key, the login key) and prepare it for the new host
// (sites.PrepareRecovery) before it commits. It runs in the control plane
// container with the control plane stopped (`pressfleet state recover`,
// driven by the host's `pressfleet recover`), since the host CLI never
// touches the state database (§4.7).
package statebackup

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/backup"
	"github.com/rchouinard/pressfleet/control-plane/internal/backup/repository"
	"github.com/rchouinard/pressfleet/control-plane/internal/jobs"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Names of the backup job, its schedule, and its target.
const (
	JobType      = "platform.state_backup"
	ScheduleName = "state-backup"
	targetType   = "platform"
	targetID     = "state_database"
)

// Snapshot tags and names.
const (
	// Tag marks every state snapshot.
	Tag = "pressfleet-state"
	// tagSchema and tagDialect prefix the dump's schema version and
	// dialect; tagVersion the PressFleet release that took it.
	tagSchema  = "schema:"
	tagDialect = "dialect:"
	tagVersion = "version:"
	// Host is the host name every state snapshot is recorded under, so a
	// new host's backups and the old host's share one retention group.
	Host = "pressfleet"
	// FileName is the dump's name in a snapshot.
	FileName = "pressfleet-state.jsonl"
)

// lockWait is how long restic waits for another state backup's lock.
const lockWait = 10 * time.Minute

// Sentinel errors.
var (
	// ErrNoBackups means the state repository has no backups (or doesn't
	// exist).
	ErrNoBackups = errors.New("statebackup: no state backups found")
	// ErrSnapshotNotFound means no state backup has the given ID.
	ErrSnapshotNotFound = errors.New("statebackup: no state backup has that ID")
)

// Repository is the state repository; repository.Manager implements it.
type Repository interface {
	EnsureState(ctx context.Context) error
	StateRestic(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) error
	StateLocation() string
}

// Options configure a Service.
type Options struct {
	// DB is the state database. Required.
	DB *store.DB
	// Repository is the state repository. Required.
	Repository Repository
	// Retention is which backups Forget keeps; the zero policy keeps all.
	Retention backup.Retention
	// TmpDir holds the dump while restic reads it. Required.
	TmpDir string
	// Version is the PressFleet release, recorded on each snapshot.
	Version string
	// Logger is required.
	Logger *slog.Logger
}

// Service takes and lists state backups.
type Service struct {
	db      *store.DB
	repo    Repository
	keep    backup.Retention
	tmpDir  string
	version string
	log     *slog.Logger
}

// New returns a Service.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.Repository == nil || opts.TmpDir == "" || opts.Logger == nil {
		return nil, errors.New("statebackup: DB, Repository, TmpDir, and Logger are required")
	}
	return &Service{db: opts.DB, repo: opts.Repository, keep: opts.Retention, tmpDir: opts.TmpDir, version: opts.Version, log: opts.Logger}, nil
}

// Snapshot is a state backup.
type Snapshot struct {
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	// SchemaVersion, Dialect, and Version are read back from the tags;
	// zero or empty if a tag is missing.
	SchemaVersion int    `json:"schema_version"`
	Dialect       string `json:"dialect"`
	Version       string `json:"version,omitempty"`
	// SizeBytes is the dump's size; Backup fills it in.
	SizeBytes int64 `json:"size_bytes,omitempty"`
}

// Backup dumps the state database and stores the dump in the state
// repository, creating the repository first if needed. Running it again
// takes another snapshot, which restic deduplicates against the last.
func (s *Service) Backup(ctx context.Context) (Snapshot, error) {
	start := time.Now()
	if err := os.MkdirAll(s.tmpDir, 0o700); err != nil {
		return Snapshot{}, fmt.Errorf("state backup: %w", err)
	}
	// A file rather than a pipe: restic takes whatever it reads from stdin
	// as the snapshot, so a dump that failed half way must never reach it.
	f, err := os.CreateTemp(s.tmpDir, "state-backup-*.jsonl")
	if err != nil {
		return Snapshot{}, fmt.Errorf("state backup: %w", err)
	}
	defer func() {
		_ = f.Close()
		_ = os.Remove(f.Name())
	}()
	info, err := s.db.Dump(ctx, f)
	if err != nil {
		return Snapshot{}, fmt.Errorf("state backup: %w", err)
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return Snapshot{}, fmt.Errorf("state backup: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Snapshot{}, fmt.Errorf("state backup: %w", err)
	}
	if err := s.repo.EnsureState(ctx); err != nil {
		return Snapshot{}, fmt.Errorf("state backup: %w", err)
	}
	tags := []string{Tag, tagSchema + strconv.Itoa(info.SchemaVersion), tagDialect + string(info.Dialect)}
	if s.version != "" {
		tags = append(tags, tagVersion+s.version)
	}
	args := []string{"backup", "--json", "--quiet", "--stdin", "--stdin-filename", FileName, "--host", Host,
		"--retry-lock", lockWait.String()}
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	var out bytes.Buffer
	if err := s.repo.StateRestic(ctx, f, &out, args...); err != nil {
		return Snapshot{}, fmt.Errorf("state backup: %w", err)
	}
	id, err := snapshotID(out.Bytes())
	if err != nil {
		return Snapshot{}, fmt.Errorf("state backup: %w", err)
	}
	snap := Snapshot{
		ID: id, Time: info.CreatedAt, SchemaVersion: info.SchemaVersion, Dialect: string(info.Dialect),
		Version: s.version, SizeBytes: size,
	}
	s.log.InfoContext(ctx, "state database backed up", "snapshot_id", id, "schema_version", info.SchemaVersion,
		"tables", info.Tables, "rows", info.Rows, "size_bytes", size, "duration", time.Since(start))
	return snap, nil
}

// snapshotID reads the snapshot ID from restic backup's JSON summary.
func snapshotID(out []byte) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var msg struct {
			MessageType string `json:"message_type"`
			SnapshotID  string `json:"snapshot_id"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.MessageType == "summary" && msg.SnapshotID != "" {
			return msg.SnapshotID, nil
		}
	}
	return "", errors.New("restic reported no snapshot")
}

// Forget applies the retention policy to the state backups and removes the
// data only the forgotten ones used. It returns how many it forgot.
func (s *Service) Forget(ctx context.Context) (int, error) {
	if s.keep.KeepsEverything() {
		return 0, nil
	}
	args := []string{"forget", "--json", "--prune", "--tag", Tag, "--host", Host, "--group-by", "host",
		"--retry-lock", lockWait.String()}
	for _, rule := range []struct {
		flag string
		n    int
	}{
		{"--keep-last", s.keep.KeepLast}, {"--keep-hourly", s.keep.KeepHourly}, {"--keep-daily", s.keep.KeepDaily},
		{"--keep-weekly", s.keep.KeepWeekly}, {"--keep-monthly", s.keep.KeepMonthly}, {"--keep-yearly", s.keep.KeepYearly},
	} {
		if rule.n > 0 {
			args = append(args, rule.flag, strconv.Itoa(rule.n))
		}
	}
	var out bytes.Buffer
	err := s.repo.StateRestic(ctx, nil, &out, args...)
	if errors.Is(err, repository.ErrNoRepository) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("applying state backup retention: %w", err)
	}
	// restic forget --json prints the groups on the first line; --prune's
	// own output follows.
	line, _, _ := bytes.Cut(out.Bytes(), []byte("\n"))
	var groups []struct {
		Remove []json.RawMessage `json:"remove"`
	}
	if err := json.Unmarshal(line, &groups); err != nil {
		return 0, fmt.Errorf("applying state backup retention: reading restic's output: %w", err)
	}
	n := 0
	for _, g := range groups {
		n += len(g.Remove)
	}
	return n, nil
}

// List returns the state backups, oldest first; none if the repository
// doesn't exist yet.
func List(ctx context.Context, repo Repository) ([]Snapshot, error) {
	var out bytes.Buffer
	err := repo.StateRestic(ctx, nil, &out, "snapshots", "--json", "--tag", Tag)
	if errors.Is(err, repository.ErrNoRepository) {
		return []Snapshot{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing state backups: %w", err)
	}
	var raw []struct {
		ID   string    `json:"id"`
		Time time.Time `json:"time"`
		Tags []string  `json:"tags"`
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		return nil, fmt.Errorf("listing state backups: reading restic's output: %w", err)
	}
	snaps := make([]Snapshot, 0, len(raw))
	for _, r := range raw {
		snap := Snapshot{ID: r.ID, Time: r.Time.UTC()}
		for _, t := range r.Tags {
			switch {
			case strings.HasPrefix(t, tagSchema):
				snap.SchemaVersion, _ = strconv.Atoi(strings.TrimPrefix(t, tagSchema))
			case strings.HasPrefix(t, tagDialect):
				snap.Dialect = strings.TrimPrefix(t, tagDialect)
			case strings.HasPrefix(t, tagVersion):
				snap.Version = strings.TrimPrefix(t, tagVersion)
			}
		}
		snaps = append(snaps, snap)
	}
	slices.SortStableFunc(snaps, func(a, b Snapshot) int { return a.Time.Compare(b.Time) })
	return snaps, nil
}

// Find returns the state backup with the given ID, or a unique prefix of it
// of at least 8 characters (restic's short IDs), or the newest for "latest"
// or "".
func Find(ctx context.Context, repo Repository, id string) (Snapshot, error) {
	snaps, err := List(ctx, repo)
	if err != nil {
		return Snapshot{}, err
	}
	if len(snaps) == 0 {
		return Snapshot{}, fmt.Errorf("%w in %s", ErrNoBackups, repo.StateLocation())
	}
	if id == "" || id == "latest" {
		return snaps[len(snaps)-1], nil
	}
	var found []Snapshot
	for _, s := range snaps {
		if len(id) >= 8 && strings.HasPrefix(s.ID, id) {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		return Snapshot{}, fmt.Errorf("%w: %q", ErrSnapshotNotFound, id)
	}
	return found[0], nil
}

// RestoreOptions configure Restore.
type RestoreOptions struct {
	// Store is the state database to replace.
	Store store.Config
	// BackupDir is where an SQLite database is backed up before it is
	// replaced (config.Layout.StateBackupsDir).
	BackupDir string
	// Check runs in the loading transaction before it commits; see
	// store.LoadOptions.
	Check func(ctx context.Context, q store.Querier) error
	// Logger is required.
	Logger *slog.Logger
}

// Restore replaces the state database with the state backup snap (found
// with Find), streaming it from restic into store.Load. Nothing changes
// unless the whole backup loads and opts.Check accepts it.
func Restore(ctx context.Context, repo Repository, snap Snapshot, opts RestoreOptions) (store.LoadResult, error) {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A failure reaches the load through the pipe, which then fails
		// with restic's error.
		_ = pw.CloseWithError(repo.StateRestic(ctx, nil, pw, "dump", "--quiet", snap.ID, "/"+FileName))
	}()
	res, err := store.Load(ctx, opts.Store, pr, store.LoadOptions{BackupDir: opts.BackupDir, Check: opts.Check}, opts.Logger)
	// Unblocks restic if the load stopped reading early.
	_ = pr.Close()
	<-done
	if err != nil {
		return res, fmt.Errorf("restoring state backup %s: %w", snap.ID, err)
	}
	return res, nil
}

// SettleRestored marks the backup job that took a restored backup as
// succeeded: the dump was taken while it ran, so the restored state has it
// running, although it finished on the old host (the backup exists). Call
// it in the restore's transaction, before failing the jobs the old host
// left unfinished.
func SettleRestored(ctx context.Context, q store.Querier, now time.Time) error {
	_, err := store.NewJobs(q).FinishRunning(ctx, JobType, store.JobSucceeded, "", now)
	return err
}

// Handler returns the job handler that backs the state database up and
// then applies the retention policy.
func (s *Service) Handler() jobs.Handler {
	return jobs.Handler{Type: JobType, Steps: []jobs.Step{
		{Name: "backup", Do: func(ctx context.Context, r *jobs.Run) error {
			snap, err := s.Backup(ctx)
			if err != nil {
				return err
			}
			r.Logf("Backed up the state database (schema version %d, %d bytes) as snapshot %s in %s.",
				snap.SchemaVersion, snap.SizeBytes, snap.ID, s.repo.StateLocation())
			return r.SetResult("snapshot_id", snap.ID)
		}},
		{Name: "apply_retention", Do: func(ctx context.Context, r *jobs.Run) error {
			n, err := s.Forget(ctx)
			if err != nil {
				return err
			}
			r.Logf("Retention removed %d older state backups.", n)
			return nil
		}},
	}}
}

// EnsureSchedule creates or updates the backup schedule. It is safe to call
// on every start: an unchanged schedule keeps its next run.
func EnsureSchedule(ctx context.Context, q store.Querier, interval time.Duration) error {
	_, err := jobs.UpsertSchedule(ctx, q, jobs.ScheduleSpec{
		Name:            ScheduleName,
		JobType:         JobType,
		TargetType:      targetType,
		TargetID:        targetID,
		LockKey:         JobType,
		Interval:        interval,
		MissedRunPolicy: store.MissedCoalesce,
	})
	if err != nil {
		return fmt.Errorf("state backup schedule: %w", err)
	}
	return nil
}

// Enqueue queues a backup now, behind any that is running.
func Enqueue(ctx context.Context, q store.Querier) (store.Job, error) {
	return jobs.Enqueue(ctx, q, jobs.Spec{Type: JobType, TargetType: targetType, TargetID: targetID, LockKey: JobType})
}
