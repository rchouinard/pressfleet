package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/rchouinard/pressfleet/control-plane/internal/redact"
	"github.com/rchouinard/pressfleet/control-plane/internal/store"
)

// WebAuthn security keys and passkeys as a second factor (design doc §6.2).
// Verification (CBOR, COSE keys, signature algorithms, flag and origin
// checks) is github.com/go-webauthn/webauthn's: it is too easy to get wrong
// by hand. This file holds what is PressFleet's: where challenges live,
// which options are sent, and what is stored.

// DefaultRelyingPartyID is the relying party ID without a dashboard domain
// (local development).
const DefaultRelyingPartyID = "localhost"

// relyingPartyName is shown by authenticators when registering.
const relyingPartyName = "PressFleet"

// webAuthnCeremonyTTL is how long the client has to answer a WebAuthn
// challenge, registration or login. It matches the pending login, so a
// login's challenge lasts as long as the login.
const webAuthnCeremonyTTL = pendingLoginTTL

// MaxWebAuthnCredentialNameLength is the longest credential name, in
// characters.
const MaxWebAuthnCredentialNameLength = 100

// WebAuthn sentinel errors.
var (
	// ErrInvalidWebAuthnResponse means a registration response doesn't
	// verify: it is malformed, for another origin or relying party, for an
	// unknown, expired, or used challenge, or lacks user presence.
	ErrInvalidWebAuthnResponse = errors.New("the security key response is invalid")
	// ErrWebAuthnCredentialExists means the credential is already
	// registered, to this user or another.
	ErrWebAuthnCredentialExists = errors.New("this security key or passkey is already registered")
	// ErrWebAuthnCredentialNotFound means the user has no such credential.
	ErrWebAuthnCredentialNotFound = errors.New("security key or passkey not found")
	// ErrInvalidWebAuthnCredentialName means a credential name is empty, too
	// long, or contains control characters.
	ErrInvalidWebAuthnCredentialName = errors.New("name must be between 1 and 100 characters, without control characters")
)

// RelyingParty is the WebAuthn relying party: the dashboard. Credentials
// are bound to its ID, so changing the dashboard domain makes every
// registered credential unusable.
type RelyingParty struct {
	// ID is the relying party ID, the dashboard domain.
	ID string
	// Origins are the origins the browser may report in a ceremony. They
	// come from configuration, never from the request's Host header.
	Origins []string
}

// RelyingPartyFor returns the relying party for a dashboard domain: the
// domain as the ID (DefaultRelyingPartyID without one) and https://<ID>
// as the only origin.
func RelyingPartyFor(dashboardDomain string) RelyingParty {
	id := strings.ToLower(strings.TrimSpace(dashboardDomain))
	if id == "" {
		id = DefaultRelyingPartyID
	}
	return RelyingParty{ID: id, Origins: []string{"https://" + id}}
}

// webAuthnCeremony is a WebAuthn challenge waiting for its answer, kept in
// the challenge store under the challenge's hash. userID is empty for
// ceremonies that start before the user is known (passwordless sign-in).
type webAuthnCeremony struct {
	userID   string
	register bool
	session  webauthn.SessionData
}

// webAuthnUser adapts a user and their credentials to the library.
type webAuthnUser struct {
	handle []byte
	user   store.User
	creds  []webauthn.Credential
}

func (u webAuthnUser) WebAuthnID() []byte   { return u.handle }
func (u webAuthnUser) WebAuthnName() string { return u.user.Email }

func (u webAuthnUser) WebAuthnDisplayName() string {
	if u.user.DisplayName != "" {
		return u.user.DisplayName
	}
	return u.user.Email
}

