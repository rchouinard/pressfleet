import type { components } from '../api/schema.gen'

export type ApiToken = components['schemas']['ApiToken']
export type TokenScope = components['schemas']['TokenScope']
export type ApiTokenCreate = components['schemas']['ApiTokenCreate']

export interface ScopeView {
  label: string
  /** One line for the create form: what the scope allows. */
  hint: string
  /** Shown with the label, so scope is never conveyed by colour alone. */
  icon: string
  color: 'neutral' | 'info'
}

/** How a token scope looks. Unknown scopes (the spec may grow) show as sent. */
export function scopeView(scope: TokenScope | string): ScopeView {
  switch (scope) {
    case 'full':
      return { label: 'Full', hint: 'Everything your role allows.', icon: 'i-lucide-key-round', color: 'info' }
    case 'read-only':
      return { label: 'Read-only', hint: 'Can read, never change anything. Good for monitoring and reports.', icon: 'i-lucide-eye', color: 'neutral' }
    default:
      return { label: String(scope), hint: '', icon: 'i-lucide-circle-help', color: 'neutral' }
  }
}

/**
 * The warning shown when an Admin creates a full-scope token (#182): it is
 * as powerful as the Admin account, including creating users, whose access
 * outlives the token's revocation. Other roles and read-only tokens get none.
 */
export function fullAdminTokenWarning(role: string | undefined, scope: TokenScope | string): string | undefined {
  if (role !== 'admin' || scope !== 'full') return undefined
  return 'A full token for an Admin is as powerful as your account: it can create and change users without asking for a second factor. Treat it like your password, and prefer a read-only token for automation.'
}

export interface ExpiryChoice {
  value: 'never' | '7' | '30' | '90' | '365'
  label: string
}

export const expiryChoices: readonly ExpiryChoice[] = [
  { value: 'never', label: 'Never expires' },
  { value: '7', label: 'In 7 days' },
  { value: '30', label: 'In 30 days' },
  { value: '90', label: 'In 90 days' },
  { value: '365', label: 'In 1 year' },
]

const DAY_MS = 24 * 60 * 60 * 1000

/** The `expires_at` to send for an expiry choice (`null` means no expiry). */
export function expiresAtFor(choice: ExpiryChoice['value'], now: Date = new Date()): string | null {
  if (choice === 'never') return null
  return new Date(now.getTime() + Number(choice) * DAY_MS).toISOString()
}

export const TOKEN_NAME_MAX = 100

export interface TokenFormInput {
  name: string
  scope: TokenScope
  expiry: ExpiryChoice['value']
}

/**
 * Checks the create-token form the way the API would (a name of 1-100
 * characters), so the obvious mistakes are caught before a round trip. The
 * server stays authoritative; its 422 field errors are shown too.
 */
export function validateTokenForm(input: Pick<TokenFormInput, 'name'>): { name?: string } {
  const name = input.name.trim()
  if (name === '') return { name: 'Give the token a name so you can tell it apart later.' }
  if (name.length > TOKEN_NAME_MAX) return { name: `Use at most ${TOKEN_NAME_MAX} characters.` }
  return {}
}

/** The request body for `POST /api/v1/auth/tokens`. */
export function tokenCreateBody(input: TokenFormInput, now: Date = new Date()): ApiTokenCreate {
  return { name: input.name.trim(), scope: input.scope, expires_at: expiresAtFor(input.expiry, now) }
}

export interface TokenStatus {
  label: 'Active' | 'Expired'
  color: 'success' | 'error'
  icon: string
}

/** Expired tokens stay listed (the server rejects them) until revoked. */
export function tokenStatus(token: Pick<ApiToken, 'expires_at'>, now: Date = new Date()): TokenStatus {
  if (token.expires_at && new Date(token.expires_at).getTime() <= now.getTime()) {
    return { label: 'Expired', color: 'error', icon: 'i-lucide-circle-x' }
  }
  return { label: 'Active', color: 'success', icon: 'i-lucide-circle-check' }
}

/** A readable local timestamp; unparseable input is shown as it came. */
export function formatTimestamp(iso: string | null | undefined, empty = 'Never'): string {
  if (!iso) return empty
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString()
}
