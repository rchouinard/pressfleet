// Staging sites in the dashboard (#209): creating one, pushing it, and
// refreshing it, through the dialogs in real Nuxt UI (the Vitest tests stub its
// components). Outcomes are checked on the sites themselves, through WP-CLI and
// their files (harness), and in the API's records.
//
// The API suite (TestStagingSites) covers what the jobs do. This covers what a
// person sees: the production page picking up a push that is still waiting for
// its backup, the typed slug, the undo link, and the links to production's core
// policy and to maintenance mode.
import { createClientGroup, createSite, waitForJob, type Site } from './fixtures/api'
import type { Harness } from './fixtures/harness'
import { JobState, jobProgress, jobStartedBy, waitForJobInUI } from './fixtures/jobs'
import { getSite } from './fixtures/sites'
import { expect, test } from './fixtures'
import type { Locator, Page } from '@playwright/test'

// The tests share the two sites, one after another: provisioning a pair per
// test takes longer than the tests do, and each test continues from the state
// of the one before it, as an operator would.
test.describe.configure({ mode: 'serial' })

let production: Site
let staging: Site

test.beforeAll(async ({ adminApi, env }) => {
  production = await createSite(adminApi, (await createClientGroup(adminApi)).id, env.phpVersion)
})

const plugins = '/var/www/wordpress/wp-content/plugins'

/** Writes a one-file plugin into a site's wp-content. */
async function writePlugin(harness: Harness, siteId: string, slug: string, body = ''): Promise<void> {
  const source = `<?php\n/* Plugin Name: ${slug} */\n${body}\n`
  const encoded = Buffer.from(source).toString('base64')
  await harness.php(siteId, ['sh', '-c', `mkdir -p ${plugins}/${slug} && echo ${encoded} | base64 -d > ${plugins}/${slug}/${slug}.php`])
}

function pushDialog(page: Page): Locator {
  return page.getByRole('dialog', { name: `Push ${staging.name} to production` })
}

async function openPushDialog(page: Page): Promise<Locator> {
  await page.goto(`/sites/${staging.id}`)
  await page.getByRole('button', { name: 'Push to production' }).click()
  const dialog = pushDialog(page)
  await expect(dialog).toBeVisible()
  return dialog
}

/**
 * Checks that following a link landed on a section: the URL names it, and its
 * heading is the topmost thing at its place on the screen, so it isn't off
 * screen, under the page's sticky bar, or behind a dialog left open.
 */
async function expectLandedOn(page: Page, heading: Locator, hash: string): Promise<void> {
  await expect(page).toHaveURL(url => url.hash === hash)
  await expect(heading).toBeInViewport()
  await expect.poll(() => heading.evaluate((el) => {
    const box = el.getBoundingClientRect()
    const top = document.elementFromPoint(box.left + Math.min(box.width, 40) / 2, box.top + box.height / 2)
    return top === el || el.contains(top)
  })).toBe(true)
}

test('creates a staging site through the dialog', async ({ adminPage: page, adminApi, harness }) => {
  await harness.wp(production.id, 'post', 'create', '--post_status=publish', '--post_title=PF E2E production post')

  await page.goto(`/sites/${production.id}`)
  const card = page.getByRole('region', { name: 'Staging sites' })
  await expect(card.getByText('No staging sites yet.')).toBeVisible()
  await card.getByRole('button', { name: 'Create staging site' }).click()

  const dialog = page.getByRole('dialog', { name: 'Create staging site' })
  await expect(dialog.getByText('The copy carries production\'s users and data.')).toBeVisible()
  await expect(dialog.getByRole('radio', { name: /fresh/i })).toBeChecked()
  const clone = await jobStartedBy(page, () => dialog.getByRole('button', { name: 'Create staging site' }).click())
  expect(clone.type).toBe('site.clone')

  // The preview password, once, then the new site's page following the clone.
  const credentials = page.getByRole('dialog', { name: 'Staging preview password' })
  await expect(credentials).toBeVisible()
  await credentials.getByRole('button', { name: 'I have saved the password' }).click()
  await expect(page).toHaveURL(new RegExp(`/sites/${clone.target.id}$`))
  await waitForJobInUI(page, clone.id)

  staging = await getSite(adminApi, clone.target.id)
  expect(staging.environment).toBe('staging')
  expect(staging.parent_site_id).toBe(production.id)
  const banner = page.getByRole('note').filter({ hasText: 'This is a staging site of' })
  await expect(banner.getByRole('link', { name: production.name })).toHaveAttribute('href', `/sites/${production.id}`)
  await expect(banner).toContainText('Never pushed to production')
  expect(await harness.wp(staging.id, 'option', 'get', 'home')).toBe(`https://${staging.preview_domain}`)
  expect(await harness.wp(staging.id, 'post', 'list', '--field=post_title')).toContain('PF E2E production post')

  // Production's page lists it.
  await page.goto(`/sites/${production.id}`)
  await expect(page.getByRole('region', { name: 'Staging sites' }).getByRole('link', { name: staging.name })).toBeVisible()
})

