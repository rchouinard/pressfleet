import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import AuthReauthenticateDialog from '../app/components/auth/AuthReauthenticateDialog.vue'
import ProblemAlert from '../app/components/ProblemAlert.vue'
import { useReauthentication } from '../app/composables/useReauthentication'
import { ApiError } from '../app/utils/api-error'
import type { FactorChallenge, FactorProof } from '../app/utils/two-factor'
import type { AssertionJSON } from '../app/utils/webauthn'
import { uiStubs } from './stubs'

const UModal = defineComponent({
  props: ['open', 'title', 'description', 'dismissible'],
  emits: ['update:open'],
  setup(props: { open: boolean, title: string, description: string }, { slots, emit }) {
    return () => (props.open
      ? h('div', { role: 'dialog' }, [
          h('h2', props.title),
          h('p', { class: 'description' }, props.description),
          h('button', { class: 'close', onClick: () => emit('update:open', false) }, 'Close'),
          slots.body?.(),
          slots.footer?.(),
        ])
      : null)
  },
})
const global = { stubs: { ...uiStubs, UModal }, components: { ProblemAlert } }

const assertion: AssertionJSON = {
  id: 'BAUG',
  rawId: 'BAUG',
  type: 'public-key',
  clientExtensionResults: {},
  response: { clientDataJSON: 'e30', authenticatorData: 'AA', signature: 'AA' },
}
const apiError = (status: number, extra: Partial<ConstructorParameters<typeof ApiError>[0]> = {}) =>
  new ApiError({ type: 'about:blank', title: `HTTP ${status}`, status, ...extra })

function setup(options: FactorChallenge | (() => Promise<FactorChallenge>), submit = vi.fn(async (_: FactorProof) => {})) {
  const loadOptions = vi.fn(typeof options === 'function' ? options : async () => options)
  const wrapper = mount(AuthReauthenticateDialog, {
    props: { open: false, loadOptions, submit, assert: async () => assertion },
    global,
    attachTo: document.body,
  })
  return { wrapper, loadOptions, submit }
}

async function open(w: ReturnType<typeof setup>['wrapper']) {
  await w.setProps({ open: true })
  await flushPromises()
}

async function enter(w: ReturnType<typeof setup>['wrapper'], value: string) {
  await w.find('input').setValue(value)
  await w.find('form').trigger('submit')
  await flushPromises()
}

afterEach(() => {
  vi.useRealTimers()
  document.body.innerHTML = ''
})

describe('AuthReauthenticateDialog', () => {
  it('loads the methods each time it opens', async () => {
    const { wrapper, loadOptions } = setup({ methods: ['totp'] })
    expect(loadOptions).not.toHaveBeenCalled()
    await open(wrapper)
    expect(wrapper.find('h2').text()).toBe('Confirm it\'s you')
    expect(wrapper.find('[autocomplete="one-time-code"]').exists()).toBe(true)
    await wrapper.setProps({ open: false })
    await open(wrapper)
    expect(loadOptions).toHaveBeenCalledTimes(2)
  })

  it('asks for the password of users without a second factor', async () => {
    const { wrapper, submit } = setup({ methods: ['password'] })
    await open(wrapper)
    expect(wrapper.find('.description').text()).toContain('Enter your password')
    expect(wrapper.find('input[type="password"]').exists()).toBe(true)
    await enter(wrapper, 'hunter2 ')
    expect(submit).toHaveBeenCalledWith({ method: 'password', password: 'hunter2 ' })
    expect(wrapper.emitted('finish')).toEqual([[true]])
  })

  it('finishes with true after a security key', async () => {
    const { wrapper, submit } = setup({ methods: ['webauthn', 'recovery_code'], webauthn: { challenge: 'AQID' } })
    await open(wrapper)
    await wrapper.findAll('button').find(b => b.text() === 'Use a security key or passkey')!.trigger('click')
    await flushPromises()
    expect(submit).toHaveBeenCalledWith({ method: 'webauthn', credential: assertion })
    expect(wrapper.emitted('finish')).toEqual([[true]])
  })

  it('stays open after a wrong code', async () => {
    const submit = vi.fn<(p: FactorProof) => Promise<void>>()
      .mockRejectedValueOnce(apiError(422, { errors: [{ field: '/code', detail: 'is incorrect' }] }))
      .mockResolvedValueOnce()
    const { wrapper } = setup({ methods: ['totp'] }, submit)
    await open(wrapper)
    await enter(wrapper, '000000')
    expect(wrapper.text()).toContain('That code is incorrect')
    expect(wrapper.emitted('finish')).toBeUndefined()
    await enter(wrapper, '123456')
    expect(wrapper.emitted('finish')).toEqual([[true]])
  })

  it('waits out throttling', async () => {
    vi.useFakeTimers()
    const { wrapper } = setup({ methods: ['totp'] }, vi.fn(async () => {
      throw apiError(429, { retry_after: 3 })
    }))
    await open(wrapper)
    await enter(wrapper, '123456')
    expect(wrapper.text()).toContain('You can try again in 3 seconds')
    expect(wrapper.emitted('finish')).toBeUndefined()
  })

  it('finishes with false when the session is gone', async () => {
    const { wrapper } = setup({ methods: ['totp'] }, vi.fn(async () => {
      throw apiError(401)
    }))
    await open(wrapper)
    await enter(wrapper, '123456')
    expect(wrapper.emitted('finish')).toEqual([[false]])
  })

  it('shows a failure to load the methods with a retry', async () => {
    const options = vi.fn<() => Promise<FactorChallenge>>()
      .mockRejectedValueOnce(apiError(503, { title: 'Service Unavailable' }))
      .mockResolvedValueOnce({ methods: ['totp'] })
    const { wrapper } = setup(options)
    await open(wrapper)
    expect(wrapper.findComponent(ProblemAlert).text()).toContain('Service Unavailable')
    await wrapper.findAll('button').find(b => b.text() === 'Try again')!.trigger('click')
    await flushPromises()
    expect(wrapper.find('[autocomplete="one-time-code"]').exists()).toBe(true)
  })

  it.each([['Cancel'], ['Close']])('finishes with false on %s', async (label) => {
    const { wrapper, submit } = setup({ methods: ['totp'] })
    await open(wrapper)
    await wrapper.findAll('button').find(b => b.text() === label)!.trigger('click')
    expect(wrapper.emitted('finish')).toEqual([[false]])
    expect(submit).not.toHaveBeenCalled()
  })
})

describe('useReauthentication', () => {
  it('shares one prompt until it is answered', async () => {
    const { pending, ask, finish } = useReauthentication()
    const refusal = apiError(403)
    const first = ask(refusal)
    const second = ask(apiError(403))
    expect(pending.value?.error).toBe(refusal)
    finish(true)
    expect(pending.value).toBeUndefined()
    await expect(Promise.all([first, second])).resolves.toEqual([true, true])

    const third = ask(refusal)
    finish(false)
    await expect(third).resolves.toBe(false)
  })
})
