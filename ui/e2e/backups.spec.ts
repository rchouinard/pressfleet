// Backups (#87) and exports from them (#134): back a site up, see the
// backup listed, restore it over later changes, and export the site as it
// was at a backup. Outcomes are checked on the site itself, through
// WP-CLI and its files (harness), and in the API's backup and export
// records.
import { gunzipSync } from 'node:zlib'
import { createClientGroup, createSite, waitForJob, type Api, type Site } from './fixtures/api'
import { untarGz } from './fixtures/archive'
import type { Harness } from './fixtures/harness'
import { jobIdIn, waitForJobInUI } from './fixtures/jobs'
import { download, toast } from './fixtures/ui'
import { expect, test } from './fixtures'

// The tests share a site, one after another: provisioning one per test
// takes longer than the tests, and the control plane runs out of memory
// with too many sites provisioning at once.
test.describe.configure({ mode: 'default' })

let site: Site
test.beforeAll(async ({ adminApi, env }) => {
  site = await createSite(adminApi, (await createClientGroup(adminApi)).id, env.phpVersion)
})

/** The test files' directory under wp-content, in the site's php container. */
const uploads = '/var/www/wordpress/wp-content/uploads/pf-e2e'

/** Gives the site the state a backup should keep: an option and a file. */
async function setUp(harness: Harness): Promise<void> {
  await harness.wp(site.id, 'option', 'update', 'pf_e2e_marker', 'before')
  await harness.siteExec(site.id, ['sh', '-c', `rm -rf ${uploads} && mkdir -p ${uploads} && echo kept > ${uploads}/kept.txt`])
}

/** Moves the site on from the backup: the option changes, a file is added and the kept one removed. */
async function moveOn(harness: Harness): Promise<void> {
  await harness.wp(site.id, 'option', 'update', 'pf_e2e_marker', 'after')
  await harness.siteExec(site.id, ['sh', '-c', `echo added > ${uploads}/added.txt && rm ${uploads}/kept.txt`])
}

/** The site's backups, newest first. */
async function backups(api: Api) {
  const { data } = await api.client.GET('/api/v1/sites/{site_id}/backups', { params: { path: { site_id: site.id } } })
  return data!.items
}

test('backs up a site, lists the backup, and restores the earlier content', async ({ adminPage: page, adminApi, harness }) => {
  await setUp(harness)

  await page.goto(`/sites/${site.id}`)
  // The action bar's link (the export card links there too).
  await page.getByRole('link', { name: 'Backups', exact: true }).first().click()
  await expect(page.getByRole('heading', { level: 1, name: `Backups: ${site.name}` })).toBeVisible()
  await page.getByRole('button', { name: 'Back up now' }).click()
  const progress = page.getByRole('region', { name: 'Job progress' })
  const backupJob = await jobIdIn(progress)
  await waitForJobInUI(page, backupJob)

  // The history lists the backup, and the API has its snapshot.
  const [backup] = await backups(adminApi)
  expect(backup).toMatchObject({ kind: 'manual', status: 'succeeded', wp_core_version: site.wp_core_version })
  expect(backup!.snapshot_id).toBeTruthy()
  const history = page.getByRole('region', { name: 'Backups', exact: true })
  const row = history.getByRole('row').filter({ has: page.locator(`time[datetime="${backup!.started_at}"]`) })
  await expect(row.getByRole('cell', { name: 'Manual' })).toBeVisible()
  await expect(row.getByRole('cell', { name: 'Succeeded' })).toBeVisible()

  // Later changes, then a restore of the backup over them.
  await moveOn(harness)
  const postId = await harness.wp(site.id, 'post', 'create', '--post_title=PF E2E after the backup', '--post_status=publish', '--porcelain')
  await row.getByRole('button', { name: /^Restore the backup from / }).click()
  const confirm = page.getByRole('dialog', { name: `Restore ${site.name} from a backup?` })
  const overwrite = confirm.getByRole('button', { name: 'Overwrite site and restore' })
  await expect(overwrite).toBeDisabled()
  await confirm.getByLabel(`Type ${site.name} to confirm`).fill(site.name)
  await overwrite.click()
  await expect(confirm).toBeHidden()
  const restoreJob = await jobIdIn(progress, backupJob)
  await waitForJobInUI(page, restoreJob)
  await expect(toast(page, 'Restore finished')).toBeVisible()

  // The site is back to the backup's content: the option, the files, and
  // no post from after it.
  expect(await harness.wp(site.id, 'option', 'get', 'pf_e2e_marker')).toBe('before')
  expect(await harness.siteExec(site.id, ['ls', uploads])).toBe('kept.txt')
  expect(await harness.wp(site.id, 'post', 'list', `--post__in=${postId}`, '--post_status=any', '--format=count')).toBe('0')
  const { data: restored } = await adminApi.client.GET('/api/v1/sites/{site_id}', { params: { path: { site_id: site.id } } })
  expect(restored).toMatchObject({ status: 'running', maintenance: false })

  // What was there before the restore was backed up first, and is listed.
  const [preRestore] = await backups(adminApi)
  expect(preRestore).toMatchObject({ kind: 'pre_restore', status: 'succeeded' })
  await expect(history.getByRole('row').filter({ has: page.locator(`time[datetime="${preRestore!.started_at}"]`) })
    .getByRole('cell', { name: 'Before a restore' })).toBeVisible()
})

test('exports a site as it was at a backup', async ({ adminPage: page, adminApi, harness }) => {
  await setUp(harness)
  const { data: started } = await adminApi.client.POST('/api/v1/sites/{site_id}/backups', { params: { path: { site_id: site.id } } })
  expect((await waitForJob(adminApi, started!.job.id)).state).toBe('succeeded')
  const [backup] = await backups(adminApi)
  await moveOn(harness)

  await page.goto(`/sites/${site.id}/backups`)
  await page.getByRole('region', { name: 'Backups', exact: true }).getByRole('row')
    .filter({ has: page.locator(`time[datetime="${backup!.started_at}"]`) })
    .getByRole('button', { name: /^Export the backup from / }).click()
  const exports = page.getByRole('region', { name: 'Exports from backups' })
  const job = await jobIdIn(exports)
  await waitForJobInUI(page, job)

  // The export lists both files; they hold the site as it was at the backup.
  const archive = untarGz(await download(page, exports.getByRole('link', { name: /^wp-content archive/ })))
  expect(archive.get('wp-content/uploads/pf-e2e/kept.txt')).toBe('kept\n')
  expect(archive.has('wp-content/uploads/pf-e2e/added.txt')).toBe(false)
  const dump = gunzipSync(await download(page, exports.getByRole('link', { name: /^Database dump/ }))).toString()
  expect(dump).toContain(`'pf_e2e_marker','before'`)
  expect(dump).not.toContain(`'pf_e2e_marker','after'`)

  // The API has the export, made from that backup; the live site is as it was.
  const { data: list } = await adminApi.client.GET('/api/v1/exports', { params: { query: { site_id: site.id } } })
  expect(list!.items).toEqual([expect.objectContaining({ kind: 'snapshot', status: 'ready' })])
  expect(await harness.wp(site.id, 'option', 'get', 'pf_e2e_marker')).toBe('after')
})
