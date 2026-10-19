// Sites and jobs (#84): provision a site and follow its job to the end,
// the site's status, the jobs list and a job's page, a failed job's error,
// and deleting the site without leaving anything behind.
import { apiError, createClientGroup, startFailingImport, unique, waitForJob } from './fixtures/api'
import { JobState, jobIdFromURL, jobProgress, waitForJobInUI } from './fixtures/jobs'
import { choose, toast } from './fixtures/ui'
import { expect, test } from './fixtures'


test('provisions a site, follows its job, and deletes it', async ({ adminPage: page, adminApi, harness, env }) => {
  const group = await createClientGroup(adminApi)
  const name = unique('E2E Site')

  await page.goto('/sites')
  await page.getByRole('button', { name: 'New site' }).click()
  const dialog = page.getByRole('dialog', { name: 'New site' })
  await dialog.getByLabel(/^Name/).fill(name)
  await choose(page, 'Client group', group.name)
  await dialog.getByLabel('Admin email').fill('owner@e2e.test')
  // The harness made the images of this PHP version available.
  await choose(page, 'PHP version', new RegExp(`^PHP ${env.phpVersion.replace('.', '\\.')}\\b`))
  await dialog.getByRole('button', { name: 'Create site' }).click()

  // The site's page follows the provisioning job live.
  await expect(page).toHaveURL(/\/sites\/[a-z0-9]{26}$/)
  const siteId = new URL(page.url()).pathname.split('/').pop()!
  await expect(page.getByRole('heading', { level: 1, name })).toBeVisible()
  const progress = page.getByRole('region', { name: /^Progress of job / })
  await expect(progress.getByRole('heading', { name: 'Create site' })).toBeVisible()
  const jobId = (await progress.getAttribute('aria-label'))!.replace('Progress of job ', '')
  const done = await waitForJobInUI(page, jobId)
  await expect(done.getByRole('listitem').filter({ hasText: /Failed|Pending|Running/ })).toHaveCount(0)
  await expect(page.getByText('Running', { exact: true }).first()).toBeVisible()

  const { data: site } = await adminApi.client.GET('/api/v1/sites/{site_id}', { params: { path: { site_id: siteId } } })
  expect(site).toMatchObject({ name, status: 'running', client_group_id: group.id, php_version: env.phpVersion })
  expect(await harness.siteLeftovers(siteId)).not.toEqual([])

  // The jobs list shows the job, and its page the same outcome.
  await page.getByRole('link', { name: 'Jobs' }).click()
  const jobs = page.getByRole('region', { name: /jobs/i })
  const row = jobs.getByRole('row').filter({ hasText: siteId.slice(0, 8) }).filter({ hasText: 'Create site' })
  await expect(row.getByText(JobState.succeeded)).toBeVisible()
  await row.getByRole('link', { name: 'Create site' }).click()
  await expect(page).toHaveURL(new RegExp(`/jobs/${jobId}$`))
  await waitForJobInUI(page, jobId)
  await expect(jobProgress(page, jobId).getByRole('link', { name: `site ${siteId}` })).toBeVisible()

  // Delete it from its page; the page follows the deletion, then goes back
  // to the list.
  await page.goto(`/sites/${siteId}`)
  await page.getByRole('button', { name: 'Delete', exact: true }).click()
  const confirm = page.getByRole('dialog', { name: `Delete ${name}?` })
  await confirm.getByRole('button', { name: 'Delete site' }).click()
  await expect(confirm).toBeHidden()
  await expect(page).toHaveURL(/\/sites$/, { timeout: 150_000 })
  await expect(toast(page, 'Site deleted')).toBeVisible()
  await expect(page.getByRole('link', { name })).toBeHidden()

  // Nothing is left: not in the API, on Docker, on disk, in MariaDB, or in
  // the object cache.
  expect((await apiError(adminApi.client.GET('/api/v1/sites/{site_id}', { params: { path: { site_id: siteId } } }))).status).toBe(404)
  expect(await harness.siteLeftovers(siteId)).toEqual([])
})

test('a failed job shows its error', async ({ adminPage: page, adminApi, harness, env }) => {
  const group = await createClientGroup(adminApi)
  const started = await startFailingImport(adminApi, env.apiURL, group.id)
  const job = await waitForJob(adminApi, started.id)
  expect(job.state).toBe('failed')
  expect(job.error).toBeTruthy()

  await page.goto(`/jobs/${job.id}`)
  expect(jobIdFromURL(page.url())).toBe(job.id)
  const view = await waitForJobInUI(page, job.id, JobState.failed)
  await expect(view.getByRole('alert')).toContainText(job.error!)
  await expect(view.getByRole('listitem').filter({ hasText: 'Validate' })).toContainText('Rolled back')
  await expect(view.getByRole('status').filter({ hasText: 'Roll-back' })).toContainText('everything it created was rolled back')

  // So no site is left of it.
  expect((await apiError(adminApi.client.GET('/api/v1/sites/{site_id}', { params: { path: { site_id: job.target.id } } }))).status).toBe(404)
  expect(await harness.siteLeftovers(job.target.id)).toEqual([])

  // The jobs list, filtered to failed jobs, has it too.
  await page.goto('/jobs')
  await choose(page, 'State', /^Failed/)
  const row = page.getByRole('row').filter({ hasText: job.target.id.slice(0, 8) })
  await expect(row.getByText(JobState.failed)).toBeVisible()
})