test('a code-only push asks for no slug, and production\'s page follows the queued job to its undo point', async ({ adminPage: page, adminApi, harness }) => {
  await writePlugin(harness, staging.id, 'pf-e2e-pushed')
  // Enough data that the sync backup takes a moment, so the push job is
  // visibly waiting for it when the production page opens.
  const large = '/var/www/wordpress/wp-content/uploads/pf-e2e-large.bin'
  await harness.php(staging.id, ['sh', '-c', `dd if=/dev/urandom of=${large} bs=1M count=64 2>/dev/null`])
  const dialog = await openPushDialog(page)

  // The dialog in real Nuxt UI: the scope controls and the least destructive default.
  await expect(dialog.getByRole('radio', { name: /Code only/ })).toBeChecked()
  await expect(dialog.getByRole('radio', { name: /All files/ })).toBeVisible()
  await expect(dialog.getByRole('radio', { name: /No files/ })).toBeVisible()
  const database = dialog.getByRole('checkbox', { name: 'Database' })
  await expect(database).not.toBeChecked()
  await expect(dialog.getByRole('region', { name: 'Data that will be lost' })).toHaveCount(0)
  await expect(dialog.getByRole('region', { name: 'What a push does' })).toContainText('Production\'s database stays as it is.')
  // No slug is asked for, and the submit button is on screen and ready.
  await expect(dialog.getByRole('textbox')).toHaveCount(0)
  const submit = page.getByRole('button', { name: 'Push to production', exact: true }).last()
  await expect(submit).toBeEnabled()
  await expect(submit).toBeInViewport()

  // Copying nothing is refused here, before the server.
  await dialog.getByRole('radio', { name: /No files/ }).click()
  await expect(dialog.getByRole('alert')).toHaveText('Choose something to copy: files, the database, or both.')
  await expect(submit).toBeDisabled()
  await dialog.getByRole('radio', { name: /Code only/ }).click()

  const job = await jobStartedBy(page, () => submit.click())
  expect(job.type).toBe('site.push')
  expect(job.target.id).toBe(production.id)

  // The page moves to production, and picks the job up while it waits for the
  // staging site's sync backup, which runs under the staging site's lock key.
  await expect(page).toHaveURL(new RegExp(`/sites/${production.id}$`))
  const view = jobProgress(page, job.id)
  await expect(view).toBeVisible()
  const waitsFor = view.getByTestId('waits-for')
  await expect(waitsFor).toContainText('Waiting for job')
  await expect(waitsFor.getByRole('link', { name: job.after_job_id! })).toHaveAttribute('href', `/jobs/${job.after_job_id}`)
  await expect(view.getByRole('status').first()).toHaveText(/Queued|Waiting/)

  // And follows it to its end, without a reload.
  await expect(view.getByTestId('waits-for')).toContainText('Ran after job', { timeout: 150_000 })
  await waitForJobInUI(page, job.id)
  await expect(page.getByText('Updating', { exact: true })).toHaveCount(0)

  // The plugin arrived, inactive; the database stayed.
  expect(await harness.wp(production.id, 'plugin', 'get', 'pf-e2e-pushed', '--field=status')).toBe('inactive')
  const final = await waitForJob(adminApi, job.id)
  const backupId = final.result?.pre_push_backup_id
  expect(typeof backupId).toBe('string')

  // The undo link reaches the backup, marked.
  const undo = view.getByTestId('sync-undo')
  await expect(undo).toContainText('Undo this push')
  await undo.getByRole('link', { name: 'Open the pre-push backup' }).click()
  await expect(page).toHaveURL(url => url.pathname === `/sites/${production.id}/backups` && url.searchParams.get('backup') === backupId)
  await expect(page.getByTestId('marked-backup')).toContainText('is marked "Undo point" below')
  const row = page.getByRole('row').filter({ has: page.getByText('Undo point', { exact: true }) })
  await expect(row).toHaveCount(1)
  await expect(row.getByText('Undo point', { exact: true })).toBeVisible()
  await expect(row.getByRole('button', { name: /Restore/ })).toBeVisible()

  await harness.php(staging.id, ['rm', '-f', large])
})

