import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import TwoFactorSettings from '../app/components/two-factor/TwoFactorSettings.vue'
import ProblemAlert from '../app/components/ProblemAlert.vue'
import { ApiError } from '../app/utils/api-error'
import { LAST_SECOND_FACTOR_PROBLEM_TYPE } from '../app/utils/two-factor'
import type { TotpEnrollment, TwoFactorClient, WebAuthnCredential } from '../app/utils/two-factor-settings'
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

function fakeClient(over: Partial<TwoFactorClient> = {}) {
  return {
    listCredentials: vi.fn(async () => [] as WebAuthnCredential[]),
    factorMethods: vi.fn(async () => ['totp' as const]),
    startTotp: vi.fn(async () => ({ ...enrollment })),
    confirmTotp: vi.fn(async (_code: string) => undefined as string[] | undefined),
    removeTotp: vi.fn(async () => {}),
    webAuthnOptions: vi.fn(async () => options),
    registerCredential: vi.fn(async (name: string, _c: AttestationJSON) => ({ credential: cred({ id: 'new', name }) as WebAuthnCredential, recoveryCodes: undefined as string[] | undefined })),
    renameCredential: vi.fn(async (id: string, name: string) => cred({ id, name })),
    removeCredential: vi.fn(async (_id: string) => {}),
    regenerateRecoveryCodes: vi.fn(async () => CODES),
    ...over,
  }
}

type User = { email: string, two_factor_enabled: boolean, two_factor_required: boolean, recovery_codes_remaining: number }
const newUser: User = { email: 'ann@example.com', two_factor_enabled: false, two_factor_required: true, recovery_codes_remaining: 0 }
const enrolledUser: User = { ...newUser, two_factor_enabled: true, recovery_codes_remaining: 10 }

