package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/id"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Pool defaults.
const (
	DefaultConcurrency   = 4
	DefaultPollInterval  = time.Second
	DefaultLeaseDuration = time.Minute
	// DefaultShutdownGrace leaves room inside the control plane container's
	// 60s stop_grace_period for the rest of the shutdown.
	DefaultShutdownGrace = 30 * time.Second
)

// Options configures a Pool. Zero values select the defaults.
type Options struct {
	// Concurrency is the number of jobs run at once.
	Concurrency int
	// PollInterval is how often an idle pool looks for work.
	PollInterval time.Duration
	// LeaseDuration is the visibility timeout: how long a job stays leased
	// to a worker that stops heartbeating. Heartbeats run every third of it.
	LeaseDuration time.Duration
	// ShutdownGrace is how long running steps get to finish on shutdown.
	ShutdownGrace time.Duration
	// Backoff returns the delay before retrying a job after its attempt'th
	// failed attempt. The default doubles from 15s up to 15m.
	Backoff func(attempt int) time.Duration
	// Clock defaults to the system clock.
	Clock Clock
	// Logger defaults to discarding output.
	Logger *slog.Logger
	// Secrets stores secret results; nil disables Run.SetSecretResult.
	Secrets SecretStore
	// Changes is notified whenever the pool records progress on a job;
	// nil notifies no one.
	Changes *Changes
}

// DefaultBackoff doubles from 15 seconds, capped at 15 minutes.
func DefaultBackoff(attempt int) time.Duration {
	d := 15 * time.Second
	for i := 1; i < attempt && d < 15*time.Minute; i++ {
		d *= 2
	}
	return min(d, 15*time.Minute)
}

// Pool leases jobs and runs them with their registered handlers.
type Pool struct {
	db   *store.DB
	opts Options
	log  *slog.Logger
	name string

	mu       sync.Mutex
	handlers map[string]Handler
	types    []string
}

// NewPool returns a pool that runs jobs from db. Register handlers, then
// call Run.
func NewPool(db *store.DB, opts Options) *Pool {
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = DefaultLeaseDuration
	}
	if opts.ShutdownGrace <= 0 {
		opts.ShutdownGrace = DefaultShutdownGrace
	}
	if opts.Backoff == nil {
		opts.Backoff = DefaultBackoff
	}
	if opts.Clock == nil {
		opts.Clock = systemClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	host, _ := os.Hostname()
	return &Pool{
		db:       db,
		opts:     opts,
		log:      opts.Logger,
		name:     fmt.Sprintf("%s:%d", host, os.Getpid()),
		handlers: map[string]Handler{},
	}
}

// Register adds the handler for a job type. The pool only leases jobs of
// registered types.
func (p *Pool) Register(h Handler) error {
	if err := h.validate(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.handlers[h.Type]; ok {
		return fmt.Errorf("handler for %s jobs already registered", h.Type)
	}
	p.handlers[h.Type] = h
	p.types = append(p.types, h.Type)
	sort.Strings(p.types)
	return nil
}

func (p *Pool) handler(typ string) (Handler, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.handlers[typ]
	return h, ok
}

func (p *Pool) registeredTypes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.types...)
}

func (p *Pool) now() time.Time { return p.opts.Clock.Now() }

// errShutdown cancels running steps when the shutdown grace period ends.
var errShutdown = errors.New("control plane shutting down")

// errLeaseLost cancels a job's steps when a heartbeat finds that another
// worker has taken the job over.
var errLeaseLost = errors.New("job lease lost")

// Run leases and runs jobs until ctx is canceled, then shuts down as the
// package documentation describes. It returns once no job is running, or
// once running jobs have ignored cancellation for a few seconds after the
// grace period.
func (p *Pool) Run(ctx context.Context) error {
	p.log.Info("job workers started", "concurrency", p.opts.Concurrency, "types", p.registeredTypes())
	jobsCtx, cancelJobs := context.WithCancelCause(context.WithoutCancel(ctx))
	defer cancelJobs(nil)
	slots := make(chan struct{}, p.opts.Concurrency)
	var wg sync.WaitGroup

lease:
	for {
		select {
		case <-ctx.Done():
			break lease
		case slots <- struct{}{}:
		}
		job, ok := p.lease(ctx)
		if !ok {
			<-slots
			select {
			case <-ctx.Done():
				break lease
			case <-time.After(p.opts.PollInterval):
			}
			continue
		}
		wg.Go(func() {
			defer func() { <-slots }()
			p.execute(jobsCtx, ctx.Done(), job)
		})
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		p.log.Info("job workers stopped")
		return nil
	case <-time.After(p.opts.ShutdownGrace):
	}
	p.log.Warn("shutdown grace period over; canceling running job steps", "grace", p.opts.ShutdownGrace)
	cancelJobs(errShutdown)
	select {
	case <-done:
		p.log.Info("job workers stopped")
	case <-time.After(abandonAfter):
		p.log.Error("abandoning job steps that ignored cancellation; their leases will expire")
	}
	return nil
}

// abandonAfter is how long Run waits for steps to return after canceling
// them.
const abandonAfter = 5 * time.Second

// RunNext leases one runnable job and runs it to the end of this attempt,
// reporting whether there was one. It is for tests and tools; servers use
// Run.
func (p *Pool) RunNext(ctx context.Context) (bool, error) {
	types := p.registeredTypes()
	now := p.now()
	job, err := store.NewJobs(p.db).Lease(ctx, types, p.newOwner(), now, now.Add(p.opts.LeaseDuration))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	p.execute(ctx, nil, job)
	return true, nil
}

func (p *Pool) newOwner() string { return p.name + ":" + id.New() }

// lease claims the next job, logging errors, which are expected now and
// then (e.g. SQLite busy while another process holds the lock).
func (p *Pool) lease(ctx context.Context) (store.Job, bool) {
	types := p.registeredTypes()
	if len(types) == 0 {
		return store.Job{}, false
	}
	// Don't let cancellation interrupt the statement: the job could be
	// leased in the database without the pool knowing, and sit until the
	// lease expired.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	now := p.now()
	job, err := store.NewJobs(p.db).Lease(lctx, types, p.newOwner(), now, now.Add(p.opts.LeaseDuration))
	switch {
	case err == nil:
		p.opts.Changes.Notify(job.ID)
		return job, true
	case errors.Is(err, store.ErrNotFound):
	default:
		p.log.Warn("leasing a job failed", "error", err)
	}
	return store.Job{}, false
}
