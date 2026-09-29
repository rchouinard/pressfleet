package auth

import (
	"context"
	"time"

	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Method is how a caller authenticated.
type Method string

// Authentication methods.
const (
	// MethodSession is a browser session cookie.
	MethodSession Method = "session"
	// MethodToken is a bearer API token.
	MethodToken Method = "token"
	// MethodSSHKey is an SSH public key presented to the SFTP gateway
	// (design doc §5.8). It never authenticates API requests.
	MethodSSHKey Method = "ssh_key"
)

// Principal is an authenticated caller. It is what authorization (package
// authz) decides on: the user's current role, and for API tokens, the token's
// scope. The role is read from the database on every request, so role
// changes apply immediately to existing sessions and tokens (§4.6).
type Principal struct {
	UserID string
	Email  string
	Role   store.Role
	Method Method
	// SessionID is set for MethodSession.
	SessionID string
	// TokenID and Scope are set for MethodToken. Sessions have no scope:
	// they may do everything the user's role allows.
	TokenID string
	Scope   store.TokenScope
	// TokenExpiresAt is the token's expiry, if it has one. Authorization
	// uses it so a token can't mint a token that outlives it.
	TokenExpiresAt *time.Time
	// SSHKeyID is set for MethodSSHKey: the registered key the client
	// proved it holds.
	SSHKeyID string
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the authenticated caller, if any.
func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
