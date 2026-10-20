// Two-factor setup through the API, for specs whose operation under test
// comes after it, and the audit log's view of a user's events.
import { expect, type Locator, type Page } from '@playwright/test'
import type { Api } from './api'
import { TotpApp } from './totp'

/**
 * Sets up an authenticator app for the API client's user and returns it,
 * with the recovery codes of a first factor (none otherwise).
 */
export async function enrollTotp(api: Api): Promise<{ app: TotpApp, recoveryCodes: string[] }> {
  const { data: started } = await api.client.POST('/api/v1/users/me/two-factor/totp')
  const app = new TotpApp(started!.secret)
  const { data: confirmed } = await api.client.POST('/api/v1/users/me/two-factor/totp/confirm', { body: { code: await app.code() } })
  return { app, recoveryCodes: confirmed?.recovery_codes ?? [] }
}

/**
 * Opens the audit log on `page` (an Admin's), filtered to events whose
 * target is the user, and returns their rows, newest first. Each row's
 * text includes its details (second_factor, method, stage).
 */
export async function auditRowsAbout(page: Page, userId: string): Promise<Locator> {
  await page.goto('/audit')
  await expect(page.getByRole('heading', { level: 1, name: 'Audit log' })).toBeVisible()
  await page.getByLabel('Target ID').fill(userId)
  await page.getByRole('button', { name: 'Apply filters' }).click()
  return page.getByRole('region', { name: 'Audit events, newest first' }).getByRole('row').filter({ has: page.getByRole('cell') })
}

/** The rows of an action with a result and every one of `details` (key and value). */
export function auditRows(rows: Locator, action: string, result: 'Success' | 'Failed' | 'Denied', details: Record<string, string> = {}): Locator {
  const escape = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const page = rows.page()
  let matching = rows
    .filter({ has: page.getByRole('cell', { name: action, exact: true }) })
    .filter({ has: page.getByText(result, { exact: true }) })
  for (const [key, value] of Object.entries(details)) matching = matching.filter({ hasText: new RegExp(`${escape(key)}\\s*${escape(value)}`) })
  return matching
}