func (u webAuthnUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// libraryCredential converts a stored credential to the library's record.
func libraryCredential(c store.WebAuthnCredential) webauthn.Credential {
	flags := protocol.FlagUserPresent
	if c.UserVerified {
		flags |= protocol.FlagUserVerified
	}
	if c.BackupEligible {
		flags |= protocol.FlagBackupEligible
	}
	if c.BackupState {
		flags |= protocol.FlagBackupState
	}
	transports := make([]protocol.AuthenticatorTransport, len(c.Transports))
	for i, t := range c.Transports {
		transports[i] = protocol.AuthenticatorTransport(t)
	}
	return webauthn.Credential{
		ID:            c.CredentialID,
		PublicKey:     c.PublicKey,
		Transport:     transports,
		Flags:         webauthn.NewCredentialFlags(flags),
		Authenticator: webauthn.Authenticator{AAGUID: c.AAGUID, SignCount: c.SignCount},
	}
}

// newWebAuthn returns the library's relying party for rp, or an error if
// rp isn't usable.
func newWebAuthn(rp RelyingParty) (*webauthn.WebAuthn, error) {
	timeouts := webauthn.TimeoutConfig{Timeout: webAuthnCeremonyTTL, TimeoutUVD: webAuthnCeremonyTTL}
	return webauthn.New(&webauthn.Config{
		RPID:          rp.ID,
		RPDisplayName: relyingPartyName,
		RPOrigins:     rp.Origins,
		// Attestation is none (§6.2): no attestation checks or
		// authenticator allowlists.
		AttestationPreference: protocol.PreferNoAttestation,
		// Expiry is the challenge store's, on the service's clock.
		Timeouts: webauthn.TimeoutsConfig{Login: timeouts, Registration: timeouts},
	})
}

// webAuthnVerifier is the built-in FactorVerifier and FactorChallenger for
// WebAuthn. Neither side assumes a pending login: a challenge is bound to
// the user it was made for, not to a login, so reauthentication can use
// them too.
type webAuthnVerifier struct {
	s *Service
}

func (v webAuthnVerifier) Method() FactorMethod { return FactorWebAuthn }

// Enrolled reports whether the user has a credential for the current
// relying party ID; credentials bound to another one can't be used.
func (v webAuthnVerifier) Enrolled(ctx context.Context, userID string) (bool, error) {
	creds, err := v.s.usableWebAuthnCredentials(ctx, store.NewWebAuthnCredentials(v.s.db), userID)
	return len(creds) > 0, err
}

// Challenge returns WebAuthn request options (the JSON form of
// PublicKeyCredentialRequestOptions) for the user's credentials, and keeps
// the challenge for Verify.
func (v webAuthnVerifier) Challenge(ctx context.Context, user store.User) (json.RawMessage, error) {
	wu, _, err := v.s.loadWebAuthnUser(ctx, v.s.db, user, false)
	if err != nil {
		return nil, err
	}
	if len(wu.creds) == 0 {
		return nil, ErrFactorUnavailable
	}
	assertion, session, err := v.s.webauthn.BeginLogin(wu, webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		return nil, fmt.Errorf("begin WebAuthn login: %w", err)
	}
	return v.s.putWebAuthnCeremony(user.ID, false, session, assertion.Response)
}

// Verify checks a WebAuthn assertion for a challenge from Challenge, and
// records the credential's use. A signature counter that goes backwards
// is logged and noted for the audit log (FactorDetails), not refused:
// synced passkeys report 0, and the design treats it as a warning.
func (v webAuthnVerifier) Verify(ctx context.Context, user store.User, proof FactorProof) error {
	s := v.s
	parsed, err := protocol.ParseCredentialRequestResponseBytes(proof.Credential)
	if err != nil {
		s.logWebAuthnRefusal(user.ID, "parse assertion", err)
		return ErrInvalidSecondFactor
	}
	key, c, expires, ok := s.takeWebAuthnCeremony(parsed.Response.CollectedClientData.Challenge, user.ID, false)
	if !ok {
		s.log.Info("WebAuthn assertion for an unknown or expired challenge", "user_id", user.ID)
		return ErrInvalidSecondFactor
	}
	// Until the assertion passes, the challenge goes back, with its
	// expiry: the login's options are sent once, and a failed attempt (a
	// wrong key, a cancelled prompt) shouldn't need the password again.
	// Throttling bounds the attempts.
	done := false
	defer func() {
		if !done {
			s.ceremonies.put(key, c, expires)
		}
	}()
	wu, stored, err := s.loadWebAuthnUser(ctx, s.db, user, false)
	if err != nil {
		return err
	}
	cred, err := s.webauthn.ValidateLogin(wu, c.session, parsed)
	if err != nil {
		s.logWebAuthnRefusal(user.ID, "verify assertion", err)
		return ErrInvalidSecondFactor
	}
	done = true
	var used store.WebAuthnCredential
	for _, sc := range stored {
		if string(sc.CredentialID) == string(cred.ID) {
			used = sc
		}
	}
	noteFactor(ctx, "webauthn_credential_id", used.ID)
	if cred.Authenticator.CloneWarning {
		// The stored counter is kept: the library only advances it.
		s.log.Warn("WebAuthn signature counter went backwards", "user_id", user.ID, "credential_id", used.ID,
			"stored_sign_count", used.SignCount, "sign_count", parsed.Response.AuthenticatorData.Counter)
		noteFactor(ctx, "webauthn_sign_count", "regressed")
	}
	if err := store.NewWebAuthnCredentials(s.db).RecordUse(ctx, used.ID, cred.Authenticator.SignCount, cred.Flags.BackupState, s.now()); err != nil {
		return fmt.Errorf("record use of WebAuthn credential: %w", err)
	}
	return nil
}

// logWebAuthnRefusal logs why the library refused a response, which the
// client isn't told.
func (s *Service) logWebAuthnRefusal(userID, step string, err error) {
	attrs := []any{"user_id", userID, "step", step, "error", err}
	var pe *protocol.Error
	if errors.As(err, &pe) && pe.DevInfo != "" {
		attrs = append(attrs, "detail", pe.DevInfo)
	}
	s.log.Info("WebAuthn response refused", attrs...)
}

// putWebAuthnCeremony keeps session under its challenge and returns the
// options to send, as JSON.
func (s *Service) putWebAuthnCeremony(userID string, register bool, session *webauthn.SessionData, options any) (json.RawMessage, error) {
	b, err := json.Marshal(options)
	if err != nil {
		return nil, fmt.Errorf("encode WebAuthn options: %w", err)
	}
	s.ceremonies.put(hashToken(redact.Secret(session.Challenge)), webAuthnCeremony{
		userID: userID, register: register, session: *session,
	}, s.now().Add(webAuthnCeremonyTTL))
	return b, nil
}

// takeWebAuthnCeremony takes the ceremony for challenge, as the client
// reported it, if it is of the expected kind and for userID, and returns
// its key and expiry. Taking it is what makes a challenge single use. A
// ceremony for something else goes back untouched.
func (s *Service) takeWebAuthnCeremony(challenge, userID string, register bool) (string, webAuthnCeremony, time.Time, bool) {
	if challenge == "" {
		return "", webAuthnCeremony{}, time.Time{}, false
	}
	key := hashToken(redact.Secret(challenge))
	c, expires, ok := s.ceremonies.take(key)
	if !ok {
		return "", webAuthnCeremony{}, time.Time{}, false
	}
	if c.userID != userID || c.register != register {
		s.ceremonies.put(key, c, expires)
		return "", webAuthnCeremony{}, time.Time{}, false
	}
	return key, c, expires, true
}

// usableWebAuthnCredentials returns the user's credentials bound to the
// current relying party ID.
func (s *Service) usableWebAuthnCredentials(ctx context.Context, creds store.WebAuthnCredentials, userID string) ([]store.WebAuthnCredential, error) {
	if s.webauthn == nil {
		return nil, nil
	}
	all, err := creds.ListForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	usable := all[:0]
	for _, c := range all {
		if c.RPID == s.rp.ID {
			usable = append(usable, c)
		}
	}
	return usable, nil
}

// loadWebAuthnUser returns the user as the library sees them, with their
// usable credentials. ensureHandle gives a user without a WebAuthn user
// handle a random one, as their first registration does.
func (s *Service) loadWebAuthnUser(ctx context.Context, q store.Querier, user store.User, ensureHandle bool) (webAuthnUser, []store.WebAuthnCredential, error) {
	users := store.NewUsers(q)
	var (
		handle []byte
		err    error
	)
	if ensureHandle {
		handle, err = users.EnsureWebAuthnUserHandle(ctx, user.ID)
	} else {
		handle, err = users.WebAuthnUserHandle(ctx, user.ID)
	}
	if errors.Is(err, store.ErrNotFound) {
		return webAuthnUser{}, nil, ErrUserNotFound
	} else if err != nil {
		return webAuthnUser{}, nil, err
	}
	stored, err := s.usableWebAuthnCredentials(ctx, store.NewWebAuthnCredentials(q), user.ID)
	if err != nil {
		return webAuthnUser{}, nil, err
	}
	wu := webAuthnUser{handle: handle, user: user, creds: make([]webauthn.Credential, len(stored))}
	for i, c := range stored {
		wu.creds[i] = libraryCredential(c)
	}
	return wu, stored, nil
}

// BeginWebAuthnRegistration starts registering a security key or passkey
// for the user and returns the options for navigator.credentials.create()
// (the JSON form of PublicKeyCredentialCreationOptions). The first
// registration gives the user a random WebAuthn user handle. The options
// prefer a resident key, so the credential can later sign in without a
// password, prefer user verification, ask for no attestation, and exclude
// the user's credentials, so a key isn't registered twice. It returns
// ErrFactorUnavailable if WebAuthn isn't configured.
func (s *Service) BeginWebAuthnRegistration(ctx context.Context, userID string) (json.RawMessage, error) {
	if s.webauthn == nil {
		return nil, ErrFactorUnavailable
	}
	user, err := s.activeUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	wu, _, err := s.loadWebAuthnUser(ctx, s.db, user, true)
	if err != nil {
		return nil, err
	}
	creation, session, err := s.webauthn.BeginRegistration(wu,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementPreferred,
			UserVerification: protocol.VerificationPreferred,
		}),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
		webauthn.WithExclusions(webauthn.Credentials(wu.creds).CredentialDescriptors()),
	)
	if err != nil {
		return nil, fmt.Errorf("begin WebAuthn registration: %w", err)
	}
	return s.putWebAuthnCeremony(user.ID, true, session, creation.Response)
}

