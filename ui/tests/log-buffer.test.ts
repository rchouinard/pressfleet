import { describe, expect, it } from 'vitest'
import { LogBuffer } from '../app/utils/log-buffer'

describe('log buffer', () => {
  it('rejects a capacity that is not a positive integer', () => {
    expect(() => new LogBuffer(0)).toThrow(RangeError)
    expect(() => new LogBuffer(1.5)).toThrow(RangeError)
  })

  it('appends in order and drops the oldest lines past capacity', () => {
    const b = new LogBuffer<number>(5)
    expect(b.append([1, 2, 3])).toBe(0)
    expect(b.append([4, 5, 6, 7])).toBe(2)
    expect(b.items).toEqual([3, 4, 5, 6, 7])
    expect(b.dropped).toBe(2)
  })

  it('keeps only the newest lines of a batch larger than the capacity', () => {
    const b = new LogBuffer<number>(3)
    b.append([1, 2, 3, 4, 5, 6])
    expect(b.items).toEqual([4, 5, 6])
    expect(b.dropped).toBe(3)
  })

  it('replaces items rather than mutating them, so a shallowRef sees every change', () => {
    const b = new LogBuffer<number>(5)
    b.append([1])
    const before = b.items
    b.append([2])
    expect(before).toEqual([1])
    expect(b.items).not.toBe(before)
  })

  it('prepends older lines above the first one', () => {
    const b = new LogBuffer<number>(10)
    b.append([5, 6])
    expect(b.prepend([3, 4])).toBe(2)
    expect(b.items).toEqual([3, 4, 5, 6])
    expect(b.dropped).toBe(0)
  })

  it('never pushes the newest lines out to make room for history', () => {
    const b = new LogBuffer<number>(4)
    b.append([7, 8, 9])
    // Only one slot is free: the newest of the older lines (closest to the first line) is kept.
    expect(b.prepend([4, 5, 6])).toBe(1)
    expect(b.items).toEqual([6, 7, 8, 9])
    expect(b.prepend([1, 2, 3])).toBe(0)
    expect(b.items).toEqual([6, 7, 8, 9])
  })

  it('clears lines and the dropped count', () => {
    const b = new LogBuffer<number>(2)
    b.append([1, 2, 3])
    b.clear()
    expect(b.items).toEqual([])
    expect(b.dropped).toBe(0)
    expect(b.free).toBe(2)
  })
})
