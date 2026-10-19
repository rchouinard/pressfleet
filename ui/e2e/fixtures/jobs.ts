// Following a job in the UI: the progress view (components/job/
// JobProgressView.vue) on the job's page, a site page, or anywhere else.
import { expect, type Locator, type Page } from '@playwright/test'

/** Job state labels as the UI shows them (utils/job.ts). */
export const JobState = {
  succeeded: 'Succeeded',
  failed: 'Failed, rolled back',
  rollbackFailed: 'Failed, rollback incomplete',
} as const

/** The progress view of a job. */
export function jobProgress(page: Page, jobId: string): Locator {
  return page.getByRole('region', { name: `Progress of job ${jobId}` })
}

/**
 * Waits, on the page, until the job's progress view shows it ended in the
 * state, and returns the view. The view follows the job live, so this
 * waits on what the user sees rather than polling the API.
 */
export async function waitForJobInUI(page: Page, jobId: string, state: string = JobState.succeeded, timeout = 150_000): Promise<Locator> {
  const view = jobProgress(page, jobId)
  await expect(view.getByRole('status').first()).toHaveText(new RegExp(`^\\s*${state}\\s*$`), { timeout })
  return view
}

/** The ID of the job a /jobs/<id> URL names. */
export function jobIdFromURL(url: string): string {
  const id = new URL(url).pathname.match(/^\/jobs\/([^/]+)/)?.[1]
  if (!id) throw new Error(`not a job page: ${url}`)
  return id
}

/**
 * The ID of the job whose progress view `scope` shows, once it shows one
 * other than `previous` (the job it showed before this one started).
 */
export async function jobIdIn(scope: Locator, previous?: string): Promise<string> {
  const view = scope.getByRole('region', { name: /^Progress of job / })
  let id = ''
  await expect.poll(async () => {
    const label = await view.getAttribute('aria-label', { timeout: 1000 }).catch(() => null)
    id = label?.replace('Progress of job ', '') ?? ''
    return id !== '' && id !== previous
  }).toBe(true)
  return id
}
