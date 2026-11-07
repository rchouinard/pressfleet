package kubernetes

import (
	"bytes"
	"fmt"
	"slices"
	"sync"
)

// The buffers below collect a command's or a task's output. A task
// attachment's copy goroutines may outlive a failed attachment while the
// driver reads them, so they lock.

// headBuffer keeps the first max bytes written to it and discards the
// rest, without failing the writer.
type headBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (b *headBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// Bytes returns a copy of the kept bytes.
func (b *headBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.buf)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu      sync.Mutex
	buf     []byte
	max     int
	dropped int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	// Trim only once the buffer holds twice the limit, so trimming costs
	// amortized constant time per byte.
	if len(b.buf) > 2*b.max {
		b.trim()
	}
	return len(p), nil
}

func (b *tailBuffer) trim() {
	if cut := len(b.buf) - b.max; cut > 0 {
		b.dropped += cut
		b.buf = append(b.buf[:0], b.buf[cut:]...)
	}
}

// String returns the kept bytes, prefixed with a note if some were dropped.
func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trim()
	if b.dropped == 0 {
		return string(b.buf)
	}
	return fmt.Sprintf("[%d bytes truncated]\n%s", b.dropped, bytes.ToValidUTF8(b.buf, nil))
}
