// Signing a browser in. Most specs aren't about signing in, so they get a
// session from the API and hand its cookie to the browser, which is fast;
// e2e/auth.spec.ts signs in through the form with signInThroughForm.
//
// Users with a second factor (#160) pass it to either: signIn takes the
// factor the API login uses, signInThroughForm a SecondFactorStep, such as
// withTotp or withSecurityKey below. The virtual WebAuthn authenticator a
// security key step needs is the `webauthn` fixture (webauthn.ts).
import { expect, type BrowserContext, type Page } from '@playwright/test'
import { login, SESSION_COOKIE, type Api, type ApiSecondFactor, type TestUser } from './api'
import type { TotpApp } from './totp'

/** Signs the browser context in as the user, with a session from the API. */
export async function signIn(
  context: BrowserContext,
  baseURL: string,
  apiURL: string,
  email: string,
  password: string,
  secondFactor?: ApiSecondFactor,
): Promise<Api> {
  const api = await login(apiURL, email, password, secondFactor)
  await context.addCookies([{
    name: SESSION_COOKIE,
    value: api.sessionToken!,
    domain: new URL(baseURL).hostname,
    path: '/',
    httpOnly: true,
    secure: true,
    sameSite: 'Lax',
  }])
  return api
}

/**
 * Passes the login's second step ("Two-factor authentication" heading,
 * then e.g. the "Authentication code" field and "Verify and sign in", or
 * "Use a security key or passkey"), for users with a second factor.
 */
export type SecondFactorStep = (page: Page) => Promise<void>

/**
 * Signs in through the login form and waits for the dashboard. Users with a
 * second factor pass `secondFactor`, which runs once the second step shows.
 */
export async function signInThroughForm(page: Page, user: Pick<TestUser, 'email' | 'password'>, secondFactor?: SecondFactorStep): Promise<void> {
  await page.getByLabel('Email').fill(user.email)
  await page.getByLabel('Password', { exact: true }).fill(user.password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  if (secondFactor) {
    await expect(page.getByRole('heading', { name: 'Two-factor authentication' })).toBeVisible()
    await secondFactor(page)
  }
  await expect(page.getByRole('button', { name: 'User menu' })).toBeVisible()
}

/** The second step with a code from the authenticator app. */
export function withTotp(app: TotpApp): SecondFactorStep {
  return async (page) => {
    await page.getByLabel('Authentication code').fill(await app.code())
    await page.getByRole('button', { name: 'Verify and sign in' }).click()
  }
}

/**
 * The second step with a recovery code, for a user whose first method is
 * another. The login then stops to say how many codes are left, which must
 * be `remaining`, and goes on.
 */
export function withRecoveryCode(code: string, remaining: number): SecondFactorStep {
  return async (page) => {
    await page.getByRole('button', { name: 'Use a recovery code' }).click()
    await page.getByLabel('Recovery code').fill(code)
    await page.getByRole('button', { name: 'Verify and sign in' }).click()
    await expect(page.getByRole('heading', { name: 'You signed in with a recovery code' })).toBeVisible()
    await expect(page.getByRole('status').filter({ hasText: `You have ${remaining} recovery codes left.` })).toBeVisible()
    await page.getByRole('button', { name: 'Continue' }).click()
  }
}

/**
 * The second step with a security key or passkey: the virtual
 * authenticator on the page's tab answers the browser's prompt.
 */
export const withSecurityKey: SecondFactorStep = async (page) => {
  await page.getByRole('button', { name: 'Use a security key or passkey' }).click()
}

/** Signs out through the user menu. */
export async function signOut(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'User menu' }).click()
  await page.getByRole('menuitem', { name: 'Sign out' }).click()
  await expect(page).toHaveURL(/\/login/)
}
