// Following a job in the UI: the progress view (components/job/
// JobProgressView.vue) on the job's page, a site page, or anywhere else.
import { expect, type Locator, type Page } from '@playwright/test'
import type { Job } from './api'

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
 * Runs `action`, which makes the page start a job (an API call that answers
 * 202), and returns that job. The UI follows the job on its own; specs wait
 * for it with waitForJob, or on what the page shows, before checking the
 * outcome. Taking the job from the response, rather than from the progress
 * view, can't mistake the job the view showed before for the new one.
 */
export async function jobStartedBy(page: Page, action: () => Promise<unknown>): Promise<Job> {
  const accepted = page.waitForResponse(r => r.status() === 202 && new URL(r.url()).pathname.startsWith('/api/v1/'))
  await action()
  const body = (await (await accepted).json()) as { job: Job }
  return body.job
}
