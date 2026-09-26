package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// Session lifetimes.
const (
	// DefaultSessionIdleTimeout ends sessions that haven't been used for
	// this long.
	DefaultSessionIdleTimeout = 12 * time.Hour
	// DefaultSessionMaxAge ends sessions this long after login, however
	// active they are.
	DefaultSessionMaxAge = 7 * 24 * time.Hour
	// touchInterval is how stale LastSeenAt / LastUsedAt may get before an
	// authenticated request writes them, so that reads don't each cost a
	// write.
	touchInterval = time.Minute
)

// Sentinel errors.
var (
	// ErrInvalidCredentials means a login failed. It deliberately doesn't
	// say whether the email exists, the password was wrong, or the user is
	// disabled.
	ErrInvalidCredentials = errors.New("invalid email or password")
	// ErrUnauthenticated means a session or API token is unknown, expired,
	// revoked, or belongs to a disabled user.
	ErrUnauthenticated = errors.New("invalid or expired credentials")
	// ErrWrongPassword means the current password given to ChangePassword
	// is wrong.
	ErrWrongPassword = errors.New("current password is incorrect")
	// ErrInvalidEmail means an email address is malformed.
	ErrInvalidEmail = errors.New("invalid email address")
	// ErrInvalidRole means a role isn't admin, operator, or viewer.
	ErrInvalidRole = errors.New("role must be admin, operator, or viewer")
	// ErrInvalidScope means a token scope isn't full or read-only.
	ErrInvalidScope = errors.New("scope must be full or read-only")
	// ErrInvalidTokenName means an API token name is empty or too long.
	ErrInvalidTokenName = errors.New("token name must be between 1 and 100 characters")
	// ErrInvalidExpiry means a token expiry isn't in the future.
	ErrInvalidExpiry = errors.New("expiry must be in the future")
	// ErrEmailTaken means another user has the email.
	ErrEmailTaken = errors.New("a user with this email already exists")
	// ErrUserNotFound means no user matched.
	ErrUserNotFound = errors.New("user not found")
	// ErrTokenNotFound means the user has no such active API token.
	ErrTokenNotFound = errors.New("API token not found")
)

// maxTokenNameLen matches the API's limit.
const maxTokenNameLen = 100

// Options configures a Service. Zero values use the defaults.
type Options struct {
	// Logger is required.
	Logger *slog.Logger
	// Now returns the current time; tests use it to move the clock.
	Now func() time.Time
	// PasswordParams are the argon2id parameters for new hashes.
	PasswordParams Params
	// SessionIdleTimeout and SessionMaxAge bound session lifetimes.
	SessionIdleTimeout time.Duration
	SessionMaxAge      time.Duration
}

// Service implements authentication on top of the state database. It is
// safe for concurrent use.
type Service struct {
	db       *store.DB
	log      *slog.Logger
	now      func() time.Time
	hasher   *hasher
	throttle *throttle
	idle     time.Duration
	maxAge   time.Duration

	dummyOnce sync.Once
	dummyHash redact.Secret
}

// NewService returns a Service backed by db.
func NewService(db *store.DB, opts Options) *Service {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.PasswordParams == (Params{}) {
		opts.PasswordParams = DefaultParams
	}
	if opts.SessionIdleTimeout <= 0 {
		opts.SessionIdleTimeout = DefaultSessionIdleTimeout
	}
	if opts.SessionMaxAge <= 0 {
		opts.SessionMaxAge = DefaultSessionMaxAge
	}
	now := func() time.Time { return opts.Now().UTC().Truncate(time.Microsecond) }
	return &Service{
		db:       db,
		log:      opts.Logger,
		now:      now,
		hasher:   newHasher(opts.PasswordParams),
		throttle: newThrottle(now),
		idle:     opts.SessionIdleTimeout,
		maxAge:   opts.SessionMaxAge,
	}
}

// NormalizeEmail returns the canonical form of an email address, as stored.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// validEmail accepts a bare address such as a@example.com (no display name).
func validEmail(email string) bool {
	if email == "" || len(email) > 254 {
		return false
	}
	addr, err := mail.ParseAddress(email)
	return err == nil && addr.Name == "" && addr.Address == email && strings.Contains(email[strings.LastIndexByte(email, '@'):], ".")
}

