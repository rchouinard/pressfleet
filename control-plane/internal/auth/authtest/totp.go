package authtest

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // G505: RFC 6238 HMAC-SHA1, as authenticator apps compute it
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// TOTPCode returns the 6-digit RFC 6238 code an authenticator app shows at
// t for a base32 secret, as enrollment returns it. It is written apart from
// package auth's own, so tests don't check auth against itself.
func TOTPCode(secret string, t time.Time) string {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil {
		panic(fmt.Sprintf("authtest: TOTP secret isn't base32: %v", err))
	}
	return TOTPCodeForKey(key, t, 6)
}

// TOTPCodeForKey returns the RFC 6238 SHA-1 code of digits digits for a raw
// key at t, with 30-second steps.
func TOTPCodeForKey(key []byte, t time.Time, digits int) string {
	msg := make([]byte, 8)
	binary.BigEndian.PutUint64(msg, uint64(t.Unix()/30)) //nolint:gosec // G115: times in tests are after 1970
	m := hmac.New(sha1.New, key)
	m.Write(msg)
	sum := m.Sum(nil)
	off := sum[19] & 0xf
	v := (uint32(sum[off]&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])) % pow10(digits)
	return fmt.Sprintf("%0*d", digits, v)
}

func pow10(n int) uint32 {
	p := uint32(1)
	for range n {
		p *= 10
	}
	return p
}
