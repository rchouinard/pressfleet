// Alerts (#90): a webhook channel and its test notification, creating and
// editing a rule, and an alert that fires and then resolves. The harness
// receives the webhooks and writes the series the rule reads into the
// metrics store; TestDashboard evaluates the rules every 5 seconds.
import { randomUUID } from 'node:crypto'
import { unique } from './fixtures/api'
import { choose } from './fixtures/ui'
import { expect, test } from './fixtures'
import type { Page } from '@playwright/test'

/** How long an alert may take to fire or resolve: a few evaluations. */
const evaluated = 60_000

test('creates a webhook channel and sends a test notification through it', async ({ adminPage: page, adminApi, harness }) => {
  const name = unique('E2E Webhook')
  const path = `/hooks/${randomUUID()}`
  const url = (await harness.webhookURL()) + path

  await page.goto('/alerts/channels')
  await page.getByRole('button', { name: 'New channel' }).click()
  const dialog = page.getByRole('dialog', { name: 'New alert channel' })
  await dialog.getByLabel(/^Name/).fill(name)
  await choose(page, 'Type', 'Webhook')
  await dialog.getByLabel(/^Webhook URL/).fill(url)
  await dialog.getByRole('button', { name: 'Create channel' }).click()
  await expect(dialog).toBeHidden()
  const row = page.getByRole('row').filter({ hasText: name })
  await expect(row).toBeVisible()

  const { data: list } = await adminApi.client.GET('/api/v1/alert-channels', { params: { query: { limit: 200 } } })
  const channel = list!.items.find(c => c.name === name)!
  try {
    expect(channel).toMatchObject({ type: 'webhook', enabled: true, min_severity: 'info', webhook: { format: 'generic' } })
    // The URL is a credential: the API shows its scheme and host only.
    expect(channel.webhook!.url).not.toContain(path)

    await row.getByRole('button', { name: `Send a test through ${name}` }).click()
    await expect(row.getByRole('status')).toHaveText(/Test delivered in \d+ ms\./)

    // The receiver got it, once, as the generic JSON format.
    const got = await harness.webhookRequests(path)
    expect(got).toHaveLength(1)
    expect(got[0]!.method).toBe('POST')
    expect(got[0]!.headers['content-type']).toContain('application/json')
    expect(got[0]!.body).toContain('Test notification from PressFleet')
    expect(got[0]!.body).toContain(name)
  } finally {
    await adminApi.client.DELETE('/api/v1/alert-channels/{alert_channel_id}', { params: { path: { alert_channel_id: channel.id } } })
  }
})

/** The alert history, filtered to one rule and a state, refreshed until the rule's alert shows. */
async function expectAlertInHistory(page: Page, ruleName: string, state: 'Firing' | 'Resolved') {
  await page.goto('/alerts')
  await choose(page, 'State', state)
  await choose(page, 'Rule', ruleName)
  const table = page.getByRole('region', { name: 'Alerts, newest first' })
  const row = table.getByRole('row').filter({ hasText: ruleName })
  await expect(async () => {
    await page.getByRole('button', { name: 'Refresh' }).click()
    await expect(row).toBeVisible({ timeout: 5_000 })
  }).toPass({ timeout: evaluated })
  await expect(row).toHaveCount(1)
  await expect(row.getByText(state, { exact: true })).toBeVisible()
  return row
}

