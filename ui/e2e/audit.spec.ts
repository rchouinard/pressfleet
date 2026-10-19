// Audit log (#105): actions taken in the dashboard are recorded with their
// actor and target, and the audit log's filters narrow the list as the
// API does.
import { unique } from './fixtures/api'
import { choose } from './fixtures/ui'
import { expect, test } from './fixtures'
import type { Page } from '@playwright/test'

async function applyFilters(page: Page) {
  await page.getByRole('button', { name: 'Apply filters' }).click()
}

test('actions in the dashboard appear in the audit log, and its filters narrow it', async ({ adminPage: page, adminApi }) => {
  // Two actions through the UI: create a client group and rename it.
  const name = unique('Audited')
  const renamed = unique('Audited Renamed')
  await page.goto('/client-groups')
  await page.getByRole('button', { name: 'New client group' }).click()
  let dialog = page.getByRole('dialog', { name: 'New client group' })
  await dialog.getByLabel('Name').fill(name)
  await dialog.getByRole('button', { name: 'Create group' }).click()
  await expect(dialog).toBeHidden()
  await page.getByRole('button', { name: `Edit ${name}` }).click()
  dialog = page.getByRole('dialog', { name: 'Edit client group' })
  await dialog.getByLabel('Name').fill(renamed)
  await dialog.getByRole('button', { name: 'Save changes' }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByRole('link', { name: renamed })).toBeVisible()

  const { data: groups } = await adminApi.client.GET('/api/v1/client-groups', { params: { query: { limit: 200 } } })
  const group = groups!.items.find(g => g.name === renamed)!
  const { data: me } = await adminApi.client.GET('/api/v1/auth/me')

  // The platform recorded both, by the admin, against the group.
  const { data: recorded } = await adminApi.client.GET('/api/v1/audit-events', { params: { query: { target_id: group.id } } })
  const events = recorded!.items
  expect(events.map(e => e.action).sort()).toEqual(['client_group.create', 'client_group.update'])
  for (const e of events) expect(e).toMatchObject({ actor_user_id: me!.id, target_type: 'client_group', target_id: group.id, result: 'success' })

  await page.getByRole('link', { name: 'Audit log' }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'Audit log' })).toBeVisible()
  const table = page.getByRole('region', { name: 'Audit events, newest first' })
  const rows = table.getByRole('row').filter({ has: page.getByRole('cell') })
  // Other specs are busy too, so the unfiltered log has more than these.
  await expect(rows.first()).toBeVisible()

  // By target: the two events, newest first.
  await page.getByLabel('Target ID').fill(group.id)
  await applyFilters(page)
  await expect(rows).toHaveCount(2)
  await expect(rows.nth(0)).toContainText('client_group.update')
  await expect(rows.nth(1)).toContainText('client_group.create')
  for (const row of await rows.all()) {
    await expect(row).toContainText('Client group')
    await expect(row).toContainText('Success')
  }

  // And by action, and by user: the rename.
  await page.getByLabel('Action').fill('client_group.update')
  await choose(page, 'User', me!.display_name.trim() || me!.email)
  await applyFilters(page)
  await expect(rows).toHaveCount(1)
  await expect(rows.first()).toContainText('client_group.update')
  await expect(rows.first()).toContainText(group.id.slice(0, 8))

  // A target type that doesn't match: nothing.
  await choose(page, 'Target type', 'Site')
  await applyFilters(page)
  await expect(page.getByText('No events match these filters.')).toBeVisible()
  await expect(rows).toHaveCount(0)

  // Clearing shows everything again.
  await page.getByRole('button', { name: 'Clear' }).click()
  await expect(page.getByLabel('Target ID')).toHaveValue('')
  await expect.poll(() => rows.count()).toBeGreaterThan(2)
})