// LoginRequest holds login credentials and where they came from.
type LoginRequest struct {
	Email    string
	Password redact.Secret
	// ClientIP is the client's address, for throttling and the session
	// record.
	ClientIP  string
	UserAgent string
}

// LoginResult is a new session.
type LoginResult struct {
	User    store.User
	Session store.Session
	// Token goes in the session cookie. It is never stored.
	Token redact.Secret
	// CSRFToken must accompany cookie-authenticated unsafe requests.
	CSRFToken redact.Secret
}

// Login checks credentials and starts a session. Failures return an error
// wrapping ErrInvalidCredentials, or a *ThrottledError after too many
// recent failures.
func (s *Service) Login(ctx context.Context, req LoginRequest) (LoginResult, error) {
	email := NormalizeEmail(req.Email)
	keys := []throttleKey{accountKey(email), addressKey(req.ClientIP)}
	if err := s.throttle.check(keys...); err != nil {
		return LoginResult{}, err
	}

	user, err := store.NewUsers(s.db).GetByEmail(ctx, email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// Spend the same time as for a real user, so timing doesn't reveal
		// which emails exist.
		_, _, _ = s.hasher.verify(ctx, s.dummy(ctx), req.Password)
		s.throttle.fail(keys...)
		return LoginResult{}, ErrInvalidCredentials
	case err != nil:
		return LoginResult{}, fmt.Errorf("login: %w", err)
	}
	ok, rehash, err := s.hasher.verify(ctx, user.PasswordHash, req.Password)
	if err != nil {
		return LoginResult{}, fmt.Errorf("login: verifying password of user %s: %w", user.ID, err)
	}
	if !ok || user.Disabled() {
		s.throttle.fail(keys...)
		return LoginResult{}, ErrInvalidCredentials
	}
	s.throttle.reset(accountKey(email))

	if rehash {
		s.rehash(ctx, user, req.Password)
	}
	now := s.now()
	token := newSessionToken()
	sess := store.Session{
		UserID:    user.ID,
		TokenHash: hashToken(token),
		SourceIP:  req.ClientIP,
		UserAgent: truncate(req.UserAgent, 512),
		CreatedAt: now,
		ExpiresAt: now.Add(s.maxAge),
	}
	err = s.db.InTx(ctx, func(q store.Querier) error {
		if err := store.NewSessions(q).Create(ctx, &sess); err != nil {
			return err
		}
		return store.NewUsers(q).SetLastLogin(ctx, user.ID, now)
	})
	if err != nil {
		return LoginResult{}, fmt.Errorf("login: %w", err)
	}
	user.LastLoginAt = &now
	// Clean up opportunistically; logins are rare enough for this to be
	// cheap, and it keeps the table from growing without a background job.
	if _, err := store.NewSessions(s.db).DeleteExpired(ctx, now, now.Add(-s.idle)); err != nil {
		s.log.Warn("deleting expired sessions failed", "error", err)
	}
	s.log.Info("user logged in", "user_id", user.ID, "session_id", sess.ID)
	return LoginResult{User: user, Session: sess, Token: token, CSRFToken: CSRFToken(token)}, nil
}

// dummy returns a hash to verify against for unknown emails, made with the
// current parameters so it costs the same as a real one.
func (s *Service) dummy(ctx context.Context) redact.Secret {
	s.dummyOnce.Do(func() {
		h, err := s.hasher.hash(ctx, newSessionToken())
		if err != nil {
			// Only a cancelled context fails; fall back to a fixed hash with
			// the default parameters.
			h = redact.Secret(encodeHash(DefaultParams, make([]byte, DefaultParams.SaltLen), make([]byte, DefaultParams.KeyLen)))
		}
		s.dummyHash = h
	})
	return s.dummyHash
}

// rehash upgrades a password hash to the current parameters after a
// successful login. Failure is logged, not fatal: the old hash still works.
func (s *Service) rehash(ctx context.Context, u store.User, pw redact.Secret) {
	h, err := s.hasher.hash(ctx, pw)
	if err == nil {
		u.PasswordHash = h
		err = store.NewUsers(s.db).Update(ctx, &u, u.UpdatedAt)
	}
	if err != nil {
		s.log.Warn("upgrading password hash failed", "user_id", u.ID, "error", err)
	}
}