test('the typed slug gates a database push and an all-files push', async ({ adminPage: page, adminApi, harness }) => {
  const stagURL = `https://${staging.preview_domain}`
  await harness.wp(staging.id, 'post', 'create', '--post_status=publish', '--post_title=PF E2E pushed from staging', `--post_content=<a href="${stagURL}/x/">x</a>`)
  const dialog = await openPushDialog(page)
  const database = dialog.getByRole('checkbox', { name: 'Database' })
  const slug = dialog.getByRole('textbox', { name: `Type ${production.slug} to confirm` })
  const submit = page.getByRole('button', { name: 'Overwrite production and push' })

  // All files.
  await dialog.getByRole('radio', { name: /All files/ }).click()
  await expect(slug).toBeVisible()
  await expect(submit).toBeDisabled()
  const lost = dialog.getByRole('region', { name: 'Data that will be lost' })
  await expect(lost).toContainText('Replaces production\'s uploads with the staging site\'s copy.')
  await expect(lost).not.toContainText('Replaces production\'s database')
  await slug.fill(`${production.slug}x`)
  await expect(submit).toBeDisabled()
  await slug.fill(production.slug)
  await expect(submit).toBeEnabled()

  // What was typed doesn't carry over to a scope that was changed and reopened.
  await page.getByRole('button', { name: 'Cancel' }).click()
  await expect(dialog).toBeHidden()
  await page.getByRole('button', { name: 'Push to production' }).click()
  await expect(dialog.getByRole('textbox')).toHaveCount(0)

  // The database, on its own.
  await dialog.getByRole('radio', { name: /No files/ }).click()
  await database.click()
  await expect(slug).toBeVisible()
  await expect(slug).toHaveValue('')
  await expect(submit).toBeDisabled()
  await expect(lost).toContainText('Replaces production\'s database with the staging site\'s.')
  // Pressing Enter in the field doesn't get past the gate either.
  await slug.fill('wrong')
  await slug.press('Enter')
  await expect(dialog).toBeVisible()
  await expect(page).toHaveURL(new RegExp(`/sites/${staging.id}$`))
  await slug.fill(production.slug)
  await expect(submit).toBeInViewport()
  const job = await jobStartedBy(page, () => submit.click())

  await expect(page).toHaveURL(new RegExp(`/sites/${production.id}$`))
  await waitForJobInUI(page, job.id)
  const url = `https://${production.preview_domain}`
  expect(await harness.wp(production.id, 'option', 'get', 'home')).toBe(url)
  const posts = await harness.wp(production.id, 'post', 'list', '--post_status=publish', '--fields=post_title,post_content', '--format=json')
  expect(posts).toContain('PF E2E pushed from staging')
  expect(posts).not.toContain(staging.preview_domain)
  expect((await waitForJob(adminApi, job.id)).state).toBe('succeeded')
})

test('refreshes the staging site, database only', async ({ adminPage: page, adminApi, harness }) => {
  await writePlugin(harness, staging.id, 'pf-e2e-kept')
  await harness.wp(production.id, 'post', 'create', '--post_status=publish', '--post_title=PF E2E new on production')
  const before = await getSite(adminApi, staging.id)

  await page.goto(`/sites/${staging.id}`)
  await page.getByRole('button', { name: 'Refresh from production' }).click()
  const dialog = page.getByRole('dialog', { name: `Refresh ${staging.name} from production` })
  await expect(dialog).toBeVisible()
  await dialog.getByRole('radio', { name: /No files/ }).click()
  await expect(dialog.getByRole('checkbox', { name: 'Database' })).toBeChecked()
  await expect(dialog.getByRole('region', { name: 'What a refresh does' })).toContainText('this staging site\'s wp-content stays as it is')
  await expect(dialog.getByRole('radio', { name: /A fresh copy of production/ })).toBeChecked()

  const job = await jobStartedBy(page, () => dialog.getByRole('button', { name: 'Refresh from production' }).click())
  expect(job.type).toBe('site.refresh')
  await waitForJobInUI(page, job.id)

  expect(await harness.wp(staging.id, 'post', 'list', '--field=post_title')).toContain('PF E2E new on production')
  expect(await harness.php(staging.id, ['ls', `${plugins}/pf-e2e-kept`])).toContain('pf-e2e-kept.php')
  const after = await getSite(adminApi, staging.id)
  expect(after.staging!.database_as_of! > before.staging!.database_as_of!).toBe(true)
  expect(after.staging!.uploads_as_of).toBe(before.staging!.uploads_as_of)
  expect(after.preview_protection.enabled).toBe(true)
  await expect(jobProgress(page, job.id).getByTestId('sync-undo')).toContainText('Undo this refresh')
})

