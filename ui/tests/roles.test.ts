import { describe, expect, it } from 'vitest'
import { roleAtLeast } from '../app/utils/roles'

describe('roleAtLeast', () => {
  it('orders viewer < operator < admin and denies an unknown session', () => {
    expect(roleAtLeast('admin', 'operator')).toBe(true)
    expect(roleAtLeast('operator', 'operator')).toBe(true)
    expect(roleAtLeast('viewer', 'operator')).toBe(false)
    expect(roleAtLeast(undefined, 'viewer')).toBe(false)
  })
})
