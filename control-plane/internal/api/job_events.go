package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/api/gen"
	"github.com/rchouinard/pressfleet/control-plane/internal/authz"
	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Job event streams (#120).

// jobEventLogPage bounds the log lines read at once.
const jobEventLogPage = 500

// jobEventPoll is how often a job event stream reads the job when nothing
// in this process says it changed: changes made by other processes reach
// the stream this late at most. Handlers copy it when created; tests
// change it.
var jobEventPoll = 2 * time.Second

// jobEventLogEntry is the JobEventLogEntry schema, which the generator
// leaves out because no JSON body refers to it.
type jobEventLogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Step    string    `json:"step"`
}

// StreamJobEvents follows a job's progress as server-sent events until it
// reaches a terminal state.
func (h *Handlers) StreamJobEvents(ctx context.Context, req gen.StreamJobEventsRequestObject) (gen.StreamJobEventsResponseObject, error) {
	if _, err := h.authorize(ctx, authz.ViewJobs, authz.Job(req.JobID)); err != nil {
		return nil, err
	}
	var after int64
	if p := req.Params.LastEventID; p != nil && *p != "" {
		n, err := strconv.ParseInt(*p, 10, 64)
		if err != nil || n <= 0 {
			return nil, httpserver.NewProblem(http.StatusBadRequest, "Last-Event-ID is invalid.")
		}
		after = n
	}
	// Watch before the first read, so that no change falls between them.
	changes, stop := h.jobChanges.Watch(req.JobID)
	// Read the job first, so that a missing job is an error response and
	// not an empty stream.
	if _, err := store.NewJobs(h.db).Get(ctx, req.JobID); err != nil {
		stop()
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("job %s: %w", req.JobID, ErrNotFound)
		}
		return nil, err
	}
	f := &jobFollower{h: h, jobID: req.JobID, after: after}
	body := newEventStream(ctx, func(ctx context.Context, w io.Writer) error {
		defer stop()
		return f.run(ctx, w, changes)
	})
	return gen.StreamJobEvents200TextEventStreamResponse{
		Body:    body,
		Headers: gen.StreamJobEvents200ResponseHeaders{CacheControl: new("no-store")},
	}, nil
}

// jobFollower writes one job's events.
type jobFollower struct {
	h     *Handlers
	jobID string
	// after is the ID of the last log line sent.
	after int64
	// last is the fingerprint of the last job event sent.
	last string
}

// run reads the job whenever it may have changed and writes what changed,
// until the job is done or ctx ends. It returns the error that ended the
// stream early, if any; it has been sent as an error event then.
func (f *jobFollower) run(ctx context.Context, w io.Writer, changes <-chan struct{}) error {
	poll := time.NewTicker(f.h.jobEventPoll)
	defer poll.Stop()
	keepalive := time.NewTicker(streamKeepalive)
	defer keepalive.Stop()
	for {
		done, err := f.update(ctx, w)
		if ctx.Err() != nil {
			// The client is gone; whatever failed doesn't matter.
			return nil
		}
		if err != nil || done {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-keepalive.C:
			if err := writeKeepalive(w); err != nil {
				return err
			}
		case <-changes:
		case <-poll.C:
		}
	}
}

// update reads the job, its steps, and its new log lines, and writes them.
// While the job runs, its snapshot goes first, so a client sees the job
// before its log; once it is done, the snapshot goes last, so the event
// that shows the job finished follows every line. The job is read first:
// its steps and log lines are written before it finishes, so those read
// after a final job are complete.
func (f *jobFollower) update(ctx context.Context, w io.Writer) (done bool, err error) {
	jobs := store.NewJobs(f.h.db)
	j, err := jobs.Get(ctx, f.jobID)
	if errors.Is(err, store.ErrNotFound) {
		return false, writeProblemEvent(w, httpserver.NewProblem(http.StatusNotFound, "The job no longer exists."), err)
	}
	if err != nil {
		return false, f.fail(ctx, w, err)
	}
	steps, err := jobs.Steps(ctx, f.jobID)
	if err != nil {
		return false, f.fail(ctx, w, err)
	}
	snapshot, err := jobResponse(j, steps, nil)
	if err != nil {
		return false, f.fail(ctx, w, err)
	}
	done = j.State.Final()
	if !done {
		if err := f.writeJob(w, snapshot); err != nil {
			return false, err
		}
	}
	for pending := false; !pending; {
		lines, err := jobs.Logs(ctx, f.jobID, f.after, jobEventLogPage)
		if err != nil {
			return false, f.fail(ctx, w, err)
		}
		for _, l := range lines {
			// While the job runs, a worker can record its steps and log
			// a line between the reads of the steps and of the log. Such
			// a line waits for the next update, which knows its step.
			if !done && l.StepSeq >= len(steps) {
				pending = true
				break
			}
			f.after = l.ID
			// As in GetJob: workers only write step logs, and a line
			// without a step would have nowhere to go.
			if l.StepSeq < 0 || l.StepSeq >= len(steps) {
				continue
			}
			e := jobEventLogEntry{Time: l.LoggedAt.UTC(), Level: l.Level, Message: l.Message, Step: steps[l.StepSeq].Name}
			if err := writeEvent(w, "log", strconv.FormatInt(l.ID, 10), e); err != nil {
				return false, err
			}
		}
		if len(lines) < jobEventLogPage {
			break
		}
	}
	if done {
		if err := f.writeJob(w, snapshot); err != nil {
			return false, err
		}
	}
	return done, nil
}

// writeJob writes the job snapshot unless the client has it already. The
// fingerprint leaves out updated_at, which heartbeats move without
// changing anything a client shows.
func (f *jobFollower) writeJob(w io.Writer, j gen.Job) error {
	key := j
	key.UpdatedAt = time.Time{}
	b, err := json.Marshal(key)
	if err != nil {
		return err
	}
	if string(b) == f.last {
		return nil
	}
	if err := writeEvent(w, "job", "", j); err != nil {
		return err
	}
	f.last = string(b)
	return nil
}

// fail logs an unexpected error and ends the stream with a 500 error event.
func (f *jobFollower) fail(ctx context.Context, w io.Writer, cause error) error {
	if ctx.Err() != nil {
		return cause
	}
	f.h.log.ErrorContext(ctx, "following a job failed", "job_id", f.jobID, "error", cause)
	return writeProblemEvent(w, httpserver.NewProblem(http.StatusInternalServerError, ""), cause)
}
