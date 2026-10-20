// WebAuthn for the login's second step, reauthentication, and registering
// security keys and passkeys (design doc §6.2). The control plane sends
// request and creation options in their JSON forms
// (PublicKeyCredentialRequestOptionsJSON and ...CreationOptionsJSON: binary
// members base64url without padding) and takes the credential back in the
// JSON form of toJSON(). Newer browsers convert both ways themselves; the
// helpers here do it for the rest.

/** Request options in their JSON form, as the API sends them. */
export interface RequestOptionsJSON {
  challenge: string
  timeout?: number
  rpId?: string
  allowCredentials?: { id: string, type: string, transports?: string[] }[]
  userVerification?: string
  extensions?: Record<string, unknown>
  [key: string]: unknown
}

/** A PublicKeyCredential from navigator.credentials.get() in its JSON form. */
export interface AssertionJSON {
  id: string
  rawId: string
  type: string
  authenticatorAttachment?: string | null
  clientExtensionResults: Record<string, unknown>
  response: {
    clientDataJSON: string
    authenticatorData: string
    signature: string
    userHandle?: string | null
  }
  [key: string]: unknown
}

/** Creation options in their JSON form, as the API sends them for registration. */
export interface CreationOptionsJSON {
  challenge: string
  rp: { id?: string, name: string }
  user: { id: string, name: string, displayName: string }
  pubKeyCredParams: { type: string, alg: number }[]
  timeout?: number
  excludeCredentials?: { id: string, type: string, transports?: string[] }[]
  authenticatorSelection?: Record<string, unknown>
  attestation?: string
  extensions?: Record<string, unknown>
  [key: string]: unknown
}

/** A PublicKeyCredential from navigator.credentials.create() in its JSON form. */
export interface AttestationJSON {
  id: string
  rawId: string
  type: string
  authenticatorAttachment?: string | null
  clientExtensionResults: Record<string, unknown>
  response: {
    clientDataJSON: string
    attestationObject: string
    transports?: string[]
    [key: string]: unknown
  }
  [key: string]: unknown
}

/** Base64url without padding, as WebAuthn's JSON forms use. */
export function toBase64url(data: ArrayBuffer | ArrayBufferView): string {
  const bytes = data instanceof ArrayBuffer ? new Uint8Array(data) : new Uint8Array(data.buffer, data.byteOffset, data.byteLength)
  let binary = ''
  for (const b of bytes) binary += String.fromCharCode(b)
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

/** Decodes base64url, with or without padding. Throws on anything else. */
export function fromBase64url(value: string): Uint8Array<ArrayBuffer> {
  if (!/^[A-Za-z0-9_-]*={0,2}$/.test(value)) throw new TypeError('Not base64url.')
  const b64 = value.replace(/=+$/, '').replace(/-/g, '+').replace(/_/g, '/')
  const binary = atob(b64 + '='.repeat((4 - (b64.length % 4)) % 4))
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)
  return bytes
}

/** Request options for navigator.credentials.get(), from their JSON form. */
export function parseRequestOptions(json: RequestOptionsJSON): PublicKeyCredentialRequestOptions {
  const native = (globalThis.PublicKeyCredential as { parseRequestOptionsFromJSON?: (o: unknown) => PublicKeyCredentialRequestOptions } | undefined)
    ?.parseRequestOptionsFromJSON
  if (native) return native.call(globalThis.PublicKeyCredential, json)
  const options: PublicKeyCredentialRequestOptions = { challenge: fromBase64url(json.challenge) }
  if (json.timeout !== undefined) options.timeout = json.timeout
  if (json.rpId !== undefined) options.rpId = json.rpId
  if (json.userVerification !== undefined) options.userVerification = json.userVerification as UserVerificationRequirement
  if (json.extensions !== undefined) options.extensions = json.extensions as AuthenticationExtensionsClientInputs
  if (json.allowCredentials) {
    options.allowCredentials = json.allowCredentials.map(c => ({
      id: fromBase64url(c.id),
      type: c.type as PublicKeyCredentialType,
      ...(c.transports ? { transports: c.transports as AuthenticatorTransport[] } : {}),
    }))
  }
  return options
}

/** Creation options for navigator.credentials.create(), from their JSON form. */
export function parseCreationOptions(json: CreationOptionsJSON): PublicKeyCredentialCreationOptions {
  const native = (globalThis.PublicKeyCredential as { parseCreationOptionsFromJSON?: (o: unknown) => PublicKeyCredentialCreationOptions } | undefined)
    ?.parseCreationOptionsFromJSON
  if (native) return native.call(globalThis.PublicKeyCredential, json)
  const options: PublicKeyCredentialCreationOptions = {
    challenge: fromBase64url(json.challenge),
    rp: json.rp,
    user: { ...json.user, id: fromBase64url(json.user.id) },
    pubKeyCredParams: json.pubKeyCredParams.map(p => ({ type: p.type as PublicKeyCredentialType, alg: p.alg })),
  }
  if (json.timeout !== undefined) options.timeout = json.timeout
  if (json.authenticatorSelection !== undefined) options.authenticatorSelection = json.authenticatorSelection as AuthenticatorSelectionCriteria
  if (json.attestation !== undefined) options.attestation = json.attestation as AttestationConveyancePreference
  if (json.extensions !== undefined) options.extensions = json.extensions as AuthenticationExtensionsClientInputs
  if (json.excludeCredentials) {
    options.excludeCredentials = json.excludeCredentials.map(c => ({
      id: fromBase64url(c.id),
      type: c.type as PublicKeyCredentialType,
      ...(c.transports ? { transports: c.transports as AuthenticatorTransport[] } : {}),
    }))
  }
  return options
}

