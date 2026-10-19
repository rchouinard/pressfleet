// Signing a browser in. Most specs aren't about signing in, so they get a
// session from the API and hand its cookie to the browser, which is fast;
// e2e/auth.spec.ts signs in through the form with signInThroughForm.
//
// Two-factor authentication (#160) adds a step after the password. It
// belongs here, next to signInThroughForm, with the virtual WebAuthn
// authenticator as a fixture of its own (CDP: WebAuthn.enable and
// WebAuthn.addVirtualAuthenticator on a page's context), so specs keep
// calling the same helpers.
import { expect, type BrowserContext, type Page } from '@playwright/test'
import { login, SESSION_COOKIE, type Api, type TestUser } from './api'

/** Signs the browser context in as the user, with a session from the API. */
export async function signIn(context: BrowserContext, baseURL: string, apiURL: string, email: string, password: string): Promise<Api> {
  const api = await login(apiURL, email, password)
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

/** Signs out through the user menu. */
export async function signOut(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'User menu' }).click()
  await page.getByRole('menuitem', { name: 'Sign out' }).click()
  await expect(page).toHaveURL(/\/login/)
}
