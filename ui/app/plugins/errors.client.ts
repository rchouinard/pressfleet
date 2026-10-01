import { ApiError } from '../utils/api-error'

/**
 * Last-resort error handling. Failures a page handles itself never get here;
 * this catches what escapes: a rejected promise nobody awaited (typically an
 * ApiError from an event handler) and errors thrown while rendering or in a
 * handler. They become a toast (with the request ID for API errors) and a
 * console entry, so the app stays usable instead of going blank. Full-page
 * error states (403, 404, 5xx on load) belong to error.vue.
 */
export default defineNuxtPlugin((nuxtApp) => {
  // Don't flood the user when the same failure repeats in a loop.
  let last = ''
  let lastAt = 0

  function report(e: unknown, source: string) {
    // Unauthorized is handled by the API client (redirect to login).
    if (e instanceof ApiError && e.status === 401) return
    console.error(`[pressfleet] ${source}`, e)
    const key = e instanceof Error ? `${e.name}:${e.message}` : String(e)
    const now = Date.now()
    if (key === last && now - lastAt < 5000) return
    last = key
    lastAt = now
    // Raw messages and stack traces stay in the console.
    const shown = e instanceof ApiError
      ? e
      : new ApiError({ type: 'urn:pressfleet:ui:unexpected', title: 'Something went wrong', status: 0 })
    nuxtApp.runWithContext(() => useNotify().error(shown))
  }

  nuxtApp.hook('vue:error', e => report(e, 'vue:error'))
  window.addEventListener('unhandledrejection', (event) => {
    report(event.reason, 'unhandledrejection')
  })
})
