// The dashboard side of two-factor login and reauthentication (design doc
// §6.2, docs/configuration.md "Dashboard two-factor authentication"): the
// problem types the UI branches on, how to read a login's second-factor
// challenge, and the request bodies the second step sends.
import type { components } from '../api/schema.gen'
import type { ApiError } from './api-error'
import type { AssertionJSON, RequestOptionsJSON } from './webauthn'

/** The `401` from login when the password was right and a second factor is needed. */
export const SECOND_FACTOR_REQUIRED_PROBLEM_TYPE
  = 'https://github.com/rchouinard/pressfleet/blob/main/docs/configuration.md#logging-in-with-a-second-factor'

/** The `403` of a sensitive action from a session whose last check is too old. */
export const REAUTHENTICATION_REQUIRED_PROBLEM_TYPE
  = 'https://github.com/rchouinard/pressfleet/blob/main/docs/configuration.md#reauthenticating-for-sensitive-actions'

/** Where users regenerate their recovery codes (the account page until #159 adds two-factor settings). */
export const RECOVERY_CODES_PAGE = '/account'

export type SecondFactorMethod = components['schemas']['SecondFactorMethod']
export type ReauthenticationMethod = components['schemas']['ReauthenticationMethod']
/** A login's second factor, or `password` when reauthenticating without one. */
export type FactorMethod = SecondFactorMethod | ReauthenticationMethod

/** What the second step needs: the user's methods and, for WebAuthn, the request options. */
export interface FactorChallenge {
  methods: FactorMethod[]
  webauthn?: RequestOptionsJSON
}

/** The proof the second step sends, for login and for reauthentication alike. */
export type FactorProof
  = | { method: 'totp' | 'recovery_code', code: string }
    | { method: 'webauthn', credential: AssertionJSON }
    | { method: 'password', password: string }

const knownMethods: readonly FactorMethod[] = ['totp', 'webauthn', 'recovery_code', 'password']

/** Reads methods and WebAuthn options from a problem document or an options response. */
export function factorChallenge(source: { methods?: unknown, webauthn?: unknown }): FactorChallenge {
  const methods = Array.isArray(source.methods)
    ? source.methods.filter((m): m is FactorMethod => knownMethods.includes(m as FactorMethod))
    : []
  const w = source.webauthn
  const webauthn = w && typeof w === 'object' && typeof (w as RequestOptionsJSON).challenge === 'string' ? w as RequestOptionsJSON : undefined
  // WebAuthn is offered only with options to sign.
  return { methods: methods.filter(m => m !== 'webauthn' || webauthn), webauthn }
}

/** The login's second-factor challenge, if the error is the "second factor required" 401. */
export function secondFactorChallenge(error: ApiError): FactorChallenge | undefined {
  if (error.status !== 401 || error.type !== SECOND_FACTOR_REQUIRED_PROBLEM_TYPE) return undefined
  return factorChallenge(error.members)
}

/** Whether the error asks the session to reauthenticate before a sensitive action. */
export function isReauthenticationRequired(error: ApiError): boolean {
  return error.status === 403 && error.type === REAUTHENTICATION_REQUIRED_PROBLEM_TYPE
}

/** The request field a method's proof is checked under, where a 422 puts its message. */
export function proofField(method: FactorMethod): string {
  switch (method) {
    case 'webauthn':
      return '/credential'
    case 'password':
      return '/password'
    default:
      return '/code'
  }
}

/** The method the second step shows first: a code field when there is one, a security key otherwise. */
export function initialMethod(methods: readonly FactorMethod[]): FactorMethod | undefined {
  for (const m of ['password', 'totp', 'webauthn', 'recovery_code'] as const) {
    if (methods.includes(m)) return m
  }
  return undefined
}

/** Recovery codes are typed with dashes, spaces or neither; the server takes them either way. */
export function normalizeCode(method: FactorMethod, code: string): string {
  return method === 'totp' ? code.replace(/\s+/g, '') : code.trim()
}
