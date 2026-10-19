package authtest_test

import (
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/auth/authtest"
)

// The RFC 6238 appendix B SHA-1 vectors.
func TestTOTPCodeForKey(t *testing.T) {
	key := []byte("12345678901234567890")
	for unix, want := range map[int64]string{
		59: "94287082", 1111111109: "07081804", 1111111111: "14050471",
		1234567890: "89005924", 2000000000: "69279037", 20000000000: "65353130",
	} {
		if got := authtest.TOTPCodeForKey(key, time.Unix(unix, 0), 8); got != want {
			t.Errorf("at %d: %s, want %s", unix, got, want)
		}
	}
	// The same key in base32, 6 digits.
	if got := authtest.TOTPCode("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ", time.Unix(59, 0)); got != "287082" {
		t.Errorf("TOTPCode = %s", got)
	}
}
