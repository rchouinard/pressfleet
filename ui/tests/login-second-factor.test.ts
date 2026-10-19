import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AuthFactorForm from '../app/components/auth/AuthFactorForm.vue'
import AuthLoginSecondFactor from '../app/components/auth/AuthLoginSecondFactor.vue'
import AuthRecoveryCodesNotice from '../app/components/auth/AuthRecoveryCodesNotice.vue'
import ProblemAlert from '../app/components/ProblemAlert.vue'
import type { components } from '../app/api/schema.gen'
import { ApiError } from '../app/utils/api-error'
import {
  factorChallenge,
  initialMethod,
  isReauthenticationRequired,
  normalizeCode,
  REAUTHENTICATION_REQUIRED_PROBLEM_TYPE,
  SECOND_FACTOR_REQUIRED_PROBLEM_TYPE,
  secondFactorChallenge,
  type FactorChallenge,
  type FactorProof,
} from '../app/utils/two-factor'
import { WebAuthnError, type AssertionJSON } from '../app/utils/webauthn'
import { uiStubs } from './stubs'

type CurrentUser = components['schemas']['CurrentUser']

const global = { stubs: uiStubs, components: { ProblemAlert } }
const webauthn = { challenge: 'AQID', rpId: 'fleet.example.com', allowCredentials: [{ id: 'BAUG', type: 'public-key' }] }
const assertion: AssertionJSON = {
  id: 'BAUG',
  rawId: 'BAUG',
  type: 'public-key',
  clientExtensionResults: {},
  response: { clientDataJSON: 'e30', authenticatorData: 'AA', signature: 'AA' },
}
const user = { id: 'u1', email: 'ed@example.com', role: 'admin', recovery_codes_remaining: 7 } as CurrentUser

function apiError(status: number, extra: Partial<ConstructorParameters<typeof ApiError>[0]> = {}) {
  return new ApiError({ type: 'about:blank', title: `HTTP ${status}`, status, ...extra })
}
const wrong = (field: string) => apiError(422, { errors: [{ field, detail: 'is incorrect' }] })

afterEach(() => {
  vi.useRealTimers()
  document.body.innerHTML = ''
})

describe('two-factor helpers', () => {
  it('reads the second step from the login\'s problem document', () => {
    const err = new ApiError({
      type: SECOND_FACTOR_REQUIRED_PROBLEM_TYPE,
      title: 'Second factor required',
      status: 401,
      methods: ['totp', 'webauthn', 'recovery_code', 'sms'],
      webauthn,
    } as never)
    expect(secondFactorChallenge(err)).toEqual({ methods: ['totp', 'webauthn', 'recovery_code'], webauthn })
  })

  it('ignores other 401s', () => {
    expect(secondFactorChallenge(apiError(401))).toBeUndefined()
    expect(secondFactorChallenge(apiError(403, { type: SECOND_FACTOR_REQUIRED_PROBLEM_TYPE }))).toBeUndefined()
  })

  it('offers WebAuthn only with options to sign', () => {
    expect(factorChallenge({ methods: ['webauthn', 'recovery_code'] })).toEqual({ methods: ['recovery_code'], webauthn: undefined })
    expect(factorChallenge({ methods: ['webauthn'], webauthn: { nope: 1 } }).methods).toEqual([])
    expect(factorChallenge({}).methods).toEqual([])
  })

  it('recognises the reauthentication refusal', () => {
    expect(isReauthenticationRequired(apiError(403, { type: REAUTHENTICATION_REQUIRED_PROBLEM_TYPE }))).toBe(true)
    expect(isReauthenticationRequired(apiError(403))).toBe(false)
    expect(isReauthenticationRequired(apiError(401, { type: REAUTHENTICATION_REQUIRED_PROBLEM_TYPE }))).toBe(false)
  })

  it('starts with a code field when there is one', () => {
    expect(initialMethod(['webauthn', 'totp', 'recovery_code'])).toBe('totp')
    expect(initialMethod(['webauthn', 'recovery_code'])).toBe('webauthn')
    expect(initialMethod(['recovery_code'])).toBe('recovery_code')
    expect(initialMethod(['password'])).toBe('password')
    expect(initialMethod([])).toBeUndefined()
  })

  it('normalises codes', () => {
    expect(normalizeCode('totp', ' 123 456 ')).toBe('123456')
    expect(normalizeCode('recovery_code', ' ABCD-EFGH-IJKL-MNOP ')).toBe('ABCD-EFGH-IJKL-MNOP')
  })
})

function mountForm(challenge: FactorChallenge, props: Record<string, unknown> = {}) {
  return mount(AuthFactorForm, { props: { challenge, ...props }, global, attachTo: document.body })
}

