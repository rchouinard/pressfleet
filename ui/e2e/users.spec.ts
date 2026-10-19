// User management (#82): an Admin creates a user of each role, changes a
// role, and deletes a user; Viewers and Operators don't see what their role
// forbids, and the API refuses it too.
import { apiError, createClientGroup, createUser, login, randomPassword, uniqueEmail, type Api, type Role } from './fixtures/api'
import { choose } from './fixtures/ui'
import { expect, test } from './fixtures'

const roleLabels: Record<Role, string> = { viewer: 'Viewer', operator: 'Operator', admin: 'Admin' }


async function getUser(api: Api, id: string) {
  const { data } = await api.client.GET('/api/v1/users/{user_id}', { params: { path: { user_id: id } } })
  return data!
}

test('creates a user of each role', async ({ adminPage: page, adminApi, env }) => {
  await page.goto('/users')
  for (const role of ['viewer', 'operator', 'admin'] as const) {
    const email = uniqueEmail(`new-${role}`)
    const password = randomPassword()
    await page.getByRole('button', { name: 'Add user' }).click()
    const dialog = page.getByRole('dialog', { name: 'Add user' })
    await dialog.getByLabel('Email').fill(email)
    await dialog.getByLabel('Starting password').fill(password)
    await choose(page, 'Role', roleLabels[role])
    await dialog.getByRole('button', { name: 'Create user' }).click()
    await expect(dialog).toBeHidden()

    const row = page.getByRole('row').filter({ has: page.getByRole('rowheader', { name: email }) })
    await expect(row.getByRole('cell').first()).toHaveText(role)

    // The account exists with that role and its password signs in.
    const session = await login(env.apiURL, email, password)
    const { data: me } = await session.client.GET('/api/v1/auth/me')
    expect(me).toMatchObject({ email, role })
    expect((await adminApi.client.GET('/api/v1/users', { params: { query: { limit: 200 } } })).data!.items.some(u => u.email === email)).toBe(true)
  }
})

test('changes a user\'s role', async ({ adminPage: page, adminApi }) => {
  const user = await createUser(adminApi, 'viewer')
  await page.goto('/users')
  await page.getByRole('button', { name: `Edit ${user.email}` }).click()
  const dialog = page.getByRole('dialog', { name: 'Edit user' })
  await choose(page, 'Role', 'Operator')
  await dialog.getByRole('button', { name: 'Save changes' }).click()
  await expect(dialog).toBeHidden()

  await expect(page.getByRole('row').filter({ has: page.getByRole('rowheader', { name: user.email }) }).getByRole('cell').first()).toHaveText('operator')
  expect((await getUser(adminApi, user.id)).role).toBe('operator')
})

test('deletes a user', async ({ adminPage: page, adminApi, env }) => {
  const user = await createUser(adminApi, 'operator')
  await page.goto('/users')
  await page.getByRole('button', { name: `Delete ${user.email}` }).click()
  const dialog = page.getByRole('dialog', { name: 'Delete this user?' })
  await dialog.getByRole('button', { name: 'Delete user' }).click()
  await expect(dialog).toBeHidden()
  await expect(page.getByRole('rowheader', { name: user.email })).toBeHidden()

  expect((await apiError(adminApi.client.GET('/api/v1/users/{user_id}', { params: { path: { user_id: user.id } } }))).status).toBe(404)
  await expect(login(env.apiURL, user.email, user.password)).rejects.toThrow(/status 401/)
})

test('a Viewer doesn\'t see what the role forbids, and the API refuses it', async ({ asRole, adminApi }) => {
  const { page, api } = await asRole('viewer')
  const group = await createClientGroup(adminApi)

  await page.goto('/client-groups')
  await expect(page.getByRole('link', { name: group.name })).toBeVisible()
  await expect(page.getByRole('button', { name: 'New client group' })).toBeHidden()
  await expect(page.getByRole('button', { name: `Edit ${group.name}` })).toBeHidden()
  await expect(page.getByRole('button', { name: `Delete ${group.name}` })).toBeHidden()

  await page.goto('/sites')
  await expect(page.getByRole('heading', { level: 1, name: 'Sites' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'New site' })).toBeHidden()
  await expect(page.getByRole('link', { name: 'Import site' })).toBeHidden()

  await expect(page.getByRole('link', { name: 'Users' })).toBeHidden()
  await page.goto('/users')
  await expect(page.getByRole('heading', { name: 'You do not have access' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Add user' })).toBeHidden()

  // The API is the authority.
  expect((await apiError(api.client.POST('/api/v1/client-groups', { body: { name: 'Not allowed' } }))).status).toBe(403)
  expect((await apiError(api.client.PATCH('/api/v1/client-groups/{client_group_id}', {
    params: { path: { client_group_id: group.id } }, body: { name: 'Renamed' },
  }))).status).toBe(403)
  expect((await apiError(api.client.GET('/api/v1/users'))).status).toBe(403)
})

test('an Operator doesn\'t see what the role forbids, and the API refuses it', async ({ asRole, adminApi }) => {
  const { page, api } = await asRole('operator')
  const group = await createClientGroup(adminApi)

  await page.goto('/client-groups')
  await expect(page.getByRole('button', { name: 'New client group' })).toBeVisible()
  await expect(page.getByRole('button', { name: `Edit ${group.name}` })).toBeVisible()
  await expect(page.getByRole('button', { name: `Delete ${group.name}` })).toBeHidden()

  await page.goto('/sites')
  await expect(page.getByRole('button', { name: 'New site' })).toBeVisible()

  await expect(page.getByRole('link', { name: 'Users' })).toBeHidden()
  await page.goto('/users')
  await expect(page.getByRole('heading', { name: 'You do not have access' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Add user' })).toBeHidden()

  expect((await apiError(api.client.DELETE('/api/v1/client-groups/{client_group_id}', {
    params: { path: { client_group_id: group.id } },
  }))).status).toBe(403)
  expect((await apiError(api.client.POST('/api/v1/users', {
    body: { email: uniqueEmail('nope'), password: randomPassword(), role: 'viewer' },
  }))).status).toBe(403)
  // The group is still there.
  await adminApi.client.GET('/api/v1/client-groups/{client_group_id}', { params: { path: { client_group_id: group.id } } })
})
