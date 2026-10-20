import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it, vi } from 'vitest'
import { createApiClient } from '../app/utils/api-client'
import { ApiError } from '../app/utils/api-error'
import {
  ENROLLMENT_PAGE,
  ENROLLMENT_REQUIRED_PROBLEM_TYPE,
  enrollmentRedirect,
  enrollmentRequiredHandler,
  isLastSecondFactor,
  LAST_SECOND_FACTOR_PROBLEM_TYPE,
  RECOVERY_CODES_PAGE,
  REAUTHENTICATION_REQUIRED_PROBLEM_TYPE,
} from '../app/utils/two-factor'
import {
  credentialState,
  recoveryCodesHref,
  recoveryCodesText,
  twoFactorClient,
  validateCredentialName,
  type WebAuthnCredential,
} from '../app/utils/two-factor-settings'

const base = 'http://pressfleet.test'
const src = (path: string) => readFileSync(resolve(process.cwd(), path), 'utf8')

const to = (fullPath: string) => {
  const url = new URL(fullPath, base)
  return { path: url.pathname, fullPath, query: Object.fromEntries(url.searchParams) }
}

describe('forced enrollment middleware', () => {
  const restricted = { enrollment_required: true }
  const free = { enrollment_required: false }

  it('sends a restricted session to the setup page, keeping where it was going', () => {
    expect(enrollmentRedirect(restricted, to('/sites/abc?tab=logs'))).toEqual({ path: ENROLLMENT_PAGE, query: { redirect: '/sites/abc?tab=logs' } })
    expect(enrollmentRedirect(restricted, to('/account'))).toEqual({ path: ENROLLMENT_PAGE, query: { redirect: '/account' } })
    expect(enrollmentRedirect(restricted, to('/'))).toEqual({ path: ENROLLMENT_PAGE, query: {} })
  })

  it('lets a restricted session stay on the setup page', () => {
    expect(enrollmentRedirect(restricted, to(`${ENROLLMENT_PAGE}?redirect=/jobs`))).toBeUndefined()
  })

  it('lets everyone else through, and sends them on from the setup page', () => {
    expect(enrollmentRedirect(free, to('/sites'))).toBeUndefined()
    expect(enrollmentRedirect(free, to(`${ENROLLMENT_PAGE}?redirect=/jobs`))).toBe('/jobs')
    expect(enrollmentRedirect(free, to(ENROLLMENT_PAGE))).toBe('/account')
    // The redirect is validated like the login's.
    expect(enrollmentRedirect(free, to(`${ENROLLMENT_PAGE}?redirect=//evil.example`))).toBe('/')
  })

  it('runs in the route guard for signed-in users, before role gating', () => {
    const guard = src('app/middleware/auth.global.ts')
    const signedOut = guard.indexOf('if (!user) return navigateTo(loginLocation')
    const enroll = guard.indexOf('enrollmentRedirect(user, to)')
    const roles = guard.indexOf('const requires =')
    expect(signedOut).toBeGreaterThan(0)
    expect(enroll).toBeGreaterThan(signedOut)
    expect(roles).toBeGreaterThan(enroll)
  })

  it('has a setup page that reuses the account page\'s settings and offers sign-out', () => {
    const page = src(`app/pages${ENROLLMENT_PAGE}.vue`)
    expect(page).toContain('<TwoFactorSettings')
    expect(page).toContain('logout()')
    expect(page).not.toContain('requires:')
    expect(src('app/pages/account.vue')).toContain('<TwoFactorSettings')
  })

  it('points the recovery-code login notice at the two-factor section', () => {
    expect(RECOVERY_CODES_PAGE).toBe('/account#two-factor')
    expect(src('app/pages/account.vue')).toContain('id="two-factor"')
  })
})

