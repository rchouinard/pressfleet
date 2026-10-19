// Monitoring (#88, #89, #91): the site and platform metrics charts with
// collected data, the site log stream and the logs page, and the platform
// overview with its capacity warning. TestDashboard runs the collectors
// every 5 seconds; VictoriaMetrics still holds samples back for 30 seconds,
// so the first data of a new site takes about a minute.
import { randomUUID } from 'node:crypto'
import { apiError, createClientGroup, unique, waitForJob, type Api } from './fixtures/api'
import { expect, test } from './fixtures'
import type { PlatformEnv } from './env'
import type { Page } from '@playwright/test'

/** How long the collectors may take to report a new series, VictoriaMetrics' delay included. */
const collected = 150_000

type MetricScope = 'site' | 'platform'

/** The series of a metric over the last hour that have points, as the API returns them. */
async function seriesWithData(api: Api, scope: MetricScope, metric: string, siteId?: string) {
  const query = { metric: [metric] }
  const { data } = scope === 'site'
    ? await api.client.GET('/api/v1/sites/{site_id}/metrics', { params: { path: { site_id: siteId! }, query } })
    : await api.client.GET('/api/v1/metrics/platform', { params: { query } })
  return (data!.metrics.find(m => m.id === metric)?.series ?? []).filter(s => s.points.length > 0)
}

/** Creates a site through the API and waits until it is running. */
async function createSite(api: Api, env: PlatformEnv, resources?: { memory_reservation_mb: number, memory_limit_mb: number }) {
  const group = await createClientGroup(api)
  const { data } = await api.client.POST('/api/v1/sites', {
    body: { client_group_id: group.id, name: unique('E2E Monitored'), admin_email: 'owner@e2e.test', php_version: env.phpVersion, resources },
  })
  const job = await waitForJob(api, data!.job.id)
  expect(job.state, job.error ?? '').toBe('succeeded')
  return data!.site
}

async function deleteSite(api: Api, siteId: string) {
  const { data } = await api.client.DELETE('/api/v1/sites/{site_id}', { params: { path: { site_id: siteId } } })
  expect((await waitForJob(api, data!.job.id)).state).toBe('succeeded')
}

/**
 * Checks that a metric's card has a chart and one table row per series the
 * API has data for. The card's image is named by its summary, which only
 * reads "<title>. <series>: latest ..." when there is data.
 */
async function expectChart(page: Page, api: Api, scope: MetricScope, metric: string, title: string, siteId?: string) {
  await expect.poll(async () => (await seriesWithData(api, scope, metric, siteId)).length, { timeout: collected, intervals: [5_000] })
    .toBeGreaterThan(0)
  const series = await seriesWithData(api, scope, metric, siteId)

  // The page queries when it loads; reload it until it sees what the API saw.
  const card = page.getByRole('region', { name: title, exact: true })
  await expect(async () => {
    await page.reload()
    await expect(card.getByRole('img', { name: new RegExp(`^${title}\\. .+: latest `) })).toBeVisible({ timeout: 10_000 })
  }).toPass({ timeout: 60_000 })
  await expect(card.getByText('No data in this range.')).toBeHidden()
  const rows = card.getByRole('table').getByRole('row')
  // The header row, then one per series.
  await expect(rows).toHaveCount(series.length + 1)
}

test('platform metrics charts render with collected data', async ({ adminPage: page, adminApi }) => {
  await page.goto('/metrics')
  await expect(page.getByRole('heading', { level: 1, name: 'Platform metrics' })).toBeVisible()
  await expectChart(page, adminApi, 'platform', 'host_memory', 'Host memory')
  await expectChart(page, adminApi, 'platform', 'host_cpu', 'Host CPU')
  await expectChart(page, adminApi, 'platform', 'mariadb_connections', 'MariaDB connections')
})

