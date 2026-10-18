# Dashboard Two-Factor Authentication: Implementation Plan

**Status:** Draft, 2026-10-18. Part of the MVP; not yet in the design doc or on the project board.

This plan adds a second factor to dashboard logins: TOTP authenticator apps, and WebAuthn security keys and passkeys (FIDO2, YubiKey). It doesn't change two-factor authentication in sites' wp-admin, which the WordPress security set already covers (design doc §6.1).

## Decisions

| Topic | Decision |
| --- | --- |
| Release | MVP. The issues go in M5 (Dashboard), and the security review ([#97](https://github.com/rchouinard/pressfleet/issues/97)) covers them. |
| Mechanisms | TOTP and WebAuthn, plus recovery codes. Passkeys and security keys are both WebAuthn credentials; they differ only in registration options. |
| Passkeys | Second factor first. Passwordless sign-in is planned (see [Passwordless sign-in](#passwordless-sign-in-later)), and the first release must not need a migration to support it. |
| Enforcement | A platform setting can require a second factor. Any user can turn it on for themselves whatever the setting. |
| Sensitive actions | Configurable. The default asks for a fresh check (a second factor, or the password for users without one) before sensitive actions. |
| API tokens | API tokens can no longer create API tokens, whatever the settings; only dashboard sessions can. Tokens can still list and revoke tokens and manage SSH keys. |
| Lockout recovery | Admins reset another user's second factors in the dashboard. `pressfleet user reset-two-factor`, run in the control plane container, covers a locked-out last Admin. |

## Scope

**In scope:** TOTP, WebAuthn as a second factor, recovery codes, a required-two-factor setting with forced enrollment, reauthentication for sensitive actions, session-only API token creation, Admin and CLI resets, audit events, dashboard UI, and docs.

**Out of scope:** passwordless sign-in (planned, below); SMS and email codes; WebAuthn attestation checks and authenticator allowlists (attestation is `none`); per-role or per-user settings beyond the platform setting; second factors for API tokens and SSH keys (below).

### What two-factor doesn't cover

- **API tokens** stay bearer credentials. Only a dashboard session can create one (see [API token creation](#api-token-creation)), so every token traces back to a login that passed two-factor. With reauthentication on (the default), creating one also needs a fresh factor check.
- **SSH keys** (SFTP gateway, database tunnels, §5.8) stay single-factor. Adding one from a session is a sensitive action and needs a fresh factor check; full-scope API tokens can still add and remove them for automation, such as onboarding scripts and CI deploy keys.
- **One-click wp-admin login** (§5.9) relies on the dashboard session. With dashboard two-factor, "the platform login stands in for the second factor" holds for users who have enrolled.

Turning the required setting on doesn't break automation: tokens and SSH keys of users who haven't enrolled yet keep working. Only their dashboard sessions are restricted to enrollment.

## Configuration

Platform settings live in the config file (see [docs/configuration.md](configuration.md)), so this follows the `config.Config` convention: a yaml key, a `PRESSFLEET_*` variable, validation, and a row in the settings table.

```yaml
two_factor:
  required: off            # off, admins, all
  reauthentication: on     # on, off
  reauthentication_max_age: 10m
```

| Key | Env var | Default | Meaning |
| --- | --- | --- | --- |
| `two_factor.required` | `PRESSFLEET_TWO_FACTOR_REQUIRED` | `off` | `off`: optional for everyone. `admins`: Admins must enroll. `all`: every user must. |
| `two_factor.reauthentication` | `PRESSFLEET_TWO_FACTOR_REAUTHENTICATION` | `on` | Ask for a fresh check before sensitive actions in the dashboard. |
| `two_factor.reauthentication_max_age` | `PRESSFLEET_TWO_FACTOR_REAUTHENTICATION_MAX_AGE` | `10m` | How long a check counts as fresh. 1m to 1h. |

Like other settings, a change applies when `pressfleet upgrade` recreates the control plane. Role changes apply immediately (§4.6): a user promoted to Admin under `admins` must enroll on their next request.

**Domain coupling:** WebAuthn credentials are bound to a relying party ID, which is `dashboard_domain`. Changing the domain makes every registered security key and passkey unusable. The plan adds this to the domain-change warnings in [Changing settings](configuration.md#changing-settings) and a `doctor` check (issue 11). Without `dashboard_domain` (local development), the relying party ID is `localhost`.

## Design

### Login flow

1. `POST /api/v1/auth/login` checks the password as today. A user without second factors gets a session, unchanged.
2. A user with second factors gets `401` with problem type `second-factor-required`. Its extension members list the user's methods (`totp`, `webauthn`, `recovery_code`) and the WebAuthn request options. The response sets an `HttpOnly` `pressfleet_login` cookie holding a pending-login token (256 random bits, 5 minutes, single use). This is additive: the existing `200` response doesn't change shape.
3. `POST /api/v1/auth/login/second-factor` takes `{method, code}` or `{method: "webauthn", credential}`, checks the factor against the pending login, and creates the session as login does today.

Failed factor checks are throttled with the existing per-account and per-address counters (`auth/throttle.go`), so a known password still allows only five factor guesses per 15 minutes. Factor failures are audited like failed logins, with the user as actor since the password was correct.

Pending logins and WebAuthn challenges are kept in memory with a TTL and a size bound, like the throttle. A control plane restart drops them, which only means logging in again. The challenge store is keyed by challenge, not by user, so passwordless sign-in (which starts before the user is known) can use it unchanged.

### TOTP

- RFC 6238 with the defaults every authenticator app supports: HMAC-SHA1, 6 digits, 30-second steps, one step of clock skew either way, and a 160-bit secret.
- Written with the standard library (about 50 lines plus the RFC test vectors), so no new module.
- The server needs the plaintext secret to check codes, so it can't be hashed. It is sealed with the master key through `secrets.Store` under a new `Kind`, like other stored secrets. Losing the master key then also loses TOTP enrollments, so the master key warning (§4.1) should say so.
- Each accepted code records its time step, and codes from that step or earlier are refused, so a code can't be replayed.
- Enrollment has two steps. `POST /users/me/two-factor/totp` returns the secret and an `otpauth://` URI (issuer `PressFleet`, the dashboard domain, and the user's email) and stores it unconfirmed. `POST .../totp/confirm` with a valid code activates it. Unconfirmed secrets expire after 15 minutes.
- One TOTP per user. Replacing it means removing it and enrolling again.

### WebAuthn

- Uses `github.com/go-webauthn/webauthn`. WebAuthn verification (CBOR, COSE keys, several signature algorithms, flag and origin checks) is too easy to get wrong to write by hand, and this is the standard Go library. It brings in a CBOR decoder and a few small modules; the dependency issue records why.
- **Relying party:** ID from `dashboard_domain`, origin `https://<dashboard_domain>`. Origins come from config, never from the `Host` header.
- **Registration options:** `residentKey: preferred`, `userVerification: preferred`, `attestation: none`, and `excludeCredentials` set to the user's existing credentials. Preferring resident keys means most new credentials can later be used as passkeys for passwordless sign-in without registering again.
- **User handle:** each user gets a random 64-byte WebAuthn user handle, not their user ID, as the spec recommends. Passwordless sign-in looks users up by it.
- **Stored per credential:** credential ID, public key (COSE), algorithm, signature count, AAGUID, transports, the user verification, backup eligible and backup state flags, the relying party ID, a user-given name, and created and last-used times.
- **Signature counter:** synced passkeys always report 0, so a counter that goes backwards is logged and audited, not rejected.
- In the UI they're listed together as "Security keys and passkeys", each labeled synced (backup eligible) or device-bound.

### Recovery codes

- Ten codes of 80 random bits each, shown as 16 base32 characters in groups of four. At 80 bits a SHA-256 hash can't be brute-forced offline, so they're hashed like session tokens.
- Generated on first enrollment and shown once. Regenerating them replaces the whole set.
- A code works once in place of a second factor. Using one is audited, and the UI then shows how many codes are left.
- Recovery codes alone don't count as enrollment. A user must keep TOTP or at least one WebAuthn credential.

### Forced enrollment

If `two_factor.required` covers a user's role and they have no second factor, a password login creates a restricted session. The authorization check (`authz`) lets a restricted principal view itself, log out, and enroll, and nothing else. Every other request gets `403` with problem type `two-factor-enrollment-required`. This is checked on every request, not stored in the session, so setting changes and role changes apply immediately.

A user can't remove their last second factor while the setting requires one (`auth.ErrSecondFactorRequired`).

### Reauthentication for sensitive actions

- `sessions` gets a `verified_at` column, set at login and by `POST /api/v1/auth/reauthenticate`. Reauthentication takes the user's second factor, or the password for users without one.
- Actions in `authz` get a `sensitive` flag. With `two_factor.reauthentication: on`, the single authorization check refuses a sensitive action from a session whose `verified_at` is older than the max age, with `403` and problem type `reauthentication-required`. The UI shows a reauthentication dialog and retries the request.
- API tokens aren't asked to reauthenticate; there is no one to ask. Sensitive actions a token's role and scope allow keep working for it, except creating API tokens (never, see below) and managing second factors (session-only, since WebAuthn needs a browser anyway).
- Sensitive actions, as the authz table would mark them:

| Action | Why |
| --- | --- |
| Add, remove, or reset second factors; regenerate recovery codes | Takeover of the account's protection |
| Create API tokens (sessions only); add SSH keys | Long-lived credentials that bypass two-factor |
| Create, update (including roles), and delete users; manage others' tokens and SSH keys | Privilege changes |
| Open read-write database sessions | Direct write access to site data |
| Delete sites, delete client groups, offboard client groups | Destructive, and can delete backups |

  Password changes already ask for the current password, and one-click wp-admin login isn't on the list because it is frequent and already scoped to one site (§5.9).

### API token creation

Today any full-scope token can create more tokens. `authz.CheckNewToken` caps the new token's scope and expiry at the caller's, but a token without an expiry can still create any number of tokens without one. Someone with a stolen token can make tokens of their own, so revoking the stolen token doesn't end their access. Other platforms either don't allow it (GitHub has no API for creating personal access tokens) or make it a permission granted on purpose.

- `createApiToken` is refused for token callers with `403`, in the authorization check, whatever the reauthentication setting. Only session callers can create tokens.
- `CheckNewToken` is removed, since its caps only applied to token callers.
- Token callers can still list and revoke their own tokens, and an Admin's token can still revoke other users' tokens. Revoking only removes access.
- Token callers with full scope can still add and remove SSH keys, audited as now.
- This changes existing behavior, so the API description of `createApiToken`, the release notes, and the API docs say so. Nothing in the repository creates tokens with a token: the dashboard creates them from a session.
- If automation ever needs to create tokens, it can come back later as a separate, explicitly granted token scope, not as the default.

This doesn't depend on the rest of the plan and can land first.

### Resets and session handling

- Removing a factor signs the user out of their other sessions, like `ChangePassword`. API tokens are kept.
- An Admin reset (`DELETE /api/v1/users/{id}/two-factor`) removes the target's TOTP, WebAuthn credentials, and recovery codes, and signs them out everywhere. With the setting on, they must enroll at their next login. It is audited with the Admin as actor.
- `pressfleet user reset-two-factor --email` does the same from inside the control plane container (§4.7), following `user reset-password`. This is the recovery path for a locked-out last Admin.

### Data model

One migration, forward-only, for both SQLite and PostgreSQL:

- `user_totp`: `user_id` (primary key), `secret_id` (sealed with `secrets.Store`), `confirmed_at`, `last_used_step`, `created_at`.
- `webauthn_credentials`: the columns listed under [WebAuthn](#webauthn), with a unique credential ID and an index on `user_id`.
- `users.webauthn_user_handle`: nullable and unique, generated at first registration.
- `recovery_codes`: `id`, `user_id`, `code_hash`, `used_at`, `created_at`.
- `sessions.verified_at`: existing sessions get their `created_at`.

User and current-user API schemas gain `two_factor_enabled`. `GET /auth/me` adds `two_factor_required`, `enrollment_required`, and `verified_at`, so the UI can route to enrollment and know when to reauthenticate.

### Audit events

Following the existing action names: `self.two_factor.totp.enroll`, `self.two_factor.totp.remove`, `self.two_factor.webauthn.register`, `self.two_factor.webauthn.rename`, `self.two_factor.webauthn.remove`, `self.two_factor.recovery_codes.regenerate`, `user.two_factor.reset`, `self.reauthenticate`, and login events carrying the factor method in `details` (including recovery-code logins and signature-counter warnings). No secrets, codes, or credential public keys appear in events.

### UI

- **`login.vue`:** a second step with a code field (`autocomplete="one-time-code"`), a "Use a security key or passkey" button, and a "Use a recovery code" link.
- **`account.vue`:** a two-factor section with authenticator-app setup (QR code and secret, confirmed with a code), the list of security keys and passkeys (add, rename, remove), and recovery codes (regenerate, copy, download).
- **Forced enrollment:** route middleware sends restricted users to a setup page until they enroll.
- **Reauthentication dialog:** the API client catches `reauthentication-required`, shows the dialog, and retries.
- **`users.vue`:** a two-factor column and an Admin reset action.
- WebAuthn calls go through `navigator.credentials` directly, with base64url helpers. The QR code needs a small library (the CSP already allows `data:` images).

### Passwordless sign-in (later)

Not built in this plan, but the first release keeps it open:

- Resident keys are preferred at registration, so most credentials are already discoverable.
- User handles are stored, and the user verification flag is stored per credential.
- The challenge store doesn't need a known user.
- Login finishes through one function whatever the factors, so a passkey login is another way in, not a separate session path.

When it's built: `POST /auth/passkey/options` and `POST /auth/passkey`, conditional mediation on the login form (`autocomplete="username webauthn"`), and a rule that only an assertion with user verification from a discoverable credential counts as both factors. A setting may then let users sign in with a passkey alone and, later, remove their password.

## Issues

Sizes and priorities follow the [roadmap](roadmap.md#conventions). Issue numbers are placeholders until the issues are created. All but the passwordless tracking issue are P1 in M5, and #97 depends on issue 14.

| # | Issue | Size | Depends on |
| --- | --- | --- | --- |
| 1 | Design: dashboard two-factor authentication (design doc §4.5, §4.6, §6; this plan's decisions) | S | — |
| 2 | Config: `two_factor` settings, validation, and docs | S | 1 |
| 3 | Store: two-factor tables, `sessions.verified_at`, and repositories (SQLite + PostgreSQL) | M | 1 |
| 4 | Auth: two-step login, challenge store, factor throttling, and recovery codes | M | 3 |
| 5 | Auth + API: TOTP enrollment and login | M | 4 |
| 6 | Auth + API: WebAuthn security keys and passkeys as a second factor (adds `go-webauthn`) | L | 4 |
| 7 | Authz: required two-factor and forced enrollment | S | 2, 4 |
| 8 | Authz + API: reauthentication for sensitive actions | M | 2, 4 |
| 9 | API: Admin two-factor reset and two-factor status on users | S | 5 |
| 10 | CLI: `pressfleet user reset-two-factor` | S | 9 |
| 11 | Doctor: WebAuthn credentials bound to a different relying party ID | S | 6 |
| 12 | UI: login second step and reauthentication dialog | M | 5, 6, 8 |
| 13 | UI: two-factor settings, forced enrollment, and Admin reset | M | 5, 6, 7, 9 |
| 14 | Integration and end-to-end tests: two-factor | M | 10, 12, 13, 15 |
| 15 | Authz: only sessions can create API tokens | S | — |
| — | Post-MVP: passwordless passkey sign-in (tracking) | M | 6, 12 |

Each issue updates the OpenAPI spec first and regenerates the Go and TypeScript code, and documents its user-facing behavior.

### Order

1. **Foundation (15, 1–4):** 15 is independent and can land at any time. 1–4 settle the design and land storage and the two-step login, with nothing visible yet.
2. **TOTP slice (5, 7, 9, 10, then the TOTP parts of 12–13):** a usable, shippable feature on its own, including enforcement and recovery.
3. **WebAuthn (6, 11):** fits into the same login step and UI.
4. **Reauthentication (8):** independent of the factors once 4 is in. Its UI dialog is part of 12.
5. **Tests (14)**, on top of each issue's own unit tests.

### Testing

- **TOTP:** the RFC 6238 SHA-1 test vectors; skew and replay cases with the injected clock (`auth.Options.Now`).
- **Store:** each repository on SQLite and PostgreSQL, as for existing tables.
- **WebAuthn:** a software authenticator in an `authtest` fake (ECDSA P-256, `none` attestation) for registration and assertions, covering wrong origin, wrong relying party ID, replayed challenge, a counter going backwards, and missing user presence. No browser or Docker.
- **Authz:** table tests for restricted principals, sensitive actions with fresh and stale sessions, and token callers (refused token creation and factor management; allowed token revocation and SSH key management).
- **End to end:** Playwright with Chromium's virtual authenticator (`WebAuthn.addVirtualAuthenticator` over CDP) for security keys and passkeys, and TOTP codes computed in the test. This fits with the UI tests in [#93](https://github.com/rchouinard/pressfleet/issues/93).

## Decision log

1. ~~**MVP or post-MVP?**~~ **Resolved (2026-10-18):** MVP, in M5, ahead of the security review in [#97](https://github.com/rchouinard/pressfleet/issues/97).
2. ~~**Token callers and credential-creating actions:**~~ **Resolved (2026-10-18):** API tokens can no longer create API tokens, whatever the settings; they can still manage SSH keys. See [API token creation](#api-token-creation).
3. ~~**Sensitive action list:**~~ **Resolved (2026-10-18):** the table as written; one-click wp-admin login stays off it.
