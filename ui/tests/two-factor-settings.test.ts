import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import TwoFactorSettings from '../app/components/two-factor/TwoFactorSettings.vue'
import ProblemAlert from '../app/components/ProblemAlert.vue'
import { ApiError } from '../app/utils/api-error'
import { createApiClient } from '../app/utils/api-client'
import { LAST_SECOND_FACTOR_PROBLEM_TYPE } from '../app/utils/two-factor'
import { twoFactorClient, type TotpEnrollment, type TwoFactorClient, type TwoFactorStatus, type WebAuthnCredential } from '../app/utils/two-factor-settings'
import { WebAuthnError, type AttestationJSON, type CreationOptionsJSON } from '../app/utils/webauthn'
import { uiStubs } from './stubs'

const UModal = defineComponent({
  props: ['open', 'title', 'description', 'dismissible', 'close'],
  emits: ['update:open'],
  setup(props: { open: boolean, title: string, description?: string }, { slots, emit }) {
    return () => (props.open
      ? h('div', { 'role': 'dialog', 'data-title': props.title }, [
          h('h2', props.title),
          h('p', { class: 'description' }, props.description),
          h('button', { class: 'x', onClick: () => emit('update:open', false) }, 'Close dialog'),
          slots.body?.(),
          slots.footer?.(),
        ])
      : null)
  },
})
const global = { stubs: { ...uiStubs, UModal }, components: { ProblemAlert } }

const RP = 'dash.example.com'
const SECRET = 'JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP'
const enrollment: TotpEnrollment = {
  secret: SECRET,
  uri: `otpauth://totp/PressFleet:ann%40example.com?secret=${SECRET}&issuer=PressFleet`,
  expires_at: '2026-10-20T12:15:00Z',
}
const CODES = ['AAAA-BBBB-CCCC-DDDD', 'EEEE-FFFF-GGGG-HHHH']
const options: CreationOptionsJSON = {
  challenge: 'AQID',
  rp: { id: RP, name: 'PressFleet' },
  user: { id: 'BAUG', name: 'ann@example.com', displayName: 'Ann' },
  pubKeyCredParams: [{ type: 'public-key', alg: -7 }],
}
const attestation: AttestationJSON = {
  id: 'CQoL',
  rawId: 'CQoL',
  type: 'public-key',
  clientExtensionResults: {},
  response: { clientDataJSON: 'e30', attestationObject: 'AA' },
}

const cred = (over: Partial<WebAuthnCredential> = {}): WebAuthnCredential => ({
  id: 'c1',
  name: 'YubiKey 5C',
  rp_id: RP,
  aaguid: '00000000-0000-0000-0000-000000000000',
  transports: ['usb'],
  user_verified: true,
  backup_eligible: false,
  backup_state: false,
  sign_count: 3,
  created_at: '2026-09-01T00:00:00Z',
  last_used_at: null,
  ...over,
})

const apiError = (status: number, extra: Partial<ConstructorParameters<typeof ApiError>[0]> = {}) =>
  new ApiError({ type: 'about:blank', title: `HTTP ${status}`, status, ...extra })

// What the fake server says the user has; tests change it as calls succeed.
function statusOf(over: { totp?: boolean, pending?: string, credentials?: WebAuthnCredential[], rpId?: string | null, codes?: number } = {}): TwoFactorStatus {
  const credentials = over.credentials ?? []
  const rpId = over.rpId === undefined ? RP : over.rpId
  const usable = credentials.filter(c => c.rp_id === rpId).length
  const enabled = !!over.totp || usable > 0
  return {
    enabled,
    rp_id: rpId,
    totp: { confirmed: !!over.totp, confirmed_at: over.totp ? '2026-09-01T00:00:00Z' : null, pending_expires_at: over.pending ?? null },
    webauthn: { credentials, usable },
    recovery_codes_remaining: over.codes ?? (enabled ? 10 : 0),
  }
}

