import { describe, expect, it } from 'vitest'
import {
  createPayload, siteCountLabel, updatePayload, validateClientGroup,
} from '../app/utils/client-group'
import { can } from '../app/utils/permissions'
import { isActive, navRegistry } from '../app/utils/nav'

describe('validateClientGroup', () => {
  it('requires a name and bounds name and notes like the spec', () => {
    expect(validateClientGroup({ name: '  ', notes: '' }).name).toBeDefined()
    expect(validateClientGroup({ name: 'a'.repeat(101), notes: '' }).name).toBeDefined()
    expect(validateClientGroup({ name: 'a'.repeat(100), notes: '' })).toEqual({})
    expect(validateClientGroup({ name: 'Acme', notes: 'n'.repeat(10001) }).notes).toBeDefined()
  })
})

describe('payloads', () => {
  it('create trims the name and omits empty notes', () => {
    expect(createPayload({ name: ' Acme ', notes: '' })).toEqual({ name: 'Acme' })
    expect(createPayload({ name: 'Acme', notes: 'VIP' })).toEqual({ name: 'Acme', notes: 'VIP' })
  })

  it('update sends only changed fields, and nothing when unchanged', () => {
    const original = { name: 'Acme', notes: 'VIP' }
    expect(updatePayload(original, { name: 'Acme', notes: 'VIP' })).toBeUndefined()
    expect(updatePayload(original, { name: 'Acme Corp', notes: 'VIP' })).toEqual({ name: 'Acme Corp' })
    // Clearing the notes is a change.
    expect(updatePayload(original, { name: 'Acme', notes: '' })).toEqual({ notes: '' })
  })
})

describe('siteCountLabel', () => {
  it('pluralizes', () => {
    expect([0, 1, 2].map(siteCountLabel)).toEqual(['0 sites', '1 site', '2 sites'])
  })
})

describe('client group role gating', () => {
  it('viewers only view, operators create and edit, admins also delete', () => {
    const rows = [
      ['viewer', true, false, false, false],
      ['operator', true, true, true, false],
      ['admin', true, true, true, true],
    ] as const
    for (const [role, view, create, update, del] of rows) {
      expect(can(role, 'client_group.view')).toBe(view)
      expect(can(role, 'client_group.create')).toBe(create)
      expect(can(role, 'client_group.update')).toBe(update)
      expect(can(role, 'client_group.delete')).toBe(del)
    }
    expect(can(undefined, 'client_group.view')).toBe(false)
  })
})

describe('client groups nav entry', () => {
  it('links to /client-groups', () => {
    const item = navRegistry.find(i => i.id === 'client-groups')!
    expect(item.route).toBe('/client-groups')
    expect(isActive(item, '/client-groups')).toBe(true)
  })
})
