package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// StepFunc does or undoes one step. It must be idempotent (see the package
// documentation) and should return promptly when ctx is canceled.
type StepFunc func(ctx context.Context, r *Run) error

// Step is one named unit of a job's work.
type Step struct {
	// Name identifies the step in progress records and the API, e.g.
	// "create_database". Keep names stable: a job enqueued by an older
	// release resumes by matching step names.
	Name string
	// Do performs the step.
	Do StepFunc
	// Undo reverses Do when the job fails permanently. Nil means there is
	// nothing to undo.
	Undo StepFunc
}

// Handler runs one job type as an ordered list of steps.
//
// A provisioning handler looks like:
//
//	jobs.Handler{
//		Type: "site.create",
//		Steps: []jobs.Step{
//			{Name: "create_database", Do: createDB, Undo: dropDB},
//			{Name: "start_stack", Do: startStack, Undo: removeStack},
//			{Name: "install_wordpress", Do: installWP},
//		},
//	}
//
// where each function has the StepFunc signature and reads the job's
// payload with Run.Payload.
type Handler struct {
	Type  string
	Steps []Step
}

func (h Handler) validate() error {
	if h.Type == "" {
		return errors.New("handler type is required")
	}
	if len(h.Steps) == 0 {
		return fmt.Errorf("handler %s has no steps", h.Type)
	}
	seen := map[string]bool{}
	for i, s := range h.Steps {
		switch {
		case s.Name == "":
			return fmt.Errorf("handler %s: step %d has no name", h.Type, i)
		case seen[s.Name]:
			return fmt.Errorf("handler %s: duplicate step name %q", h.Type, s.Name)
		case s.Do == nil:
			return fmt.Errorf("handler %s: step %q has no Do", h.Type, s.Name)
		}
		seen[s.Name] = true
	}
	return nil
}

// stepNamed returns the handler's step called name.
func (h Handler) stepNamed(name string) (Step, bool) {
	for _, s := range h.Steps {
		if s.Name == name {
			return s, true
		}
	}
	return Step{}, false
}

// Run is a step's view of the job it is running. Its methods write to the
// database at once, so their effects survive a crash; they fail with
// store.ErrLeaseLost if another worker has taken the job over.
type Run struct {
	e      *execution
	step   int
	data   map[string]json.RawMessage
	result map[string]json.RawMessage
}

func newRun(e *execution) (*Run, error) {
	r := &Run{e: e, step: -1}
	if err := json.Unmarshal(orEmpty(e.job.Data), &r.data); err != nil {
		return nil, fmt.Errorf("decoding data of job %s: %w", e.job.ID, err)
	}
	if err := json.Unmarshal(orEmpty(e.job.Result), &r.result); err != nil {
		return nil, fmt.Errorf("decoding result of job %s: %w", e.job.ID, err)
	}
	if r.data == nil {
		r.data = map[string]json.RawMessage{}
	}
	if r.result == nil {
		r.result = map[string]json.RawMessage{}
	}
	return r, nil
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

// Job returns the job as it was when this worker leased it. Attempts is
// the current attempt number.
func (r *Run) Job() store.Job { return r.e.job }

// Payload decodes the job's payload into v.
func (r *Run) Payload(v any) error {
	if err := json.Unmarshal(orEmpty(r.e.job.Payload), v); err != nil {
		return Permanent(fmt.Errorf("decoding payload of %s job: %w", r.e.job.Type, err))
	}
	return nil
}

// Get decodes the data value stored under key into v, reporting whether it
// exists.
func (r *Run) Get(key string, v any) (bool, error) {
	return get(r.data, key, v)
}

func get(m map[string]json.RawMessage, key string, v any) (bool, error) {
	raw, ok := m[key]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return true, fmt.Errorf("decoding job value %q: %w", key, err)
	}
	return true, nil
}

// Set stores v under key in the job's data for later steps and later
// attempts, writing it to the database before returning. Never store
// secrets; a redact.Secret is stored as a placeholder.
func (r *Run) Set(key string, v any) error {
	return r.put(r.data, key, v, func(ctx context.Context, raw json.RawMessage) error {
		return store.NewJobs(r.e.p.db).SetData(ctx, r.e.job.ID, r.e.owner, raw, r.e.p.now())
	})
}

// SetResult stores v under key in the job's result, which API clients see.
// Never store secrets; use SetSecretResult.
func (r *Run) SetResult(key string, v any) error {
	return r.put(r.result, key, v, func(ctx context.Context, raw json.RawMessage) error {
		return store.NewJobs(r.e.p.db).SetResult(ctx, r.e.job.ID, r.e.owner, raw, r.e.p.now())
	})
}

// SecretResult is how a secret output appears in a job's result: a
// reference that an API handler resolves with secrets.Store.Get, e.g. to
// show a generated admin password once.
type SecretResult struct {
	SecretRef secrets.Ref `json:"secret_ref"`
}

// SetSecretResult stores value in the pool's SecretStore and puts a
// SecretResult referring to it under key in the result. It returns
// ErrNoSecretStore if the pool has none. Each call stores a new secret, so
// a step that can run again should check GetResult first rather than leave
// an orphaned secret behind.
func (r *Run) SetSecretResult(key string, kind secrets.Kind, value redact.Secret) error {
	if r.e.p.opts.Secrets == nil {
		return ErrNoSecretStore
	}
	ctx, cancel := r.e.writeContext()
	defer cancel()
	ref, err := r.e.p.opts.Secrets.Put(ctx, kind, value)
	if err != nil {
		return fmt.Errorf("storing secret result %q: %w", key, err)
	}
	return r.SetResult(key, SecretResult{SecretRef: ref})
}

// GetResult decodes the result value stored under key into v, reporting
// whether it exists.
func (r *Run) GetResult(key string, v any) (bool, error) {
	return get(r.result, key, v)
}

func (r *Run) put(m map[string]json.RawMessage, key string, v any, write func(context.Context, json.RawMessage) error) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding job value %q: %w", key, err)
	}
	old, had := m[key]
	m[key] = b
	raw, err := json.Marshal(m)
	if err == nil {
		ctx, cancel := r.e.writeContext()
		defer cancel()
		err = write(ctx, raw)
	}
	if err != nil {
		// Keep memory in line with the database.
		if had {
			m[key] = old
		} else {
			delete(m, key)
		}
		return err
	}
	return nil
}

// Logf adds an info line to the step's log, which users see. Never format a
// revealed secret into it.
func (r *Run) Logf(format string, args ...any) {
	r.log(slog.LevelInfo, fmt.Sprintf(format, args...))
}

// Warnf adds a warning line to the step's log.
func (r *Run) Warnf(format string, args ...any) {
	r.log(slog.LevelWarn, fmt.Sprintf(format, args...))
}

func (r *Run) log(level slog.Level, msg string) {
	r.e.writeLog(r.step, level, msg)
}
