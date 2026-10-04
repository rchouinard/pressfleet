package api

import (
	"testing"
	"time"
)

// SetJobEventPoll sets how often job event streams poll, for handlers
// created until the test ends.
func SetJobEventPoll(t testing.TB, d time.Duration) {
	old := jobEventPoll
	jobEventPoll = d
	t.Cleanup(func() { jobEventPoll = old })
}