describe('immediate redirect to enrollment', () => {
  const refusal = () => problem(403, { type: ENROLLMENT_REQUIRED_PROBLEM_TYPE, detail: 'Set up a second factor first.' })

  // A router whose replace moves it, as vue-router's does.
  function fakeRouter(fullPath: string) {
    const router = { current: to(fullPath), replace: vi.fn(async (loc: { path: string, query: Record<string, string> } | string) => {
      router.current = typeof loc === 'string' ? to(loc) : to(loc.path + (loc.query.redirect ? `?redirect=${encodeURIComponent(loc.query.redirect)}` : ''))
    }) }
    return router
  }

  function restrictedMidUse(fullPath: string, refresh = vi.fn(async () => ({ enrollment_required: true }))) {
    const router = fakeRouter(fullPath)
    const onEnrollmentRequired = vi.fn(enrollmentRequiredHandler({ route: () => router.current, refresh, replace: loc => router.replace(loc) }))
    const api = createApiClient({ baseUrl: base, fetch: async () => refusal(), onEnrollmentRequired })
    return { api, router, refresh, onEnrollmentRequired }
  }

  it('refreshes the session and goes to the setup page once, keeping the page, when requests fail together', async () => {
    const { api, router, refresh, onEnrollmentRequired } = restrictedMidUse('/sites/abc?tab=logs')
    const results = await Promise.allSettled([
      api.GET('/api/v1/sites'),
      api.GET('/api/v1/jobs'),
      api.POST('/api/v1/sites/{site_id}/start', { params: { path: { site_id: 'abc' } } }),
    ])
    // Each request still fails with its 403.
    for (const r of results) expect(r).toMatchObject({ status: 'rejected', reason: { status: 403, type: ENROLLMENT_REQUIRED_PROBLEM_TYPE } })
    await vi.waitFor(() => expect(router.replace).toHaveBeenCalledTimes(1))
    expect(onEnrollmentRequired).toHaveBeenCalledTimes(1)
    expect(refresh).toHaveBeenCalledTimes(1)
    expect(router.replace).toHaveBeenCalledWith({ path: ENROLLMENT_PAGE, query: { redirect: '/sites/abc?tab=logs' } })

    // Late refusals find the user already there.
    await new Promise(resolve => setTimeout(resolve))
    await expect(api.GET('/api/v1/sites')).rejects.toMatchObject({ status: 403 })
    await vi.waitFor(() => expect(onEnrollmentRequired).toHaveBeenCalledTimes(2))
    expect(router.replace).toHaveBeenCalledTimes(1)
    expect(refresh).toHaveBeenCalledTimes(1)
  })

  it('stays put when the reloaded session needn\'t enroll, or is signed out', async () => {
    for (const session of [{ enrollment_required: false }, null]) {
      const { api, router, refresh } = restrictedMidUse('/sites', vi.fn(async () => session))
      await expect(api.GET('/api/v1/sites')).rejects.toMatchObject({ status: 403 })
      await vi.waitFor(() => expect(refresh).toHaveBeenCalledTimes(1))
      expect(router.replace).not.toHaveBeenCalled()
    }
  })

  it('ignores other 403s, including reauthentication', async () => {
    for (const body of [{ type: 'about:blank' }, { type: REAUTHENTICATION_REQUIRED_PROBLEM_TYPE }]) {
      const onEnrollmentRequired = vi.fn()
      const api = createApiClient({ baseUrl: base, fetch: async () => problem(403, body), onEnrollmentRequired })
      await expect(api.GET('/api/v1/sites')).rejects.toMatchObject({ status: 403 })
      expect(onEnrollmentRequired).not.toHaveBeenCalled()
    }
  })

  it('is wired in useApi, beside the route guard that stays', () => {
    const useApi = src('app/composables/useApi.ts')
    expect(useApi).toContain('onEnrollmentRequired: enrollmentRequiredHandler(')
    expect(useApi).toContain('/api/v1/auth/me')
    expect(src('app/middleware/auth.global.ts')).toContain('enrollmentRedirect(user, to)')
  })
})

describe('two-factor secrets stay out of storage, URLs and logs', () => {
  const holders = [
    'app/components/two-factor/TwoFactorSettings.vue',
    'app/components/two-factor/TwoFactorQrCode.vue',
    'app/components/two-factor/TwoFactorRecoveryCodes.vue',
    'app/utils/two-factor-settings.ts',
  ]

  it('never touches web storage or the console', () => {
    for (const file of [...holders, 'app/pages/two-factor-setup.vue', 'app/pages/account.vue']) {
      expect(src(file), file).not.toMatch(/localStorage|sessionStorage|useCookie|console\./)
    }
  })

  it('never navigates from where the secrets are held', () => {
    for (const file of holders) expect(src(file), file).not.toMatch(/navigateTo|useRouter|router\./)
  })
})

