// Package auth authenticates control plane users (design doc §4.5, §6):
// passwords, browser sessions, and scoped API tokens. It answers "who is
// calling?" and puts the answer, a [Principal], in the request context.
// Deciding what the caller may do is authorization (§4.6, #103), which uses
// the Principal but is not part of this package.
//
// # Passwords
//
// Passwords are hashed with argon2id and stored in the PHC string format
// ($argon2id$v=19$m=...,t=...,p=...$salt$hash), so the parameters can be
// raised later: hashes made with older parameters still verify, and are
// rehashed on the next successful login. The defaults follow the OWASP
// recommendation of 19 MiB, two passes, and one lane. RFC 9106's 64 MiB
// profile doesn't fit the control plane's memory limit (192 MiB by default)
// with concurrent logins, and at most two hashes run at once for the same
// reason. The policy is a minimum length of 12 characters and no composition
// rules (NIST SP 800-63B).
//
// # Sessions
//
// A session token is 256 random bits. The browser holds it in the
// pressfleet_session cookie; the sessions table holds only its SHA-256 hash,
// so a copy of the database can't be used to log in. Hashing tokens with a
// fast hash is fine because, unlike passwords, they can't be guessed.
// Sessions end after an idle timeout or an absolute lifetime, whichever
// comes first, when the user logs out, and when their password changes or
// the user is disabled. LastSeenAt is written at most once a minute rather
// than on every request.
//
// The CSRF token for a session is an HMAC of the session token, so it is
// tied to the session, changes with it, and needs no storage.
//
// # Second factors
//
// A user with a second factor (design doc §6.2) logs in in two steps. A
// right password starts no session: Login returns a
// *SecondFactorRequiredError holding a pending-login token (256 random
// bits, valid for five minutes) and the methods the user can use. The
// pending login is kept in memory, keyed by the token's hash, in a store
// with a TTL and a size bound that is keyed by challenge rather than by
// user, so passwordless sign-in can reuse it. LoginSecondFactor dispatches
// the proof to the FactorVerifier registered for its method (recovery
// codes and WebAuthn are built in; TOTP plugs in through Options.Factors)
// and, if it passes, uses up the pending login. Every login, one step or
// two, ends in the same function, which creates the session and sets its
// verified_at.
//
// Failed factor checks count against the same per-account and per-address
// throttle as wrong passwords, and a right password doesn't reset the
// account's count while a second step is pending, so knowing the password
// allows no more guesses at the second factor than at the password. A
// wrong proof leaves the pending login in place, so a mistyped code
// doesn't mean entering the password again.
//
// Security keys and passkeys are WebAuthn credentials, verified with
// github.com/go-webauthn/webauthn, the one dependency two-factor adds:
// WebAuthn verification (CBOR, COSE keys, several signature algorithms,
// flag and origin checks) is too easy to get wrong by hand. The relying
// party comes from the dashboard domain (Options.RelyingParty), never
// from a request. Challenges live in a challenge store like pending
// logins, keyed by challenge and bound to the user they were made for but
// not to a login, so reauthentication and passwordless sign-in can use
// them; taking one makes it single use. Registration prefers resident
// keys and gives each user a random 64-byte user handle, and each
// credential's user verification and backup flags are stored, so
// passwordless sign-in needs no migration. A signature counter that goes
// backwards is logged and noted for the audit log, not refused, since
// synced passkeys always report 0.
//
// Recovery codes are ten codes of 80 random bits, shown once as 16 base32
// characters in groups of four and stored as SHA-256 hashes, like tokens.
// Each logs in once. They stand in for a second factor but aren't one: a
// user with only recovery codes logs in with the password alone.
//
// TOTP (RFC 6238, TOTPVerifier) is written with the standard library:
// HMAC-SHA1, 6 digits, 30-second steps, one step of skew either way, and
// 160-bit secrets, compared in constant time. The secret is sealed with
// the master key (package secrets), since checking a code needs it. Each
// accepted code records its time step, and codes of that step or earlier
// are refused, so a code can't be replayed. Enrollment is unconfirmed
// until a code from the app confirms it, and expires after 15 minutes;
// until then it isn't a factor.
//
// # API tokens
//
// API tokens are "pft_" followed by 256 random bits in lowercase base32.
// The prefix makes leaked tokens easy to recognize (and to add to secret
// scanners). Like session tokens, only a SHA-256 hash is stored, and the
// plaintext is returned once. Each token has a scope (full or read-only),
// an optional expiry, and can be revoked. LastUsedAt is written at most once
// a minute.
//
// # SSH keys
//
// Users register SSH public keys for the SFTP gateway (design doc §5.8).
// The SSH username names the site, so the key alone identifies the user: a
// key is registered to one user at most and looked up by its SHA-256
// fingerprint. Only public keys are stored, never passwords. Removing a key,
// or disabling or deleting its user, ends its access; the gateway rechecks
// live connections (RecheckSSHKey) so that holds for open sessions too.
//
// # Brute force
//
// Failed logins return the same error whether or not the email exists, and
// a login for an unknown email still performs a password hash, so neither
// the response nor its timing reveals which emails have accounts. Failures
// are throttled in memory, per account and per client address. The counters
// reset when the control plane restarts, which is acceptable for a single
// control plane instance.
package auth