async function setup(client: TwoFactorClient, user: User = newUser, create = vi.fn(async (_o: CreationOptionsJSON, _s: AbortSignal) => attestation)) {
  const wrapper = mount(TwoFactorSettings, {
    props: { client, user, rpId: RP, create, webAuthnAvailable: true },
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

describe('TwoFactorSettings: authenticator app', () => {
  it('sets one up as the first factor and shows the recovery codes before saying it changed', async () => {
    const client = fakeClient({ confirmTotp: vi.fn(async () => CODES) })
    const { wrapper } = await setup(client)
    expect(wrapper.find('[data-testid=totp]').text()).toContain('Not set up')
    expect(client.factorMethods).not.toHaveBeenCalled()

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
    const confirmTotp = vi.fn<(code: string) => Promise<string[] | undefined>>()
      .mockRejectedValueOnce(apiError(422, { errors: [{ field: '/code', detail: 'is incorrect' }] }))
      .mockRejectedValueOnce(apiError(404))
    const client = fakeClient({ confirmTotp })
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
    const client = fakeClient({ startTotp: vi.fn(async () => {
      throw apiError(403, { detail: 'Reauthenticate first.' })
    }) })
    const { wrapper } = await setup(client)
    await click(wrapper, 'Set up authenticator app')
    expect(dialog(wrapper, 'Set up an authenticator app').exists()).toBe(false)
    expect(wrapper.find('[role=alert]').text()).toContain('Reauthenticate first.')
  })

  it('removes it, or explains why the last factor stays', async () => {
    const removeTotp = vi.fn<() => Promise<void>>()
      .mockRejectedValueOnce(apiError(409, { type: LAST_SECOND_FACTOR_PROBLEM_TYPE }))
      .mockResolvedValueOnce()
    const client = fakeClient({ removeTotp })
    const { wrapper } = await setup(client, enrolledUser)
    // No usable key: the factor must be the app, without asking.
    expect(client.factorMethods).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid=totp]').text()).toContain('On.')

    await click(wrapper, 'Remove authenticator app')
    const confirm = () => dialog(wrapper, 'Remove your authenticator app?')
    await click(confirm(), 'Remove')
    expect(confirm().find('[data-testid=last-factor]').text()).toContain('can\'t remove your last one')
    expect(wrapper.emitted('changed')).toBeUndefined()

    await click(confirm(), 'Remove')
    expect(confirm().exists()).toBe(false)
    expect(removeTotp).toHaveBeenCalledTimes(2)
    expect(client.listCredentials).toHaveBeenCalledTimes(2)
    expect(wrapper.find('[data-testid=totp]').text()).toContain('Not set up')
    expect(wrapper.emitted('changed')).toEqual([[]])
  })

  it('asks which factors the user has when they also have a usable key', async () => {
    const client = fakeClient({ listCredentials: vi.fn(async () => [cred()]), factorMethods: vi.fn(async () => ['webauthn' as const, 'recovery_code' as const]) })
    const { wrapper } = await setup(client, enrolledUser)
    expect(client.factorMethods).toHaveBeenCalledTimes(1)
    expect(wrapper.find('[data-testid=totp]').text()).toContain('Not set up')
  })
})

describe('TwoFactorSettings: security keys and passkeys', () => {
  it('registers one as the first factor, with the options fetched when the dialog opens', async () => {
    const client = fakeClient({
      registerCredential: vi.fn(async (name: string) => ({ credential: cred({ id: 'new', name }), recoveryCodes: CODES })),
    })
    const { wrapper, create } = await setup(client)
    expect(wrapper.text()).toContain('You have no security keys or passkeys.')

    await click(wrapper, 'Add security key or passkey')
    expect(client.webAuthnOptions).toHaveBeenCalledTimes(1)
    const add = dialog(wrapper, 'Add a security key or passkey')

    // A name is needed first.
    await add.find('form').trigger('submit')
    await flushPromises()
    expect(add.text()).toContain('Enter a name')
    expect(create).not.toHaveBeenCalled()

    client.listCredentials.mockResolvedValue([cred({ id: 'new', name: 'Laptop' })])
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
    const { wrapper } = await setup(client, newUser, create)
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
    const registerCredential = vi.fn<TwoFactorClient['registerCredential']>()
      .mockRejectedValueOnce(apiError(422, { errors: [{ field: '/credential', detail: 'challenge expired' }] }))
      .mockRejectedValueOnce(apiError(409))
    const { wrapper } = await setup(fakeClient({ registerCredential }))
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
    const client = fakeClient({
      listCredentials: vi.fn(async () => [
        cred(),
        cred({ id: 'c2', name: 'Phone', backup_eligible: true, backup_state: true, last_used_at: '2026-10-01T00:00:00Z' }),
      ]),
    })
    const { wrapper } = await setup(client, enrolledUser)
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
    const client = fakeClient({
      listCredentials: vi.fn(async () => [cred()]),
      renameCredential: vi.fn(async () => {
        throw apiError(422, { errors: [{ field: '/name', detail: 'is too long' }] })
      }),
    })
    const { wrapper } = await setup(client, enrolledUser)
    await wrapper.find('[data-testid=rename]').trigger('click')
    await flushPromises()
    const rename = dialog(wrapper, 'Rename security key or passkey')
    await rename.find('form').trigger('submit')
    await flushPromises()
    expect(rename.find('[role=alert]').text()).toBe('is too long')
  })

  it('removes one, and keeps the last factor with the reason', async () => {
    const removeCredential = vi.fn<(id: string) => Promise<void>>()
      .mockRejectedValueOnce(apiError(409, { type: LAST_SECOND_FACTOR_PROBLEM_TYPE, detail: 'last factor' }))
      .mockResolvedValueOnce()
    const client = fakeClient({ listCredentials: vi.fn(async () => [cred()]), factorMethods: vi.fn(async () => ['webauthn' as const]), removeCredential })
    const { wrapper } = await setup(client, enrolledUser)
    await wrapper.find('[data-testid=remove]').trigger('click')
    await flushPromises()
    const confirm = () => dialog(wrapper, 'Remove YubiKey 5C?')
    expect(confirm().find('.description').text()).toContain('other sessions are signed out')
    await click(confirm(), 'Remove')
    expect(confirm().find('[data-testid=last-factor]').text()).toContain('Add another first')

    client.listCredentials.mockResolvedValue([])
    await click(confirm(), 'Remove')
    expect(removeCredential).toHaveBeenLastCalledWith('c1')
    expect(confirm().exists()).toBe(false)
    expect(wrapper.text()).toContain('You have no security keys or passkeys.')
    // The session in `user` is out of date until the page reloads it; that
    // mustn't make the removed key look like an authenticator app.
    expect(wrapper.find('[data-testid=totp]').text()).toContain('Not set up')
    expect(wrapper.emitted('changed')).toEqual([[]])
  })

  it('shows keys from an old dashboard domain as unusable, with Remove only', async () => {
    const client = fakeClient({ listCredentials: vi.fn(async () => [cred({ id: 'old', name: 'Old key', rp_id: 'old.example.com' })]) })
    const { wrapper } = await setup(client, enrolledUser)
    const item = wrapper.find('[data-testid=credential]')
    expect(item.text()).toContain('Registered for old.example.com, can\'t be used')
    expect(item.text()).toContain('Can\'t be used')
    expect(item.find('[data-testid=rename]').exists()).toBe(false)
    expect(item.find('[data-testid=remove]').exists()).toBe(true)
    // Only unusable keys: the enabled factor must be the authenticator app.
    expect(client.factorMethods).not.toHaveBeenCalled()
    expect(wrapper.find('[data-testid=totp]').text()).toContain('On.')

    await item.find('[data-testid=remove]').trigger('click')
    await flushPromises()
    expect(dialog(wrapper, 'Remove Old key?').find('.description').text()).toContain('can\'t be used on this dashboard')
  })

  it('says when the browser can\'t register keys', async () => {
    const wrapper = mount(TwoFactorSettings, {
      props: { client: fakeClient(), user: newUser, rpId: RP, webAuthnAvailable: false },
      global,
    })
    await flushPromises()
    expect(wrapper.text()).toContain('This browser can\'t register security keys or passkeys.')
    expect(button(wrapper, 'Add security key or passkey').attributes('disabled')).toBeDefined()
  })
})

describe('TwoFactorSettings: recovery codes', () => {
  it('says how many are left and generates new ones after a confirmation', async () => {
    const client = fakeClient()
    const { wrapper } = await setup(client, { ...enrolledUser, recovery_codes_remaining: 1 })
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
    expect(wrapper.emitted('changed')).toEqual([[]])
  })

  it('copies the codes', async () => {
    const writeText = vi.fn(async (_: string) => {})
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })
    try {
      const { wrapper } = await setup(fakeClient(), enrolledUser)
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
