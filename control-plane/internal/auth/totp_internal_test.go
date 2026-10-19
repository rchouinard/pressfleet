package auth

import (
	"testing"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// rfc6238Key is the SHA-1 key of RFC 6238 appendix B.
var rfc6238Key = []byte("12345678901234567890")

func TestHOTPRFC6238Vectors(t *testing.T) {
	tests := []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, tt := range tests {
		step := totpStep(time.Unix(tt.unix, 0))
		if got := hotp(rfc6238Key, uint64(step), 8); got != tt.want {
			t.Errorf("8 digits at %d: %s, want %s", tt.unix, got, tt.want)
		}
		// Six digits are the last six of eight.
		if got := hotp(rfc6238Key, uint64(step), totpDigits); got != tt.want[2:] {
			t.Errorf("6 digits at %d: %s, want %s", tt.unix, got, tt.want[2:])
		}
	}
}

func TestMatchTOTP(t *testing.T) {
	now := time.Unix(1111111111, 0)
	cur := totpStep(now)
	code := func(step int64) redact.Secret { return redact.Secret(hotp(rfc6238Key, uint64(step), totpDigits)) }
	tests := []struct {
		name     string
		code     redact.Secret
		after    int64
		wantStep int64
		wantOK   bool
	}{
		{"current step", code(cur), 0, cur, true},
		{"one step behind", code(cur - 1), 0, cur - 1, true},
		{"one step ahead", code(cur + 1), 0, cur + 1, true},
		{"two steps behind", code(cur - 2), 0, 0, false},
		{"two steps ahead", code(cur + 2), 0, 0, false},
		{"spaces", redact.Secret(code(cur).Reveal()[:3] + " " + code(cur).Reveal()[3:]), 0, cur, true},
		{"replayed step", code(cur), cur, 0, false},
		{"earlier step than the last used", code(cur - 1), cur, 0, false},
		{"later step than the last used", code(cur + 1), cur, cur + 1, true},
		{"too short", "12345", 0, 0, false},
		{"not digits", "12345a", 0, 0, false},
		{"empty", "", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step, ok := matchTOTP(rfc6238Key, tt.code, now, tt.after)
			if ok != tt.wantOK || step != tt.wantStep {
				t.Errorf("matchTOTP = %d, %v; want %d, %v", step, ok, tt.wantStep, tt.wantOK)
			}
		})
	}
}

func TestTOTPURI(t *testing.T) {
	v := NewTOTPVerifier(nil, "dash.example.com")
	got := v.uri("alice@example.com", "JBSWY3DPEHPK3PXP").Reveal()
	want := "otpauth://totp/PressFleet:alice@example.com%20%28dash.example.com%29?algorithm=SHA1&digits=6&issuer=PressFleet&period=30&secret=JBSWY3DPEHPK3PXP"
	if got != want {
		t.Errorf("uri =\n %s\nwant\n %s", got, want)
	}
	if got := NewTOTPVerifier(nil, "").uri("alice@example.com", "JBSWY3DPEHPK3PXP").Reveal(); got != "otpauth://totp/PressFleet:alice@example.com?algorithm=SHA1&digits=6&issuer=PressFleet&period=30&secret=JBSWY3DPEHPK3PXP" {
		t.Errorf("uri without a domain = %s", got)
	}
}
