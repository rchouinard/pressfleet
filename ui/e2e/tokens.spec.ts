// API tokens and SSH keys on the account page (#82, #116).
import { generateKeyPairSync } from 'node:crypto'
import { apiError, unique, withToken } from './fixtures/api'
import { toast } from './fixtures/ui'
import { expect, test } from './fixtures'

test('creates an API token, shown once, and revokes it', async ({ asRole, env }) => {
  const { page, user } = await asRole('operator')
  const name = unique('CI deploys')
  await page.goto('/account')

  await page.getByRole('button', { name: 'Create token' }).click()
  const form = page.getByRole('dialog', { name: 'Create API token' })
  await form.getByLabel('Name').fill(name)
  await form.getByRole('button', { name: 'Create token' }).click()

  const shown = page.getByRole('dialog', { name: 'Your new API token' })
  const field = shown.getByRole('textbox', { name: 'API token' })
  await expect(field).toHaveValue(/\S{20,}/)
  const token = await field.inputValue()

  // The token works, as the user.
  const { data: me } = await withToken(env.apiURL, token).GET('/api/v1/auth/me')
  expect(me).toMatchObject({ email: user.email, auth_method: 'token' })

  // Once the dialog is closed the secret is gone from the page, and the
  // list shows only its prefix.
  await shown.getByRole('button', { name: 'Done, I\'ve copied it' }).click()
  await expect(shown).toBeHidden()
  const tokens = page.getByRole('region', { name: 'Your API tokens' })
  const row = tokens.getByRole('row').filter({ has: page.getByRole('rowheader', { name: new RegExp(name) }) })
  await expect(row).toBeVisible()
  await expect(page.getByText(token)).toHaveCount(0)
  expect(await page.content()).not.toContain(token)
  await page.reload()
  await expect(tokens.getByRole('rowheader', { name: new RegExp(name) })).toBeVisible()
  expect(await page.content()).not.toContain(token)

  await page.getByRole('button', { name: `Revoke token ${name}` }).click()
  const confirm = page.getByRole('dialog', { name: 'Revoke this token?' })
  await confirm.getByRole('button', { name: 'Revoke token' }).click()
  await expect(confirm).toBeHidden()
  await expect(toast(page, 'Token revoked')).toBeVisible()

  expect((await apiError(withToken(env.apiURL, token).GET('/api/v1/auth/me'))).status).toBe(401)
})

/** A new Ed25519 public key in OpenSSH's authorized_keys format. */
function sshPublicKey(comment: string): string {
  const { publicKey } = generateKeyPairSync('ed25519')
  const raw = Buffer.from(publicKey.export({ format: 'jwk' }).x!, 'base64url')
  // The wire format: each field is a uint32 length, then the bytes.
  const field = (b: Buffer) => {
    const length = Buffer.alloc(4)
    length.writeUInt32BE(b.length)
    return Buffer.concat([length, b])
  }
  const type = Buffer.from('ssh-ed25519')
  return `ssh-ed25519 ${Buffer.concat([field(type), field(raw)]).toString('base64')} ${comment}`
}

test('adds an SSH key and removes it', async ({ asRole }) => {
  const { page, api } = await asRole('operator')
  const name = unique('laptop')
  await page.goto('/account')

  await page.getByRole('button', { name: 'Add SSH key' }).click()
  const dialog = page.getByRole('dialog', { name: 'Add SSH key' })
  await dialog.getByLabel('Public key').fill(sshPublicKey('e2e@laptop'))
  await dialog.getByLabel('Name').fill(name)
  await dialog.getByRole('button', { name: 'Add key' }).click()
  await expect(dialog).toBeHidden()

  const keys = page.getByRole('region', { name: 'Your SSH keys' })
  await expect(keys.getByRole('rowheader', { name: new RegExp(name) })).toBeVisible()
  const { data: listed } = await api.client.GET('/api/v1/users/me/ssh-keys')
  const key = listed!.items.find(k => k.name === name)
  expect(key).toMatchObject({ type: 'ssh-ed25519' })
  await expect(keys.getByRole('cell', { name: key!.fingerprint })).toBeVisible()

  await page.getByRole('button', { name: `Remove key ${name}` }).click()
  const confirm = page.getByRole('dialog', { name: `Remove ${name}?` })
  await confirm.getByRole('button', { name: 'Remove key' }).click()
  await expect(confirm).toBeHidden()
  await expect(page.getByText('You have no SSH keys.')).toBeVisible()
  expect((await api.client.GET('/api/v1/users/me/ssh-keys')).data!.items).toEqual([])
})
