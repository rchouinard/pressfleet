// Forced enrollment and the reauthentication dialog (#146, #156, #158,
// #160). They need two_factor.required covering admins and a stale
// session, so they run in the suite's second run only, after the control
// plane restarts with two_factor.required: admins and the shortest
// reauthentication max age (docs/testing.md). The platform's admin can't
// create users then (its sessions are restricted to enrolling), so each
// test creates its own with the CLI, through the harness.
import { login, SESSION_COOKIE, unique, withToken, type Api } from './fixtures/api'
import { signIn, signInThroughForm } from './fixtures/session'
import { TotpApp } from './fixtures/totp'
import { auditRows, auditRowsAbout, enrollTotp } from './fixtures/two-factor'
import { expectLoginPage } from './fixtures/ui'
import { expect, test } from './fixtures'
import type { Page } from '@playwright/test'

/** The problem type of a request refused until the session enrolls (docs/configuration.md). */
const ENROLLMENT_REQUIRED = 'https://github.com/rchouinard/pressfleet/blob/main/docs/configuration.md#two-factor-enrollment-required'

/** GETs an API path with the page's session cookie, as the dashboard would. */
async function getWithPageSession(page: Page, apiURL: string, path: string): Promise<Response> {
  const cookie = (await page.context().cookies()).find(c => c.name === SESSION_COOKIE)
  return fetch(new URL(path, apiURL), { headers: { Cookie: `${SESSION_COOKIE}=${cookie?.value}` } })
}

/**
 * The URL check for the enrollment page, sent there on the way to `redirect`.
 */
function enrollmentPage(redirect: string) {
  return (url: URL) => url.pathname === '/two-factor-setup' && url.searchParams.get('redirect') === redirect
}

test.beforeAll(({ env }) => {
  // These specs only make sense with the policy on; a mistaken run says so
  // rather than failing on the first assertion.
  expect(env.twoFactorRequired, 'two-factor-required.spec.ts runs in the second run only (docs/testing.md)').toBe('admins')
})

