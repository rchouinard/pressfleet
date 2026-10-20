import { describe, expect, it } from 'vitest'
import { CHUNK_RELOAD_KEY, chunkReloadDone, reloadForChunkError, type ReloadStorage } from '../app/utils/chunk-reload'

function memoryStorage(): ReloadStorage & { items: Map<string, string> } {
  const items = new Map<string, string>()
  return {
    items,
    getItem: key => items.get(key) ?? null,
    setItem: (key, value) => void items.set(key, value),
    removeItem: key => void items.delete(key),
  }
}

const throwing: ReloadStorage = {
  getItem: () => { throw new Error('SecurityError') },
  setItem: () => { throw new Error('QuotaExceededError') },
  removeItem: () => { throw new Error('SecurityError') },
}

describe('reloadForChunkError', () => {
  it('reloads once during the first page load', () => {
    const storage = memoryStorage()
    expect(reloadForChunkError(storage, false)).toBe(true)
    expect(storage.items.has(CHUNK_RELOAD_KEY)).toBe(true)
    // The reloaded page fails again: no second reload.
    expect(reloadForChunkError(storage, false)).toBe(false)
  })

  it('does not reload once the page is up', () => {
    const storage = memoryStorage()
    expect(reloadForChunkError(storage, true)).toBe(false)
    expect(storage.items.size).toBe(0)
  })

  it('reloads again after a page loaded', () => {
    const storage = memoryStorage()
    expect(reloadForChunkError(storage, false)).toBe(true)
    chunkReloadDone(storage)
    expect(storage.items.size).toBe(0)
    expect(reloadForChunkError(storage, false)).toBe(true)
  })

  it('does not reload without a usable storage', () => {
    expect(reloadForChunkError(undefined, false)).toBe(false)
    expect(reloadForChunkError(throwing, false)).toBe(false)
    expect(() => chunkReloadDone(throwing)).not.toThrow()
    expect(() => chunkReloadDone(undefined)).not.toThrow()
  })
})