// AuthenticateSession returns the principal for a session token, and the
// session. It returns ErrUnauthenticated if the session doesn't exist, has
// expired (it is then deleted), or belongs to a disabled user.
func (s *Service) AuthenticateSession(ctx context.Context, token redact.Secret) (Principal, error) {
	if token == "" {
		return Principal{}, ErrUnauthenticated
	}
	sessions := store.NewSessions(s.db)
	sess, err := sessions.GetByTokenHash(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthenticated
	} else if err != nil {
		return Principal{}, fmt.Errorf("authenticate session: %w", err)
	}
	now := s.now()
	if !now.Before(sess.ExpiresAt) || !now.Before(sess.LastSeenAt.Add(s.idle)) {
		if err := sessions.Delete(ctx, sess.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("deleting expired session failed", "session_id", sess.ID, "error", err)
		}
		return Principal{}, ErrUnauthenticated
	}
	user, err := s.activeUser(ctx, sess.UserID)
	if err != nil {
		return Principal{}, err
	}
	if now.Sub(sess.LastSeenAt) >= touchInterval {
		if err := sessions.Touch(ctx, sess.ID, now); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("updating session last seen failed", "session_id", sess.ID, "error", err)
		}
	}
	return Principal{UserID: user.ID, Email: user.Email, Role: user.Role, Method: MethodSession, SessionID: sess.ID}, nil
}

// AuthenticateToken returns the principal for an API token. It returns
// ErrUnauthenticated if the token doesn't exist, is revoked or expired, or
// belongs to a disabled user.
func (s *Service) AuthenticateToken(ctx context.Context, token redact.Secret) (Principal, error) {
	if !looksLikeAPIToken(token) {
		return Principal{}, ErrUnauthenticated
	}
	tokens := store.NewAPITokens(s.db)
	tok, err := tokens.GetByTokenHash(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthenticated
	} else if err != nil {
		return Principal{}, fmt.Errorf("authenticate API token: %w", err)
	}
	now := s.now()
	if tok.RevokedAt != nil || (tok.ExpiresAt != nil && !now.Before(*tok.ExpiresAt)) {
		return Principal{}, ErrUnauthenticated
	}
	user, err := s.activeUser(ctx, tok.UserID)
	if err != nil {
		return Principal{}, err
	}
	if tok.LastUsedAt == nil || now.Sub(*tok.LastUsedAt) >= touchInterval {
		if err := tokens.TouchLastUsed(ctx, tok.ID, now); err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("updating API token last used failed", "token_id", tok.ID, "error", err)
		}
	}
	return Principal{UserID: user.ID, Email: user.Email, Role: user.Role, Method: MethodToken, TokenID: tok.ID, Scope: tok.Scope}, nil
}

// activeUser returns the user, or ErrUnauthenticated if they have been
// deleted or disabled.
func (s *Service) activeUser(ctx context.Context, userID string) (store.User, error) {
	user, err := store.NewUsers(s.db).Get(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, ErrUnauthenticated
	} else if err != nil {
		return store.User{}, fmt.Errorf("loading user: %w", err)
	}
	if user.Disabled() {
		return store.User{}, ErrUnauthenticated
	}
	return user, nil
}

// Logout deletes a session. Deleting a session that is already gone is not
// an error.
func (s *Service) Logout(ctx context.Context, sessionID string) error {
	if err := store.NewSessions(s.db).Delete(ctx, sessionID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("logout: %w", err)
	}
	return nil
}

// ChangePassword changes the caller's own password after checking the
// current one. It signs out the user's other sessions and revokes all of
// their API tokens (including the caller's, for token callers). Wrong
// current passwords are throttled like failed logins.
func (s *Service) ChangePassword(ctx context.Context, p Principal, current, next redact.Secret) error {
	key := accountKey(p.Email)
	if err := s.throttle.check(key); err != nil {
		return err
	}
	if err := CheckPasswordPolicy(next); err != nil {
		return err
	}
	user, err := s.activeUser(ctx, p.UserID)
	if err != nil {
		return err
	}
	ok, _, err := s.hasher.verify(ctx, user.PasswordHash, current)
	if err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	if !ok {
		s.throttle.fail(key)
		return ErrWrongPassword
	}
	s.throttle.reset(key)
	h, err := s.hasher.hash(ctx, next)
	if err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	now := s.now()
	err = s.db.InTx(ctx, func(q store.Querier) error {
		user.PasswordHash = h
		if err := store.NewUsers(q).Update(ctx, &user, now); err != nil {
			return err
		}
		return revokeAccess(ctx, q, user.ID, p.SessionID, now)
	})
	if err != nil {
		return fmt.Errorf("change password: %w", err)
	}
	s.log.Info("user changed password", "user_id", user.ID)
	return nil
}