test('an admin without a second factor can only enroll one, then goes on', async ({ page, harness, env }) => {
  const user = await harness.createUser('admin')

  await page.goto('/users')
  await expectLoginPage(page, '/users')
  // The password alone signs in, to a session that may only enroll.
  await page.getByLabel('Email').fill(user.email)
  await page.getByLabel('Password', { exact: true }).fill(user.password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(enrollmentPage('/users'))
  await expect(page.getByRole('heading', { level: 1, name: 'Set up two-factor authentication' })).toBeVisible()
  await expect(page.getByText(`Signed in as ${user.email}.`)).toBeVisible()

  // Every other page leads back here, and the API refuses everything but
  // viewing the user, signing out, and enrolling.
  await page.goto('/sites')
  await expect(page).toHaveURL(enrollmentPage('/sites'))
  const me = await (await getWithPageSession(page, env.apiURL, '/api/v1/auth/me')).json() as { id: string, two_factor_required: boolean, enrollment_required: boolean }
  expect(me).toMatchObject({ two_factor_required: true, enrollment_required: true })
  const refused = await getWithPageSession(page, env.apiURL, '/api/v1/sites')
  expect(refused.status).toBe(403)
  expect(((await refused.json()) as { type: string }).type).toBe(ENROLLMENT_REQUIRED)

  // Enrolling an authenticator app on the page.
  await page.getByRole('button', { name: 'Set up authenticator app' }).click()
  const setup = page.getByRole('dialog', { name: 'Set up an authenticator app' })
  const app = new TotpApp(await setup.getByRole('textbox', { name: 'Secret key' }).inputValue())
  await setup.getByLabel('Code from the app').fill(await app.code())
  await setup.getByRole('button', { name: 'Verify and turn on' }).click()
  // The page waits while the recovery codes are on screen...
  const shown = page.getByRole('dialog', { name: 'Your recovery codes' })
  await expect(shown.getByRole('list', { name: 'Recovery codes' }).getByRole('listitem')).toHaveCount(10)
  await expect(page).toHaveURL(enrollmentPage('/sites'))
  // ...and goes on to the page the user wanted once they are put away.
  await shown.getByRole('button', { name: 'Done, I\'ve saved them' }).click()
  await expect(page).toHaveURL(/\/sites$/)
  await expect(page.getByRole('heading', { level: 1, name: 'Sites' })).toBeVisible()
  expect((await getWithPageSession(page, env.apiURL, '/api/v1/sites')).status).toBe(200)

  // Now an admin with a factor, the user reads their own audit trail: the
  // enrollment, and the request refused before it.
  const rows = await auditRowsAbout(page, me.id)
  await expect(auditRows(rows, 'self.two_factor.totp.enroll', 'Success', { stage: 'start' })).toHaveCount(1)
  await expect(auditRows(rows, 'self.two_factor.totp.enroll', 'Success', { stage: 'confirm' })).toHaveCount(1)
  await expect(auditRows(rows, 'self.login', 'Success')).toHaveCount(1)
  const api = await login(env.apiURL, user.email, user.password, async () => ({ method: 'totp', code: await app.code() }))
  const { data: denied } = await api.client.GET('/api/v1/audit-events', { params: { query: { actor_user_id: me.id } } })
  expect(denied!.items.filter(e => e.result === 'denied').map(e => e.action)).toContain('site.view')
})

test('the policy doesn\'t cover a viewer, who goes straight on', async ({ page, harness, env }) => {
  const user = await harness.createUser('viewer')
  await page.goto('/sites')
  await signInThroughForm(page, user)
  await expect(page).toHaveURL(/\/sites$/)
  const me = await (await getWithPageSession(page, env.apiURL, '/api/v1/auth/me')).json() as { two_factor_required: boolean, enrollment_required: boolean }
  expect(me).toMatchObject({ two_factor_required: false, enrollment_required: false })
})

test('a sensitive action from a stale session asks to confirm it\'s you; cancelling changes nothing', async ({ page, harness, env }) => {
  // An admin with an authenticator app, signed in on the page.
  const user = await harness.createUser('admin')
  const enrolling = await login(env.apiURL, user.email, user.password)
  const { app } = await enrollTotp(enrolling)
  await enrolling.client.POST('/api/v1/auth/logout')
  const api = await signIn(page.context(), env.baseURL, env.apiURL, user.email, user.password, async () => ({ method: 'totp', code: await app.code() }))
  const { data: me } = await api.client.GET('/api/v1/auth/me')
  await page.goto('/account')
  await expect(page.getByRole('heading', { level: 1, name: 'Account & tokens' })).toBeVisible()

  // The session's check (the login) goes stale after the max age, a
  // minute in this run. This is the one wait in the suite on the clock: the
  // server decides staleness by it, and nothing on the page shows it.
  await waitUntil(Date.parse(me!.verified_at!) + Number(env.reauthenticationMaxAgeSeconds) * 1000 + 1000)

  const name = unique('Deploys')
  await page.getByRole('button', { name: 'Create token' }).click()
  const form = page.getByRole('dialog', { name: 'Create API token' })
  await form.getByLabel('Name').fill(name)
  await form.getByRole('button', { name: 'Create token' }).click()

  // Cancelling: the action fails as it was refused, and nothing is created.
  const confirm = page.getByRole('dialog', { name: 'Confirm it\'s you' })
  await expect(confirm.getByText('Use your second factor to continue.', { exact: false })).toBeVisible()
  await expect(confirm.getByLabel('Authentication code')).toBeVisible()
  await confirm.getByRole('button', { name: 'Cancel' }).click()
  await expect(confirm).toBeHidden()
  await expect(form.getByText('reauthenticate, then try again', { exact: false })).toBeVisible()
  expect(await tokenNames(api)).not.toContain(name)
  const { data: still } = await api.client.GET('/api/v1/auth/me')
  expect(still!.verified_at).toBe(me!.verified_at)

  // Trying again: a wrong code stays in the dialog, the right one
  // completes the action.
  await form.getByRole('button', { name: 'Create token' }).click()
  await confirm.getByLabel('Authentication code').fill(app.wrongCode())
  await confirm.getByRole('button', { name: 'Confirm' }).click()
  await expect(confirm.getByText('That code is incorrect. Check your authenticator app and try again.')).toBeVisible()
  await confirm.getByLabel('Authentication code').fill(await app.code())
  await confirm.getByRole('button', { name: 'Confirm' }).click()
  await expect(confirm).toBeHidden()
  const secret = page.getByRole('dialog', { name: 'Your new API token' }).getByRole('textbox', { name: 'API token' })
  await expect(secret).toHaveValue(/\S{20,}/)
  const { data: viaToken } = await withToken(env.apiURL, await secret.inputValue()).GET('/api/v1/auth/me')
  expect(viaToken).toMatchObject({ email: user.email, auth_method: 'token' })
  expect(await tokenNames(api)).toContain(name)
  const { data: verified } = await api.client.GET('/api/v1/auth/me')
  expect(Date.parse(verified!.verified_at!)).toBeGreaterThan(Date.parse(me!.verified_at!))

  // The audit log has the refusals, the wrong code, and the check that
  // passed.
  await page.getByRole('button', { name: 'Done, I\'ve copied it' }).click()
  const rows = await auditRowsAbout(page, me!.id)
  await expect(auditRows(rows, 'self.reauthenticate', 'Failed', { method: 'totp' })).toHaveCount(1)
  await expect(auditRows(rows, 'self.reauthenticate', 'Success', { method: 'totp' })).toHaveCount(1)
  const { data: events } = await api.client.GET('/api/v1/audit-events', { params: { query: { actor_user_id: me!.id, action: 'self.tokens.create' } } })
  expect(events!.items.map(e => e.result).sort()).toEqual(['denied', 'denied', 'success'])
})

/** The names of the user's API tokens. */
async function tokenNames(api: Api): Promise<string[]> {
  const { data } = await api.client.GET('/api/v1/auth/tokens')
  return data!.items.map(t => t.name)
}

/** Resolves at a time, by the clock the control plane shares with the host. */
function waitUntil(time: number): Promise<void> {
  return new Promise(resolve => setTimeout(resolve, Math.max(0, time - Date.now())))
}
