import { describe, expect, it } from 'vitest'
import {
  expiresAtFor, expiryChoices, formatTimestamp, scopeView, tokenCreateBody, tokenStatus, validateTokenForm,
} from '../app/utils/tokens'

const now = new Date('2026-10-01T12:00:00Z')

describe('scopeView', () => {
  it('labels both scopes with an icon and survives unknown ones', () => {
    expect(scopeView('full').label).toBe('Full')
    expect(scopeView('read-only').label).toBe('Read-only')
    expect(scopeView('read-only').icon).toMatch(/^i-lucide-/)
    expect(scopeView('future').label).toBe('future')
  })
})

describe('expiresAtFor', () => {
  it('is null for never and a future timestamp otherwise', () => {
    expect(expiresAtFor('never', now)).toBeNull()
    expect(expiresAtFor('7', now)).toBe('2026-10-08T12:00:00.000Z')
    expect(expiresAtFor('365', now)).toBe('2027-10-01T12:00:00.000Z')
  })

  it('offers never plus fixed periods', () => {
    expect(expiryChoices.map(c => c.value)).toContain('never')
  })
})

describe('validateTokenForm', () => {
  it('requires a name of at most 100 characters', () => {
    expect(validateTokenForm({ name: '   ' }).name).toBeTruthy()
    expect(validateTokenForm({ name: 'x'.repeat(101) }).name).toBeTruthy()
    expect(validateTokenForm({ name: 'CI deploys' })).toEqual({})
  })
})

describe('tokenCreateBody', () => {
  it('trims the name and carries scope and expiry', () => {
    expect(tokenCreateBody({ name: ' CI ', scope: 'read-only', expiry: '30' }, now)).toEqual({
      name: 'CI',
      scope: 'read-only',
      expires_at: '2026-10-31T12:00:00.000Z',
    })
    expect(tokenCreateBody({ name: 'a', scope: 'full', expiry: 'never' }, now).expires_at).toBeNull()
  })
})

describe('tokenStatus', () => {
  it('marks tokens past their expiry as expired, with text and icon', () => {
    expect(tokenStatus({ expires_at: '2026-09-01T00:00:00Z' }, now).label).toBe('Expired')
    expect(tokenStatus({ expires_at: '2026-11-01T00:00:00Z' }, now).label).toBe('Active')
    expect(tokenStatus({ expires_at: null }, now).label).toBe('Active')
    expect(tokenStatus({ expires_at: null }, now).icon).toMatch(/^i-lucide-/)
  })
})

describe('formatTimestamp', () => {
  it('uses the placeholder for null and passes through unparseable input', () => {
    expect(formatTimestamp(null, 'Never used')).toBe('Never used')
    expect(formatTimestamp('not a date')).toBe('not a date')
    expect(formatTimestamp('2026-10-01T12:00:00Z')).not.toBe('Never')
  })
})
