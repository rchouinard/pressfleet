/** The session's CSRF token, set by the login flow (#82) and by GET /api/v1/auth/me. */
export function useCsrfToken() {
  return useState<string | undefined>('csrf-token', () => undefined)
}

let client: ApiClient | undefined

/**
 * The shared, typed API client (see utils/api-client.ts). One instance per
 * page load, so every caller sees the same CSRF token and 401 handling.
 */
export function useApi(): ApiClient {
  const csrf = useCsrfToken()
  client ??= createApiClient({
    getCsrfToken: () => csrf.value,
    // #82 redirects to the login page here.
    onUnauthorized: () => {},
  })
  return client
}
