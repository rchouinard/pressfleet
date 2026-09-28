package repository

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/config"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Sentinel errors callers branch on.
var (
	// ErrInvalidClientGroupID means an ID is not a well-formed ID.
	ErrInvalidClientGroupID = errors.New("repository: invalid client group id")
	// ErrClientGroupNotFound means the client group doesn't exist.
	ErrClientGroupNotFound = errors.New("repository: client group not found")
	// ErrNoRepository means the client group has no repository yet.
	ErrNoRepository = errors.New("repository: client group has no backup repository")
	// ErrGroupHasSites means Delete was refused because the client group
	// still has sites, whose backups the repository holds.
	ErrGroupHasSites = errors.New("repository: client group still has sites")
	// ErrWrongPassword means a repository exists at the group's location
	// but the stored password doesn't open it, e.g. after the state
	// database was restored from a backup older than the repository. The
	// repository is left alone.
	ErrWrongPassword = errors.New("repository: stored password doesn't open the repository")
	// ErrLocked means restic couldn't lock the repository because another
	// operation holds a conflicting lock.
	ErrLocked = errors.New("repository: repository is locked")
	// ErrS3NotConfigured means a repository lives on S3 but no S3
	// credentials are configured.
	ErrS3NotConfigured = errors.New("repository: S3 backend is not configured")
	// ErrExportDestination means the export destination is unusable: not
	// an absolute path, or an existing non-empty directory.
	ErrExportDestination = errors.New("repository: unusable export destination")
)

// Backends, the values of Repository.Backend (config.BackupConfig.Backend).
const (
	BackendLocal = config.BackupBackendLocal
	BackendS3    = config.BackupBackendS3
)

// S3Config is the S3-compatible bucket repositories are created in
// (config.BackupS3Config with the secret access key decrypted).
type S3Config struct {
	// Endpoint is the service's base URL, e.g. https://s3.amazonaws.com or
	// http://minio:9000.
	Endpoint string
	Bucket   string
	// Prefix is an optional key prefix without leading or trailing
	// slashes; each group's repository is <prefix>/<client group ID>.
	Prefix string
	// Region is the bucket's region; empty means us-east-1.
	Region          string
	AccessKeyID     redact.Secret
	SecretAccessKey redact.Secret
}

func (s S3Config) configured() bool { return s.Endpoint != "" && s.Bucket != "" }

// Options configure a Manager.
type Options struct {
	// DB is the state database. Required.
	DB *store.DB
	// Secrets stores repository passwords. Required.
	Secrets secrets.Store
	// Layout locates local repositories. Required.
	Layout config.Layout
	// Backend is where new repositories are created: BackendLocal (the
	// default when empty) or BackendS3.
	Backend string
	// S3 is required for BackendS3, and for operations on repositories
	// created on S3.
	S3 S3Config
	// Restic is the restic binary; empty means "restic" on PATH.
	Restic string
	// RepositoryGID owns local repositories; zero means
	// provisioner.RepositoryGID, the group backup tasks run with.
	RepositoryGID int
	// HTTPClient talks to S3 for export and delete; nil means a default
	// client.
	HTTPClient *http.Client
	// Logger is required.
	Logger *slog.Logger
}

// Manager creates, exports, and deletes client group repositories.
type Manager struct {
	db      *store.DB
	secrets secrets.Store
	layout  config.Layout
	backend string
	s3      S3Config
	restic  string
	gid     int
	http    *http.Client
	log     *slog.Logger
	// warnedChgrp says the skipped group ownership was logged.
	warnedChgrp atomic.Bool
}

// New returns a Manager. It doesn't run restic or contact S3.
func New(opts Options) (*Manager, error) {
	if opts.DB == nil || opts.Logger == nil {
		return nil, errors.New("repository: DB and Logger are required")
	}
	if !filepath.IsAbs(opts.Layout.Root) {
		return nil, fmt.Errorf("repository: data directory must be an absolute path, got %q", opts.Layout.Root)
	}
	m := &Manager{
		db: opts.DB, secrets: opts.Secrets, layout: opts.Layout, backend: opts.Backend, s3: opts.S3,
		restic: opts.Restic, gid: opts.RepositoryGID, http: opts.HTTPClient, log: opts.Logger,
	}
	if m.gid == 0 {
		m.gid = provisioner.RepositoryGID
	}
	switch m.backend {
	case "":
		m.backend = BackendLocal
	case BackendLocal:
	case BackendS3:
		if !opts.S3.configured() {
			return nil, fmt.Errorf("%w: endpoint and bucket are required", ErrS3NotConfigured)
		}
	default:
		return nil, fmt.Errorf("repository: unknown backend %q", opts.Backend)
	}
	if opts.S3.configured() {
		if err := config.CheckS3Endpoint(opts.S3.Endpoint); err != nil {
			return nil, fmt.Errorf("repository: %w", err)
		}
		if err := config.CheckS3Prefix(opts.S3.Prefix); err != nil {
			return nil, fmt.Errorf("repository: %w", err)
		}
	}
	if m.restic == "" {
		m.restic = "restic"
	}
	if m.http == nil {
		m.http = &http.Client{}
	}
	return m, nil
}

