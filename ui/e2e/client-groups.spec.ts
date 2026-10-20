// Client groups (#83): create, rename, delete, and offboard (delete with
// the backup repository and every backup in it).
import { apiError, createClientGroup, unique, type Api } from './fixtures/api'
import { jobStartedBy, waitForJobInUI } from './fixtures/jobs'
import { expect, test } from './fixtures'

async function groupGone(api: Api, id: string) {
  const err = await apiError(api.client.GET('/api/v1/client-groups/{client_group_id}', { params: { path: { client_group_id: id } } }))
  expect(err.status).toBe(404)
}

test('creates, renames, and deletes a client group', async ({ adminPage: page, adminApi }) => {
  const name = unique('Acme')
  await page.goto('/client-groups')

  await page.getByRole('button', { name: 'New client group' }).click()
  let dialog = page.getByRole('dialog', { name: 'New client group' })
  await dialog.getByLabel('Name').fill(name)
  await dialog.getByLabel('Notes').fill('Created by the dashboard tests.')
  await dialog.getByRole('button', { name: 'Create group' }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByRole('link', { name })).toBeVisible()

  const { data: list } = await adminApi.client.GET('/api/v1/client-groups', { params: { query: { limit: 200 } } })
  const group = list!.items.find(g => g.name === name)
  expect(group).toMatchObject({ notes: 'Created by the dashboard tests.', site_count: 0 })

  const renamed = unique('Acme Renamed')
  await page.getByRole('button', { name: `Edit ${name}` }).click()
  dialog = page.getByRole('dialog', { name: 'Edit client group' })
  await expect(dialog.getByLabel('Name')).toHaveValue(name)
  await dialog.getByLabel('Name').fill(renamed)
  await dialog.getByRole('button', { name: 'Save changes' }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByRole('link', { name: renamed })).toBeVisible()
  const { data: got } = await adminApi.client.GET('/api/v1/client-groups/{client_group_id}', { params: { path: { client_group_id: group!.id } } })
  expect(got!.name).toBe(renamed)

  await page.getByRole('button', { name: `Delete ${renamed}` }).click()
  dialog = page.getByRole('dialog', { name: `Delete ${renamed}?` })
  await dialog.getByRole('button', { name: 'Delete', exact: true }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByRole('link', { name: renamed })).toBeHidden()
  await groupGone(adminApi, group!.id)
})

test('offboards a client group with its backup repository', async ({ adminPage: page, adminApi, harness }) => {
  const group = await createClientGroup(adminApi, unique('Leaving Client'))
  await harness.createBackupRepository(group.id)
  const { data: withRepo } = await adminApi.client.GET('/api/v1/client-groups/{client_group_id}', { params: { path: { client_group_id: group.id } } })
  const location = withRepo!.backup_repository!.location
  expect(await harness.exists(location)).toBe(true)

  await page.goto('/client-groups')
  await page.getByRole('button', { name: `Delete ${group.name}` }).click()
  const dialog = page.getByRole('dialog', { name: `Delete ${group.name}?` })
  await expect(dialog.getByText('still has a backup repository')).toBeVisible()
  // Deleting the group alone is refused while its backups exist.
  await expect(dialog.getByRole('button', { name: 'Delete', exact: true })).toBeDisabled()
  await dialog.getByLabel('Also permanently delete the backup repository and every backup in it').check()
  const confirm = dialog.getByRole('button', { name: 'Delete group and backups' })
  await expect(confirm).toBeDisabled()
  await dialog.getByLabel(`Type ${group.name} to confirm`).fill(group.name)
  const job = await jobStartedBy(page, () => confirm.click())
  await expect(dialog).toBeHidden()

  // The page follows the deletion job to the end.
  await waitForJobInUI(page, job.id)
  await expect(page.getByRole('link', { name: group.name })).toBeHidden()

  await groupGone(adminApi, group.id)
  expect(await harness.exists(location)).toBe(false)
})