function fakeClient(initial: TwoFactorStatus = statusOf(), over: Partial<TwoFactorClient> = {}) {
  const server = { status: initial }
  const client = {
    server,
    status: vi.fn(async () => JSON.parse(JSON.stringify(server.status)) as TwoFactorStatus),
    startTotp: vi.fn(async () => ({ ...enrollment })),
    confirmTotp: vi.fn(async (_code: string) => undefined as string[] | undefined),
    removeTotp: vi.fn(async () => {}),
    webAuthnOptions: vi.fn(async () => options),
    registerCredential: vi.fn(async (name: string, _c: AttestationJSON) => ({ credential: cred({ id: 'new', name }) as WebAuthnCredential, recoveryCodes: undefined as string[] | undefined })),
    renameCredential: vi.fn(async (id: string, name: string) => cred({ id, name })),
    removeCredential: vi.fn(async (_id: string) => {}),
    regenerateRecoveryCodes: vi.fn(async () => CODES),
  }
  // Overrides are mocks too.
  return { ...client, ...over } as typeof client
}

const user = { email: 'ann@example.com' }

async function setup(client: TwoFactorClient, create = vi.fn(async (_o: CreationOptionsJSON, _s: AbortSignal) => attestation)) {
  const wrapper = mount(TwoFactorSettings, {
    props: { client, user, create, webAuthnAvailable: true },
    global,
    attachTo: document.body,
  })
  await flushPromises()
  return { wrapper, create }
}

const button = (w: VueWrapper | ReturnType<VueWrapper['find']>, text: string) => {
  const b = w.findAll('button').find(b => b.text() === text)
  if (!b) throw new Error(`no button "${text}" in ${w.text()}`)
  return b
}
const dialog = (w: VueWrapper, title: string) => w.find(`[role=dialog][data-title="${title}"]`)

async function click(w: VueWrapper | ReturnType<VueWrapper['find']>, text: string) {
  await button(w, text).trigger('click')
  await flushPromises()
}

afterEach(() => {
  document.body.innerHTML = ''
})

describe('TwoFactorSettings: loading', () => {
  it('learns everything from the status read, without starting a challenge', async () => {
    const client = fakeClient(statusOf({ totp: true, credentials: [cred()], codes: 7 }))
    const { wrapper } = await setup(client)
    expect(client.status).toHaveBeenCalledTimes(1)
    expect(client.webAuthnOptions).not.toHaveBeenCalled()
    expect(client.startTotp).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid=totp]').text()).toContain('On.')
    expect(wrapper.find('[data-testid=credential]').text()).toContain('YubiKey 5C')
    expect(wrapper.find('[data-testid=recovery-remaining]').text()).toContain('You have 7 unused recovery codes.')
  })

  it('tells a usable key from an authenticator app', async () => {
    const { wrapper } = await setup(fakeClient(statusOf({ credentials: [cred()] })))
    expect(wrapper.find('[data-testid=totp]').text()).toContain('Not set up')
    expect(wrapper.find('[data-testid=credential]').text()).not.toContain('Can\'t be used')
  })

  it('shows a failed read with a retry', async () => {
    const status = vi.fn<TwoFactorClient['status']>()
      .mockRejectedValueOnce(apiError(500, { detail: 'boom' }))
      .mockResolvedValueOnce(statusOf({ totp: true }))
    const { wrapper } = await setup(fakeClient(statusOf(), { status }))
    expect(wrapper.find('[role=alert]').text()).toContain('boom')
    await click(wrapper, 'Try again')
    expect(wrapper.find('[data-testid=totp]').text()).toContain('On.')
  })
})

describe('twoFactorClient', () => {
  it('loads the settings with GET /users/me/two-factor alone, never reauthentication options', async () => {
    const requests: string[] = []
    const api = createApiClient({
      baseUrl: 'http://pressfleet.test',
      fetch: async (r) => {
        requests.push(`${r.method} ${new URL(r.url).pathname}`)
        return new Response(JSON.stringify(statusOf({ totp: true })), { status: 200, headers: { 'Content-Type': 'application/json' } })
      },
    })
    const { wrapper } = await setup(twoFactorClient(api))
    expect(requests).toEqual(['GET /api/v1/users/me/two-factor'])
    expect(wrapper.find('[data-testid=totp]').text()).toContain('On.')
  })
})