// Repository is a client group's repository record.
type Repository struct {
	ClientGroupID string
	// Backend is BackendLocal or BackendS3.
	Backend string
	// Location is the restic repository as the control plane opens it: an
	// absolute path (on the host and in the control plane container, which
	// mount the data directory at the same path) or an s3: URL. It holds no
	// credentials.
	Location  string
	SecretRef secrets.Ref
	CreatedAt time.Time
}

// Get returns the group's repository record, or ErrNoRepository.
func (m *Manager) Get(ctx context.Context, clientGroupID string) (Repository, error) {
	if !id.Valid(clientGroupID) {
		return Repository{}, fmt.Errorf("%w %q", ErrInvalidClientGroupID, clientGroupID)
	}
	r, err := getRecord(ctx, m.db, clientGroupID)
	if errors.Is(err, store.ErrNotFound) {
		return Repository{}, fmt.Errorf("client group %s: %w", clientGroupID, ErrNoRepository)
	}
	return r, err
}

// Ensure returns the group's repository, creating it if needed: it stores
// a new random password with the group's record, then runs restic init
// unless the repository already exists. It is idempotent and safe to run
// concurrently for the same group.
func (m *Manager) Ensure(ctx context.Context, clientGroupID string) (Repository, error) {
	if !id.Valid(clientGroupID) {
		return Repository{}, fmt.Errorf("%w %q", ErrInvalidClientGroupID, clientGroupID)
	}
	rec, err := getRecord(ctx, m.db, clientGroupID)
	if errors.Is(err, store.ErrNotFound) {
		rec, err = m.createRecord(ctx, clientGroupID)
	}
	if err != nil {
		return Repository{}, fmt.Errorf("ensure backup repository of client group %s: %w", clientGroupID, err)
	}
	password, err := m.password(ctx, rec)
	if err != nil {
		return Repository{}, fmt.Errorf("ensure backup repository of client group %s: %w", clientGroupID, err)
	}
	if err := m.initialize(ctx, rec, password); err != nil {
		return Repository{}, fmt.Errorf("ensure backup repository of client group %s: %w", clientGroupID, err)
	}
	return rec, nil
}

// createRecord generates a password and stores it with the group's record,
// in one transaction.
func (m *Manager) createRecord(ctx context.Context, groupID string) (Repository, error) {
	location, err := m.newLocation(groupID)
	if err != nil {
		return Repository{}, err
	}
	rec := Repository{ClientGroupID: groupID, Backend: m.backend, Location: location, CreatedAt: time.Now().UTC()}
	err = m.db.InTx(ctx, func(q store.Querier) error {
		groups := store.NewClientGroups(q)
		if _, err := groups.Get(ctx, groupID); errors.Is(err, store.ErrNotFound) {
			return ErrClientGroupNotFound
		} else if err != nil {
			return err
		}
		ref, err := m.secrets.With(q).Put(ctx, secrets.KindResticRepoKey, newPassword())
		if err != nil {
			return err
		}
		rec.SecretRef = ref
		if err := insertRecord(ctx, q, rec); err != nil {
			return err
		}
		return groups.SetBackupRepository(ctx, groupID, location)
	})
	if errors.Is(err, store.ErrConflict) {
		// A concurrent Ensure for this group committed first; our secret
		// rolled back with the transaction. Use its record.
		return getRecord(ctx, m.db, groupID)
	}
	if err != nil {
		return Repository{}, fmt.Errorf("storing repository password: %w", err)
	}
	m.log.Info("backup repository password created", "client_group_id", groupID, "backend", rec.Backend)
	return rec, nil
}

// newLocation returns where a new repository for the group goes.
func (m *Manager) newLocation(groupID string) (string, error) {
	if m.backend == BackendS3 {
		return s3Location(m.s3, groupID), nil
	}
	return m.layout.BackupRepositoryDir(groupID)
}

// s3Location returns the restic repository URL of a group's repository.
func s3Location(s S3Config, groupID string) string {
	key := groupID
	if s.Prefix != "" {
		key = s.Prefix + "/" + groupID
	}
	return "s3:" + strings.TrimSuffix(s.Endpoint, "/") + "/" + s.Bucket + "/" + key
}

// s3Object is a repository location on S3, parsed from its s3: URL.
type s3Object struct {
	endpoint *url.URL
	bucket   string
	key      string // the repository's key prefix, without a trailing slash
}

