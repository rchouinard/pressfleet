import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  assertionToJSON,
  attestationToJSON,
  createCredential,
  fromBase64url,
  getAssertion,
  parseCreationOptions,
  parseRequestOptions,
  toBase64url,
  WebAuthnError,
  type CreationOptionsJSON,
  type RequestOptionsJSON,
} from '../app/utils/webauthn'

const bytes = (...b: number[]) => new Uint8Array(b)

describe('base64url', () => {
  it('encodes without padding, URL-safe', () => {
    expect(toBase64url(bytes())).toBe('')
    expect(toBase64url(bytes(0xfb, 0xff))).toBe('-_8')
    expect(toBase64url(bytes(1, 2, 3).buffer)).toBe('AQID')
    expect(toBase64url(new TextEncoder().encode('hello'))).toBe('aGVsbG8')
  })

  it('encodes a view at an offset', () => {
    const view = new Uint8Array([9, 1, 2, 3, 9]).subarray(1, 4)
    expect(toBase64url(view)).toBe('AQID')
  })

  it('decodes with or without padding', () => {
    expect(Array.from(fromBase64url('-_8'))).toEqual([0xfb, 0xff])
    expect(Array.from(fromBase64url('aGVsbG8='))).toEqual(Array.from(new TextEncoder().encode('hello')))
    expect(Array.from(fromBase64url(''))).toEqual([])
  })

  it('round-trips every byte value', () => {
    const all = new Uint8Array(256).map((_, i) => i)
    expect(Array.from(fromBase64url(toBase64url(all)))).toEqual(Array.from(all))
  })

  it('refuses standard base64 and junk', () => {
    expect(() => fromBase64url('a+b/')).toThrow(TypeError)
    expect(() => fromBase64url('not base64!')).toThrow(TypeError)
  })
})

