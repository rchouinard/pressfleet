package api

import (
	"context"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
)

// Jobs (#14).

// ListJobs lists jobs, newest first.
func (h *Handlers) ListJobs(context.Context, gen.ListJobsRequestObject) (gen.ListJobsResponseObject, error) {
	return nil, notImplemented(14)
}

// GetJob returns a job with its steps and step log.
func (h *Handlers) GetJob(context.Context, gen.GetJobRequestObject) (gen.GetJobResponseObject, error) {
	return nil, notImplemented(14)
}
