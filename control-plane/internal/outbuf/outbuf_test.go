package outbuf

import (
	"strings"
	"sync"
	"testing"
)

func TestHead(t *testing.T) {
	b := &Head{Max: 5}
	for _, s := range []string{"ab", "cdef", "gh"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	if got := string(b.Bytes()); got != "abcde" {
		t.Errorf("kept %q, want abcde", got)
	}
}

func TestTail(t *testing.T) {
	b := &Tail{Max: 4}
	if _, _ = b.Write([]byte("ab")); b.String() != "ab" {
		t.Errorf("short = %q", b.String())
	}
	for range 10 {
		if n, err := b.Write([]byte("0123456789")); n != 10 || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	got := b.String()
	if !strings.HasSuffix(got, "\n6789") || !strings.HasPrefix(got, "[98 bytes truncated]") {
		t.Errorf("String() = %q", got)
	}
	if n := b.held(); n > 2*b.Max {
		t.Errorf("buffer holds %d bytes, limit %d", n, b.Max)
	}
}

// TestConcurrent is for the race detector: writers may outlive the reader.
func TestConcurrent(_ *testing.T) {
	h, tl := &Head{Max: 10}, &Tail{Max: 10}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				_, _ = h.Write([]byte("abc"))
				_, _ = tl.Write([]byte("abc"))
			}
		})
	}
	for range 100 {
		_ = h.Bytes()
		_ = tl.String()
	}
	wg.Wait()
}
