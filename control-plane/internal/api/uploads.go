package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/auth"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

// Resumable uploads (#111).

// errNoUploads is a wiring bug: serve always passes the upload service.
var errNoUploads = errors.New("api: no upload service configured")

// uploadAction is the permission an upload for purpose needs: that of the
// job it is for. Unknown purposes are checked as imports; the service
// refuses them.
func uploadAction(purpose store.UploadPurpose) authz.Action {
	if purpose == store.UploadSiteDeploy {
		return authz.UploadForDeploy
	}
	return authz.UploadForImport
}

// CreateUpload starts an upload. It is authorized like the import (on the
// client group) or the deployment (on the site) it is for.
func (h *Handlers) CreateUpload(ctx context.Context, req gen.CreateUploadRequestObject) (gen.CreateUploadResponseObject, error) {
	b := deref(req.Body)
	purpose := store.UploadPurpose(b.Purpose)
	target := authz.ClientGroup(deref(b.ClientGroupID))
	if purpose == store.UploadSiteDeploy {
		target = authz.Site(deref(b.SiteID))
	}
	p, err := h.authorize(ctx, uploadAction(purpose), target)
	if err != nil {
		return nil, err
	}
	if h.uploads == nil {
		return nil, errNoUploads
	}
	u, err := h.uploads.Create(ctx, uploads.NewUpload{
		Purpose:         purpose,
		ClientGroupID:   deref(b.ClientGroupID),
		SiteID:          deref(b.SiteID),
		Filename:        deref(b.Filename),
		SizeBytes:       b.SizeBytes,
		SHA256:          deref(b.Sha256),
		CreatedByUserID: p.UserID,
	})
	if err != nil {
		return nil, uploadError(err)
	}
	setAuditTarget(ctx, authz.Upload(u.ID))
	location := "/api/v1/uploads/" + u.ID
	return gen.CreateUpload201JSONResponse{
		Body:    uploadResponse(u),
		Headers: gen.CreateUpload201ResponseHeaders{Location: &location},
	}, nil
}

// GetUpload returns an upload, so a client can resume from received_bytes.
func (h *Handlers) GetUpload(ctx context.Context, req gen.GetUploadRequestObject) (gen.GetUploadResponseObject, error) {
	_, u, err := h.authorizeUpload(ctx, req.UploadID)
	if err != nil {
		return nil, err
	}
	return gen.GetUpload200JSONResponse(uploadResponse(u)), nil
}

// AppendUploadChunk stores the request body at the given offset. The body
// is streamed to disk: the validation middleware neither reads it nor
// holds it to the server's usual body limit (operationBodyLimits).
func (h *Handlers) AppendUploadChunk(ctx context.Context, req gen.AppendUploadChunkRequestObject) (gen.AppendUploadChunkResponseObject, error) {
	if _, _, err := h.authorizeUpload(ctx, req.UploadID); err != nil {
		return nil, err
	}
	u, err := h.uploads.Write(ctx, req.UploadID, req.Params.Offset, req.Body)
	if err != nil {
		return nil, uploadError(err)
	}
	return gen.AppendUploadChunk200JSONResponse(uploadResponse(u)), nil
}

// CompleteUpload verifies the checksum and makes the upload available to
// its job.
func (h *Handlers) CompleteUpload(ctx context.Context, req gen.CompleteUploadRequestObject) (gen.CompleteUploadResponseObject, error) {
	if _, _, err := h.authorizeUpload(ctx, req.UploadID); err != nil {
		return nil, err
	}
	u, err := h.uploads.Complete(ctx, req.UploadID, deref(deref(req.Body).Sha256))
	if err != nil {
		return nil, uploadError(err)
	}
	return gen.CompleteUpload200JSONResponse(uploadResponse(u)), nil
}

// DeleteUpload cancels an upload no job has taken.
func (h *Handlers) DeleteUpload(ctx context.Context, req gen.DeleteUploadRequestObject) (gen.DeleteUploadResponseObject, error) {
	if _, _, err := h.authorizeUpload(ctx, req.UploadID); err != nil {
		return nil, err
	}
	if err := h.uploads.Delete(ctx, req.UploadID); err != nil {
		return nil, uploadError(err)
	}
	return gen.DeleteUpload204Response{}, nil
}