describe('AuthFactorForm', () => {
  it('offers only the user\'s methods, with a one-time-code field for an authenticator app', async () => {
    const w = mountForm({ methods: ['totp', 'recovery_code'] })
    expect(w.text()).toContain('Authentication code')
    expect(w.find('[autocomplete="one-time-code"]').exists()).toBe(true)
    expect(w.find('[inputmode="numeric"]').exists()).toBe(true)
    expect(w.text()).toContain('Use a recovery code')
    expect(w.text()).not.toContain('security key')
    await flushPromises()
    expect(document.activeElement).toBe(w.find('input').element)
  })

  it('submits an authenticator code without spaces', async () => {
    const w = mountForm({ methods: ['totp'] })
    await w.find('input').setValue('123 456')
    await w.find('form').trigger('submit')
    expect(w.emitted('submit')).toEqual([[{ method: 'totp', code: '123456' }]])
  })

  it('switches to a recovery code', async () => {
    const w = mountForm({ methods: ['totp', 'recovery_code'] })
    await w.findAll('button').find(b => b.text() === 'Use a recovery code')!.trigger('click')
    expect(w.text()).toContain('Recovery code')
    expect(w.find('[autocomplete="one-time-code"]').exists()).toBe(false)
    expect(w.text()).toContain('Use your authenticator app')
    await w.find('input').setValue('ABCD-EFGH-IJKL-MNOP')
    await w.find('form').trigger('submit')
    expect(w.emitted('submit')).toEqual([[{ method: 'recovery_code', code: 'ABCD-EFGH-IJKL-MNOP' }]])
  })

  it('doesn\'t submit an empty code or while disabled', async () => {
    const w = mountForm({ methods: ['totp'] }, { disabled: true })
    await w.find('form').trigger('submit')
    await w.find('input').setValue('123456')
    await w.find('form').trigger('submit')
    expect(w.emitted('submit')).toBeUndefined()
  })

  it('submits a WebAuthn assertion from the security key button', async () => {
    const assert = vi.fn(async () => assertion)
    const w = mountForm({ methods: ['webauthn', 'recovery_code'], webauthn }, { assert })
    const button = w.findAll('button').find(b => b.text() === 'Use a security key or passkey')!
    await button.trigger('click')
    await flushPromises()
    expect(assert).toHaveBeenCalledWith(webauthn, expect.any(AbortSignal))
    expect(w.emitted('submit')).toEqual([[{ method: 'webauthn', credential: assertion }]])
  })

  it('starts the security key prompt when chosen from the alternatives', async () => {
    const assert = vi.fn(async () => assertion)
    const w = mountForm({ methods: ['totp', 'webauthn'], webauthn }, { assert })
    await w.findAll('button').find(b => b.text() === 'Use a security key or passkey')!.trigger('click')
    await flushPromises()
    expect(w.emitted('submit')).toEqual([[{ method: 'webauthn', credential: assertion }]])
  })

  it('shows why the browser couldn\'t make an assertion', async () => {
    const assert = vi.fn(async () => {
      throw new WebAuthnError('The request was cancelled or timed out.')
    })
    const w = mountForm({ methods: ['webauthn'], webauthn }, { assert })
    await w.find('button').trigger('click')
    await flushPromises()
    expect(w.find('[role="alert"]').text()).toBe('The request was cancelled or timed out.')
    expect(w.emitted('submit')).toBeUndefined()
  })

  it.each([
    ['totp', '/code', 'That code is incorrect.'],
    ['recovery_code', '/code', 'That recovery code is incorrect or was already used.'],
    ['password', '/password', 'That password is incorrect.'],
  ] as const)('words a wrong %s next to the field and clears it', async (method, field, message) => {
    const w = mountForm({ methods: [method] })
    await w.find('input').setValue('000000')
    await w.setProps({ error: wrong(field) })
    expect(w.find('[role="alert"]').text()).toContain(message)
    expect((w.find('input').element as HTMLInputElement).value).toBe('')
    expect(w.findComponent(ProblemAlert).exists()).toBe(false)
  })

  it('words a refused security key', async () => {
    const w = mountForm({ methods: ['webauthn'], webauthn })
    await w.setProps({ error: wrong('/credential') })
    expect(w.find('[role="alert"]').text()).toContain('wasn\'t accepted')
  })

  it('shows other failures as a problem', async () => {
    const w = mountForm({ methods: ['totp'] })
    await w.setProps({ error: apiError(422, { errors: [{ field: '/method', detail: 'isn\'t available for this account' }] }) })
    expect(w.findComponent(ProblemAlert).text()).toContain('isn\'t available for this account')
  })

  it('says so when no method can be used', () => {
    const w = mountForm({ methods: [] })
    expect(w.text()).toContain('can\'t be used on this dashboard')
    expect(w.find('form').exists()).toBe(false)
  })
})

function mountStep(challenge: FactorChallenge, submit: (p: FactorProof) => Promise<CurrentUser>, assert?: () => Promise<AssertionJSON>) {
  return mount(AuthLoginSecondFactor, { props: { challenge, submit, assert }, global, attachTo: document.body })
}

