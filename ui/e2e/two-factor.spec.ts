// Dashboard two-factor authentication (#146, #160): setting up an
// authenticator app and signing in with its codes or a recovery code, and
// registering a security key and a passkey and signing in with each, on
// Chromium's virtual authenticator. Each test's user is its own; the
// platform's admin only reads the audit log. Forced enrollment and the
// reauthentication dialog need the policy on and are in
// two-factor-required.spec.ts.
import { login } from './fixtures/api'
import { signInThroughForm, signOut, withRecoveryCode, withSecurityKey, withTotp } from './fixtures/session'
import { TotpApp } from './fixtures/totp'
import { auditRows, auditRowsAbout, enrollTotp } from './fixtures/two-factor'
import type { AuthenticatorKind } from './fixtures/webauthn'
import { expect, test } from './fixtures'
import type { Page } from '@playwright/test'

/** The account page's two-factor section. */
function settings(page: Page) {
  return page.getByRole('region', { name: 'Two-factor authentication' })
}

test('sets up an authenticator app and signs in with its codes', async ({ asRole, adminPage, adminApi }) => {
  const { page, user, api } = await asRole('operator')
  await page.goto('/account')
  await expect(settings(page).getByText('Not set up.', { exact: false })).toBeVisible()

  await settings(page).getByRole('button', { name: 'Set up authenticator app' }).click()
  const setup = page.getByRole('dialog', { name: 'Set up an authenticator app' })
  await expect(setup.getByRole('img', { name: 'QR code for your authenticator app' })).toBeVisible()
  const secret = await setup.getByRole('textbox', { name: 'Secret key' }).inputValue()
  expect(secret).toMatch(/^[A-Z2-7]{32}$/)
  const app = new TotpApp(secret)

  // A wrong code is refused, and the app isn't on.
  await setup.getByLabel('Code from the app').fill(app.wrongCode())
  await setup.getByRole('button', { name: 'Verify and turn on' }).click()
  await expect(setup.getByText('That code is incorrect.', { exact: false })).toBeVisible()
  await setup.getByLabel('Code from the app').fill(await app.code())
  await setup.getByRole('button', { name: 'Verify and turn on' }).click()

  // The first factor comes with ten recovery codes, shown once.
  const shown = page.getByRole('dialog', { name: 'Your recovery codes' })
  const codes = await shown.getByRole('list', { name: 'Recovery codes' }).getByRole('listitem').allTextContents()
  expect(codes).toHaveLength(10)
  for (const c of codes) expect(c).toMatch(/^[A-Za-z0-9]{4}(-[A-Za-z0-9]{4}){3}$/)
  await shown.getByRole('button', { name: 'Done, I\'ve saved them' }).click()
  await expect(shown).toBeHidden()
  await expect(settings(page).getByText('On. Sign in with the 6-digit codes your app shows.')).toBeVisible()
  await expect(settings(page).getByText('You have 10 unused recovery codes.', { exact: false })).toBeVisible()
  expect(await page.content()).not.toContain(secret)

  const { data: status } = await api.client.GET('/api/v1/users/me/two-factor')
  expect(status).toMatchObject({ enabled: true, totp: { confirmed: true }, recovery_codes_remaining: 10 })

  // Signing in now takes a code: a wrong one keeps the login on its second
  // step, without asking for the password again; the right one finishes it.
  await signOut(page)
  await signInThroughForm(page, user, async (p) => {
    await p.getByLabel('Authentication code').fill(app.wrongCode())
    await p.getByRole('button', { name: 'Verify and sign in' }).click()
    await expect(p.getByText('That code is incorrect. Check your authenticator app and try again.')).toBeVisible()
    await expect(p.getByLabel('Authentication code')).toHaveValue('')
    await withTotp(app)(p)
  })
  const { data: after } = await adminApi.client.GET('/api/v1/users/{user_id}', { params: { path: { user_id: user.id } } })
  expect(after!.two_factor_enabled).toBe(true)

  // The audit log has the enrollment and both logins' steps.
  const rows = await auditRowsAbout(adminPage, user.id)
  await expect(auditRows(rows, 'self.two_factor.totp.enroll', 'Failed', { stage: 'confirm' })).toHaveCount(1)
  await expect(auditRows(rows, 'self.two_factor.totp.enroll', 'Success', { stage: 'start' })).toHaveCount(1)
  await expect(auditRows(rows, 'self.two_factor.totp.enroll', 'Success', { stage: 'confirm' })).toHaveCount(1)
  // The password step answers "second factor required", a 401.
  await expect(auditRows(rows, 'self.login', 'Failed', { second_factor: 'required' })).toHaveCount(1)
  await expect(auditRows(rows, 'self.login', 'Failed', { second_factor: 'totp' })).toHaveCount(1)
  await expect(auditRows(rows, 'self.login', 'Success', { second_factor: 'totp' })).toHaveCount(1)
})

