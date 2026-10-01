import createClient, { type Middleware } from 'openapi-fetch'
import type { paths } from '../api/schema.gen'
import { ApiError, problemFromResponse } from './api-error'

export type ApiClient = ReturnType<typeof createClient<paths>>

export interface ApiClientOptions {
  /** Origin to send requests to. Defaults to the page's own origin. */
  baseUrl?: string
  /** Replaces the global fetch; tests inject a fake. */
  fetch?: (request: Request) => Promise<Response>
  /** The session's CSRF token, sent on unsafe methods (OpenAPI "CSRF protection"). */
  getCsrfToken?: () => string | undefined
  /** Called for every 401 before the ApiError is thrown, e.g. to go to /login. */
  onUnauthorized?: (error: ApiError) => void
  /**
   * Called with false when a request never got an answer (network failure)
   * and with true when any response arrives, so the shell can show an
   * "unreachable" banner and clear it again.
   */
  onReachable?: (reachable: boolean) => void
}

const safeMethods = new Set(['GET', 'HEAD', 'OPTIONS'])

/**
 * Creates the typed API client. This module is the only place in the UI that
 * calls fetch for the JSON API.
 *
 * Calls resolve with `{ data }` for 2xx responses and throw ApiError for
 * everything else, so `error` is never set on the result. Not for streams:
 * server-sent events use EventSource (see apiUrl) and binary uploads and
 * downloads use a plain fetch or an anchor against apiUrl, because the
 * wrapper reads bodies as JSON.
 */
export function createApiClient(options: ApiClientOptions = {}): ApiClient {
  const baseFetch = options.fetch ?? (request => globalThis.fetch(request))

  const client = createClient<paths>({
    baseUrl: options.baseUrl ?? globalThis.location?.origin ?? '',
    // The session cookie is HttpOnly and same-origin; never send it elsewhere.
    credentials: 'same-origin',
    fetch: async (request) => {
      try {
        const response = await baseFetch(request)
        options.onReachable?.(true)
        return response
      } catch (e) {
        // An aborted request (a poll cancelled on unmount) is not an outage.
        if (!request.signal.aborted) options.onReachable?.(false)
        throw new ApiError(
          {
            type: 'urn:pressfleet:ui:network',
            title: 'Network error',
            status: 0,
            detail: 'The control plane could not be reached.',
          },
          { cause: e },
        )
      }
    },
  })

  const middleware: Middleware = {
    onRequest({ request }) {
      const token = options.getCsrfToken?.()
      if (token && !safeMethods.has(request.method)) {
        request.headers.set('X-CSRF-Token', token)
      }
      return request
    },
    async onResponse({ response }) {
      if (response.ok) return undefined
      const error = await problemFromResponse(response)
      if (response.status === 401) options.onUnauthorized?.(error)
      throw error
    },
  }
  client.use(middleware)
  return client
}

/**
 * The URL of an API path for consumers that can't go through the client:
 * `new EventSource(apiUrl('/api/v1/sites/' + id + '/logs/stream'))`. The
 * path is relative, so the session cookie goes along (same origin).
 */
export function apiUrl(path: string): string {
  if (!path.startsWith('/')) throw new Error(`apiUrl: path must start with "/", got ${path}`)
  return path
}
