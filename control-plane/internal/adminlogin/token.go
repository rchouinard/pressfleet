package adminlogin

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// TokenLifetime is how long a token is valid (§5.9).
const TokenLifetime = 60 * time.Second

// tokenVersion prefixes the token and the signed bytes.
const tokenVersion = "v1"

// ErrInvalidToken means a token failed verification. It deliberately doesn't
// say why, like the site side's single generic error.
var ErrInvalidToken = errors.New("invalid login token")

// ErrInvalidClaims means Sign was given an empty site, user, or actor.
var ErrInvalidClaims = errors.New("login token needs a site, a user, and an actor")

// Claims is the token payload. Field order is the JSON order of the v1
// format.
type Claims struct {
	// Site is the site's ID.
	Site string `json:"site"`
	// User is the WordPress user's login.
	User string `json:"user"`
	// Actor is the platform user's ID.
	Actor string `json:"actor"`
	// JTI is a random 128-bit ID, hex encoded; the site records it to
	// make the token single-use.
	JTI string `json:"jti"`
	// IssuedAt and ExpiresAt are Unix times, TokenLifetime apart.
	IssuedAt  int64 `json:"iat"`
	ExpiresAt int64 `json:"exp"`
}

// Signer issues login tokens with the platform's login key.
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
	now  func() time.Time
	rand io.Reader
}

// PublicKey returns the public half of the key.
func (s *Signer) PublicKey() ed25519.PublicKey { return s.pub }

// PublicKeyBase64 returns the public key as written to wp-config.php
// (PRESSFLEET_LOGIN_PUBLIC_KEY): the raw 32 bytes in standard base64.
func (s *Signer) PublicKeyBase64() string { return base64.StdEncoding.EncodeToString(s.pub) }

// Verifier returns a Verifier for the signer's public key.
func (s *Signer) Verifier() *Verifier { return &Verifier{pub: s.pub, now: s.now} }

// Sign returns a v1 token that logs the WordPress user in to the site, on
// behalf of the platform user actor, along with its jti (for the audit
// event; never audit the token) and its expiry.
func (s *Signer) Sign(siteID, user, actor string) (token, jti string, expiresAt time.Time, err error) {
	if siteID == "" || user == "" || actor == "" {
		return "", "", time.Time{}, ErrInvalidClaims
	}
	var raw [16]byte
	if _, err := io.ReadFull(s.rand, raw[:]); err != nil {
		return "", "", time.Time{}, fmt.Errorf("generating token ID: %w", err)
	}
	iat := s.now().Unix()
	c := Claims{
		Site: siteID, User: user, Actor: actor, JTI: hex.EncodeToString(raw[:]),
		IssuedAt: iat, ExpiresAt: iat + int64(TokenLifetime/time.Second),
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("encoding token: %w", err)
	}
	signed := tokenVersion + "." + base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(s.priv, []byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig), c.JTI, time.Unix(c.ExpiresAt, 0), nil
}

// Verifier checks tokens the way a site does, except that it can't know
// whether a jti was already used.
type Verifier struct {
	pub ed25519.PublicKey
	now func() time.Time
}

// NewVerifier returns a Verifier for a public key.
func NewVerifier(pub ed25519.PublicKey) *Verifier {
	return &Verifier{pub: pub, now: time.Now}
}

// Verify checks the token's format, signature, site, and lifetime, and
// returns its claims. Every failure is ErrInvalidToken.
func (v *Verifier) Verify(token, siteID string) (Claims, error) {
	version, rest, ok := strings.Cut(token, ".")
	if !ok || version != tokenVersion {
		return Claims{}, ErrInvalidToken
	}
	payloadText, sigText, ok := strings.Cut(rest, ".")
	if !ok || strings.Contains(sigText, ".") {
		return Claims{}, ErrInvalidToken
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(payloadText)
	if err != nil {
		return Claims{}, ErrInvalidToken
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(sigText)
	if err != nil || !ed25519.Verify(v.pub, []byte(tokenVersion+"."+payloadText), sig) {
		return Claims{}, ErrInvalidToken
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, ErrInvalidToken
	}
	now := v.now().Unix()
	if c.Site != siteID || c.Site == "" || c.User == "" || c.JTI == "" ||
		c.ExpiresAt-c.IssuedAt != int64(TokenLifetime/time.Second) ||
		now >= c.ExpiresAt || now < c.IssuedAt {
		return Claims{}, ErrInvalidToken
	}
	return c, nil
}
