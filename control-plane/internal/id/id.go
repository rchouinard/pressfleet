// Package id generates entity IDs.
//
// IDs are ULIDs (a 48-bit millisecond timestamp followed by 80 random bits)
// written in lowercase Crockford base32: 26 characters from [0-9a-z]. They
// sort in creation order, which keeps database indexes append-mostly and makes
// time-ordered listings cheap.
//
// The lowercase alphabet matters because IDs are embedded in names with
// strict rules: Docker Compose project and network names (pf-<id>), MariaDB
// database and user names, Redis ACL key prefixes, and DNS labels. Callers
// always add a prefix, because an ID starts with a digit until the year 10889.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"
	"time"
)

// Len is the length of an ID.
const Len = 26

// alphabet is Crockford's base32 in lowercase; it omits i, l, o, and u to
// avoid ambiguity.
const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// ErrInvalid is returned for strings that are not IDs.
var ErrInvalid = errors.New("invalid id")

// New returns a new ID for the current time.
func New() string {
	return NewAt(time.Now())
}

// NewAt returns a new ID whose timestamp is t. Times before the Unix epoch
// are clamped to it.
func NewAt(t time.Time) string {
	var b [16]byte
	ms := max(t.UnixMilli(), 0)
	binary.BigEndian.PutUint64(b[0:8], uint64(ms)<<16)
	_, _ = rand.Read(b[6:]) // crypto/rand.Read never returns an error
	return encode(b)
}

// Monotonic generates IDs that sort in the order they were generated, even
// within one millisecond, where New's random part would order them
// arbitrarily: an ID in the same millisecond as the previous one (or
// earlier, if the clock stepped back) takes the previous ID's timestamp and
// increments its random part. Use it where the order of events matters,
// such as the audit log. The zero value is ready to use and safe for
// concurrent use.
type Monotonic struct {
	mu   sync.Mutex
	last [16]byte
}

// NewAt returns the next ID for time t.
func (m *Monotonic) NewAt(t time.Time) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := uint64(max(t.UnixMilli(), 0))
	lastMS := binary.BigEndian.Uint64(m.last[0:8]) >> 16
	var b [16]byte
	if ms > lastMS || m.last == ([16]byte{}) {
		binary.BigEndian.PutUint64(b[0:8], ms<<16)
		_, _ = rand.Read(b[6:])
		// Leave room to increment without overflowing into the timestamp.
		b[6] &= 0x7f
	} else {
		b = m.last
		for i := 15; i >= 6; i-- {
			b[i]++
			if b[i] != 0 {
				break
			}
		}
	}
	m.last = b
	return encode(b)
}

// encode writes 128 bits as 26 base32 characters. The first character holds
// only the top 3 bits, so the most significant character is always 0-7.
func encode(b [16]byte) string {
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])
	var out [Len]byte
	for i := Len - 1; i >= 0; i-- {
		out[i] = alphabet[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// Valid reports whether s is a well-formed ID.
func Valid(s string) bool {
	if len(s) != Len || s[0] > '7' {
		return false
	}
	for i := range len(s) {
		if decodeChar(s[i]) < 0 {
			return false
		}
	}
	return true
}

// Time returns the timestamp embedded in an ID.
func Time(s string) (time.Time, error) {
	if !Valid(s) {
		return time.Time{}, ErrInvalid
	}
	// The first 10 characters hold 50 bits; the top 2 are always zero for a
	// valid ID, leaving the 48-bit millisecond timestamp.
	var ms int64
	for i := range 10 {
		ms = ms<<5 | int64(decodeChar(s[i]))
	}
	return time.UnixMilli(ms).UTC(), nil
}

func decodeChar(c byte) int {
	for i := range len(alphabet) {
		if alphabet[i] == c {
			return i
		}
	}
	return -1
}
