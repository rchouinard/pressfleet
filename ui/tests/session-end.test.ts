import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { computed, ref } from 'vue'
import { useReauthentication } from '../app/composables/useReauthentication'
import { ApiError } from '../app/utils/api-error'
import { loginLocation } from '../app/utils/redirect'

// useApi and useSession are written against Nuxt's auto-imports; these tests
// supply just the ones they use.
const refusal = new ApiError({ type: 'urn:pressfleet:reauthentication-required', title: 'Forbidden', status: 403 })

/** Opens the global reauthentication dialog, as a refused sensitive action does. */
function openDialog() {
  const { ask, pending } = useReauthentication()
  const answer = ask(refusal)
  expect(pending.value).toBeDefined()
  return answer
}

const states = new Map<string, { value: unknown }>()

beforeEach(() => {
  states.clear()
  vi.stubGlobal('useState', (key: string, init?: () => unknown) => {
    if (!states.has(key)) states.set(key, ref(init?.()))
    return states.get(key)
  })
  vi.stubGlobal('useSessionUser', () => (globalThis as unknown as { useState: (k: string, i: () => unknown) => unknown }).useState('session-user', () => undefined))
  vi.stubGlobal('useReauthentication', useReauthentication)
  vi.stubGlobal('computed', computed)
  vi.stubGlobal('toApiError', (e: unknown) => (e instanceof ApiError ? e : new ApiError({ type: 'about:blank', title: String(e), status: 0 })))
  vi.stubGlobal('sessionProbe', { active: 0 })
  vi.stubGlobal('loginLocation', loginLocation)
  vi.resetModules()
})

afterEach(() => {
  useReauthentication().finish(false)
  vi.unstubAllGlobals()
})

describe('an expired session (401)', () => {
  it('closes the reauthentication dialog when it sends the user to /login', async () => {
    const options: { onUnauthorized?: () => void } = {}
    const replace = vi.fn(async () => {})
    vi.stubGlobal('useApiReachable', () => ref(true))
    vi.stubGlobal('useRouter', () => ({ currentRoute: ref({ path: '/sites', fullPath: '/sites' }), replace }))
    vi.stubGlobal('enrollmentRequiredHandler', () => async () => {})
    vi.stubGlobal('createApiClient', (o: typeof options) => Object.assign(options, o))
    const { useApi, useCsrfToken } = await import('../app/composables/useApi')
    useCsrfToken().value = 'csrf'
    ;(globalThis as unknown as { useSessionUser: () => { value: unknown } }).useSessionUser().value = { id: 'u1' }
    useApi()

    const answer = openDialog()
    options.onUnauthorized!()

    expect(replace).toHaveBeenCalledWith(loginLocation('/sites', { expired: true }))
    expect(useReauthentication().pending.value).toBeUndefined()
    await expect(answer).resolves.toBe(false)
  })
})

describe('logout', () => {
  it('closes the reauthentication dialog when it goes to /login', async () => {
    const post = vi.fn(async () => ({}))
    vi.stubGlobal('useCsrfToken', () => ref<string | undefined>('csrf'))
    vi.stubGlobal('useApi', () => ({ POST: post }))
    const navigateTo = vi.fn(async () => {})
    vi.stubGlobal('navigateTo', navigateTo)
    const { useSession } = await import('../app/composables/useSession')

    const answer = openDialog()
    await useSession().logout()

    expect(navigateTo).toHaveBeenCalledWith('/login', { replace: true })
    expect(useReauthentication().pending.value).toBeUndefined()
    await expect(answer).resolves.toBe(false)
  })
})
