package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/secrets"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Sentinel errors.
var (
	// ErrPermanent marks errors that retrying can't fix. Wrap errors with
	// Permanent rather than returning it directly.
	ErrPermanent = errors.New("permanent failure")
	// ErrNoSecretStore means a step stored a secret result but the pool has
	// no SecretStore.
	ErrNoSecretStore = errors.New("no secret store configured")
)

// Permanent marks err as permanent: the job rolls back without retrying.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

type permanentError struct{ err error }

func (e permanentError) Error() string   { return e.err.Error() }
func (e permanentError) Unwrap() []error { return []error{e.err, ErrPermanent} }

// IsPermanent reports whether err was marked with Permanent.
func IsPermanent(err error) bool { return errors.Is(err, ErrPermanent) }

// Clock tells the time. Tests inject a fake (jobstest.Clock) to drive lease
// expiry, backoff, and schedules without sleeping.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SecretStore keeps secret job outputs encrypted, outside the job's
// plaintext result. secrets.Store implements it.
type SecretStore interface {
	Put(ctx context.Context, kind secrets.Kind, plaintext redact.Secret) (secrets.Ref, error)
}

// Spec describes a job to enqueue.
type Spec struct {
	Type       string
	TargetType string // e.g. "site"
	TargetID   string
	// Payload is marshaled to a JSON object; nil means {}. Never put
	// secrets in it.
	Payload any
	// LockKey, if set, serializes the job with other jobs with the same
	// key, e.g. "site:<id>".
	LockKey string
	// MaxAttempts defaults to store.DefaultMaxAttempts.
	MaxAttempts int
	// RunAfter delays the job; zero means now.
	RunAfter        time.Time
	CreatedByUserID string
}

// Enqueue adds a job to the queue. Pass a transaction's Querier (from
// store.DB.InTx) to enqueue atomically with other changes: if the
// transaction rolls back, the job was never queued.
func Enqueue(ctx context.Context, q store.Querier, s Spec) (store.Job, error) {
	payload, err := marshalObject(s.Payload)
	if err != nil {
		return store.Job{}, fmt.Errorf("enqueue %s job: payload: %w", s.Type, err)
	}
	j := store.Job{
		Type:            s.Type,
		TargetType:      s.TargetType,
		TargetID:        s.TargetID,
		Payload:         payload,
		LockKey:         s.LockKey,
		MaxAttempts:     s.MaxAttempts,
		RunAfter:        s.RunAfter,
		CreatedByUserID: s.CreatedByUserID,
	}
	if err := store.NewJobs(q).Enqueue(ctx, &j); err != nil {
		return store.Job{}, err
	}
	return j, nil
}

func marshalObject(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("{}"), nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil || obj == nil {
		return nil, errors.New("must marshal to a JSON object")
	}
	return b, nil
}