// revokeAccess signs a user out everywhere except keepSessionID and revokes
// all of their API tokens.
func revokeAccess(ctx context.Context, q store.Querier, userID, keepSessionID string, now time.Time) error {
	if _, err := store.NewSessions(q).DeleteForUser(ctx, userID, keepSessionID); err != nil {
		return err
	}
	_, err := store.NewAPITokens(q).RevokeAllForUser(ctx, userID, now)
	return err
}

// NewUser describes a user to create.
type NewUser struct {
	Email    string
	Password redact.Secret
	Role     store.Role
}

// CreateUser validates and creates a user.
func (s *Service) CreateUser(ctx context.Context, nu NewUser) (store.User, error) {
	email := NormalizeEmail(nu.Email)
	if !validEmail(email) {
		return store.User{}, ErrInvalidEmail
	}
	if !nu.Role.Valid() {
		return store.User{}, ErrInvalidRole
	}
	if err := CheckPasswordPolicy(nu.Password); err != nil {
		return store.User{}, err
	}
	h, err := s.hasher.hash(ctx, nu.Password)
	if err != nil {
		return store.User{}, fmt.Errorf("create user: %w", err)
	}
	u := store.User{Email: email, PasswordHash: h, Role: nu.Role, CreatedAt: s.now()}
	if err := store.NewUsers(s.db).Create(ctx, &u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return store.User{}, ErrEmailTaken
		}
		return store.User{}, err
	}
	s.log.Info("user created", "user_id", u.ID, "role", string(u.Role))
	return u, nil
}

// GetUser returns a user, or an error wrapping ErrUserNotFound.
func (s *Service) GetUser(ctx context.Context, userID string) (store.User, error) {
	u, err := store.NewUsers(s.db).Get(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, ErrUserNotFound
	}
	return u, err
}

// GetUserByEmail returns a user, or an error wrapping ErrUserNotFound.
func (s *Service) GetUserByEmail(ctx context.Context, email string) (store.User, error) {
	u, err := store.NewUsers(s.db).GetByEmail(ctx, NormalizeEmail(email))
	if errors.Is(err, store.ErrNotFound) {
		return store.User{}, ErrUserNotFound
	}
	return u, err
}

// ListUsers lists users.
func (s *Service) ListUsers(ctx context.Context, f store.UserFilter) ([]store.User, error) {
	if f.Role != "" && !f.Role.Valid() {
		return nil, ErrInvalidRole
	}
	return store.NewUsers(s.db).List(ctx, f)
}

// UserChanges lists changes to a user; nil fields are left alone.
type UserChanges struct {
	Email    *string
	Password *redact.Secret
	Role     *store.Role
	Disabled *bool
}

