// Helpers for Nuxt UI controls whose roles take more than one step.
import { readFile } from 'node:fs/promises'
import { expect, type Locator, type Page } from '@playwright/test'

/** Picks an option of a select (a combobox that opens a listbox). */
export async function choose(page: Page, field: string, option: string | RegExp): Promise<void> {
  await page.getByRole('combobox', { name: field }).click()
  await page.getByRole('option', { name: option, exact: typeof option === 'string' }).click()
}

/** A toast by its title. */
export function toast(page: Page, title: string): Locator {
  return page.getByText(title, { exact: true })
}

/** Waits until the page is /login, asked to return to `redirect`. */
export async function expectLoginPage(page: Page, redirect: string, expired = false): Promise<void> {
  await expect(page).toHaveURL(url =>
    url.pathname === '/login' && url.searchParams.get('redirect') === redirect && (url.searchParams.get('expired') === '1') === expired)
}

/**
 * Clicks a download link and returns the file the browser saved, so a
 * spec can check what the user got.
 */
export async function download(page: Page, link: Locator): Promise<Buffer> {
  const [saved] = await Promise.all([page.waitForEvent('download'), link.click()])
  const failure = await saved.failure()
  if (failure) throw new Error(`download of ${saved.suggestedFilename()} failed: ${failure}`)
  return readFile(await saved.path())
}