test.describe('a running site', () => {
  // Both tests use one site, so they run one after the other in one worker.
  test.describe.configure({ mode: 'serial' })
  let siteId = ''
  let siteName = ''

  test.beforeAll(async ({ adminApi, env }) => {
    test.setTimeout(4 * 60_000)
    const site = await createSite(adminApi, env)
    siteId = site.id
    siteName = site.name
  })

  test.afterAll(async ({ adminApi }) => {
    test.setTimeout(3 * 60_000)
    if (siteId) await deleteSite(adminApi, siteId)
  })

  test('site metrics charts render with collected data', async ({ adminPage: page, adminApi }) => {
    // From the site's page, as a user gets there.
    await page.goto(`/sites/${siteId}`)
    await page.getByRole('main').getByRole('link', { name: 'Metrics', exact: true }).click()
    await expect(page).toHaveURL(new RegExp(`/sites/${siteId}/metrics$`))
    await expect(page.getByRole('heading', { level: 1, name: `Metrics: ${siteName}` })).toBeVisible()
    // Not CPU and memory: they need cgroup v2, which not every Docker
    // host running this suite has.
    await expectChart(page, adminApi, 'site', 'php_workers', 'PHP-FPM workers', siteId)
    await expectChart(page, adminApi, 'site', 'object_cache_memory', 'Object cache memory', siteId)
  })

  test('the logs page opens a site\'s live log stream, which shows new lines', async ({ adminPage: page, adminApi, harness }) => {
    await page.goto('/logs')
    await expect(page.getByRole('heading', { level: 1, name: 'Logs' })).toBeVisible()
    // A select menu (searchable), whose trigger is a button.
    await page.getByRole('button', { name: 'Site' }).click()
    await page.getByRole('option', { name: new RegExp(`^${siteName} \\(`) }).click()
    await expect(page).toHaveURL(new RegExp(`/sites/${siteId}/logs$`))
    await expect(page.getByRole('heading', { level: 1, name: `Logs: ${siteName}` })).toBeVisible()
    await expect(page.getByRole('status').filter({ hasText: /^Live$/ })).toBeVisible({ timeout: 30_000 })

    // A visitor's request to a path nobody else asks for: its access log
    // line arrives on the open stream, without reloading.
    const path = `/e2e-log-${randomUUID().slice(0, 8)}/`
    const log = page.getByRole('log', { name: 'Runtime log lines, oldest first' })
    const line = log.getByTestId('log-line').filter({ hasText: path })
    await expect(async () => {
      await harness.visitSite(siteId, path)
      await expect(line.first()).toBeVisible({ timeout: 10_000 })
    }).toPass({ timeout: 60_000 })
    await expect(line.first()).toContainText('access')

    // The same line, from the site's logs on the platform.
    const { data } = await adminApi.client.GET('/api/v1/sites/{site_id}/logs', { params: { path: { site_id: siteId }, query: { q: path } } })
    expect(data!.items.some(e => e.source === 'access' && e.message.includes(path))).toBe(true)
  })
})

test('the platform overview loads, and warns when site reservations exceed host memory', async ({ adminPage: page, adminApi, env }) => {
  test.setTimeout(6 * 60_000)
  const capacity = async () => (await adminApi.client.GET('/api/v1/platform/capacity')).data!
  // The host's memory comes from the collectors.
  await expect.poll(async () => (await capacity()).level, { timeout: collected, intervals: [5_000] }).not.toBe('unknown')
  const before = await capacity()
  expect(before.level).toBe('ok')

  await page.goto('/overview')
  await expect(page.getByRole('heading', { level: 1, name: 'Platform overview' })).toBeVisible()
  const panel = page.getByRole('region', { name: 'Capacity' })
  await expect(panel.getByRole('status').filter({ hasText: 'Memory reservations fit' })).toBeVisible()
  await expect(panel.getByText('Host memory', { exact: true })).toBeVisible()
  await expect(page.getByRole('region', { name: 'Platform health' }).getByRole('listitem').first()).toBeVisible()
  await expect(page.getByRole('region', { name: 'Host checks (doctor)' }).getByRole('listitem').first()).toBeVisible()

  // A site that reserves more memory than the host has crosses both
  // thresholds (90% and 100%), whatever the other specs' sites reserve.
  const reserve = Math.ceil(before.host_memory_mb! * 1.1)
  const site = await createSite(adminApi, env, { memory_reservation_mb: reserve, memory_limit_mb: reserve })
  try {
    const over = await capacity()
    expect(over.level).toBe('exceeded')
    expect(over.reserved_memory_mb).toBeGreaterThanOrEqual(reserve)

    await page.getByRole('button', { name: 'Refresh' }).click()
    await expect(panel.getByRole('alert').filter({ hasText: 'Memory over-committed' })).toBeVisible()
  } finally {
    await deleteSite(adminApi, site.id)
  }
  expect((await apiError(adminApi.client.GET('/api/v1/sites/{site_id}', { params: { path: { site_id: site.id } } }))).status).toBe(404)

  // Once it is gone, the warning goes too.
  expect((await capacity()).level).toBe('ok')
  await page.getByRole('button', { name: 'Refresh' }).click()
  await expect(panel.getByRole('status').filter({ hasText: 'Memory reservations fit' })).toBeVisible()
})
