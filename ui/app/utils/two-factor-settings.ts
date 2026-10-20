// The two-factor settings of the account page and the forced-enrollment page
// (design doc §6.2, docs/configuration.md "Dashboard two-factor
// authentication"): the requests they make, wrapped so the components can be
// mounted with a fake, and the wording of the credentials' state.
import type { components } from '../api/schema.gen'
import type { ApiClient } from './api-client'
import { formatTimestamp } from './tokens'
import { factorChallenge, type FactorMethod } from './two-factor'
import type { AttestationJSON, CreationOptionsJSON } from './webauthn'

export type WebAuthnCredential = components['schemas']['WebAuthnCredential']
export type TotpEnrollment = components['schemas']['TotpEnrollment']

/** The longest name the API takes for a security key or passkey. */
export const CREDENTIAL_NAME_MAX = 100

/** The requests behind the two-factor settings. Each throws ApiError. */
export interface TwoFactorClient {
  /** Every security key and passkey, unusable ones from an old dashboard domain included. */
  listCredentials(): Promise<WebAuthnCredential[]>
  /** The second factors the user can sign in with now. */
  factorMethods(): Promise<FactorMethod[]>
  /** A new, unconfirmed authenticator app secret; it replaces an unconfirmed one. */
  startTotp(): Promise<TotpEnrollment>
  /** Turns the authenticator app on; returns recovery codes for the first factor. */
  confirmTotp(code: string): Promise<string[] | undefined>
  removeTotp(): Promise<void>
  webAuthnOptions(): Promise<CreationOptionsJSON>
  /** Registers a credential; returns recovery codes for the first factor. */
  registerCredential(name: string, credential: AttestationJSON): Promise<{ credential: WebAuthnCredential, recoveryCodes?: string[] }>
  renameCredential(id: string, name: string): Promise<WebAuthnCredential>
  removeCredential(id: string): Promise<void>
  regenerateRecoveryCodes(): Promise<string[]>
}

/**
 * The settings' requests through the API client, so reauthentication for
 * these sensitive actions works as everywhere else (see api-client.ts).
 */
export function twoFactorClient(api: ApiClient): TwoFactorClient {
  const credentialPath = (id: string) => ({ params: { path: { webauthn_credential_id: id } } })
  return {
    async listCredentials() {
      const { data } = await api.GET('/api/v1/users/me/two-factor/webauthn')
      return data?.items ?? []
    },
    async factorMethods() {
      // The API has no "which factors do I have" call; the reauthentication
      // options list exactly the factors a check would accept.
      const { data } = await api.POST('/api/v1/auth/reauthenticate/options')
      return factorChallenge({ methods: data?.methods, webauthn: data?.webauthn }).methods
    },
    async startTotp() {
      const { data } = await api.POST('/api/v1/users/me/two-factor/totp')
      if (!data) throw new Error('The authenticator app response was empty.')
      return data
    },
    async confirmTotp(code) {
      const { data } = await api.POST('/api/v1/users/me/two-factor/totp/confirm', { body: { code } })
      return data?.recovery_codes
    },
    async removeTotp() {
      await api.DELETE('/api/v1/users/me/two-factor/totp')
    },
    async webAuthnOptions() {
      const { data } = await api.POST('/api/v1/users/me/two-factor/webauthn/options')
      return data as CreationOptionsJSON
    },
    async registerCredential(name, credential) {
      const { data } = await api.POST('/api/v1/users/me/two-factor/webauthn', { body: { name, credential } })
      if (!data) throw new Error('The registration response was empty.')
      return { credential: data.credential, recoveryCodes: data.recovery_codes }
    },
    async renameCredential(id, name) {
      const { data } = await api.PATCH('/api/v1/users/me/two-factor/webauthn/{webauthn_credential_id}', { ...credentialPath(id), body: { name } })
      if (!data) throw new Error('The rename response was empty.')
      return data
    },
    async removeCredential(id) {
      await api.DELETE('/api/v1/users/me/two-factor/webauthn/{webauthn_credential_id}', credentialPath(id))
    },
    async regenerateRecoveryCodes() {
      const { data } = await api.POST('/api/v1/users/me/two-factor/recovery-codes')
      return data?.codes ?? []
    },
  }
}

/**
 * Whether a credential can be used here. Credentials are bound to the
 * dashboard domain they were registered on (`rp_id`); after the domain
 * changes the old ones are listed but can't sign in.
 */
export function isUsable(credential: Pick<WebAuthnCredential, 'rp_id'>, rpId: string): boolean {
  return credential.rp_id === rpId
}

export interface CredentialState {
  usable: boolean
  /** "Synced" (a passkey backed up to the user's account) or "Device-bound". */
  kind: string
  icon: string
  /** One line about it: when it was last used, or why it can't be. */
  note: string
}

/** What the list says about a credential. Never by colour alone: each has a label and icon. */
export function credentialState(credential: WebAuthnCredential, rpId: string): CredentialState {
  const synced = credential.backup_eligible
  const usable = isUsable(credential, rpId)
  return {
    usable,
    kind: synced ? 'Synced' : 'Device-bound',
    icon: synced ? 'i-lucide-cloud' : 'i-lucide-key-round',
    note: usable
      ? `Last used ${formatTimestamp(credential.last_used_at, 'never')}`
      : `Registered for ${credential.rp_id}, can't be used`,
  }
}

/**
 * Whether the user has an authenticator app, from what is cheap to know:
 * without any second factor they don't; with one but no usable key it must
 * be the app. Otherwise only `factorMethods` can tell, so it is asked.
 */
export async function hasAuthenticatorApp(
  twoFactorEnabled: boolean,
  credentials: readonly WebAuthnCredential[],
  rpId: string,
  factorMethods: () => Promise<FactorMethod[]>,
): Promise<boolean> {
  if (!twoFactorEnabled) return false
  if (!credentials.some(c => isUsable(c, rpId))) return true
  return (await factorMethods()).includes('totp')
}

/** A name for a new credential, checked like the API does; the server's 422 is shown as well. */
export function validateCredentialName(name: string): string | undefined {
  const n = name.trim()
  if (!n) return 'Enter a name, such as the key\'s model or the device it\'s on.'
  if ([...n].length > CREDENTIAL_NAME_MAX) return `Use at most ${CREDENTIAL_NAME_MAX} characters.`
  return undefined
}

/** The text of the recovery codes file the user downloads. */
export function recoveryCodesText(codes: readonly string[], account: { email: string, host: string }, now = new Date()): string {
  return [
    `PressFleet recovery codes for ${account.email} (${account.host})`,
    `Generated ${now.toISOString()}`,
    '',
    'Each code signs you in once in place of your second factor.',
    'Keep them somewhere safe. Generating new codes makes these stop working.',
    '',
    ...codes,
    '',
  ].join('\n')
}

/**
 * The recovery codes file as a `data:` URL for an `<a download>`: nothing
 * is uploaded or kept, and the CSP needs no change for it.
 */
export function recoveryCodesHref(text: string): string {
  return `data:text/plain;charset=utf-8,${encodeURIComponent(text)}`
}
