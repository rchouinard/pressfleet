// Package uploads receives large files for jobs through a resumable,
// chunked upload API (#111, design doc §5.7 step 1, §5.8): the wp-content
// archive and SQL dump of a site import, and the theme or plugin package of
// a code deployment.
//
// # Lifecycle
//
// A client creates an upload ([Service.Create]) declaring what it is for
// (its purpose, and the client group or site), its size, and optionally its
// SHA-256 checksum. It then sends the content in chunks, in order
// ([Service.Write]). A chunk only counts once all of it is on disk, so a
// dropped connection loses at most the chunk in flight: the client asks
// where the upload stands (received_bytes) and resumes from there. Last,
// the client completes the upload ([Service.Complete]), which verifies the
// checksum against what arrived; an upload whose checksum doesn't match is
// discarded.
//
//	uploading ──Complete──▶ complete ──Claim──▶ in_use ──job ends──▶ (removed)
//	    │                      │
//	    └──────expired─────────┴──▶ (removed)
//
// The files live under the data directory (config.Layout.UploadsDir), one
// per upload, and are never backed up.
//
// # Using an upload from a job
//
// Site import (#112) and code deployment (#113) take uploads by ID. The
// API handler that starts the job authorizes the caller for the job itself
// (authz.ImportSite, authz.DeployCode), then claims each upload in the
// transaction that enqueues the job:
//
//	err := db.InTx(ctx, func(q store.Querier) error {
//		job, err := jobs.Enqueue(ctx, q, spec)
//		if err != nil {
//			return err
//		}
//		_, err = uploadSvc.Claim(ctx, q, req.UploadID, uploads.Use{
//			Purpose: store.UploadSiteDeploy, SiteID: siteID, UserID: p.UserID,
//		}, job.ID)
//		return err
//	})
//
// Claim checks that the upload is the caller's, was made for this purpose
// and target, and is complete; otherwise the transaction rolls back and no
// job is queued. A claimed upload no longer expires. The job's steps read
// it with [Service.Open] (or [Service.Path], to hand the file to a task),
// which only work for the job that claimed it.
//
// A claimed upload is removed once its job reaches a final state
// (succeeded, failed, or rollback_failed), by the cleanup job below, so a
// job that is retried or rolls back can still read it. A job may remove it
// sooner with [Service.Release], for example in its last step.
//
// # Cleanup
//
// A platform job ([CleanupJobType]), every [CleanupInterval], removes
// uploads that expired (config uploads.expiry_hours after their last
// chunk, while no job has claimed them), uploads whose job has finished,
// and files left without a record.
//
// # Concurrency
//
// One request at a time writes to an upload; a second one gets [ErrBusy].
// That lock is in memory: uploads live on the control plane's own disk,
// so one control plane process serves them.
package uploads

