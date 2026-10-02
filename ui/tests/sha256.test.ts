import { createHash } from 'node:crypto'
import { describe, expect, it } from 'vitest'
import { Sha256, sha256File } from '../app/utils/sha256'

function reference(data: Uint8Array): string {
  return createHash('sha256').update(data).digest('hex')
}

function bytes(n: number): Uint8Array {
  const b = new Uint8Array(n)
  for (let i = 0; i < n; i++) b[i] = (i * 31 + 7) & 0xff
  return b
}

describe('Sha256', () => {
  it('matches the known vectors', () => {
    expect(new Sha256().digest()).toBe('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855')
    expect(new Sha256().update(new TextEncoder().encode('abc')).digest()).toBe('ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad')
  })

  it('matches Node for lengths around the block and padding boundaries', () => {
    for (const n of [1, 54, 55, 56, 57, 63, 64, 65, 119, 120, 128, 1000, 100_000]) {
      const data = bytes(n)
      expect(new Sha256().update(data).digest(), `length ${n}`).toBe(reference(data))
    }
  })

  it('gives the same digest however the input is split', () => {
    const data = bytes(10_000)
    for (const step of [1, 7, 63, 64, 65, 1000]) {
      const h = new Sha256()
      for (let i = 0; i < data.length; i += step) h.update(data.subarray(i, i + step))
      expect(h.digest(), `step ${step}`).toBe(reference(data))
    }
  })
})

describe('sha256File', () => {
  it('hashes a blob in slices and reports progress', async () => {
    const data = bytes(10_000)
    const blob = new Blob([data as BlobPart])
    const seen: number[] = []
    const digest = await sha256File(blob, done => seen.push(done), undefined, 4096)
    expect(digest).toBe(reference(data))
    expect(seen).toEqual([4096, 8192, 10_000])
  })

  it('stops when aborted', async () => {
    const c = new AbortController()
    c.abort()
    await expect(sha256File(new Blob(['x']), undefined, c.signal)).rejects.toThrow()
  })
})