test('creates and edits a rule, and its alert fires and then resolves', async ({ adminPage: page, adminApi, harness }) => {
  test.setTimeout(4 * 60_000)
  const name = unique('E2E Probe')
  // A series of its own, which nothing else writes.
  const key = randomUUID().slice(0, 8)
  const expr = `pressfleet_e2e_probe{e2e="${key}"}`

  await page.goto('/alerts/rules')
  await page.getByRole('button', { name: 'New rule' }).click()
  let dialog = page.getByRole('dialog', { name: 'New alert rule' })
  await dialog.getByLabel(/^Name/).fill(name)
  await dialog.getByLabel('Description').fill('Created by the dashboard tests.')
  await choose(page, 'Scope', 'Platform')
  await dialog.getByLabel(/^Expression/).fill(expr)
  await choose(page, 'Severity', 'Critical')
  await choose(page, 'Alert when the value', 'is greater than')
  await dialog.getByLabel(/^Threshold/).fill('10')
  await dialog.getByLabel(/^Hold for/).fill('0')
  await dialog.getByRole('button', { name: 'Create rule' }).click()
  await expect(dialog).toBeHidden()
  const row = page.getByRole('row').filter({ hasText: name })
  await expect(row).toBeVisible()
  await expect(row).toContainText('> 10')

  const { data: rules } = await adminApi.client.GET('/api/v1/alert-rules', { params: { query: { limit: 200 } } })
  const rule = rules!.items.find(r => r.name === name)!
  try {
    expect(rule).toMatchObject({
      scope: 'platform', kind: 'threshold', expr, operator: 'gt', threshold: 10, for_seconds: 0,
      severity: 'critical', enabled: true, builtin: false, description: 'Created by the dashboard tests.',
    })

    // Lower the threshold, so the value written below crosses it only if
    // the edit took effect.
    await row.getByRole('button', { name: `Edit ${name}` }).click()
    dialog = page.getByRole('dialog', { name: `Edit rule: ${name}` })
    await expect(dialog.getByLabel(/^Threshold/)).toHaveValue('10')
    await dialog.getByLabel(/^Threshold/).fill('5')
    await dialog.getByRole('button', { name: 'Save changes' }).click()
    await expect(dialog).toBeHidden()
    await expect(row).toContainText('> 5')
    const { data: edited } = await adminApi.client.GET('/api/v1/alert-rules/{alert_rule_id}', { params: { path: { alert_rule_id: rule.id } } })
    expect(edited).toMatchObject({ threshold: 5, operator: 'gt', expr })

    // 7 is above the new threshold and below the old one.
    await harness.writeSeries(`pressfleet_e2e_probe{e2e="${key}"} 7`)
    const history = await expectAlertInHistory(page, name, 'Firing')
    const { data: firing } = await adminApi.client.GET('/api/v1/alerts', { params: { query: { rule_id: rule.id } } })
    expect(firing!.items).toHaveLength(1)
    const alert = firing!.items[0]!
    expect(alert).toMatchObject({ state: 'firing', severity: 'critical', value: 7, resolved_at: null })
    expect(alert.fired_at).toBeTruthy()

    // Its page shows the same.
    await history.getByRole('link').first().click()
    await expect(page).toHaveURL(new RegExp(`/alerts/${alert.id}$`))
    await expect(page.getByText('Firing', { exact: true })).toBeVisible()
    await expect(page.getByText(`e2e=${key}`)).toBeVisible()

    // The condition ends: the series is gone.
    await harness.deleteSeries(`{e2e="${key}"}`)
    await expectAlertInHistory(page, name, 'Resolved')
    const { data: resolved } = await adminApi.client.GET('/api/v1/alerts/{alert_id}', { params: { path: { alert_id: alert.id } } })
    expect(resolved).toMatchObject({ id: alert.id, state: 'resolved' })
    expect(resolved!.resolved_at).toBeTruthy()
    // Nothing is firing for the rule any more.
    const { data: still } = await adminApi.client.GET('/api/v1/alerts', { params: { query: { rule_id: rule.id, state: 'firing' } } })
    expect(still!.items).toEqual([])

    await page.goto(`/alerts/${alert.id}`)
    await expect(page.getByText('Resolved', { exact: true }).first()).toBeVisible()
  } finally {
    await harness.deleteSeries(`{e2e="${key}"}`)
    await adminApi.client.DELETE('/api/v1/alert-rules/{alert_rule_id}', { params: { path: { alert_rule_id: rule.id } } })
  }
})