import (
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// MaxChunkBytes is the largest chunk one request may carry. Clients may
// send smaller ones; 8 to 32 MiB keeps the work lost to a dropped
// connection small.
const MaxChunkBytes = 32 << 20

// MaxFilenameLength bounds the client's name for a file.
const MaxFilenameLength = 255

// Sentinel errors.
var (
	// ErrNotFound: the upload doesn't exist, or isn't the caller's.
	ErrNotFound = errors.New("upload not found")
	// ErrBusy: another request is writing to the upload.
	ErrBusy = errors.New("another request is writing to the upload")
	// ErrOffsetMismatch: a chunk doesn't start where the upload left off.
	// The error is an *OffsetError.
	ErrOffsetMismatch = errors.New("the chunk doesn't start where the upload left off")
	// ErrNotReceiving: the upload is complete or in use and takes no more
	// chunks.
	ErrNotReceiving = errors.New("the upload is complete and takes no more chunks")
	// ErrPastEnd: a chunk goes past the upload's declared size.
	ErrPastEnd = errors.New("the chunk goes past the upload's declared size")
	// ErrEmptyChunk: a chunk has no bytes.
	ErrEmptyChunk = errors.New("the chunk is empty")
	// ErrInterrupted: reading a chunk failed, normally because the client
	// went away. None of the chunk counts; the client sends it again.
	ErrInterrupted = errors.New("the chunk was cut short")
	// ErrIncomplete: the upload hasn't received all its bytes yet.
	ErrIncomplete = errors.New("the upload hasn't received all its bytes")
	// ErrChecksumMismatch: what arrived doesn't have the declared checksum.
	// The upload has been discarded.
	ErrChecksumMismatch = errors.New("the upload's content doesn't match its SHA-256 checksum")
	// ErrInUse: a job has claimed the upload.
	ErrInUse = errors.New("the upload is in use by a job")
	// ErrNotComplete: the upload can't be claimed before it is complete.
	ErrNotComplete = errors.New("the upload isn't complete")
	// ErrWrongUpload: the upload was made for another purpose or target
	// than the job that tries to claim it.
	ErrWrongUpload = errors.New("the upload was made for something else")
	// ErrNotClaimed: the job hasn't claimed the upload.
	ErrNotClaimed = errors.New("the upload isn't claimed by this job")
)

// OffsetError is a chunk sent at the wrong offset. Expected is where the
// upload stands, where the client resumes.
type OffsetError struct {
	Offset, Expected int64
}

func (e *OffsetError) Error() string {
	return fmt.Sprintf("the chunk starts at byte %d, but the upload has received %d bytes; send the next chunk from there", e.Offset, e.Expected)
}

// Is makes an OffsetError match ErrOffsetMismatch.
func (e *OffsetError) Is(target error) bool { return target == ErrOffsetMismatch }

// FieldError is one invalid field of a new upload, named by a JSON Pointer
// into the API request.
type FieldError struct {
	Field  string
	Detail string
}

// ValidationError reports an invalid new upload or completion.
type ValidationError struct {
	Fields []FieldError
}

func (e *ValidationError) Error() string {
	var parts []string
	for _, f := range e.Fields {
		parts = append(parts, f.Field+": "+f.Detail)
	}
	return "invalid upload: " + strings.Join(parts, "; ")
}

func invalid(field, detail string) error {
	return &ValidationError{Fields: []FieldError{{Field: field, Detail: detail}}}
}

// Options configure a Service.
type Options struct {
	// DB is the state database. Required.
	DB *store.DB
	// Dir holds the uploaded files (config.Layout.UploadsDir). Required;
	// created if missing.
	Dir string
	// MaxSize is the largest upload in bytes (config uploads.max_size_mb).
	// Required.
	MaxSize int64
	// Expiry is how long an upload no job has claimed lives after its last
	// chunk (config uploads.expiry_hours). Required.
	Expiry time.Duration
	// Logger defaults to discarding output.
	Logger *slog.Logger
	// Now is the clock; time.Now if nil.
	Now func() time.Time
}

// Service stores uploads and hands them to jobs.
type Service struct {
	db      *store.DB
	dir     string
	maxSize int64
	expiry  time.Duration
	log     *slog.Logger
	now     func() time.Time

	mu   sync.Mutex
	busy map[string]bool
}

// New returns a Service, creating its directory.
func New(opts Options) (*Service, error) {
	switch {
	case opts.DB == nil:
		return nil, errors.New("uploads: DB is required")
	case !filepath.IsAbs(opts.Dir):
		return nil, fmt.Errorf("uploads: directory %q is not absolute", opts.Dir)
	case opts.MaxSize <= 0 || opts.Expiry <= 0:
		return nil, errors.New("uploads: MaxSize and Expiry are required")
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("uploads: creating %s: %w", opts.Dir, err)
	}
	s := &Service{db: opts.DB, dir: opts.Dir, maxSize: opts.MaxSize, expiry: opts.Expiry, log: opts.Logger, now: opts.Now,
		busy: map[string]bool{}}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// MaxSize is the largest upload in bytes.
func (s *Service) MaxSize() int64 { return s.maxSize }

// NewUpload describes an upload to create.
type NewUpload struct {
	Purpose store.UploadPurpose
	// ClientGroupID is the group a site import creates its site in;
	// required for imports, and must be empty for deployments.
	ClientGroupID string
	// SiteID is the site a deployment is for; required for deployments,
	// and must be empty for imports.
	SiteID string
	// Filename is the client's name for the file, for display only.
	Filename string
	// SizeBytes is the file's exact size.
	SizeBytes int64
	// SHA256 is the file's checksum in hex; optional here, and then
	// required on completion.
	SHA256          string
	CreatedByUserID string
}

// Create validates in and creates an empty upload for it.
func (s *Service) Create(ctx context.Context, in NewUpload) (store.Upload, error) {
	var fields []FieldError
	u := store.Upload{
		Purpose: in.Purpose, Filename: in.Filename, SizeBytes: in.SizeBytes,
		SHA256: strings.ToLower(in.SHA256), CreatedByUserID: in.CreatedByUserID,
	}
	switch in.Purpose {
	case store.UploadSiteImport:
		if in.ClientGroupID == "" {
			fields = append(fields, FieldError{"/client_group_id", "is required for a site import"})
		}
		if in.SiteID != "" {
			fields = append(fields, FieldError{"/site_id", "must not be set for a site import"})
		}
		u.ClientGroupID = in.ClientGroupID
	case store.UploadSiteDeploy:
		if in.SiteID == "" {
			fields = append(fields, FieldError{"/site_id", "is required for a deployment"})
		}
		if in.ClientGroupID != "" {
			fields = append(fields, FieldError{"/client_group_id", "must not be set for a deployment; it is the site's"})
		}
		u.SiteID = in.SiteID
	default:
		fields = append(fields, FieldError{"/purpose", fmt.Sprintf("unknown purpose %q", in.Purpose)})
	}
	if in.SizeBytes < 1 {
		fields = append(fields, FieldError{"/size_bytes", "must be at least 1"})
	} else if in.SizeBytes > s.maxSize {
		fields = append(fields, FieldError{"/size_bytes", fmt.Sprintf("exceeds the upload limit of %d bytes (uploads.max_size_mb)", s.maxSize)})
	}
	if u.SHA256 != "" && !validSHA256(u.SHA256) {
		fields = append(fields, FieldError{"/sha256", "must be 64 hexadecimal digits"})
	}
	if len(in.Filename) > MaxFilenameLength || strings.ContainsAny(in.Filename, "/\\\x00") {
		fields = append(fields, FieldError{"/filename", fmt.Sprintf("must be a file name of at most %d bytes, without a directory", MaxFilenameLength)})
	}
	if len(fields) > 0 {
		return store.Upload{}, &ValidationError{Fields: fields}
	}
	if u.CreatedByUserID == "" {
		return store.Upload{}, errors.New("create upload: the user is required")
	}

	// A deployment belongs to its site's client group.
	if u.SiteID != "" {
		site, err := store.NewSites(s.db).Get(ctx, u.SiteID)
		if errors.Is(err, store.ErrNotFound) {
			return store.Upload{}, invalid("/site_id", "site does not exist")
		} else if err != nil {
			return store.Upload{}, err
		}
		u.ClientGroupID = site.ClientGroupID
	}
	at := s.now()
	u.ExpiresAt = at.Add(s.expiry)
	if err := store.NewUploads(s.db).Create(ctx, &u, at); errors.Is(err, store.ErrReference) {
		// The group or site doesn't exist (or was just deleted), or the user
		// was deleted meanwhile.
		if u.SiteID != "" {
			return store.Upload{}, invalid("/site_id", "site does not exist")
		}
		return store.Upload{}, invalid("/client_group_id", "client group does not exist")
	} else if err != nil {
		return store.Upload{}, err
	}
	// The record comes first, so a file never exists without one except
	// while it is being removed; the cleanup job removes such files.
	f, err := os.OpenFile(s.path(u.ID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		err = f.Close()
	}
	if err != nil {
		if _, derr := store.NewUploads(s.db).Delete(context.WithoutCancel(ctx), u.ID); derr != nil {
			s.log.Error("removing the record of a failed upload failed", "upload_id", u.ID, "error", derr)
		}
		return store.Upload{}, fmt.Errorf("create upload file: %w", err)
	}
	s.log.Info("upload created", "upload_id", u.ID, "purpose", string(u.Purpose), "size_bytes", u.SizeBytes)
	return u, nil
}

// Get returns the upload, or ErrNotFound.
func (s *Service) Get(ctx context.Context, uploadID string) (store.Upload, error) {
	if !id.Valid(uploadID) {
		return store.Upload{}, ErrNotFound
	}
	u, err := store.NewUploads(s.db).Get(ctx, uploadID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Upload{}, ErrNotFound
	}
	return u, err
}

// Write appends the chunk read from r, which must start at offset, the
// number of bytes the upload has received. The chunk only counts once all
// of it is read and on disk: if reading r fails, the upload stays where it
// was and the client sends the chunk again. It returns the upload after
// the chunk.
func (s *Service) Write(ctx context.Context, uploadID string, offset int64, r io.Reader) (store.Upload, error) {
	if !s.lock(uploadID) {
		return store.Upload{}, ErrBusy
	}
	defer s.unlock(uploadID)
	u, err := s.Get(ctx, uploadID)
	if err != nil {
		return store.Upload{}, err
	}
	switch {
	case u.Status != store.UploadReceiving:
		return store.Upload{}, ErrNotReceiving
	case offset != u.ReceivedBytes:
		return store.Upload{}, &OffsetError{Offset: offset, Expected: u.ReceivedBytes}
	}
	h, err := restoreHash(u.HashState)
	if err != nil {
		return store.Upload{}, fmt.Errorf("upload %s: %w", u.ID, err)
	}
	n, err := s.writeAt(u, offset, r, h)
	if err != nil {
		return store.Upload{}, err
	}
	state, err := h.(encoding.BinaryMarshaler).MarshalBinary()
	if err != nil {
		return store.Upload{}, fmt.Errorf("saving the checksum state of upload %s: %w", u.ID, err)
	}
	at := s.now()
	ok, err := store.NewUploads(s.db).Advance(ctx, u.ID, offset, offset+n, state, at.Add(s.expiry), at)
	if err != nil {
		return store.Upload{}, err
	}
	if !ok {
		// Removed meanwhile: the lock keeps other writers out.
		return store.Upload{}, ErrNotFound
	}
	return s.Get(ctx, u.ID)
}

// writeAt writes the chunk from r into the upload's file at offset and
// into h, returning its length. Anything past offset from an earlier,
// interrupted chunk is overwritten.
func (s *Service) writeAt(u store.Upload, offset int64, r io.Reader, h io.Writer) (int64, error) {
	f, err := os.OpenFile(s.path(u.ID), os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return 0, fmt.Errorf("opening upload %s: %w", u.ID, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(offset); err != nil {
		return 0, fmt.Errorf("writing upload %s: %w", u.ID, err)
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("writing upload %s: %w", u.ID, err)
	}
	remaining := u.SizeBytes - offset
	// One byte more than fits tells a chunk that goes past the end from one
	// that just reaches it.
	body := &readTracker{r: r}
	n, err := io.CopyBuffer(io.MultiWriter(f, h), io.LimitReader(body, remaining+1), make([]byte, 1<<20))
	switch {
	case body.err != nil:
		return 0, fmt.Errorf("%w: %w", ErrInterrupted, body.err)
	case err != nil:
		return 0, fmt.Errorf("receiving a chunk of upload %s: %w", u.ID, err)
	case n > remaining:
		return 0, fmt.Errorf("%w (%d bytes)", ErrPastEnd, u.SizeBytes)
	case n == 0:
		return 0, ErrEmptyChunk
	}
	if err := f.Sync(); err != nil {
		return 0, fmt.Errorf("writing upload %s: %w", u.ID, err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("writing upload %s: %w", u.ID, err)
	}
	return n, nil
}

// Complete finishes an upload that has received all its bytes: it checks
// them against the SHA-256 checksum, given here (sum) or when the upload
// was created, and makes the upload available to jobs. If the content
// doesn't match, the upload is discarded and ErrChecksumMismatch returned.
// Completing a complete upload again returns it.
func (s *Service) Complete(ctx context.Context, uploadID, sum string) (store.Upload, error) {
	sum = strings.ToLower(sum)
	if sum != "" && !validSHA256(sum) {
		return store.Upload{}, invalid("/sha256", "must be 64 hexadecimal digits")
	}
	if !s.lock(uploadID) {
		return store.Upload{}, ErrBusy
	}
	defer s.unlock(uploadID)
	u, err := s.Get(ctx, uploadID)
	if err != nil {
		return store.Upload{}, err
	}
	if u.Status != store.UploadReceiving {
		if sum != "" && sum != u.SHA256 {
			return store.Upload{}, ErrNotReceiving
		}
		return u, nil
	}
	want := u.SHA256
	switch {
	case sum != "" && want != "" && sum != want:
		return store.Upload{}, invalid("/sha256", "differs from the checksum given when the upload was created")
	case sum != "":
		want = sum
	case want == "":
		return store.Upload{}, invalid("/sha256", "is required, since none was given when the upload was created")
	}
	if u.ReceivedBytes != u.SizeBytes {
		return store.Upload{}, fmt.Errorf("%w: %d of %d bytes", ErrIncomplete, u.ReceivedBytes, u.SizeBytes)
	}
	h, err := restoreHash(u.HashState)
	if err != nil {
		return store.Upload{}, fmt.Errorf("upload %s: %w", u.ID, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		if err := s.discard(ctx, u.ID); err != nil {
			return store.Upload{}, err
		}
		s.log.Warn("upload discarded: checksum mismatch", "upload_id", u.ID)
		return store.Upload{}, ErrChecksumMismatch
	}
	// Drop anything a chunk that went past the end left behind.
	if err := os.Truncate(s.path(u.ID), u.SizeBytes); err != nil {
		return store.Upload{}, fmt.Errorf("completing upload %s: %w", u.ID, err)
	}
	at := s.now()
	ok, err := store.NewUploads(s.db).Complete(ctx, u.ID, want, at.Add(s.expiry), at)
	if err != nil {
		return store.Upload{}, err
	}
	if !ok {
		return store.Upload{}, ErrNotFound
	}
	s.log.Info("upload complete", "upload_id", u.ID, "size_bytes", u.SizeBytes)
	return s.Get(ctx, u.ID)
}

// Delete removes an upload no job has claimed.
func (s *Service) Delete(ctx context.Context, uploadID string) error {
	if !s.lock(uploadID) {
		return ErrBusy
	}
	defer s.unlock(uploadID)
	u, err := s.Get(ctx, uploadID)
	if err != nil {
		return err
	}
	if u.Status == store.UploadInUse {
		return ErrInUse
	}
	return s.discard(ctx, u.ID)
}

// discard removes an unclaimed upload: the record, then the file.
func (s *Service) discard(ctx context.Context, uploadID string) error {
	ok, err := store.NewUploads(s.db).DeleteUnclaimed(ctx, uploadID)
	if err != nil {
		return err
	}
	if !ok {
		// Claimed since the caller looked (or already gone).
		if _, gerr := s.Get(ctx, uploadID); errors.Is(gerr, ErrNotFound) {
			return ErrNotFound
		}
		return ErrInUse
	}
	return s.removeFile(uploadID)
}

// Use is what a job needs an upload to be, to claim it.
type Use struct {
	// Purpose is the job's kind.
	Purpose store.UploadPurpose
	// ClientGroupID is the group a site import creates its site in.
	ClientGroupID string
	// SiteID is the site a deployment is for.
	SiteID string
	// UserID is the user starting the job, who must have created the
	// upload.
	UserID string
}

// Claim hands a complete upload to the job jobID, which must be the
// caller's and made for use: ErrNotFound if it doesn't exist or isn't
// use.UserID's, ErrWrongUpload if it was made for another purpose or
// target, ErrNotComplete or ErrInUse if it isn't available. Call it with
// the transaction that enqueues the job, after authorizing the caller for
// the job, so a job is only queued with its uploads. A claimed upload no
// longer expires; it is removed when the job ends.
func (s *Service) Claim(ctx context.Context, q store.Querier, uploadID string, use Use, jobID string) (store.Upload, error) {
	if !id.Valid(uploadID) {
		return store.Upload{}, ErrNotFound
	}
	uploads := store.NewUploads(q)
	u, err := uploads.Get(ctx, uploadID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (use.UserID == "" || u.CreatedByUserID != use.UserID)) {
		return store.Upload{}, ErrNotFound
	} else if err != nil {
		return store.Upload{}, err
	}
	switch {
	case u.Purpose != use.Purpose:
		return store.Upload{}, fmt.Errorf("%w: it is for %s, not %s", ErrWrongUpload, u.Purpose, use.Purpose)
	case u.Purpose == store.UploadSiteDeploy && u.SiteID != use.SiteID:
		return store.Upload{}, fmt.Errorf("%w: it is for another site", ErrWrongUpload)
	case u.Purpose == store.UploadSiteImport && u.ClientGroupID != use.ClientGroupID:
		return store.Upload{}, fmt.Errorf("%w: it is for another client group", ErrWrongUpload)
	case u.Status == store.UploadReceiving:
		return store.Upload{}, fmt.Errorf("%w: %d of %d bytes received", ErrNotComplete, u.ReceivedBytes, u.SizeBytes)
	case u.Status == store.UploadInUse:
		return store.Upload{}, ErrInUse
	}
	ok, err := uploads.Claim(ctx, uploadID, jobID, s.now())
	if err != nil {
		return store.Upload{}, err
	}
	if !ok {
		return store.Upload{}, ErrInUse
	}
	return uploads.Get(ctx, uploadID)
}

// Claimed returns the upload if the job has claimed it: ErrNotFound if it
// is gone, ErrNotClaimed if the job didn't claim it. Jobs should treat both
// as permanent.
func (s *Service) Claimed(ctx context.Context, uploadID, jobID string) (store.Upload, error) {
	u, err := s.Get(ctx, uploadID)
	if err != nil {
		return store.Upload{}, err
	}
	if u.Status != store.UploadInUse || u.JobID != jobID {
		return store.Upload{}, ErrNotClaimed
	}
	return u, nil
}

// Open opens the content of an upload the job has claimed (see Claimed)
// for reading.
func (s *Service) Open(ctx context.Context, uploadID, jobID string) (*os.File, store.Upload, error) {
	u, err := s.Claimed(ctx, uploadID, jobID)
	if err != nil {
		return nil, store.Upload{}, err
	}
	f, err := os.Open(s.path(u.ID))
	if err != nil {
		return nil, store.Upload{}, fmt.Errorf("opening upload %s: %w", u.ID, err)
	}
	return f, u, nil
}

// Path returns the path of an upload the job has claimed (see Claimed),
// for handing the file to a task, e.g. as a read-only bind mount (the data
// directory is at the same path on the host). The file belongs to the
// control plane's user with mode 0600; copy it for tasks that run as
// another user.
func (s *Service) Path(ctx context.Context, uploadID, jobID string) (string, store.Upload, error) {
	u, err := s.Claimed(ctx, uploadID, jobID)
	if err != nil {
		return "", store.Upload{}, err
	}
	return s.path(u.ID), u, nil
}

// Release removes an upload the job has claimed, once it no longer needs
// it. It succeeds if the upload is already gone, and returns ErrNotClaimed
// if another job claimed it. The cleanup job removes claimed uploads when
// their job ends anyway; Release only frees the space sooner.
func (s *Service) Release(ctx context.Context, uploadID, jobID string) error {
	if !id.Valid(uploadID) {
		return ErrNotFound
	}
	ok, err := store.NewUploads(s.db).DeleteClaimed(ctx, uploadID, jobID)
	if err != nil {
		return err
	}
	if !ok {
		if _, err := s.Get(ctx, uploadID); errors.Is(err, ErrNotFound) {
			return s.removeFile(uploadID)
		} else if err != nil {
			return err
		}
		return ErrNotClaimed
	}
	return s.removeFile(uploadID)
}

// path is where an upload's content lives. IDs are checked before they
// get here (id.Valid), so they never leave the directory.
func (s *Service) path(uploadID string) string { return filepath.Join(s.dir, uploadID) }

func (s *Service) removeFile(uploadID string) error {
	if err := os.Remove(s.path(uploadID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing upload %s: %w", uploadID, err)
	}
	return nil
}

// lock takes the upload's write lock, reporting false if another request
// holds it.
func (s *Service) lock(uploadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[uploadID] {
		return false
	}
	s.busy[uploadID] = true
	return true
}

func (s *Service) unlock(uploadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, uploadID)
}

// readTracker remembers the error reading from r, to tell a client that
// went away from a failing disk.
type readTracker struct {
	r   io.Reader
	err error
}

func (t *readTracker) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		t.err = err
	}
	return n, err
}

// restoreHash returns a SHA-256 hash in the saved state, or a new one.
func restoreHash(state []byte) (hash.Hash, error) {
	h := sha256.New()
	if len(state) > 0 {
		if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state); err != nil {
			return nil, fmt.Errorf("restoring the checksum state: %w", err)
		}
	}
	return h, nil
}

func validSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
