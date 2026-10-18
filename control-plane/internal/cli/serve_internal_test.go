package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe for the workers' concurrent logging.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestWorkerGroup(t *testing.T) {
	var logs syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	w := &workerGroup{ctx: ctx, log: slog.New(slog.NewTextHandler(&logs, nil))}

	w.Go("broken", func(context.Context) error { return errors.New("database gone") })
	w.Go("quitter", func(context.Context) error { return nil })
	// Well-behaved workers run until shutdown, whatever they return then.
	w.Go("steady", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	w.Go("noisy at shutdown", func(ctx context.Context) error { <-ctx.Done(); return errors.New("flush failed") })
	ran := false
	w.Once(func(context.Context) { ran = true })

	// Shut down only once both early stops are logged: the group tells
	// an early stop from a shutdown by whether ctx is done when the worker
	// returns.
	early := []string{"worker=broken", "worker=quitter"}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		out := logs.String()
		if strings.Contains(out, early[0]) && strings.Contains(out, early[1]) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("early stops not logged:\n%s", out)
		}
	}
	cancel()
	w.Wait()

	out := logs.String()
	for _, want := range []string{
		`level=ERROR msg="background worker failed; the control plane keeps running without it" worker=broken error="database gone"`,
		`level=ERROR msg="background worker stopped early; the control plane keeps running without it" worker=quitter`,
		`level=WARN msg="background worker stopped with an error during shutdown" worker="noisy at shutdown" error="flush failed"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "worker=steady") {
		t.Errorf("a worker that stopped at shutdown was reported:\n%s", out)
	}
	if !ran {
		t.Error("Once didn't run its task")
	}
}