// authorizeUpload is the authorization check (see authorize) of the
// operations on one upload. The permission depends on what the upload is
// for, so the upload is read first, as authorize reads a target to resolve
// its client group; nothing else is read or changed before the check. An
// upload that doesn't exist is checked as an import, then reported as not
// found, as is another user's: only an upload's creator may use it.
func (h *Handlers) authorizeUpload(ctx context.Context, uploadID string) (auth.Principal, store.Upload, error) {
	var (
		u     store.Upload
		found bool
	)
	if h.uploads != nil {
		var err error
		u, err = h.uploads.Get(ctx, uploadID)
		switch {
		case err == nil:
			found = true
		case !errors.Is(err, uploads.ErrNotFound):
			return auth.Principal{}, store.Upload{}, err
		}
	}
	p, err := h.authorize(ctx, uploadAction(u.Purpose), authz.Upload(uploadID))
	if err != nil {
		return auth.Principal{}, store.Upload{}, err
	}
	if h.uploads == nil {
		return auth.Principal{}, store.Upload{}, errNoUploads
	}
	if !found || u.CreatedByUserID != p.UserID {
		return auth.Principal{}, store.Upload{}, fmt.Errorf("%w: %w", ErrNotFound, uploads.ErrNotFound)
	}
	return p, u, nil
}

// uploadError maps upload errors to API errors.
func uploadError(err error) error {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return httpserver.NewProblem(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("A chunk may hold at most %d bytes; send smaller chunks.", mbe.Limit))
	}
	var ve *uploads.ValidationError
	if errors.As(err, &ve) {
		out := &ValidationError{}
		for _, f := range ve.Fields {
			out.Fields = append(out.Fields, httpserver.FieldError{Field: f.Field, Detail: f.Detail})
		}
		return out
	}
	switch {
	case errors.Is(err, uploads.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	case errors.Is(err, uploads.ErrChecksumMismatch):
		return &ValidationError{Fields: []httpserver.FieldError{{Field: "/sha256",
			Detail: "doesn't match the uploaded content; the upload was removed, so start a new one"}}}
	case errors.Is(err, uploads.ErrPastEnd):
		return httpserver.NewProblem(http.StatusRequestEntityTooLarge, err.Error()+".")
	case errors.Is(err, uploads.ErrEmptyChunk), errors.Is(err, uploads.ErrInterrupted):
		// The message of an interrupted chunk says what failed reading it,
		// which is the client's own connection.
		return httpserver.NewProblem(http.StatusBadRequest, err.Error()+".")
	}
	for _, conflict := range []error{uploads.ErrBusy, uploads.ErrOffsetMismatch, uploads.ErrNotReceiving,
		uploads.ErrIncomplete, uploads.ErrInUse} {
		if errors.Is(err, conflict) {
			// The wrapping says what the upload is doing, such as the
			// offset to resume from.
			return fmt.Errorf("%w: %w", ErrConflict, err)
		}
	}
	return err
}

func uploadResponse(u store.Upload) gen.Upload {
	out := gen.Upload{
		ID:            u.ID,
		Purpose:       gen.UploadPurpose(u.Purpose),
		ClientGroupID: u.ClientGroupID,
		Filename:      u.Filename,
		SizeBytes:     u.SizeBytes,
		ReceivedBytes: u.ReceivedBytes,
		Status:        gen.UploadStatus(u.Status),
		MaxChunkBytes: uploads.MaxChunkBytes,
		CreatedAt:     u.CreatedAt,
		UpdatedAt:     u.UpdatedAt,
		SiteID:        optional(u.SiteID),
		Sha256:        optional(u.SHA256),
		JobID:         optional(u.JobID),
	}
	if !u.ExpiresAt.IsZero() {
		expires := u.ExpiresAt
		out.ExpiresAt = &expires
	}
	return out
}