// UpdateUser applies changes to a user. Setting a password or disabling the
// user signs them out everywhere and revokes their API tokens. Role changes
// need no revocation: every request reads the current role.
func (s *Service) UpdateUser(ctx context.Context, userID string, c UserChanges) (store.User, error) {
	var email string
	if c.Email != nil {
		email = NormalizeEmail(*c.Email)
		if !validEmail(email) {
			return store.User{}, ErrInvalidEmail
		}
	}
	if c.Role != nil && !c.Role.Valid() {
		return store.User{}, ErrInvalidRole
	}
	var hash redact.Secret
	if c.Password != nil {
		if err := CheckPasswordPolicy(*c.Password); err != nil {
			return store.User{}, err
		}
		var err error
		if hash, err = s.hasher.hash(ctx, *c.Password); err != nil {
			return store.User{}, fmt.Errorf("update user: %w", err)
		}
	}

	now := s.now()
	var user store.User
	err := s.db.InTx(ctx, func(q store.Querier) error {
		users := store.NewUsers(q)
		var err error
		if user, err = users.Get(ctx, userID); err != nil {
			return err
		}
		revoke := false
		if c.Email != nil {
			user.Email = email
		}
		if c.Role != nil {
			user.Role = *c.Role
		}
		if c.Password != nil {
			user.PasswordHash, revoke = hash, true
		}
		if c.Disabled != nil {
			switch {
			case *c.Disabled && !user.Disabled():
				user.DisabledAt, revoke = &now, true
			case !*c.Disabled:
				user.DisabledAt = nil
			}
		}
		if err := users.Update(ctx, &user, now); err != nil {
			return err
		}
		if revoke {
			return revokeAccess(ctx, q, user.ID, "", now)
		}
		return nil
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.User{}, ErrUserNotFound
	case errors.Is(err, store.ErrConflict):
		return store.User{}, ErrEmailTaken
	case err != nil:
		return store.User{}, fmt.Errorf("update user: %w", err)
	}
	s.log.Info("user updated", "user_id", user.ID)
	return user, nil
}

// ResetPassword sets the password of the user with the given email, signing
// them out everywhere and revoking their API tokens. It is for recovery,
// from the command line.
func (s *Service) ResetPassword(ctx context.Context, email string, pw redact.Secret) (store.User, error) {
	u, err := s.GetUserByEmail(ctx, email)
	if err != nil {
		return store.User{}, err
	}
	return s.UpdateUser(ctx, u.ID, UserChanges{Password: &pw})
}

// DeleteUser deletes a user; their sessions and tokens go with them.
func (s *Service) DeleteUser(ctx context.Context, userID string) error {
	err := store.NewUsers(s.db).Delete(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrUserNotFound
	} else if err != nil {
		return err
	}
	s.log.Info("user deleted", "user_id", userID)
	return nil
}

// NewAPIToken describes an API token to create.
type NewAPIToken struct {
	Name  string
	Scope store.TokenScope
	// ExpiresAt is optional.
	ExpiresAt *time.Time
}

// CreateAPIToken creates an API token for a user and returns it with its
// plaintext, which is not stored and can't be retrieved again.
func (s *Service) CreateAPIToken(ctx context.Context, userID string, nt NewAPIToken) (store.APIToken, redact.Secret, error) {
	name := strings.TrimSpace(nt.Name)
	if name == "" || utf8.RuneCountInString(name) > maxTokenNameLen {
		return store.APIToken{}, "", ErrInvalidTokenName
	}
	if !nt.Scope.Valid() {
		return store.APIToken{}, "", ErrInvalidScope
	}
	now := s.now()
	if nt.ExpiresAt != nil && !nt.ExpiresAt.After(now) {
		return store.APIToken{}, "", ErrInvalidExpiry
	}
	plain := newAPIToken()
	tok := store.APIToken{
		UserID:    userID,
		Name:      name,
		TokenHash: hashToken(plain),
		Prefix:    displayPrefix(plain),
		Scope:     nt.Scope,
		CreatedAt: now,
		ExpiresAt: nt.ExpiresAt,
	}
	if err := store.NewAPITokens(s.db).Create(ctx, &tok); err != nil {
		if errors.Is(err, store.ErrReference) {
			return store.APIToken{}, "", ErrUserNotFound
		}
		return store.APIToken{}, "", err
	}
	s.log.Info("API token created", "user_id", userID, "token_id", tok.ID, "scope", string(tok.Scope))
	return tok, plain, nil
}

// ListAPITokens lists a user's unrevoked API tokens, after the cursor after,
// at most limit of them (zero means no limit).
func (s *Service) ListAPITokens(ctx context.Context, userID, after string, limit int) ([]store.APIToken, error) {
	return store.NewAPITokens(s.db).ListActiveForUser(ctx, userID, after, limit)
}

// RevokeAPIToken revokes one of a user's API tokens, or returns
// ErrTokenNotFound.
func (s *Service) RevokeAPIToken(ctx context.Context, userID, tokenID string) error {
	err := store.NewAPITokens(s.db).Revoke(ctx, userID, tokenID, s.now())
	if errors.Is(err, store.ErrNotFound) {
		return ErrTokenNotFound
	} else if err != nil {
		return err
	}
	s.log.Info("API token revoked", "user_id", userID, "token_id", tokenID)
	return nil
}

// truncate shortens s to at most n bytes without splitting a UTF-8
// sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
