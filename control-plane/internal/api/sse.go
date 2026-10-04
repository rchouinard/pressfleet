package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/httpserver"
)

// Server-sent event streams: site logs (#78) and job events (#120).

// streamKeepalive is how often a stream sends a comment, so proxies keep
// it open and a vanished client's writes fail.
const streamKeepalive = 15 * time.Second

// streamingOperations last as long as the client watches, so the server's
// write timeout, which is for responses that end, doesn't apply to them.
var streamingOperations = map[string]bool{
	"streamSiteLogs":  true,
	"streamJobEvents": true,
}

// newEventStream runs write in a goroutine and returns the response body
// it writes to. Closing the body, which the response does when the client
// is gone, cancels write's context; write's error ends the body.
func newEventStream(ctx context.Context, write func(ctx context.Context, w io.Writer) error) io.ReadCloser {
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	go func() {
		defer cancel()
		pw.CloseWithError(write(ctx, pw))
	}()
	return &eventStream{PipeReader: pr, cancel: cancel}
}

// eventStream is the body of a stream: closing it stops the writer.
type eventStream struct {
	*io.PipeReader
	cancel context.CancelFunc
	once   sync.Once
}

func (s *eventStream) Close() error {
	s.once.Do(s.cancel)
	return s.PipeReader.Close()
}

// writeEvent writes v as JSON in an event of the given type, with an ID
// unless id is empty.
func writeEvent(w io.Writer, event, id string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if id != "" {
		_, err = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", id, event, data)
	} else {
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	}
	return err
}

// writeKeepalive writes a comment that clients ignore.
func writeKeepalive(w io.Writer) error {
	_, err := io.WriteString(w, ": keepalive\n\n")
	return err
}

// writeProblemEvent tells the client why the stream ends, as the Problem
// Details a failed request would have got, and returns cause.
func writeProblemEvent(w io.Writer, p *httpserver.Problem, cause error) error {
	if err := writeEvent(w, "error", "", p); err != nil {
		return err
	}
	return cause
}