describe('TwoFactorSettings: authenticator app', () => {
  it('sets one up as the first factor and shows the recovery codes before saying it changed', async () => {
    const client = fakeClient()
    client.confirmTotp.mockImplementation(async () => {
      client.server.status = statusOf({ totp: true })
      return CODES
    })
    const { wrapper } = await setup(client)
    expect(wrapper.find('[data-testid=totp]').text()).toContain('Not set up')

    await click(wrapper, 'Set up authenticator app')
    const setup_ = dialog(wrapper, 'Set up an authenticator app')
    expect(setup_.exists()).toBe(true)
    expect(setup_.find('[data-testid=qr-code]').attributes('src')).toMatch(/^data:image\/svg\+xml;/)
    expect((setup_.find('input[readonly]').element as HTMLInputElement).value).toBe(SECRET)

    await setup_.find('form input').setValue(' 123 456 ')
    await setup_.find('form').trigger('submit')
    await flushPromises()
    expect(client.confirmTotp).toHaveBeenCalledWith('123456')
    expect(dialog(wrapper, 'Set up an authenticator app').exists()).toBe(false)
    expect(wrapper.find('[data-testid=totp]').text()).toContain('On.')
    expect(wrapper.find('[data-testid=recovery-remaining]').text()).toContain('You have 10 unused')

    const codes = dialog(wrapper, 'Your recovery codes')
    expect(codes.findAll('[data-testid=recovery-codes] li').map(l => l.text())).toEqual(CODES)
    expect(decodeURIComponent(codes.find('[data-testid=download-codes]').attributes('href')!)).toContain(CODES[0])
    expect(wrapper.emitted('changed')).toBeUndefined()

    await click(codes, 'Done, I\'ve saved them')
    expect(dialog(wrapper, 'Your recovery codes').exists()).toBe(false)
    expect(wrapper.text()).not.toContain(CODES[0])
    expect(wrapper.emitted('changed')).toEqual([[]])
  })

  it('drops the secret when the dialog closes and asks for a new one next time', async () => {
    const client = fakeClient()
    const { wrapper } = await setup(client)
    await click(wrapper, 'Set up authenticator app')
    await dialog(wrapper, 'Set up an authenticator app').find('form input').setValue('12')
    expect(wrapper.html()).toContain(SECRET)

    await click(dialog(wrapper, 'Set up an authenticator app'), 'Close dialog')
    expect(dialog(wrapper, 'Set up an authenticator app').exists()).toBe(false)
    expect(wrapper.html()).not.toContain(SECRET)
    expect(wrapper.html()).not.toContain(encodeURIComponent(SECRET))
    expect((wrapper.vm as unknown as { totpSetup?: unknown }).totpSetup).toBeUndefined()

    await click(wrapper, 'Set up authenticator app')
    expect(client.startTotp).toHaveBeenCalledTimes(2)
    expect((dialog(wrapper, 'Set up an authenticator app').find('form input').element as HTMLInputElement).value).toBe('')
  })

  it('keeps the dialog open after a wrong code and starts again after it expired', async () => {
    const client = fakeClient()
    client.confirmTotp
      .mockRejectedValueOnce(apiError(422, { errors: [{ field: '/code', detail: 'is incorrect' }] }))
      .mockRejectedValueOnce(apiError(404))
    const { wrapper } = await setup(client)
    await click(wrapper, 'Set up authenticator app')
    const form = () => dialog(wrapper, 'Set up an authenticator app').find('form')

    await form().find('input').setValue('000000')
    await form().trigger('submit')
    await flushPromises()
    expect(dialog(wrapper, 'Set up an authenticator app').text()).toContain('That code is incorrect')

    await form().find('input').setValue('111111')
    await form().trigger('submit')
    await flushPromises()
    expect(dialog(wrapper, 'Set up an authenticator app').text()).toContain('This setup expired')
    await click(dialog(wrapper, 'Set up an authenticator app'), 'Start again')
    expect(client.startTotp).toHaveBeenCalledTimes(2)
    expect(form().exists()).toBe(true)
    expect(wrapper.emitted('changed')).toBeUndefined()
  })

  it('shows a refused start (a cancelled reauthentication) without opening the dialog', async () => {
    const client = fakeClient()
    client.startTotp.mockRejectedValue(apiError(403, { detail: 'Reauthenticate first.' }))
    const { wrapper } = await setup(client)
    await click(wrapper, 'Set up authenticator app')
    expect(dialog(wrapper, 'Set up an authenticator app').exists()).toBe(false)
    expect(wrapper.find('[role=alert]').text()).toContain('Reauthenticate first.')
  })

  it('reloads the status when one was set up elsewhere since the page loaded', async () => {
    const client = fakeClient()
    client.startTotp.mockRejectedValue(apiError(409))
    const { wrapper } = await setup(client)
    client.server.status = statusOf({ totp: true })
    await click(wrapper, 'Set up authenticator app')
    expect(client.status).toHaveBeenCalledTimes(2)
    expect(dialog(wrapper, 'Set up an authenticator app').exists()).toBe(false)
    expect(wrapper.find('[data-testid=totp]').text()).toContain('On.')
  })

  it('removes it, or explains why the last factor stays', async () => {
    const client = fakeClient(statusOf({ totp: true }))
    client.removeTotp
      .mockRejectedValueOnce(apiError(409, { type: LAST_SECOND_FACTOR_PROBLEM_TYPE }))
      .mockImplementationOnce(async () => {
        client.server.status = statusOf()
      })
    const { wrapper } = await setup(client)
    expect(wrapper.find('[data-testid=totp]').text()).toContain('On.')

    await click(wrapper, 'Remove authenticator app')
    const confirm = () => dialog(wrapper, 'Remove your authenticator app?')
    await click(confirm(), 'Remove')
    expect(confirm().find('[data-testid=last-factor]').text()).toContain('can\'t remove your last one')
    expect(wrapper.emitted('changed')).toBeUndefined()

    await click(confirm(), 'Remove')
    expect(confirm().exists()).toBe(false)
    expect(client.removeTotp).toHaveBeenCalledTimes(2)
    expect(client.status).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid=totp]').text()).toContain('Not set up')
    expect(wrapper.emitted('changed')).toEqual([[]])
  })
})

