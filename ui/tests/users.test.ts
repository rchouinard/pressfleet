import { describe, expect, it } from 'vitest'
import { isSelf, userCreateBody, userLabel, userStatus, userUpdateBody, validateCreateUser, validateDisplayName } from '../app/utils/users'

describe('validateCreateUser', () => {
  it('accepts a good email and a 12+ character password', () => {
    expect(validateCreateUser({ email: 'a@example.com', password: 'x'.repeat(12), role: 'viewer' })).toEqual({})
  })

  it('flags a bad email and a short password', () => {
    const errors = validateCreateUser({ email: 'nope', password: 'short', role: 'viewer' })
    expect(errors.email).toBeTruthy()
    expect(errors.password).toContain('12')
  })

  it('flags an over-long password', () => {
    expect(validateCreateUser({ email: 'a@b.co', password: 'x'.repeat(257), role: 'admin' }).password).toBeTruthy()
  })
})

describe('userCreateBody', () => {
  it('trims the email and keeps the password untouched', () => {
    expect(userCreateBody({ email: ' a@b.co ', password: ' pw with spaces ', role: 'operator' })).toEqual({
      email: 'a@b.co',
      password: ' pw with spaces ',
      role: 'operator',
    })
  })
})

describe('userUpdateBody', () => {
  const user = { role: 'viewer', disabled: false, display_name: '' } as const

  it('sends only what changed', () => {
    expect(userUpdateBody(user, { role: 'admin', disabled: false })).toEqual({ role: 'admin' })
    expect(userUpdateBody(user, { role: 'viewer', disabled: true })).toEqual({ disabled: true })
    expect(userUpdateBody(user, { role: 'operator', disabled: true })).toEqual({ role: 'operator', disabled: true })
  })

  it('has nothing to send when nothing changed', () => {
    expect(userUpdateBody(user, { role: 'viewer', disabled: false })).toBeUndefined()
  })
})

describe('userStatus and isSelf', () => {
  it('shows disabled accounts with text and icon', () => {
    expect(userStatus({ disabled: true }).label).toBe('Disabled')
    expect(userStatus({ disabled: false }).label).toBe('Active')
  })

  it('recognises the signed-in user', () => {
    expect(isSelf({ id: '1' }, { id: '1' })).toBe(true)
    expect(isSelf({ id: '1' }, { id: '2' })).toBe(false)
    expect(isSelf({ id: '1' }, undefined)).toBe(false)
  })
})

describe('display names', () => {
  it('falls back to the email', () => {
    expect(userLabel({ email: 'a@example.com', display_name: '' })).toBe('a@example.com')
    expect(userLabel({ email: 'a@example.com', display_name: ' Alex ' })).toBe('Alex')
  })

  it('validates length and control characters', () => {
    expect(validateDisplayName('Alex Rivera')).toBeUndefined()
    expect(validateDisplayName('')).toBeUndefined()
    expect(validateDisplayName('x'.repeat(101))).toBeTruthy()
    expect(validateDisplayName('a\nb')).toBeTruthy()
  })

  it('sends a changed or cleared display name, and only when given', () => {
    const user = { role: 'viewer', disabled: false, display_name: 'Alex' } as const
    expect(userUpdateBody(user, { role: 'viewer', disabled: false, displayName: ' Sam ' })).toEqual({ display_name: 'Sam' })
    expect(userUpdateBody(user, { role: 'viewer', disabled: false, displayName: '' })).toEqual({ display_name: '' })
    expect(userUpdateBody(user, { role: 'viewer', disabled: false, displayName: 'Alex' })).toBeUndefined()
    expect(userUpdateBody(user, { role: 'viewer', disabled: false })).toBeUndefined()
  })

  it('omits an empty display name when creating', () => {
    expect(userCreateBody({ email: 'a@b.co', displayName: ' ', password: 'x'.repeat(12), role: 'viewer' })).not.toHaveProperty('display_name')
    expect(userCreateBody({ email: 'a@b.co', displayName: 'Alex', password: 'x'.repeat(12), role: 'viewer' }).display_name).toBe('Alex')
  })
})