const options: RequestOptionsJSON = {
  challenge: 'AQID',
  timeout: 60000,
  rpId: 'fleet.example.com',
  allowCredentials: [{ id: 'BAUG', type: 'public-key', transports: ['usb', 'nfc'] }, { id: 'Bwg', type: 'public-key' }],
  userVerification: 'preferred',
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('parseRequestOptions', () => {
  it('decodes the binary members itself without browser support', () => {
    vi.stubGlobal('PublicKeyCredential', undefined)
    const parsed = parseRequestOptions(options)
    expect(Array.from(parsed.challenge as Uint8Array)).toEqual([1, 2, 3])
    expect(parsed.timeout).toBe(60000)
    expect(parsed.rpId).toBe('fleet.example.com')
    expect(parsed.userVerification).toBe('preferred')
    expect(parsed.allowCredentials!.map(c => [Array.from(c.id as Uint8Array), c.type, c.transports])).toEqual([
      [[4, 5, 6], 'public-key', ['usb', 'nfc']],
      [[7, 8], 'public-key', undefined],
    ])
  })

  it('leaves out members the server didn\'t send', () => {
    vi.stubGlobal('PublicKeyCredential', undefined)
    expect(parseRequestOptions({ challenge: 'AQID' })).toEqual({ challenge: bytes(1, 2, 3) })
  })

  it('uses the browser\'s parser where there is one', () => {
    const native = { challenge: new ArrayBuffer(3) }
    const parseRequestOptionsFromJSON = vi.fn(() => native)
    vi.stubGlobal('PublicKeyCredential', Object.assign(function PublicKeyCredential() {}, { parseRequestOptionsFromJSON }))
    expect(parseRequestOptions(options)).toBe(native)
    expect(parseRequestOptionsFromJSON).toHaveBeenCalledWith(options)
  })
})

// A credential as navigator.credentials.get() returns it, without toJSON.
function fakeCredential(extra: Record<string, unknown> = {}) {
  return {
    id: 'BAUG',
    rawId: bytes(4, 5, 6).buffer,
    type: 'public-key',
    authenticatorAttachment: 'cross-platform',
    getClientExtensionResults: () => ({}),
    response: {
      clientDataJSON: new TextEncoder().encode('{"type":"webauthn.get"}').buffer,
      authenticatorData: bytes(10, 11).buffer,
      signature: bytes(12).buffer,
      userHandle: null,
    },
    ...extra,
  } as unknown as PublicKeyCredential
}

describe('assertionToJSON', () => {
  it('encodes the assertion itself without toJSON', () => {
    expect(assertionToJSON(fakeCredential())).toEqual({
      id: 'BAUG',
      rawId: 'BAUG',
      type: 'public-key',
      authenticatorAttachment: 'cross-platform',
      clientExtensionResults: {},
      response: {
        clientDataJSON: 'eyJ0eXBlIjoid2ViYXV0aG4uZ2V0In0',
        authenticatorData: 'Cgs',
        signature: 'DA',
        userHandle: null,
      },
    })
  })

  it('encodes a user handle', () => {
    const c = fakeCredential()
    ;(c.response as unknown as { userHandle: ArrayBuffer }).userHandle = bytes(1).buffer
    expect(assertionToJSON(c).response.userHandle).toBe('AQ')
  })

  it('uses toJSON where the browser has it', () => {
    const json = { id: 'native' }
    expect(assertionToJSON(fakeCredential({ toJSON: () => json }))).toBe(json)
  })
})

describe('getAssertion', () => {
  function supportWith(get: (o: CredentialRequestOptions) => Promise<Credential | null>) {
    vi.stubGlobal('PublicKeyCredential', function PublicKeyCredential() {})
    vi.stubGlobal('navigator', { credentials: { get: vi.fn(get) } })
    return navigator.credentials.get as ReturnType<typeof vi.fn>
  }

  it('asks the browser with the parsed options and returns JSON', async () => {
    const get = supportWith(async () => fakeCredential())
    const signal = new AbortController().signal
    const assertion = await getAssertion(options, signal)
    expect(assertion.id).toBe('BAUG')
    const arg = get.mock.calls[0]![0] as CredentialRequestOptions
    expect(arg.signal).toBe(signal)
    expect(Array.from(arg.publicKey!.challenge as Uint8Array)).toEqual([1, 2, 3])
  })

  it('says so when the browser has no WebAuthn', async () => {
    vi.stubGlobal('PublicKeyCredential', undefined)
    await expect(getAssertion(options)).rejects.toThrow(/doesn't support security keys/)
  })

  it('words a cancelled or timed-out prompt', async () => {
    supportWith(async () => {
      throw new DOMException('nope', 'NotAllowedError')
    })
    const err = await getAssertion(options).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(WebAuthnError)
    expect((err as Error).message).toMatch(/cancelled or timed out/)
    expect((err as Error).cause).toBeInstanceOf(DOMException)
  })

  it('words an origin the keys aren\'t registered for', async () => {
    supportWith(async () => {
      throw new DOMException('nope', 'SecurityError')
    })
    await expect(getAssertion(options)).rejects.toThrow(/isn't allowed/)
  })

  it('fails when no credential comes back', async () => {
    supportWith(async () => null)
    await expect(getAssertion(options)).rejects.toBeInstanceOf(WebAuthnError)
  })
})

const creation: CreationOptionsJSON = {
  challenge: 'AQID',
  rp: { id: 'dash.example.com', name: 'PressFleet' },
  user: { id: 'BAUG', name: 'ann@example.com', displayName: 'Ann' },
  pubKeyCredParams: [{ type: 'public-key', alg: -7 }, { type: 'public-key', alg: -257 }],
  timeout: 300000,
  excludeCredentials: [{ id: 'BwgJ', type: 'public-key', transports: ['usb'] }],
  authenticatorSelection: { residentKey: 'preferred', userVerification: 'preferred' },
  attestation: 'none',
}

describe('parseCreationOptions', () => {
  it('decodes the binary members without browser support', () => {
    vi.stubGlobal('PublicKeyCredential', undefined)
    const o = parseCreationOptions(creation)
    expect(Array.from(o.challenge as Uint8Array)).toEqual([1, 2, 3])
    expect(Array.from(o.user.id as Uint8Array)).toEqual([4, 5, 6])
    expect(o.user.name).toBe('ann@example.com')
    expect(o.rp).toEqual({ id: 'dash.example.com', name: 'PressFleet' })
    expect(o.pubKeyCredParams.map(p => p.alg)).toEqual([-7, -257])
    expect(o.timeout).toBe(300000)
    expect(o.attestation).toBe('none')
    expect(o.authenticatorSelection).toEqual({ residentKey: 'preferred', userVerification: 'preferred' })
    expect(Array.from(o.excludeCredentials![0]!.id as Uint8Array)).toEqual([7, 8, 9])
    expect(o.excludeCredentials![0]!.transports).toEqual(['usb'])
  })

  it('uses the browser\'s parser when there is one', () => {
    const parsed = { challenge: new Uint8Array([9]) }
    const parseCreationOptionsFromJSON = vi.fn(() => parsed)
    vi.stubGlobal('PublicKeyCredential', Object.assign(function PublicKeyCredential() {}, { parseCreationOptionsFromJSON }))
    expect(parseCreationOptions(creation)).toBe(parsed)
    expect(parseCreationOptionsFromJSON).toHaveBeenCalledWith(creation)
  })
})

function newCredential(withToJSON = false) {
  const response = {
    clientDataJSON: bytes(0x7b, 0x7d).buffer,
    attestationObject: bytes(0xa0).buffer,
    getTransports: () => ['usb', 'nfc'],
  }
  return {
    id: 'CQoL',
    rawId: bytes(9, 10, 11).buffer,
    type: 'public-key',
    authenticatorAttachment: 'cross-platform',
    response,
    getClientExtensionResults: () => ({ credProps: { rk: true } }),
    ...(withToJSON ? { toJSON: () => ({ id: 'from-browser' }) } : {}),
  } as unknown as PublicKeyCredential
}

describe('attestationToJSON', () => {
  it('encodes the new credential for the API', () => {
    expect(attestationToJSON(newCredential())).toEqual({
      id: 'CQoL',
      rawId: 'CQoL',
      type: 'public-key',
      authenticatorAttachment: 'cross-platform',
      clientExtensionResults: { credProps: { rk: true } },
      response: { clientDataJSON: 'e30', attestationObject: 'oA', transports: ['usb', 'nfc'] },
    })
  })

  it('prefers the browser\'s toJSON', () => {
    expect(attestationToJSON(newCredential(true))).toEqual({ id: 'from-browser' })
  })
})

describe('createCredential', () => {
  function supportWith(create: (o: CredentialCreationOptions) => Promise<Credential | null>) {
    vi.stubGlobal('PublicKeyCredential', function PublicKeyCredential() {})
    vi.stubGlobal('navigator', { credentials: { create: vi.fn(create) } })
  }

  it('creates a credential for the options and returns its JSON form', async () => {
    let arg: CredentialCreationOptions | undefined
    supportWith(async (o) => {
      arg = o
      return newCredential() as unknown as Credential
    })
    const signal = new AbortController().signal
    const json = await createCredential(creation, signal)
    expect(json.rawId).toBe('CQoL')
    expect(arg!.signal).toBe(signal)
    expect(Array.from(arg!.publicKey!.challenge as Uint8Array)).toEqual([1, 2, 3])
  })

  it('says so when the browser can\'t register keys', async () => {
    vi.stubGlobal('PublicKeyCredential', undefined)
    await expect(createCredential(creation)).rejects.toThrow(/doesn't support security keys/)
  })

  it('words a key that is already registered, and a cancelled prompt', async () => {
    supportWith(async () => {
      throw new DOMException('nope', 'InvalidStateError')
    })
    await expect(createCredential(creation)).rejects.toThrow(/already registered/)
    supportWith(async () => {
      throw new DOMException('nope', 'NotAllowedError')
    })
    const err = await createCredential(creation).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(WebAuthnError)
    expect((err as Error).message).toBe('The request was cancelled or timed out. Try again.')
  })

  it('fails when no credential comes back', async () => {
    supportWith(async () => null)
    await expect(createCredential(creation)).rejects.toBeInstanceOf(WebAuthnError)
  })
})
