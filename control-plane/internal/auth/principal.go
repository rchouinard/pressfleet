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
	// SessionID and VerifiedAt are set for MethodSession. VerifiedAt is
	// when the user last proved who they are in the session: at login, or
	// by reauthenticating.
	SessionID  string
	VerifiedAt time.Time
	// EnrollmentRequired is set for MethodSession when the two-factor
	// policy covers the user's current role and the user has no second
	// factor (design doc §6.2): authz then lets the session only enroll.
	// It is decided on every request, never stored with the session, so
	// role and policy changes apply at once. Tokens and SSH keys never
	// have it, so turning the policy on doesn't break automation.
	EnrollmentRequired bool
	// TokenID and Scope are set for MethodToken. Sessions have no scope:
	// they may do everything the user's role allows.
	TokenID string
	Scope   store.TokenScope
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