describe('isLastSecondFactor', () => {
  it('matches only the last-factor 409', () => {
    expect(isLastSecondFactor(new ApiError({ type: LAST_SECOND_FACTOR_PROBLEM_TYPE, title: 'Conflict', status: 409 }))).toBe(true)
    expect(isLastSecondFactor(new ApiError({ type: 'about:blank', title: 'Conflict', status: 409 }))).toBe(false)
  })
})

const cred = (over: Partial<WebAuthnCredential> = {}): WebAuthnCredential => ({
  id: 'c1',
  name: 'Key',
  rp_id: 'dash.example.com',
  aaguid: '',
  transports: [],
  user_verified: true,
  backup_eligible: false,
  backup_state: false,
  sign_count: 0,
  created_at: '2026-09-01T00:00:00Z',
  last_used_at: null,
  ...over,
})

describe('credentialState', () => {
  it('labels synced and device-bound credentials with an icon, and says when they were used', () => {
    expect(credentialState(cred(), 'dash.example.com')).toMatchObject({ usable: true, kind: 'Device-bound', note: 'Last used never' })
    const synced = credentialState(cred({ backup_eligible: true, last_used_at: '2026-10-01T10:00:00Z' }), 'dash.example.com')
    expect(synced.kind).toBe('Synced')
    expect(synced.icon).toMatch(/^i-lucide-/)
    expect(synced.note).toMatch(/^Last used /)
    expect(synced.note).not.toContain('never')
  })

  it('marks credentials of another dashboard domain as unusable', () => {
    expect(credentialState(cred({ rp_id: 'old.example.com' }), 'dash.example.com')).toMatchObject({
      usable: false,
      note: 'Registered for old.example.com, can\'t be used',
    })
  })

  it('marks every credential unusable without a relying party ID', () => {
    expect(credentialState(cred(), null).usable).toBe(false)
  })
})


describe('validateCredentialName', () => {
  it('needs a name of at most 100 characters', () => {
    expect(validateCredentialName('  ')).toMatch(/Enter a name/)
    expect(validateCredentialName('x'.repeat(101))).toMatch(/at most 100/)
    expect(validateCredentialName(' YubiKey ')).toBeUndefined()
  })
})

describe('recovery codes file', () => {
  it('names the account and lists the codes', () => {
    const text = recoveryCodesText(['AAAA-BBBB', 'CCCC-DDDD'], { email: 'ann@example.com', host: 'dash.example.com' }, new Date('2026-10-20T00:00:00Z'))
    expect(text).toContain('ann@example.com (dash.example.com)')
    expect(text).toContain('2026-10-20T00:00:00.000Z')
    expect(text.split('\n')).toEqual(expect.arrayContaining(['AAAA-BBBB', 'CCCC-DDDD']))
    const href = recoveryCodesHref(text)
    expect(href.startsWith('data:text/plain;charset=utf-8,')).toBe(true)
    expect(decodeURIComponent(href.slice(href.indexOf(',') + 1))).toBe(text)
  })
})

function problem(status: number, body: Record<string, unknown>) {
  return new Response(JSON.stringify({ status, title: 'Problem', ...body }), { status, headers: { 'Content-Type': 'application/problem+json' } })
}

