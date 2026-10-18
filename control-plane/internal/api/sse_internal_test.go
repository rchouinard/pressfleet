package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/provisioner"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

func TestWriteKeepalive(t *testing.T) {
	var buf bytes.Buffer
	if err := writeKeepalive(&buf); err != nil || buf.String() != ": keepalive\n\n" {
		t.Errorf("keepalive = %q, %v", buf.String(), err)
	}
}

// Streams that end on an error send it as an "error" event with the
// Problem Details a request would have got, and log only unexpected ones.
func TestStreamErrorEvents(t *testing.T) {
	var logs bytes.Buffer
	h := &Handlers{log: slog.New(slog.NewTextHandler(&logs, nil))}
	ctx := context.Background()
	tests := []struct {
		name       string
		write      func(*bytes.Buffer) error
		cause      error
		wantStatus int
		wantLog    bool
	}{
		{
			name:  "site log stream, site gone",
			cause: fmt.Errorf("reading: %w", provisioner.ErrSiteNotFound),
			write: func(b *bytes.Buffer) error {
				return h.writeLogError(ctx, b, "s1", fmt.Errorf("reading: %w", provisioner.ErrSiteNotFound))
			},
			wantStatus: 404,
		},
		{
			name:       "site log stream, unexpected",
			cause:      errors.New("docker down"),
			write:      func(b *bytes.Buffer) error { return h.writeLogError(ctx, b, "s1", errors.New("docker down")) },
			wantStatus: 500, wantLog: true,
		},
		{
			name:  "job stream, unexpected",
			cause: errors.New("db down"),
			write: func(b *bytes.Buffer) error {
				return (&jobFollower{h: h, jobID: "j1"}).fail(ctx, b, errors.New("db down"))
			},
			wantStatus: 500, wantLog: true,
		},
	}
	for _, tt := range tests {
		logs.Reset()
		var buf bytes.Buffer
		err := tt.write(&buf)
		if err == nil || err.Error() != tt.cause.Error() {
			t.Errorf("%s: returned %v, want the cause", tt.name, err)
		}
		out := buf.String()
		if !strings.HasPrefix(out, "event: error\ndata: {") || !strings.Contains(out, fmt.Sprintf(`"status":%d`, tt.wantStatus)) {
			t.Errorf("%s: event %q, want an error event with status %d", tt.name, out, tt.wantStatus)
		}
		if strings.Contains(out, tt.cause.Error()) && tt.wantStatus == 500 {
			t.Errorf("%s: event %q reveals the internal error", tt.name, out)
		}
		if logged := logs.Len() > 0; logged != tt.wantLog {
			t.Errorf("%s: logged %v, want %v: %s", tt.name, logged, tt.wantLog, logs.String())
		}
	}

	// A job stream whose client went away ends quietly.
	logs.Reset()
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	var buf bytes.Buffer
	cause := errors.New("canceled")
	if err := (&jobFollower{h: h, jobID: "j1"}).fail(canceled, &buf, cause); !errors.Is(err, cause) || buf.Len() != 0 || logs.Len() != 0 {
		t.Errorf("canceled: err %v, wrote %q, logged %q; want the cause and nothing else", err, buf.String(), logs.String())
	}
}

func TestCoreFailureResponse(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.FixedZone("EDT", -4*3600))
	got := coreFailureResponse(store.RolloutFailure{SiteID: "s1", Target: "6.9.2", Message: "health check failed", FailedAt: at})
	if got.SiteID != "s1" || got.Version != "6.9.2" || got.Message != "health check failed" ||
		!got.FailedAt.Equal(at) || got.FailedAt.Location() != time.UTC {
		t.Errorf("coreFailureResponse = %+v", got)
	}
}
