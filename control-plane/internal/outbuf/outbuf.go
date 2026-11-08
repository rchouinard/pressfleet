// Package outbuf holds the bounded buffers the drivers collect command and
// task output in. It is driver-neutral: the Compose and Kubernetes drivers
// share it, and it imports nothing of either's SDK.
package outbuf

import (
	"bytes"
	"fmt"
	"slices"
	"sync"
)

// Head keeps the first Max bytes written to it and discards the rest,
// without failing the writer. Callers that must tell whether output was cut
// set Max to their limit plus one and compare. It locks because a task
// attachment's copy goroutines may outlive a failed attachment while the
// driver reads the buffer.
type Head struct {
	// Max is the number of bytes kept.
	Max int

	mu  sync.Mutex
	buf []byte
}

// Write implements io.Writer.
func (b *Head) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.Max - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// Bytes returns a copy of the kept bytes.
func (b *Head) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.buf)
}

// Tail keeps the last Max bytes written to it. Like [Head] it locks.
type Tail struct {
	// Max is the number of bytes kept.
	Max int

	mu      sync.Mutex
	buf     []byte
	dropped int
}

// Write implements io.Writer.
func (b *Tail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	// Trim only once the buffer holds twice the limit, so trimming costs
	// amortized constant time per byte.
	if len(b.buf) > 2*b.Max {
		b.trim()
	}
	return len(p), nil
}

func (b *Tail) trim() {
	if cut := len(b.buf) - b.Max; cut > 0 {
		b.dropped += cut
		b.buf = append(b.buf[:0], b.buf[cut:]...)
	}
}

// String returns the kept bytes, prefixed with a note if some were dropped.
func (b *Tail) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trim()
	if b.dropped == 0 {
		return string(b.buf)
	}
	return fmt.Sprintf("[%d bytes truncated]\n%s", b.dropped, bytes.ToValidUTF8(b.buf, nil))
}

// held reports how many bytes the buffer currently holds, for tests.
func (b *Tail) held() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}