// WebAuthnRegistration is a newly registered credential.
type WebAuthnRegistration struct {
	Credential store.WebAuthnCredential
	// RecoveryCodes is the user's first set of recovery codes, to show
	// once, if this was their first second factor; otherwise nil.
	RecoveryCodes []redact.Secret
}

// FinishWebAuthnRegistration checks the response to a challenge from
// BeginWebAuthnRegistration and stores the credential under name. The
// user's first second factor also issues their recovery codes, in the
// same transaction. It returns an error wrapping
// ErrInvalidWebAuthnResponse if the response doesn't verify,
// ErrWebAuthnCredentialExists if the credential is already registered, or
// ErrInvalidWebAuthnCredentialName.
func (s *Service) FinishWebAuthnRegistration(ctx context.Context, userID, name string, response json.RawMessage) (WebAuthnRegistration, error) {
	if s.webauthn == nil {
		return WebAuthnRegistration{}, ErrFactorUnavailable
	}
	name, err := normalizeWebAuthnCredentialName(name)
	if err != nil {
		return WebAuthnRegistration{}, err
	}
	user, err := s.activeUser(ctx, userID)
	if err != nil {
		return WebAuthnRegistration{}, err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBytes(response)
	if err != nil {
		s.logWebAuthnRefusal(user.ID, "parse registration", err)
		return WebAuthnRegistration{}, ErrInvalidWebAuthnResponse
	}
	// Taken for good: a failed registration starts again with new options.
	_, c, _, ok := s.takeWebAuthnCeremony(parsed.Response.CollectedClientData.Challenge, user.ID, true)
	if !ok {
		s.log.Info("WebAuthn registration for an unknown or expired challenge", "user_id", user.ID)
		return WebAuthnRegistration{}, ErrInvalidWebAuthnResponse
	}
	wu, _, err := s.loadWebAuthnUser(ctx, s.db, user, false)
	if err != nil {
		return WebAuthnRegistration{}, err
	}
	cred, err := s.webauthn.CreateCredential(wu, c.session, parsed)
	if err != nil {
		s.logWebAuthnRefusal(user.ID, "verify registration", err)
		return WebAuthnRegistration{}, ErrInvalidWebAuthnResponse
	}
	var key webauthncose.PublicKeyData
	if err := webauthncbor.Unmarshal(cred.PublicKey, &key); err != nil {
		return WebAuthnRegistration{}, fmt.Errorf("decode WebAuthn public key: %w", err)
	}
	sc := store.WebAuthnCredential{
		UserID:         user.ID,
		CredentialID:   cred.ID,
		PublicKey:      cred.PublicKey,
		Algorithm:      key.Algorithm,
		SignCount:      cred.Authenticator.SignCount,
		AAGUID:         cred.Authenticator.AAGUID,
		Transports:     make([]string, len(cred.Transport)),
		UserVerified:   cred.Flags.UserVerified,
		BackupEligible: cred.Flags.BackupEligible,
		BackupState:    cred.Flags.BackupState,
		RPID:           s.rp.ID,
		Name:           name,
		CreatedAt:      s.now(),
	}
	for i, t := range cred.Transport {
		sc.Transports[i] = string(t)
	}
	var reg WebAuthnRegistration
	err = s.db.InTx(ctx, func(q store.Querier) error {
		had, err := store.NewUsers(q).HasSecondFactor(ctx, user.ID)
		if err != nil {
			return err
		}
		if err := store.NewWebAuthnCredentials(q).Create(ctx, &sc); err != nil {
			return err
		}
		if !had {
			reg.RecoveryCodes, err = s.IssueRecoveryCodes(ctx, q, user.ID)
		}
		return err
	})
	switch {
	case errors.Is(err, store.ErrConflict):
		return WebAuthnRegistration{}, ErrWebAuthnCredentialExists
	case errors.Is(err, store.ErrReference):
		return WebAuthnRegistration{}, ErrUserNotFound
	case err != nil:
		return WebAuthnRegistration{}, fmt.Errorf("register WebAuthn credential: %w", err)
	}
	reg.Credential = sc
	s.log.Info("WebAuthn credential registered", "user_id", user.ID, "credential_id", sc.ID,
		"backup_eligible", sc.BackupEligible, "first_factor", reg.RecoveryCodes != nil)
	return reg, nil
}

// ListWebAuthnCredentials returns the user's security keys and passkeys,
// including any bound to another relying party ID.
func (s *Service) ListWebAuthnCredentials(ctx context.Context, userID string) ([]store.WebAuthnCredential, error) {
	return store.NewWebAuthnCredentials(s.db).ListForUser(ctx, userID)
}

// GetWebAuthnCredential returns one of the user's credentials, or
// ErrWebAuthnCredentialNotFound.
func (s *Service) GetWebAuthnCredential(ctx context.Context, userID, credID string) (store.WebAuthnCredential, error) {
	c, err := store.NewWebAuthnCredentials(s.db).Get(ctx, userID, credID)
	if errors.Is(err, store.ErrNotFound) {
		return store.WebAuthnCredential{}, ErrWebAuthnCredentialNotFound
	}
	return c, err
}

// RenameWebAuthnCredential renames one of the user's credentials and
// returns it.
func (s *Service) RenameWebAuthnCredential(ctx context.Context, userID, credID, name string) (store.WebAuthnCredential, error) {
	name, err := normalizeWebAuthnCredentialName(name)
	if err != nil {
		return store.WebAuthnCredential{}, err
	}
	err = store.NewWebAuthnCredentials(s.db).Rename(ctx, userID, credID, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.WebAuthnCredential{}, ErrWebAuthnCredentialNotFound
	} else if err != nil {
		return store.WebAuthnCredential{}, err
	}
	s.log.Info("WebAuthn credential renamed", "user_id", userID, "credential_id", credID)
	return s.GetWebAuthnCredential(ctx, userID, credID)
}

// RemoveWebAuthnCredential removes one of the caller's credentials and
// signs them out of their other sessions (API tokens are kept). It returns
// ErrWebAuthnCredentialNotFound, or ErrLastSecondFactor if the policy
// requires the user to keep a second factor and this is their last.
func (s *Service) RemoveWebAuthnCredential(ctx context.Context, p Principal, credID string) error {
	err := s.db.InTx(ctx, func(q store.Querier) error {
		user, err := store.NewUsers(q).Get(ctx, p.UserID)
		if err != nil {
			return err
		}
		if err := store.NewWebAuthnCredentials(q).Delete(ctx, p.UserID, credID); err != nil {
			return err
		}
		if err := s.ensureSecondFactorKept(ctx, q, user); err != nil {
			return err
		}
		_, err = store.NewSessions(q).DeleteForUser(ctx, p.UserID, p.SessionID)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrWebAuthnCredentialNotFound
	case errors.Is(err, ErrLastSecondFactor):
		return ErrLastSecondFactor
	case err != nil:
		return fmt.Errorf("remove WebAuthn credential: %w", err)
	}
	s.log.Info("WebAuthn credential removed", "user_id", p.UserID, "credential_id", credID)
	return nil
}

// normalizeWebAuthnCredentialName trims a credential name and checks it.
func normalizeWebAuthnCredentialName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > MaxWebAuthnCredentialNameLength || strings.ContainsFunc(name, unicode.IsControl) {
		return "", ErrInvalidWebAuthnCredentialName
	}
	return name, nil
}

// factorNotesKey carries a login's FactorDetails while its verifier runs.
type factorNotesKey struct{}

// noteFactor adds a fact about a factor check to the login's
// FactorDetails, for the audit log. It must never hold secrets.
func noteFactor(ctx context.Context, key, value string) {
	if m, ok := ctx.Value(factorNotesKey{}).(map[string]string); ok {
		m[key] = value
	}
}

// withFactorNotes returns a context whose verifiers' notes go into m.
func withFactorNotes(ctx context.Context, m map[string]string) context.Context {
	return context.WithValue(ctx, factorNotesKey{}, m)
}
