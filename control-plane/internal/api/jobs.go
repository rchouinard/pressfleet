package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Jobs (#14).

// ListJobs lists jobs, newest first. The cursor is the ID of the last job
// on the previous page.
func (h *Handlers) ListJobs(ctx context.Context, req gen.ListJobsRequestObject) (gen.ListJobsResponseObject, error) {
	// TODO(#103): h.authorize(ctx, <read jobs>, nil) goes here, before anything else.
	if h.db == nil {
		return nil, errNoDatabase
	}
	p := req.Params
	f := store.JobFilter{}
	if p.Limit != nil {
		f.Limit = *p.Limit
	}
	if p.Cursor != nil {
		if !id.Valid(*p.Cursor) {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "The cursor is invalid; pass the next_cursor of a previous page.")
		}
		f.Before = *p.Cursor
	}
	if p.State != nil {
		f.States = []store.JobState{store.JobState(*p.State)}
	}
	if p.TargetType != nil {
		f.TargetType = string(*p.TargetType)
	}
	if p.TargetID != nil {
		f.TargetID = *p.TargetID
	}
	jobs := store.NewJobs(h.db)
	page, err := jobs.List(ctx, f)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(page.Jobs))
	for i, j := range page.Jobs {
		ids[i] = j.ID
	}
	steps, err := jobs.StepsForJobs(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := gen.ListJobs200JSONResponse{Items: make([]gen.Job, len(page.Jobs))}
	for i, j := range page.Jobs {
		if out.Items[i], err = jobResponse(j, steps[j.ID], nil); err != nil {
			return nil, err
		}
	}
	if page.NextCursor != "" {
		out.NextCursor = &page.NextCursor
	}
	return out, nil
}

// GetJob returns a job with its steps and step log.
func (h *Handlers) GetJob(ctx context.Context, req gen.GetJobRequestObject) (gen.GetJobResponseObject, error) {
	// TODO(#103): h.authorize(ctx, <read job>, req.JobID) goes here, before anything else.
	if h.db == nil {
		return nil, errNoDatabase
	}
	d, err := store.NewJobs(h.db).Detail(ctx, req.JobID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("job %s: %w", req.JobID, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	logs := map[int][]gen.JobLogEntry{}
	for _, l := range d.Logs {
		// Workers only write step logs; lines without a step would have
		// nowhere to go in the response.
		if l.StepSeq >= 0 {
			logs[l.StepSeq] = append(logs[l.StepSeq], gen.JobLogEntry{
				Time: l.LoggedAt, Level: gen.JobLogEntryLevel(l.Level), Message: l.Message,
			})
		}
	}
	j, err := jobResponse(d.Job, d.Steps, logs)
	if err != nil {
		return nil, err
	}
	return gen.GetJob200JSONResponse(j), nil
}

// errNoDatabase is a wiring bug: serve always passes the database.
var errNoDatabase = errors.New("api: no state database configured")

// jobResponse converts a job for the API. logs is nil for listings, which
// omit step logs.
func jobResponse(j store.Job, steps []store.JobStep, logs map[int][]gen.JobLogEntry) (gen.Job, error) {
	out := gen.Job{
		ID:          j.ID,
		Type:        j.Type,
		Target:      gen.JobTarget{Type: gen.JobTargetType(j.TargetType), ID: j.TargetID},
		State:       gen.JobState(j.State),
		Attempts:    j.Attempts,
		MaxAttempts: j.MaxAttempts,
		Result:      map[string]any{},
		Steps:       make([]gen.JobStep, len(steps)),
		CreatedAt:   j.CreatedAt,
		UpdatedAt:   j.UpdatedAt,
		StartedAt:   optionalTime(j.StartedAt),
		FinishedAt:  optionalTime(j.FinishedAt),
	}
	if len(j.Result) > 0 {
		if err := json.Unmarshal(j.Result, &out.Result); err != nil {
			return gen.Job{}, fmt.Errorf("decoding result of job %s: %w", j.ID, err)
		}
	}
	if j.Error != "" {
		out.Error = &j.Error
	}
	if j.CreatedByUserID != "" {
		out.CreatedBy = &j.CreatedByUserID
	}
	for i, s := range steps {
		out.Steps[i] = gen.JobStep{
			Name:       s.Name,
			State:      gen.JobStepState(s.State),
			Attempts:   s.Attempts,
			StartedAt:  optionalTime(s.StartedAt),
			FinishedAt: optionalTime(s.FinishedAt),
		}
		if logs != nil {
			l := logs[s.Seq]
			if l == nil {
				l = []gen.JobLogEntry{}
			}
			out.Steps[i].Log = &l
		}
	}
	return out, nil
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
