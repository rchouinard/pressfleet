// WebAuthn assertions for the login's second step and reauthentication
// (design doc §6.2). The control plane sends request options in the JSON form
// (PublicKeyCredentialRequestOptionsJSON: binary members base64url without
// padding) and takes the assertion back in the JSON form of toJSON(). Newer
// browsers convert both ways themselves; the helpers here do it for the rest.

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

/** Whether this browser can use security keys and passkeys at all. */
export function webAuthnSupported(): boolean {
  return typeof globalThis.PublicKeyCredential === 'function' && typeof globalThis.navigator?.credentials?.get === 'function'
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

function describeFailure(e: unknown): string {
  const name = e instanceof Error ? e.name : ''
  switch (name) {
    case 'NotAllowedError':
      return 'The request was cancelled or timed out, or no registered security key or passkey was used. Try again.'
    case 'SecurityError':
      return 'This address isn\'t allowed to use the security keys and passkeys registered for this dashboard.'
    case 'AbortError':
      return 'The request was cancelled.'
    default:
      return 'The security key or passkey couldn\'t be used. Try again, or use another method.'
  }
}
