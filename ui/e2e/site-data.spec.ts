// Moving a site's data in and out (#116, #133): export a site and its
// database and download the files, import a site from a wp-content archive
// and a SQL dump, deploy a theme package, and open and end a database
// session. Outcomes are checked on the platform: the downloaded files
// against the site's content, the imported and deployed sites' pages and
// content (harness), and the session's MariaDB user.
import { gunzipSync } from 'node:zlib'
import { createClientGroup, createSite, exportSite, unique, type Site } from './fixtures/api'
import { tarGz, untarGz } from './fixtures/archive'
import { jobIdIn, waitForJobInUI } from './fixtures/jobs'
import { choose, download, toast } from './fixtures/ui'
import { expect, test } from './fixtures'

// The tests share a site, one after another and in this order: provisioning
// one per test takes longer than the tests, and the control plane runs out
// of memory with too many sites provisioning at once. The import copies
// the site, so it comes before the deploy changes its theme.
test.describe.configure({ mode: 'default' })

let site: Site
let groupName: string
test.beforeAll(async ({ adminApi, env }) => {
  const group = await createClientGroup(adminApi)
  groupName = group.name
  site = await createSite(adminApi, group.id, env.phpVersion)
})

/** The site's wp-content, in its php container. */
const wpContent = '/var/www/wordpress/wp-content'

/** A unique value to find in a site's files and database. */
function marker(prefix: string): string {
  return unique(prefix).replace(' ', '-')
}

test('exports a site and downloads its files', async ({ adminPage: page, adminApi, harness }) => {
  const value = marker('pf-e2e-export')
  await harness.wp(site.id, 'option', 'update', 'pf_e2e_export', value)
  await harness.php(site.id, ['sh', '-c', `mkdir -p ${wpContent}/uploads/pf-e2e && echo ${value} > ${wpContent}/uploads/pf-e2e/export.txt`])

  await page.goto(`/sites/${site.id}`)
  const card = page.getByRole('region', { name: 'Export', exact: true })
  await card.getByRole('button', { name: 'Export site' }).click()
  await waitForJobInUI(page, await jobIdIn(card))

  // The two files a site import takes, with the site's files and data and
  // without the platform's drop-ins.
  const archive = untarGz(await download(page, card.getByRole('link', { name: /^wp-content archive/ })))
  expect([...archive.keys()].filter(name => !name.startsWith('wp-content/'))).toEqual([])
  expect(archive.get('wp-content/uploads/pf-e2e/export.txt')).toBe(`${value}\n`)
  expect(archive.has('wp-content/object-cache.php')).toBe(false)
  const dump = gunzipSync(await download(page, card.getByRole('link', { name: /^Database dump/ }))).toString()
  expect(dump).toContain(`'pf_e2e_export','${value}'`)

  const { data: list } = await adminApi.client.GET('/api/v1/exports', { params: { query: { site_id: site.id, kind: ['site'] } } })
  expect(list!.items).toEqual([expect.objectContaining({ kind: 'site', status: 'ready', site_slug: site.slug })])
})

test('downloads a database export', async ({ adminPage: page, harness }) => {
  const value = marker('pf-e2e-dump')
  await harness.wp(site.id, 'option', 'update', 'pf_e2e_dump', value)

  await page.goto(`/sites/${site.id}`)
  await page.getByRole('link', { name: 'Database', exact: true }).click()
  await expect(page.getByRole('heading', { level: 1, name: `Database: ${site.name}` })).toBeVisible()
  const section = page.getByRole('region', { name: 'Export', exact: true })
  await section.getByRole('button', { name: 'Export database' }).click()
  await waitForJobInUI(page, await jobIdIn(section))

  // The dump holds the site's data, and imports into any database.
  const dump = gunzipSync(await download(page, section.getByRole('link', { name: 'Download dump' }))).toString()
  expect(dump).toContain(`'pf_e2e_dump','${value}'`)
  expect(dump).toContain('CREATE TABLE `wp_options`')
  expect(dump).not.toMatch(/^(CREATE DATABASE|USE) /m)
})