describe('TwoFactorSettings: security keys and passkeys', () => {
  it('registers one as the first factor, with the options fetched when the dialog opens', async () => {
    const client = fakeClient()
    client.registerCredential.mockImplementation(async (name: string) => {
      client.server.status = statusOf({ credentials: [cred({ id: 'new', name })] })
      return { credential: cred({ id: 'new', name }), recoveryCodes: CODES }
    })
    const { wrapper, create } = await setup(client)
    expect(wrapper.text()).toContain('You have no security keys or passkeys.')
    expect(client.webAuthnOptions).not.toHaveBeenCalled()

    await click(wrapper, 'Add security key or passkey')
    expect(client.webAuthnOptions).toHaveBeenCalledTimes(1)
    const add = dialog(wrapper, 'Add a security key or passkey')

    // A name is needed first.
    await add.find('form').trigger('submit')
    await flushPromises()
    expect(add.text()).toContain('Enter a name')
    expect(create).not.toHaveBeenCalled()

    await add.find('form input').setValue(' Laptop ')
    await add.find('form').trigger('submit')
    await flushPromises()
    expect(create).toHaveBeenCalledWith(options, expect.any(AbortSignal))
    expect(client.registerCredential).toHaveBeenCalledWith('Laptop', attestation)
    expect(dialog(wrapper, 'Add a security key or passkey').exists()).toBe(false)
    expect(wrapper.find('[data-testid=credential]').text()).toContain('Laptop')
    expect(dialog(wrapper, 'Your recovery codes').text()).toContain(CODES[1])

    await click(dialog(wrapper, 'Your recovery codes'), 'Done, I\'ve saved them')
    expect(wrapper.emitted('changed')).toEqual([[]])
  })

  it('shows the browser\'s failure and gets a fresh challenge for the next try', async () => {
    const create = vi.fn<(o: CreationOptionsJSON, s: AbortSignal) => Promise<AttestationJSON>>()
      .mockRejectedValueOnce(new WebAuthnError('The request was cancelled or timed out. Try again.'))
      .mockResolvedValueOnce(attestation)
    const client = fakeClient()
    const { wrapper } = await setup(client, create)
    await click(wrapper, 'Add security key or passkey')
    const add = dialog(wrapper, 'Add a security key or passkey')
    await add.find('form input').setValue('Key')
    await add.find('form').trigger('submit')
    await flushPromises()
    expect(add.text()).toContain('cancelled or timed out')
    expect(client.registerCredential).not.toHaveBeenCalled()
    expect(client.webAuthnOptions).toHaveBeenCalledTimes(2)

    await add.find('form').trigger('submit')
    await flushPromises()
    expect(client.registerCredential).toHaveBeenCalledTimes(1)
    expect(wrapper.emitted('changed')).toEqual([[]])
  })

  it('says when the server refuses the browser\'s answer or already has the key', async () => {
    const client = fakeClient()
    client.registerCredential
      .mockRejectedValueOnce(apiError(422, { errors: [{ field: '/credential', detail: 'challenge expired' }] }))
      .mockRejectedValueOnce(apiError(409))
    const { wrapper } = await setup(client)
    await click(wrapper, 'Add security key or passkey')
    const add = dialog(wrapper, 'Add a security key or passkey')
    await add.find('form input').setValue('Key')
    await add.find('form').trigger('submit')
    await flushPromises()
    expect(add.text()).toContain('couldn\'t be verified')
    await add.find('form').trigger('submit')
    await flushPromises()
    expect(add.text()).toContain('already registered')
  })

  it('lists keys as synced or device-bound with when they were last used, and renames one', async () => {
    const client = fakeClient(statusOf({
      credentials: [
        cred(),
        cred({ id: 'c2', name: 'Phone', backup_eligible: true, backup_state: true, last_used_at: '2026-10-01T00:00:00Z' }),
      ],
    }))
    const { wrapper } = await setup(client)
    const [key, phone] = wrapper.findAll('[data-testid=credential]')
    expect(key!.text()).toContain('Device-bound')
    expect(key!.text()).toContain('Last used never')
    expect(phone!.text()).toContain('Synced')
    expect(phone!.text()).not.toContain('Last used never')

    await phone!.find('[data-testid=rename]').trigger('click')
    await flushPromises()
    const rename = dialog(wrapper, 'Rename security key or passkey')
    expect((rename.find('input').element as HTMLInputElement).value).toBe('Phone')
    await rename.find('input').setValue('Pixel 9')
    await rename.find('form').trigger('submit')
    await flushPromises()
    expect(client.renameCredential).toHaveBeenCalledWith('c2', 'Pixel 9')
    expect(dialog(wrapper, 'Rename security key or passkey').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid=credential]')[1]!.text()).toContain('Pixel 9')
  })

  it('shows a rename\'s field error next to the name', async () => {
    const client = fakeClient(statusOf({ credentials: [cred()] }))
    client.renameCredential.mockRejectedValue(apiError(422, { errors: [{ field: '/name', detail: 'is too long' }] }))
    const { wrapper } = await setup(client)
    await wrapper.find('[data-testid=rename]').trigger('click')
    await flushPromises()
    const rename = dialog(wrapper, 'Rename security key or passkey')
    await rename.find('form').trigger('submit')
    await flushPromises()
    expect(rename.find('[role=alert]').text()).toBe('is too long')
  })

  it('removes one, and keeps the last factor with the reason', async () => {
    const client = fakeClient(statusOf({ credentials: [cred()] }))
    client.removeCredential
      .mockRejectedValueOnce(apiError(409, { type: LAST_SECOND_FACTOR_PROBLEM_TYPE, detail: 'last factor' }))
      .mockImplementationOnce(async () => {
        client.server.status = statusOf()
      })
    const { wrapper } = await setup(client)
    await wrapper.find('[data-testid=remove]').trigger('click')
    await flushPromises()
    const confirm = () => dialog(wrapper, 'Remove YubiKey 5C?')
    expect(confirm().find('.description').text()).toContain('other sessions are signed out')
    await click(confirm(), 'Remove')
    expect(confirm().find('[data-testid=last-factor]').text()).toContain('Add another first')

    await click(confirm(), 'Remove')
    expect(client.removeCredential).toHaveBeenLastCalledWith('c1')
    expect(confirm().exists()).toBe(false)
    expect(wrapper.text()).toContain('You have no security keys or passkeys.')
    expect(wrapper.find('[data-testid=totp]').text()).toContain('Not set up')
    expect(wrapper.find('[data-testid=recovery]').text()).toContain('when you add your first second factor')
    expect(wrapper.emitted('changed')).toEqual([[]])
  })

  it('shows keys from an old dashboard domain as unusable, with Remove only', async () => {
    const client = fakeClient(statusOf({ totp: true, credentials: [cred({ id: 'old', name: 'Old key', rp_id: 'old.example.com' })] }))
    const { wrapper } = await setup(client)
    const item = wrapper.find('[data-testid=credential]')
    expect(item.text()).toContain('Registered for old.example.com, can\'t be used')
    expect(item.text()).toContain('Can\'t be used')
    expect(item.find('[data-testid=rename]').exists()).toBe(false)
    expect(item.find('[data-testid=remove]').exists()).toBe(true)

    await item.find('[data-testid=remove]').trigger('click')
    await flushPromises()
    expect(dialog(wrapper, 'Remove Old key?').find('.description').text()).toContain('can\'t be used on this dashboard')
  })

  it('judges keys by the server\'s relying party ID, not the page\'s host', async () => {
    const pageHost = globalThis.location.hostname
    expect(pageHost).not.toBe(RP)
    const client = fakeClient(statusOf({
      credentials: [cred({ id: 'a', name: 'Current key' }), cred({ id: 'b', name: 'Host key', rp_id: pageHost })],
    }))
    const { wrapper } = await setup(client)
    const [current, host] = wrapper.findAll('[data-testid=credential]')
    expect(current!.text()).not.toContain('Can\'t be used')
    expect(host!.text()).toContain(`Registered for ${pageHost}, can't be used`)
  })

  it('treats every key as unusable while security keys are unavailable', async () => {
    const { wrapper } = await setup(fakeClient(statusOf({ totp: true, rpId: null, credentials: [cred()] })))
    expect(wrapper.find('[data-testid=credential]').text()).toContain(`Registered for ${RP}, can't be used`)
  })

  it('says when the browser can\'t register keys', async () => {
    const wrapper = mount(TwoFactorSettings, {
      props: { client: fakeClient(), user, webAuthnAvailable: false },
      global,
    })
    await flushPromises()
    expect(wrapper.text()).toContain('This browser can\'t register security keys or passkeys.')
    expect(button(wrapper, 'Add security key or passkey').attributes('disabled')).toBeDefined()
  })
})