test('signs in with a recovery code, which works only once', async ({ asRole, adminPage, env }) => {
  const { page, user, api } = await asRole('viewer')
  const { recoveryCodes } = await enrollTotp(api)
  const [code, next] = recoveryCodes
  await signOut(page)

  await page.goto('/sites')
  await signInThroughForm(page, user, withRecoveryCode(code!, 9))
  await expect(page).toHaveURL(/\/sites$/)

  // The code is spent: the API refuses it, and takes the next one.
  await expect(login(env.apiURL, user.email, user.password, async () => ({ method: 'recovery_code', code: code! }))).rejects.toThrow(/status 422/)
  const again = await login(env.apiURL, user.email, user.password, async () => ({ method: 'recovery_code', code: next! }))
  const { data: status } = await again.client.GET('/api/v1/users/me/two-factor')
  expect(status!.recovery_codes_remaining).toBe(8)

  const rows = await auditRowsAbout(adminPage, user.id)
  await expect(auditRows(rows, 'self.login', 'Success', { second_factor: 'recovery_code' })).toHaveCount(2)
  await expect(auditRows(rows, 'self.login', 'Failed', { second_factor: 'recovery_code' })).toHaveCount(1)
})

// What each kind registers as: a passkey is discoverable, verifies the
// user, and is backed up (synced); the security key is none of these.
const authenticators: { kind: AuthenticatorKind, title: string, name: string, badge: string, passkey: boolean }[] = [
  { kind: 'security-key', title: 'a security key', name: 'YubiKey 5C', badge: 'Device-bound', passkey: false },
  { kind: 'passkey', title: 'a passkey', name: 'Work laptop', badge: 'Synced', passkey: true },
]

for (const a of authenticators) {
  test(`registers ${a.title} and signs in with it`, async ({ asRole, webauthn, adminPage, env }) => {
    const { page, user, api } = await asRole('operator')
    // The authenticator lives in this tab, so the spec stays on this page.
    const authenticator = await webauthn(page, a.kind)
    await page.goto('/account')
    await expect(settings(page).getByText('You have no security keys or passkeys.')).toBeVisible()

    await settings(page).getByRole('button', { name: 'Add security key or passkey' }).click()
    const add = page.getByRole('dialog', { name: 'Add a security key or passkey' })
    await add.getByLabel('Name').fill(a.name)
    await add.getByRole('button', { name: 'Continue' }).click()
    // The virtual authenticator answers the browser's prompt; as the first
    // factor, the key comes with recovery codes.
    const shown = page.getByRole('dialog', { name: 'Your recovery codes' })
    await expect(shown.getByRole('list', { name: 'Recovery codes' }).getByRole('listitem')).toHaveCount(10)
    await shown.getByRole('button', { name: 'Done, I\'ve saved them' }).click()
    await expect(add).toBeHidden()

    const listed = settings(page).getByRole('list', { name: 'Your security keys and passkeys' }).getByRole('listitem')
    await expect(listed).toHaveCount(1)
    await expect(listed).toContainText(a.name)
    await expect(listed).toContainText(a.badge)

    // The authenticator holds the credential, discoverable only for the
    // passkey, and the platform has it under the dashboard domain.
    const held = await authenticator.credentials()
    expect(held).toHaveLength(1)
    expect(held[0]).toMatchObject({ isResidentCredential: a.passkey, rpId: new URL(env.baseURL).hostname })
    const { data: status } = await api.client.GET('/api/v1/users/me/two-factor')
    expect(status).toMatchObject({ enabled: true, rp_id: new URL(env.baseURL).hostname, totp: { confirmed: false } })
    const [credential] = status!.webauthn.credentials
    expect(credential).toMatchObject({ name: a.name, rp_id: new URL(env.baseURL).hostname, user_verified: a.passkey, backup_eligible: a.passkey, backup_state: a.passkey })

    // Signing in now takes the key: the second step offers it alone.
    await signOut(page)
    await signInThroughForm(page, user, async (p) => {
      await expect(p.getByLabel('Authentication code')).toHaveCount(0)
      await withSecurityKey(p)
    })
    const [used] = await authenticator.credentials()
    expect(used!.signCount).toBeGreaterThan(held[0]!.signCount)
    await page.goto('/account')
    await expect(listed).not.toContainText('Last used never')

    const rows = await auditRowsAbout(adminPage, user.id)
    await expect(auditRows(rows, 'self.two_factor.webauthn.register', 'Success')).not.toHaveCount(0)
    await expect(auditRows(rows, 'self.login', 'Success', { second_factor: 'webauthn', webauthn_credential_id: credential!.id })).toHaveCount(1)
  })
}
