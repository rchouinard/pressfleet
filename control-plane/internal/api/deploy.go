package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/deploy"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/uploads"
)

// Code deployments (#113, design doc §5.8).

// errNoDeployments is a wiring bug: serve always passes the service.
var errNoDeployments = errors.New("api: no deployment service configured")

// DeploySiteCode queues a job that deploys an uploaded theme or plugin
// package to the site, claiming the upload for it, and returns 202 with
// the job.
func (h *Handlers) DeploySiteCode(ctx context.Context, req gen.DeploySiteCodeRequestObject) (gen.DeploySiteCodeResponseObject, error) {
	p, err := h.authorize(ctx, authz.DeployCode, authz.Site(req.SiteID))
	if err != nil {
		return nil, err
	}
	if h.deploys == nil {
		return nil, errNoDeployments
	}
	b := deref(req.Body)
	job, err := h.deploys.Deploy(ctx, req.SiteID, deploy.Request{
		UploadID: b.UploadID,
		Kind:     deploy.Kind(b.Kind),
		Name:     b.Name,
		Activate: deref(b.Activate),
		UserID:   p.UserID,
	})
	if err != nil {
		return nil, deployError(err)
	}
	j, err := jobResponse(job, nil, nil)
	if err != nil {
		return nil, err
	}
	location := jobLocation(job)
	return gen.DeploySiteCode202JSONResponse{
		Body:    gen.JobAccepted{Job: j},
		Headers: gen.DeploySiteCode202ResponseHeaders{Location: &location},
	}, nil
}

// deployError maps deployment errors to API errors.
func deployError(err error) error {
	var ve *deploy.ValidationError
	if errors.As(err, &ve) {
		out := &ValidationError{}
		for _, f := range ve.Fields {
			out.Fields = append(out.Fields, httpserver.FieldError{Field: f.Field, Detail: f.Detail})
		}
		return out
	}
	switch {
	case errors.Is(err, deploy.ErrSiteNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, deploy.ErrSiteNotFound)
	case errors.Is(err, deploy.ErrNotDeployable), errors.Is(err, uploads.ErrNotComplete), errors.Is(err, uploads.ErrInUse):
		// The wrapping says what the site or upload is doing.
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return err
}
