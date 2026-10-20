import { chunkReloadDone, reloadForChunkError, type ReloadStorage } from '../utils/chunk-reload'

/**
 * Reloads the page once when a script chunk fails to load before the first
 * page has rendered, which would otherwise leave it blank. Nuxt's own
 * handler only reloads for the chunk of a route being navigated to; this
 * covers any chunk (Nuxt's app:chunkError, from a failed dynamic import). A
 * sessionStorage flag allows one reload per failure, so a chunk that keeps
 * failing can't loop.
 */
export default defineNuxtPlugin((nuxtApp) => {
  const storage = sessionStorageOrUndefined()
  let firstLoadDone = false

  nuxtApp.hook('app:chunkError', () => {
    if (reloadForChunkError(storage, firstLoadDone)) window.location.reload()
  })
  nuxtApp.hook('app:suspense:resolve', () => {
    firstLoadDone = true
    chunkReloadDone(storage)
  })
})

function sessionStorageOrUndefined(): ReloadStorage | undefined {
  try {
    return window.sessionStorage
  } catch {
    // Blocked by the browser's settings.
    return undefined
  }
}