test('the core guard\'s link lands on production\'s core policy', async ({ adminPage: page }) => {
  // The refusal is the server's to give (TestStagingSites gets a real one, from
  // a staging site on a newer branch, which takes stand-in images). What this
  // needs is what the dialog does with it.
  const detail = 'would move the production site from WordPress 7.1.4 to 7.2.0, but production\'s core policy holds it on branch 7.1'
  await page.route('**/api/v1/sites/*/push', route => route.fulfill({
    status: 422,
    contentType: 'application/problem+json',
    body: JSON.stringify({ type: 'about:blank', title: 'Unprocessable Content', status: 422, errors: [{ field: '/database', detail }] }),
  }))
  const dialog = await openPushDialog(page)
  await dialog.getByRole('radio', { name: /No files/ }).click()
  await dialog.getByRole('checkbox', { name: 'Database' }).click()
  await dialog.getByRole('textbox', { name: `Type ${production.slug} to confirm` }).fill(production.slug)
  await page.getByRole('button', { name: 'Overwrite production and push' }).click()

  const refusal = dialog.getByRole('alert').filter({ hasText: 'production\'s core policy doesn\'t allow it' })
  await expect(refusal).toContainText(detail)
  await refusal.getByRole('link', { name: `Open ${production.name}'s WordPress core policy` }).click()
  await page.unroute('**/api/v1/sites/*/push')

  await expect(page).toHaveURL(url => url.pathname === `/sites/${production.id}`)
  await expect(dialog).toBeHidden()
  await expectLandedOn(page, page.getByRole('heading', { name: 'WordPress core', exact: true }), '#site-core-heading')
  // The dialog left nothing behind that blocks the page.
  await page.getByRole('link', { name: 'Backups', exact: true }).first().click()
  await expect(page).toHaveURL(new RegExp(`/sites/${production.id}/backups$`))
})

test('a failed push leaves production in maintenance mode, and the alert\'s link lands on the setting', async ({ adminPage: page, adminApi, harness }) => {
  // A plugin active on production whose code on the staging site fatals on load.
  const slug = 'pf-e2e-fatal'
  await writePlugin(harness, production.id, slug)
  await harness.wp(production.id, 'plugin', 'activate', slug)
  await writePlugin(harness, staging.id, slug, 'throw new Error("pf-e2e boom");')

  const dialog = await openPushDialog(page)
  const job = await jobStartedBy(page, () => dialog.page().getByRole('button', { name: 'Push to production', exact: true }).last().click())
  await expect(page).toHaveURL(new RegExp(`/sites/${production.id}$`))
  await waitForJobInUI(page, job.id, JobState.failed)

  const alert = page.getByRole('alert').filter({ hasText: 'This site is in maintenance mode' })
  await expect(alert).toContainText('rolled back to its pre-push backup')
  const site = await getSite(adminApi, production.id)
  expect(site.maintenance).toBe(true)
  expect(await harness.php(production.id, ['cat', `${plugins}/${slug}/${slug}.php`])).not.toContain('boom')
  // The job names the backup it rolled back to.
  const undo = jobProgress(page, job.id).getByTestId('sync-undo')
  await expect(undo).toContainText('The push failed')
  await expect(undo.getByRole('link', { name: 'Open the pre-push backup' })).toBeVisible()

  await alert.getByRole('link', { name: 'Maintenance mode' }).click()
  await expectLandedOn(page, page.getByRole('heading', { name: 'Maintenance mode', exact: true }), '#set-maint-heading')

  // The setting it landed on works, and ends the alert.
  await expect(page.getByTestId('maintenance-state')).toContainText('On:')
  const off = await jobStartedBy(page, () => page.getByRole('button', { name: 'Turn maintenance mode off' }).click())
  expect((await waitForJob(adminApi, off.id)).state).toBe('succeeded')
  await expect(page.getByTestId('maintenance-state')).toContainText('Off:')
  await expect(alert).toHaveCount(0)
})
