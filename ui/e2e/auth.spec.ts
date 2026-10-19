// Signing in and out, an ended session, and changing your own password
// (#82). The session cookie set through Traefik must be Secure and
// HttpOnly, as it ships.
import { apiError, createUser, login, randomPassword, SESSION_COOKIE } from './fixtures/api'
import { signInThroughForm, signOut } from './fixtures/session'
import { expectLoginPage, toast } from './fixtures/ui'
import { expect, test } from './fixtures'

test('signs in through the form and out again', async ({ page, adminApi, env }) => {
  const user = await createUser(adminApi, 'viewer')

  await page.goto('/sites')
  await expectLoginPage(page, '/sites')

  // A wrong password is refused, and the field is cleared.
  await page.getByLabel('Email').fill(user.email)
  await page.getByLabel('Password', { exact: true }).fill('not the password')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('alert')).toBeVisible()
  await expect(page.getByLabel('Password', { exact: true })).toHaveValue('')

  await signInThroughForm(page, user)
  await expect(page).toHaveURL(/\/sites$/)
  await expect(page.getByRole('heading', { level: 1, name: 'Sites' })).toBeVisible()

  const cookie = (await page.context().cookies()).find(c => c.name === SESSION_COOKIE)
  expect(cookie).toMatchObject({ secure: true, httpOnly: true, sameSite: 'Lax' })
  const { data: me } = await adminApi.client.GET('/api/v1/users/{user_id}', { params: { path: { user_id: user.id } } })
  expect(me!.last_login_at).toBeTruthy()

  await signOut(page)
  // The server ended the session: its cookie no longer works.
  const ended = await fetch(new URL('/api/v1/auth/me', env.apiURL), { headers: { Cookie: `${SESSION_COOKIE}=${cookie!.value}` } })
  expect(ended.status).toBe(401)
  await page.goto('/sites')
  await expect(page).toHaveURL(/\/login/)
})

test('an ended session sends the user to sign in, and back', async ({ asRole, env }) => {
  const { page, user, api } = await asRole('viewer')
  await page.goto('/account')
  await expect(page.getByRole('heading', { level: 1, name: 'Account & tokens' })).toBeVisible()

  // End the browser's session on the server, as an expiry would, while the
  // browser keeps its cookie.
  await api.client.POST('/api/v1/auth/logout')
  expect((await page.context().cookies()).some(c => c.name === SESSION_COOKIE)).toBe(true)

  // The next API call gets 401 and the dashboard asks to sign in again.
  await page.getByRole('link', { name: 'Jobs' }).click()
  await expectLoginPage(page, '/jobs', true)
  await expect(page.getByRole('status').filter({ hasText: 'Your session ended' })).toBeVisible()

  await signInThroughForm(page, user)
  await expect(page).toHaveURL(new URL('/jobs', env.baseURL).href)
})

test('changes your own password', async ({ asRole, env }) => {
  const { page, user } = await asRole('operator')
  // Another session and a token of the user's, which the change ends.
  const other = await login(env.apiURL, user.email, user.password)
  await page.goto('/account')

  const password = page.getByRole('region', { name: 'Password' })
  const next = randomPassword()
  // A wrong current password is refused by the server, next to its field.
  await password.getByLabel('Current password').fill('not the password')
  await password.getByLabel('New password', { exact: true }).fill(next)
  await password.getByLabel('Confirm new password').fill(next)
  await password.getByRole('button', { name: 'Change password' }).click()
  await expect(password.getByText(/current password/i).last()).toBeVisible()

  await password.getByLabel('Current password').fill(user.password)
  await password.getByLabel('New password', { exact: true }).fill(next)
  await password.getByLabel('Confirm new password').fill(next)
  await password.getByRole('button', { name: 'Change password' }).click()
  await expect(toast(page, 'Password changed')).toBeVisible()

  // The new password works and the old one doesn't; the other session is
  // signed out, and this one stays signed in.
  await expect(login(env.apiURL, user.email, user.password)).rejects.toThrow(/status 401/)
  await login(env.apiURL, user.email, next)
  expect((await apiError(other.client.GET('/api/v1/auth/me'))).status).toBe(401)
  await page.reload()
  await expect(page.getByRole('heading', { level: 1, name: 'Account & tokens' })).toBeVisible()
})
