import type { components } from '../api/schema.gen'

export type SshKey = components['schemas']['SshKey']
export type SshKeyCreate = components['schemas']['SshKeyCreate']

export const SSH_KEY_NAME_MAX = 100
export const SSH_KEY_MAX = 16384

// The algorithms the API accepts (OpenAPI addOwnSshKey): Ed25519, ECDSA,
// security keys and RSA. DSA keys and certificates are refused.
const keyTypes = [
  'ssh-ed25519', 'ecdsa-sha2-nistp256', 'ecdsa-sha2-nistp384', 'ecdsa-sha2-nistp521',
  'sk-ssh-ed25519@openssh.com', 'sk-ecdsa-sha2-nistp256@openssh.com', 'ssh-rsa',
]

export interface SshKeyFormValues {
  name: string
  public_key: string
}

/**
 * Client-side checks that catch the common mistakes (pasting a private key, a
 * path, or several keys) before a round trip. The server stays authoritative:
 * it parses the key and checks RSA strength.
 */
export function validateSshKey(v: SshKeyFormValues): Partial<Record<keyof SshKeyFormValues, string>> {
  const errors: Partial<Record<keyof SshKeyFormValues, string>> = {}
  const key = v.public_key.trim()
  if (key === '') {
    errors.public_key = 'Paste your public key.'
  } else if (key.includes('PRIVATE KEY')) {
    errors.public_key = 'This looks like a private key. Paste the public key (the .pub file) instead.'
  } else if (/\r?\n/.test(key)) {
    errors.public_key = 'Add one key at a time.'
  } else if (key.length > SSH_KEY_MAX) {
    errors.public_key = `Use at most ${SSH_KEY_MAX} characters.`
  } else if (!keyTypes.includes(key.split(/\s+/)[0] ?? '')) {
    errors.public_key = 'Use a key such as "ssh-ed25519 AAAA... you@host", as in your .pub file.'
  }
  if (v.name.trim().length > SSH_KEY_NAME_MAX) errors.name = `Use at most ${SSH_KEY_NAME_MAX} characters.`
  return errors
}

/** The POST body; a blank name is omitted so the key's comment is used. */
export function sshKeyPayload(v: SshKeyFormValues): SshKeyCreate {
  const body: SshKeyCreate = { public_key: v.public_key.trim() }
  if (v.name.trim() !== '') body.name = v.name.trim()
  return body
}