async function enterCode(w: ReturnType<typeof mountStep>, code: string) {
  await w.find('input').setValue(code)
  await w.find('form').trigger('submit')
  await flushPromises()
}

describe('AuthLoginSecondFactor', () => {
  it('signs in with an authenticator code', async () => {
    const submit = vi.fn(async () => user)
    const w = mountStep({ methods: ['totp', 'recovery_code'] }, submit)
    expect(w.find('h2').text()).toBe('Two-factor authentication')
    await enterCode(w, '123456')
    expect(submit).toHaveBeenCalledWith({ method: 'totp', code: '123456' })
    expect(w.emitted('done')).toEqual([[user, 'totp']])
  })

  it('signs in with a recovery code', async () => {
    const submit = vi.fn(async () => user)
    const w = mountStep({ methods: ['totp', 'recovery_code'] }, submit)
    await w.findAll('button').find(b => b.text() === 'Use a recovery code')!.trigger('click')
    await enterCode(w, 'ABCD-EFGH-IJKL-MNOP')
    expect(submit).toHaveBeenCalledWith({ method: 'recovery_code', code: 'ABCD-EFGH-IJKL-MNOP' })
    expect(w.emitted('done')).toEqual([[user, 'recovery_code']])
  })

  it('signs in with a security key', async () => {
    const submit = vi.fn(async () => user)
    const w = mountStep({ methods: ['webauthn'], webauthn }, submit, async () => assertion)
    await w.findAll('button').find(b => b.text() === 'Use a security key or passkey')!.trigger('click')
    await flushPromises()
    expect(submit).toHaveBeenCalledWith({ method: 'webauthn', credential: assertion })
    expect(w.emitted('done')).toEqual([[user, 'webauthn']])
  })

  it('stays on the step after a wrong code, and the next try can pass', async () => {
    const submit = vi.fn<(p: FactorProof) => Promise<CurrentUser>>()
      .mockRejectedValueOnce(wrong('/code'))
      .mockResolvedValueOnce(user)
    const w = mountStep({ methods: ['totp'] }, submit)
    await enterCode(w, '000000')
    expect(w.text()).toContain('That code is incorrect')
    expect(w.emitted('restart')).toBeUndefined()
    expect(w.emitted('done')).toBeUndefined()
    await enterCode(w, '123456')
    expect(w.emitted('done')).toEqual([[user, 'totp']])
  })

  it('waits out throttling with a countdown', async () => {
    vi.useFakeTimers()
    const submit = vi.fn(async () => {
      throw apiError(429, { title: 'Too Many Requests', retry_after: 2 })
    })
    const w = mountStep({ methods: ['totp'] }, submit)
    await enterCode(w, '123456')
    expect(w.text()).toContain('You can try again in 2 seconds')
    expect(w.findComponent(ProblemAlert).text()).toContain('Too Many Requests')
    expect(w.find('button[type="submit"]').attributes('disabled')).toBeDefined()
    await vi.advanceTimersByTimeAsync(1000)
    expect(w.text()).toContain('1 second.')
    await vi.advanceTimersByTimeAsync(1000)
    expect(w.text()).not.toContain('You can try again')
  })

  it('goes back to the password when the pending login expired', async () => {
    const submit = vi.fn(async () => {
      throw apiError(401)
    })
    const w = mountStep({ methods: ['totp'] }, submit)
    await enterCode(w, '123456')
    expect(w.emitted('restart')).toEqual([['expired']])
  })

  it('goes back on request', async () => {
    const w = mountStep({ methods: ['totp'] }, vi.fn())
    await w.findAll('button').find(b => b.text() === 'Back to email and password')!.trigger('click')
    expect(w.emitted('restart')).toEqual([['cancelled']])
  })
})

describe('AuthRecoveryCodesNotice', () => {
  function notice(remaining: number) {
    return mount(AuthRecoveryCodesNotice, { props: { remaining, regenerateTo: '/account' }, global, attachTo: document.body })
  }

  it('says how many codes are left and links to regenerate them', async () => {
    const w = notice(7)
    expect(w.find('[role="status"]').text()).toContain('You have 7 recovery codes left.')
    const regenerate = w.findAll('button').find(b => b.text() === 'Regenerate recovery codes')!
    expect(regenerate.attributes('to')).toBe('/account')
    await flushPromises()
    expect(document.activeElement).toBe(w.find('h2').element)
    await w.findAll('button').find(b => b.text() === 'Continue')!.trigger('click')
    expect(w.emitted('continue')).toHaveLength(1)
  })

  it('urges new codes when few are left', () => {
    expect(notice(1).text()).toContain('You have 1 recovery code left. Each code works once. Generate new ones soon')
    expect(notice(0).text()).toContain('That was your last recovery code.')
  })
})