test('imports a site from a wp-content archive and a SQL dump', async ({ adminPage: page, adminApi, harness, env }) => {
  // The files are an export of the shared site, through the API.
  const title = unique('PF E2E imported post')
  const postId = await harness.wp(site.id, 'post', 'create', `--post_title=${title}`, '--post_status=publish', '--porcelain')
  await harness.php(site.id, ['sh', '-c', `mkdir -p ${wpContent}/uploads/pf-e2e && echo imported > ${wpContent}/uploads/pf-e2e/import.txt`])
  const { archive, dump } = await exportSite(adminApi, env.apiURL, site.id)

  const name = unique('E2E Imported')
  await page.goto('/sites')
  await page.getByRole('link', { name: 'Import site' }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'Import a site' })).toBeVisible()
  await choose(page, 'Client group', groupName)
  await page.getByLabel('wp-content archive').setInputFiles({ name: 'wp-content.tar.gz', mimeType: 'application/gzip', buffer: archive })
  await page.getByLabel('Database dump').setInputFiles({ name: 'database.sql.gz', mimeType: 'application/gzip', buffer: dump })
  await page.getByLabel('Name').fill(name)
  await page.getByRole('button', { name: 'Upload and import' }).click()

  const progress = page.getByRole('region', { name: 'Import progress' })
  await waitForJobInUI(page, await jobIdIn(progress), undefined, 240_000)
  await progress.getByRole('link', { name: 'Open the site' }).click()
  await expect(page.getByRole('heading', { level: 1, name })).toBeVisible()
  const importedId = new URL(page.url()).pathname.split('/').pop()!

  // A new site, serving the imported post on its own preview domain, with
  // the imported files.
  const { data: imported } = await adminApi.client.GET('/api/v1/sites/{site_id}', { params: { path: { site_id: importedId } } })
  expect(imported).toMatchObject({ name, status: 'running', client_group_id: site.client_group_id })
  expect(imported!.id).not.toBe(site.id)
  expect(await harness.sitePage(importedId, imported!.preview_domain, `/?p=${postId}`)).toContain(title)
  expect(await harness.php(importedId, ['cat', `${wpContent}/uploads/pf-e2e/import.txt`])).toBe('imported')
  expect(await harness.wp(importedId, 'option', 'get', 'home')).toBe(`https://${imported!.preview_domain}`)
})

test('deploys a theme package', async ({ adminPage: page, harness }) => {
  const version = marker('v')
  const pkg = tarGz({
    'pf-e2e-theme/style.css': '/*\nTheme Name: PF E2E Theme\n*/\n',
    'pf-e2e-theme/index.php': `<?php echo "pf-e2e-theme ${version}";\n`,
  })

  await page.goto(`/sites/${site.id}`)
  await page.getByRole('link', { name: 'Deploy code' }).click()
  await expect(page.getByRole('heading', { level: 1, name: `Deploy code: ${site.name}` })).toBeVisible()
  await page.getByRole('radio', { name: 'Theme' }).check()
  await page.getByLabel('Directory name').fill('pf-e2e-theme')
  await page.getByLabel('Package').setInputFiles({ name: 'pf-e2e-theme.tar.gz', mimeType: 'application/gzip', buffer: pkg })
  await page.getByRole('checkbox', { name: 'Activate the theme after deploying' }).check()
  await page.getByRole('button', { name: 'Upload and deploy' }).click()

  const deployment = page.getByRole('region', { name: 'Deployment' })
  await waitForJobInUI(page, await jobIdIn(deployment))
  await expect(deployment.getByText('themes/pf-e2e-theme', { exact: true })).toBeVisible()

  // The site serves the deployed theme, active.
  expect(await harness.sitePage(site.id, site.preview_domain)).toBe(`pf-e2e-theme ${version}`)
  expect(await harness.wp(site.id, 'option', 'get', 'stylesheet')).toBe('pf-e2e-theme')
})

test('opens a database session, shows its connection details, and ends it', async ({ adminPage: page, adminApi, harness }) => {
  const mariadbUsers = (user: string) => harness.mariadb(`SELECT COUNT(*) FROM mysql.user WHERE User = '${user}'`)

  await page.goto(`/sites/${site.id}/database`)
  await page.getByRole('button', { name: 'Open session' }).click()

  // The credentials and the tunnel, shown once.
  const dialog = page.getByRole('dialog', { name: 'Database session opened' })
  const value = (label: string) => dialog.getByRole('textbox', { name: new RegExp(`^${label}`) }).inputValue()
  const username = await value('Username')
  const password = await value('Password')
  expect(username).toMatch(/^\w+$/)
  expect(password.length).toBeGreaterThanOrEqual(16)
  expect(await value('Database')).toBe(`pf_${site.id}`)
  expect(await value('Host')).toBeTruthy()
  expect(await value('Port')).toMatch(/^\d+$/)
  expect(await value('Tunnel command')).toContain(site.slug)
  await expect(dialog.getByText('read-only')).toBeVisible()

  // The session is a MariaDB user of its own, listed by the API.
  expect(await mariadbUsers(username)).toBe('1')
  const { data: open } = await adminApi.client.GET('/api/v1/sites/{site_id}/database/sessions', { params: { path: { site_id: site.id } } })
  expect(open!.items).toEqual([expect.objectContaining({ username, access: 'read_only' })])

  // Closing the dialog drops the password from the page; the session stays.
  await dialog.getByRole('button', { name: 'I have copied it' }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByText(password)).toHaveCount(0)
  const sessions = page.getByRole('region', { name: 'Active database sessions' })
  await expect(sessions.getByRole('row').filter({ hasText: username })).toBeVisible()

  // Ending it drops the user.
  await sessions.getByRole('button', { name: `End session ${username}` }).click()
  await expect(toast(page, 'Session ended')).toBeVisible()
  await expect(page.getByText('No active sessions.')).toBeVisible()
  expect(await mariadbUsers(username)).toBe('0')
  const { data: ended } = await adminApi.client.GET('/api/v1/sites/{site_id}/database/sessions', { params: { path: { site_id: site.id } } })
  expect(ended!.items).toEqual([])
})
