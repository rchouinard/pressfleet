package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
)

// APITokenPrefix starts every API token.
const APITokenPrefix = "pft_"

// tokenBytes is the randomness in session and API tokens: 256 bits.
const tokenBytes = 32

// displayPrefixLen is how much of an API token is stored for display:
// "pft_" plus four characters (20 bits, too few to help an attacker).
const displayPrefixLen = len(APITokenPrefix) + 4

// tokenEncoding is lowercase base32 without padding: URL- and header-safe,
// and selectable with a double click.
var tokenEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func randomToken() string {
	b := make([]byte, tokenBytes)
	_, _ = rand.Read(b) // never fails (crypto/rand documentation)
	return tokenEncoding.EncodeToString(b)
}

func newSessionToken() redact.Secret {
	return redact.Secret(randomToken())
}

func newAPIToken() redact.Secret {
	return redact.Secret(APITokenPrefix + randomToken())
}

// hashToken returns the hex SHA-256 of a session or API token, the form in
// which tokens are stored and looked up.
func hashToken(tok redact.Secret) string {
	sum := sha256.Sum256([]byte(tok.Reveal()))
	return hex.EncodeToString(sum[:])
}

// displayPrefix returns the stored, displayable start of an API token.
func displayPrefix(tok redact.Secret) string {
	return tok.Reveal()[:displayPrefixLen]
}

// looksLikeAPIToken rejects obviously malformed bearer tokens without a
// database lookup.
func looksLikeAPIToken(tok redact.Secret) bool {
	s := tok.Reveal()
	return strings.HasPrefix(s, APITokenPrefix) && len(s) == len(APITokenPrefix)+tokenEncoding.EncodedLen(tokenBytes)
}

// csrfLabel domain-separates CSRF tokens from other uses of the session
// token.
const csrfLabel = "pressfleet csrf v1"

// CSRFToken returns the CSRF token for a session: an HMAC keyed with the
// session token. Only someone who holds the session token (the browser, not
// a cross-site attacker who can merely make the browser send it) can know
// it, and it needs no storage of its own.
func CSRFToken(sessionToken redact.Secret) redact.Secret {
	return redact.Secret(tokenEncoding.EncodeToString(csrfMAC(sessionToken)))
}

// ValidCSRFToken reports, in constant time, whether presented is the CSRF
// token for sessionToken.
func ValidCSRFToken(sessionToken, presented redact.Secret) bool {
	got, err := tokenEncoding.DecodeString(presented.Reveal())
	if err != nil {
		return false
	}
	return hmac.Equal(got, csrfMAC(sessionToken))
}

func csrfMAC(sessionToken redact.Secret) []byte {
	m := hmac.New(sha256.New, []byte(sessionToken.Reveal()))
	m.Write([]byte(csrfLabel))
	return m.Sum(nil)
}