func parseS3Location(location string) (s3Object, error) {
	rest, ok := strings.CutPrefix(location, "s3:")
	if !ok {
		return s3Object{}, fmt.Errorf("%q is not an s3: repository location", location)
	}
	u, err := url.Parse(rest)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return s3Object{}, fmt.Errorf("%q is not an s3: repository location", location)
	}
	bucket, key, ok := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	if !ok || bucket == "" || key == "" {
		return s3Object{}, fmt.Errorf("%q has no bucket and key", location)
	}
	return s3Object{endpoint: &url.URL{Scheme: u.Scheme, Host: u.Host}, bucket: bucket, key: key}, nil
}

// s3Client returns a client for the bucket holding an S3 repository.
func (m *Manager) s3Client(obj s3Object) (*s3Client, error) {
	if m.s3.AccessKeyID == "" {
		return nil, ErrS3NotConfigured
	}
	region := m.s3.Region
	if region == "" {
		region = defaultS3Region
	}
	return &s3Client{
		endpoint: obj.endpoint, bucket: obj.bucket, region: region,
		keyID: m.s3.AccessKeyID, secret: m.s3.SecretAccessKey, http: m.http, now: time.Now,
	}, nil
}

// password returns the repository's password.
func (m *Manager) password(ctx context.Context, rec Repository) (redact.Secret, error) {
	pw, err := m.secrets.Get(ctx, rec.SecretRef)
	if err != nil {
		return "", fmt.Errorf("reading repository password: %w", err)
	}
	return pw, nil
}

// initialize runs restic init unless the repository already exists and
// opens with the password.
func (m *Manager) initialize(ctx context.Context, rec Repository, password redact.Secret) error {
	local := rec.Backend == BackendLocal
	var dir string
	if local {
		var err error
		if dir, err = m.localDir(rec); err != nil {
			return err
		}
	}
	err := m.probe(ctx, rec, password)
	switch {
	case err == nil:
		if local && !isShared(dir) {
			// An earlier Ensure stopped between restic init and sharing.
			return m.shareLocal(dir)
		}
		return nil
	case !errors.Is(err, errRepositoryMissing):
		return err
	}
	if local {
		if err := m.prepareLocalDir(dir); err != nil {
			return err
		}
	}
	if _, err := m.run(ctx, rec, password, "init"); err != nil {
		// A concurrent Ensure may have initialized it first.
		if m.probe(ctx, rec, password) != nil {
			return err
		}
	} else {
		m.log.Info("backup repository initialized", "client_group_id", rec.ClientGroupID, "location", rec.Location)
	}
	if local {
		return m.shareLocal(dir)
	}
	return nil
}

// probe checks that the repository exists and opens with the password. It
// returns errRepositoryMissing if there is no repository.
func (m *Manager) probe(ctx context.Context, rec Repository, password redact.Secret) error {
	if rec.Backend == BackendS3 {
		// restic retries a missing bucket for many minutes instead of
		// reporting that the repository doesn't exist.
		obj, err := parseS3Location(rec.Location)
		if err != nil {
			return err
		}
		c, err := m.s3Client(obj)
		if err != nil {
			return err
		}
		ok, err := c.exists(ctx, obj.key+"/config")
		if err != nil {
			return err
		}
		if !ok {
			return errRepositoryMissing
		}
	}
	_, err := m.run(ctx, rec, password, "cat", "config")
	return err
}

// Restic runs a restic command (e.g. "snapshots", "--json") against the
// group's existing repository in the control plane process and returns its
// standard output. It is for group-level operations such as forget, prune,
// and check; callers serialize operations that take an exclusive lock.
func (m *Manager) Restic(ctx context.Context, clientGroupID string, args ...string) ([]byte, error) {
	rec, err := m.Get(ctx, clientGroupID)
	if err != nil {
		return nil, err
	}
	password, err := m.password(ctx, rec)
	if err != nil {
		return nil, err
	}
	return m.run(ctx, rec, password, args...)
}

