package api

import (
	"net/http"
	"testing"
	"time"
)

// NewPartialHandler is NewHandler without the required services, for
// tests that exercise one area of the API.
func NewPartialHandler(opts Options) (http.Handler, error) { return newHandler(opts, false) }

// SetJobEventPoll sets how often job event streams poll, for handlers
// created until the test ends.
func SetJobEventPoll(t testing.TB, d time.Duration) {
	old := jobEventPoll
	jobEventPoll = d
	t.Cleanup(func() { jobEventPoll = old })
}
