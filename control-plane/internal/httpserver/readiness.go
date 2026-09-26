package httpserver

import (
	"context"
	"sync"
	"time"
)

// DefaultCheckTimeout bounds each readiness check.
const DefaultCheckTimeout = 5 * time.Second

// ReadinessCheck reports whether a dependency is ready. It must respect ctx.
// GET /readyz is unauthenticated and shows the error message, so the message
// must not contain secrets such as connection strings.
type ReadinessCheck func(ctx context.Context) error

// CheckResult is the outcome of one readiness check. Err is nil if it passed.
type CheckResult struct {
	Name string
	Err  error
}

// Readiness holds the checks behind GET /readyz. Components register their
// checks at startup (for example, the state database); the zero value has no
// checks and is always ready. It is safe for concurrent use.
type Readiness struct {
	// Timeout bounds each check; zero means DefaultCheckTimeout.
	Timeout time.Duration

	mu     sync.RWMutex
	names  []string
	checks []ReadinessCheck
}

// Register adds a named check. Checks run in registration order.
func (r *Readiness) Register(name string, check ReadinessCheck) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
	r.checks = append(r.checks, check)
}

// Check runs every registered check concurrently and returns their results
// in registration order.
func (r *Readiness) Check(ctx context.Context) []CheckResult {
	r.mu.RLock()
	names, checks := r.names, r.checks
	r.mu.RUnlock()

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	results := make([]CheckResult, len(checks))
	var wg sync.WaitGroup
	for i, check := range checks {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			results[i] = CheckResult{Name: names[i], Err: check(ctx)}
		})
	}
	wg.Wait()
	return results
}