// Delete removes the group's repository: first its data, then its record,
// password, the group's backup_repository, and the records of the backups
// it held (store.BackupRecords). It succeeds if the group has
// no repository, so a failed delete can be retried. It refuses with
// ErrGroupHasSites while the group has sites.
func (m *Manager) Delete(ctx context.Context, clientGroupID string) error {
	rec, err := m.Get(ctx, clientGroupID)
	if errors.Is(err, ErrNoRepository) {
		return nil
	}
	if err != nil {
		return err
	}
	g, err := store.NewClientGroups(m.db).Get(ctx, clientGroupID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("delete backup repository of client group %s: %w", clientGroupID, err)
	}
	if g.SiteCount > 0 {
		return fmt.Errorf("delete backup repository of client group %s: %w (%d)", clientGroupID, ErrGroupHasSites, g.SiteCount)
	}
	if err := m.removeData(ctx, rec); err != nil {
		return fmt.Errorf("delete backup repository of client group %s: %w", clientGroupID, err)
	}
	err = m.db.InTx(ctx, func(q store.Querier) error {
		// The backups those records describe are gone with the data.
		if _, err := store.NewBackupRecords(q).DeleteForClientGroup(ctx, clientGroupID); err != nil {
			return err
		}
		if err := deleteRecord(ctx, q, clientGroupID); err != nil {
			return err
		}
		if err := m.secrets.With(q).Delete(ctx, rec.SecretRef); err != nil {
			return err
		}
		err := store.NewClientGroups(q).SetBackupRepository(ctx, clientGroupID, "")
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("delete backup repository of client group %s: deleting record: %w", clientGroupID, err)
	}
	m.log.Info("backup repository deleted", "client_group_id", clientGroupID, "location", rec.Location)
	return nil
}

// removeData deletes the repository's files or objects. Nothing to delete
// is not an error.
func (m *Manager) removeData(ctx context.Context, rec Repository) error {
	switch rec.Backend {
	case BackendLocal:
		dir, err := m.localDir(rec)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("removing repository files: %w", err)
		}
		return nil
	case BackendS3:
		obj, err := parseS3Location(rec.Location)
		if err != nil {
			return err
		}
		c, err := m.s3Client(obj)
		if err != nil {
			return err
		}
		keys, err := c.list(ctx, obj.key+"/")
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := c.remove(ctx, k); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown backend %q", rec.Backend)
	}
}

// localDir returns a local repository's directory after checking that it is
// the group's directory under the data directory, so a damaged record can't
// make Delete remove anything else.
func (m *Manager) localDir(rec Repository) (string, error) {
	want, err := m.layout.BackupRepositoryDir(rec.ClientGroupID)
	if err != nil {
		return "", err
	}
	if filepath.Clean(rec.Location) != want {
		return "", fmt.Errorf("repository location %s is not %s; was the data directory moved?", rec.Location, want)
	}
	return want, nil
}

// TaskAccess is what a site task container (provisioner.TaskRunner) needs to
// run restic against a group's repository, e.g. `restic backup`.
type TaskAccess struct {
	// Env are the non-secret environment variables: RESTIC_REPOSITORY
	// (provisioner.TaskRepositoryPath for local repositories, the s3: URL
	// otherwise) and, on S3 with a region, AWS_DEFAULT_REGION.
	Env map[string]string
	// SecretEnv are the secret environment variables: RESTIC_PASSWORD and,
	// on S3, AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
	SecretEnv map[string]redact.Secret
	// RepositoryDir is the host directory for provisioner.Task.RepositoryDir,
	// which a task sees at provisioner.TaskRepositoryPath; empty for S3
	// repositories. It belongs to the repository group
	// (provisioner.RepositoryGID), which tasks get as a supplementary group.
	RepositoryDir string
}

// TaskAccess ensures the group's repository exists (see Ensure) and returns
// what a site task needs to use it.
func (m *Manager) TaskAccess(ctx context.Context, clientGroupID string) (TaskAccess, error) {
	rec, err := m.Ensure(ctx, clientGroupID)
	if err != nil {
		return TaskAccess{}, err
	}
	password, err := m.password(ctx, rec)
	if err != nil {
		return TaskAccess{}, err
	}
	a := TaskAccess{
		Env:       map[string]string{},
		SecretEnv: map[string]redact.Secret{"RESTIC_PASSWORD": password},
	}
	switch rec.Backend {
	case BackendLocal:
		dir, err := m.localDir(rec)
		if err != nil {
			return TaskAccess{}, err
		}
		a.RepositoryDir = dir
		a.Env["RESTIC_REPOSITORY"] = provisioner.TaskRepositoryPath
	case BackendS3:
		if m.s3.AccessKeyID == "" {
			return TaskAccess{}, ErrS3NotConfigured
		}
		a.Env["RESTIC_REPOSITORY"] = rec.Location
		maps.Copy(a.Env, s3Env(m.s3))
		a.SecretEnv["AWS_ACCESS_KEY_ID"] = m.s3.AccessKeyID
		a.SecretEnv["AWS_SECRET_ACCESS_KEY"] = m.s3.SecretAccessKey
	default:
		return TaskAccess{}, fmt.Errorf("unknown backend %q", rec.Backend)
	}
	return a, nil
}

// s3Env returns the non-secret S3 settings restic reads from its
// environment.
func s3Env(s S3Config) map[string]string {
	env := map[string]string{}
	if s.Region != "" {
		env["AWS_DEFAULT_REGION"] = s.Region
	}
	return env
}

// newPassword returns a random repository password: 256 bits, base64url.
func newPassword() redact.Secret {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails
	return redact.Secret(base64.RawURLEncoding.EncodeToString(b))
}
