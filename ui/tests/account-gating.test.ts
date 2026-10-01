import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import { isLive, navFor, navRegistry } from '../app/utils/nav'
import { can, minimumRole } from '../app/utils/permissions'

const ids = (role: Parameters<typeof navFor>[0]) => navFor(role).filter(isLive).map(i => i.id)

describe('account and user pages: nav and role gating', () => {
  it('links Account to every role and Users to admins only', () => {
    expect(navRegistry.find(i => i.id === 'account')?.route).toBe('/account')
    expect(navRegistry.find(i => i.id === 'users')?.route).toBe('/users')
    for (const role of ['viewer', 'operator', 'admin'] as const) expect(ids(role)).toContain('account')
    expect(ids('admin')).toContain('users')
    expect(ids('operator')).not.toContain('users')
    expect(ids('viewer')).not.toContain('users')
  })

  it('lets only admins manage users, and everyone manage their own tokens', () => {
    for (const action of ['user.view', 'user.create', 'user.update'] as const) {
      expect(minimumRole(action)).toBe('admin')
      expect(can('operator', action)).toBe(false)
      expect(can('viewer', action)).toBe(false)
      expect(can('admin', action)).toBe(true)
    }
    expect(can('viewer', 'self.tokens.manage')).toBe(true)
    expect(can(undefined, 'self.tokens.manage')).toBe(false)
  })

  it('gates the users page on user.view and not the account page', () => {
    const users = readFileSync(resolve(process.cwd(), 'app/pages/users.vue'), 'utf8')
    const account = readFileSync(resolve(process.cwd(), 'app/pages/account.vue'), 'utf8')
    expect(users).toContain("requires: 'user.view'")
    expect(account).not.toContain('requires:')
  })
})

describe('token secrets stay out of storage', () => {
  it('never touches web storage in the account page or secret component', () => {
    for (const file of ['../app/pages/account.vue', '../app/components/token/TokenSecret.vue']) {
      const src = readFileSync(resolve(process.cwd(), file.replace('../', '')), 'utf8')
      expect(src).not.toMatch(/localStorage|sessionStorage|useCookie|console\./)
    }
  })
})
