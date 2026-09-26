package auth

import (
	"context"

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
)

// Principal is an authenticated caller. It is what authorization (#103)
// decides on: the user's current role, and for API tokens, the token's
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
