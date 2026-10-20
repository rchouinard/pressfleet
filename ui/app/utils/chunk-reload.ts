/**
 * The sessionStorage key that records a reload for a chunk that failed to
 * load. It lives as long as the tab, so a chunk that keeps failing gets one
 * reload, not a loop.
 */
export const CHUNK_RELOAD_KEY = 'pressfleet:chunk-reload'

/** The sessionStorage methods the guard uses. */
export type ReloadStorage = Pick<Storage, 'getItem' | 'setItem' | 'removeItem'>

/**
 * Decides whether a script chunk that failed to load should reload the page,
 * and if so records the reload. Only a failure during the first page load
 * reloads: the page is blank then, and a reload fetches the chunk again (a
 * dropped connection, or a new deployment whose index.html names other
 * chunks). Once the page is up, a failed chunk only fails what needed it.
 * Without a usable storage the guard can't work, so nothing reloads.
 */
export function reloadForChunkError(storage: ReloadStorage | undefined, firstLoadDone: boolean): boolean {
  if (firstLoadDone || !storage) return false
  try {
    if (storage.getItem(CHUNK_RELOAD_KEY) !== null) return false
    storage.setItem(CHUNK_RELOAD_KEY, String(Date.now()))
    return true
  } catch {
    // Storage disabled or full.
    return false
  }
}

/**
 * Forgets the reload once a page has loaded, so a later failure (after the
 * next deployment, say) may reload again.
 */
export function chunkReloadDone(storage: ReloadStorage | undefined): void {
  try {
    storage?.removeItem(CHUNK_RELOAD_KEY)
  } catch {
    // Nothing to forget.
  }
}
