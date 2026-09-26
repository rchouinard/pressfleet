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
// # API tokens
//
// API tokens are "pft_" followed by 256 random bits in lowercase base32.
// The prefix makes leaked tokens easy to recognize (and to add to secret
// scanners). Like session tokens, only a SHA-256 hash is stored, and the
// plaintext is returned once. Each token has a scope (full or read-only),
// an optional expiry, and can be revoked. LastUsedAt is written at most once
// a minute.
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