/** A new credential in its JSON form, from what navigator.credentials.create() returned. */
export function attestationToJSON(credential: PublicKeyCredential): AttestationJSON {
  const toJSON = (credential as { toJSON?: () => unknown }).toJSON
  if (typeof toJSON === 'function') return toJSON.call(credential) as AttestationJSON
  const response = credential.response as AuthenticatorAttestationResponse
  const transports = typeof response.getTransports === 'function' ? response.getTransports() : undefined
  return {
    id: credential.id,
    rawId: toBase64url(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment ?? null,
    clientExtensionResults: credential.getClientExtensionResults() as Record<string, unknown>,
    response: {
      clientDataJSON: toBase64url(response.clientDataJSON),
      attestationObject: toBase64url(response.attestationObject),
      ...(transports ? { transports } : {}),
    },
  }
}

/** An assertion in its JSON form, from the credential navigator.credentials.get() returned. */
export function assertionToJSON(credential: PublicKeyCredential): AssertionJSON {
  const toJSON = (credential as { toJSON?: () => unknown }).toJSON
  if (typeof toJSON === 'function') return toJSON.call(credential) as AssertionJSON
  const response = credential.response as AuthenticatorAssertionResponse
  return {
    id: credential.id,
    rawId: toBase64url(credential.rawId),
    type: credential.type,
    authenticatorAttachment: credential.authenticatorAttachment ?? null,
    clientExtensionResults: credential.getClientExtensionResults() as Record<string, unknown>,
    response: {
      clientDataJSON: toBase64url(response.clientDataJSON),
      authenticatorData: toBase64url(response.authenticatorData),
      signature: toBase64url(response.signature),
      userHandle: response.userHandle ? toBase64url(response.userHandle) : null,
    },
  }
}

/** Whether this browser can use (or, with `'create'`, register) security keys and passkeys at all. */
export function webAuthnSupported(operation: 'get' | 'create' = 'get'): boolean {
  return typeof globalThis.PublicKeyCredential === 'function' && typeof globalThis.navigator?.credentials?.[operation] === 'function'
}

/** Why an assertion couldn't be made, worded for the user. */
export class WebAuthnError extends Error {
  constructor(message: string, options?: ErrorOptions) {
    super(message, options)
    this.name = 'WebAuthnError'
  }
}

/**
 * Asks the browser for an assertion for the API's request options and
 * returns it in JSON form. Failures that aren't the server's (no support,
 * the user cancelled, the prompt timed out, no matching key) throw
 * WebAuthnError with a message to show.
 */
export async function getAssertion(options: RequestOptionsJSON, signal?: AbortSignal): Promise<AssertionJSON> {
  if (!webAuthnSupported()) {
    throw new WebAuthnError('This browser doesn\'t support security keys or passkeys. Use another method or another browser.')
  }
  let credential: Credential | null
  try {
    credential = await navigator.credentials.get({ publicKey: parseRequestOptions(options), signal })
  } catch (e) {
    throw new WebAuthnError(describeFailure(e), { cause: e })
  }
  if (!credential) throw new WebAuthnError('No security key or passkey answered. Try again.')
  return assertionToJSON(credential as PublicKeyCredential)
}

/**
 * Asks the browser to create a credential for the API's creation options
 * and returns it in JSON form, to register. Failures that aren't the
 * server's throw WebAuthnError with a message to show, as getAssertion does.
 */
export async function createCredential(options: CreationOptionsJSON, signal?: AbortSignal): Promise<AttestationJSON> {
  if (!webAuthnSupported('create')) {
    throw new WebAuthnError('This browser doesn\'t support security keys or passkeys. Use another browser, or an authenticator app.')
  }
  let credential: Credential | null
  try {
    credential = await navigator.credentials.create({ publicKey: parseCreationOptions(options), signal })
  } catch (e) {
    throw new WebAuthnError(describeFailure(e, 'create'), { cause: e })
  }
  if (!credential) throw new WebAuthnError('No security key or passkey answered. Try again.')
  return attestationToJSON(credential as PublicKeyCredential)
}

function describeFailure(e: unknown, operation: 'get' | 'create' = 'get'): string {
  const name = e instanceof Error ? e.name : ''
  switch (name) {
    case 'NotAllowedError':
      return operation === 'create'
        ? 'The request was cancelled or timed out. Try again.'
        : 'The request was cancelled or timed out, or no registered security key or passkey was used. Try again.'
    case 'InvalidStateError':
      // Only create() raises it: the authenticator holds one of excludeCredentials.
      return 'This security key or passkey is already registered to your account.'
    case 'SecurityError':
      return operation === 'create'
        ? 'Security keys and passkeys can\'t be registered at this address. Open the dashboard at its own domain over HTTPS.'
        : 'This address isn\'t allowed to use the security keys and passkeys registered for this dashboard.'
    case 'AbortError':
      return 'The request was cancelled.'
    default:
      return 'The security key or passkey couldn\'t be used. Try again, or use another method.'
  }
}