describe('TwoFactorSettings: recovery codes', () => {
  it('says how many are left and generates new ones after a confirmation', async () => {
    const client = fakeClient(statusOf({ totp: true, codes: 1 }))
    client.regenerateRecoveryCodes.mockImplementation(async () => {
      client.server.status = statusOf({ totp: true })
      return CODES
    })
    const { wrapper } = await setup(client)
    expect(wrapper.find('[data-testid=recovery-remaining]').text()).toContain('You have 1 unused recovery code.')
    expect(wrapper.find('[data-testid=recovery]').text()).toContain('Generate new codes so')

    await click(wrapper.find('[data-testid=recovery]'), 'Generate new codes')
    await click(dialog(wrapper, 'Generate new recovery codes?'), 'Generate new codes')
    expect(client.regenerateRecoveryCodes).toHaveBeenCalledTimes(1)
    const codes = dialog(wrapper, 'Your recovery codes')
    expect(codes.findAll('li').map(l => l.text())).toEqual(CODES)
    expect(codes.find('[data-testid=download-codes]').attributes('download')).toBe(`pressfleet-recovery-codes-${RP}.txt`)

    await click(codes, 'Done, I\'ve saved them')
    expect(wrapper.html()).not.toContain(CODES[0])
    expect(wrapper.find('[data-testid=recovery-remaining]').text()).toContain('You have 10 unused')
    expect(wrapper.emitted('changed')).toEqual([[]])
  })

  it('copies the codes', async () => {
    const writeText = vi.fn(async (_: string) => {})
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
    try {
      const { wrapper } = await setup(fakeClient(statusOf({ totp: true })))
      await click(wrapper.find('[data-testid=recovery]'), 'Generate new codes')
      await click(dialog(wrapper, 'Generate new recovery codes?'), 'Generate new codes')
      await wrapper.find('[data-testid=copy-codes]').trigger('click')
      await flushPromises()
      expect(writeText).toHaveBeenCalledWith(CODES.join('\n'))
      expect(dialog(wrapper, 'Your recovery codes').text()).toContain('Copied.')
    } finally {
      vi.unstubAllGlobals()
    }
  })

  it('offers none before the first factor', async () => {
    const { wrapper } = await setup(fakeClient())
    expect(wrapper.find('[data-testid=recovery]').text()).toContain('when you add your first second factor')
    expect(wrapper.find('[data-testid=recovery]').findAll('button')).toHaveLength(0)
  })
})