describe('twoFactorClient', () => {
  function server(...responses: (() => Response)[]) {
    const seen: { method: string, path: string, body: string }[] = []
    const fetch = vi.fn(async (req: Request) => {
      seen.push({ method: req.method, path: new URL(req.url).pathname, body: await req.clone().text() })
      const next = responses.shift()
      if (!next) throw new Error('unexpected request')
      return next()
    })
    return { fetch, seen }
  }

  it('calls the documented endpoints', async () => {
    const { fetch, seen } = server(
      () => Response.json({ enabled: true, rp_id: 'dash.example.com', totp: { confirmed: true, confirmed_at: '2026-09-01T00:00:00Z', pending_expires_at: null }, webauthn: { credentials: [cred()], usable: 1 }, recovery_codes_remaining: 9 }),
      () => Response.json({ secret: 'S', uri: 'otpauth://totp/x', expires_at: '2026-10-20T00:15:00Z' }),
      () => Response.json({ recovery_codes: ['A'] }),
      () => new Response(null, { status: 204 }),
      () => Response.json({ challenge: 'AQID' }),
      () => Response.json({ credential: cred({ name: 'New' }) }, { status: 201 }),
      () => Response.json(cred({ name: 'Renamed' })),
      () => new Response(null, { status: 204 }),
      () => Response.json({ codes: ['B'] }),
    )
    const c = twoFactorClient(createApiClient({ baseUrl: base, fetch, getCsrfToken: () => 'csrf' }))
    const status = await c.status()
    expect(status.webauthn.credentials).toHaveLength(1)
    expect(status.rp_id).toBe('dash.example.com')
    expect((await c.startTotp()).secret).toBe('S')
    expect(await c.confirmTotp('123456')).toEqual(['A'])
    await c.removeTotp()
    expect((await c.webAuthnOptions()).challenge).toBe('AQID')
    const reg = await c.registerCredential('New', { id: 'x', rawId: 'x', type: 'public-key', clientExtensionResults: {}, response: { clientDataJSON: 'e30', attestationObject: 'AA' } })
    expect(reg.credential.name).toBe('New')
    expect(reg.recoveryCodes).toBeUndefined()
    expect((await c.renameCredential('c1', 'Renamed')).name).toBe('Renamed')
    await c.removeCredential('c1')
    expect(await c.regenerateRecoveryCodes()).toEqual(['B'])

    const me = '/api/v1/users/me/two-factor'
    expect(seen.map(s => `${s.method} ${s.path}`)).toEqual([
      `GET ${me}`,
      `POST ${me}/totp`,
      `POST ${me}/totp/confirm`,
      `DELETE ${me}/totp`,
      `POST ${me}/webauthn/options`,
      `POST ${me}/webauthn`,
      `PATCH ${me}/webauthn/c1`,
      `DELETE ${me}/webauthn/c1`,
      `POST ${me}/recovery-codes`,
    ])
    expect(JSON.parse(seen[2]!.body)).toEqual({ code: '123456' })
    expect(JSON.parse(seen[5]!.body)).toMatchObject({ name: 'New', credential: { id: 'x' } })
    expect(JSON.parse(seen[6]!.body)).toEqual({ name: 'Renamed' })
  })

  it('reauthenticates a stale session in the middle of enrolling and sends the same registration again', async () => {
    const credential = { id: 'x', rawId: 'x', type: 'public-key', clientExtensionResults: {}, response: { clientDataJSON: 'e30', attestationObject: 'AA' } }
    const { fetch, seen } = server(
      () => Response.json({ challenge: 'AQID' }),
      () => problem(403, { type: REAUTHENTICATION_REQUIRED_PROBLEM_TYPE }),
      () => Response.json({ credential: cred({ name: 'Key' }), recovery_codes: ['A', 'B'] }, { status: 201 }),
    )
    const onReauthenticationRequired = vi.fn(async () => true)
    const c = twoFactorClient(createApiClient({ baseUrl: base, fetch, getCsrfToken: () => 'csrf', onReauthenticationRequired }))
    await c.webAuthnOptions()
    const reg = await c.registerCredential('Key', credential)
    expect(onReauthenticationRequired).toHaveBeenCalledTimes(1)
    expect(reg.recoveryCodes).toEqual(['A', 'B'])
    expect(seen[1]!.body).toBe(seen[2]!.body)
  })

  it('surfaces the refusal when the user cancels the reauthentication', async () => {
    const { fetch } = server(() => problem(403, { type: REAUTHENTICATION_REQUIRED_PROBLEM_TYPE }))
    const c = twoFactorClient(createApiClient({ baseUrl: base, fetch, onReauthenticationRequired: async () => false }))
    await expect(c.confirmTotp('123456')).rejects.toMatchObject({ status: 403, type: REAUTHENTICATION_REQUIRED_PROBLEM_TYPE })
  })
})
