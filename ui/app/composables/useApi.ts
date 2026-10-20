/** The session's CSRF token, set by login and by GET /api/v1/auth/me (see useSession). */
export function useCsrfToken() {
  return useState<string | undefined>('csrf-token', () => undefined)
}

let client: ApiClient | undefined

/**
 * The shared, typed API client (see utils/api-client.ts). One instance per
 * page load, so every caller sees the same CSRF token, 401 handling and
 * reachability tracking.
 *
 * A 401 anywhere (the session expired, was revoked, or the user was
 * disabled) clears the session and sends the user to /login with the page
 * they were on as `redirect`, unless it is the startup probe or the login
 * request itself, which handle their own 401.
 *
 * A sensitive action refused until the session reauthenticates opens the
 * reauthentication dialog (useReauthentication, mounted in app.vue) and is
 * sent again once the user has; see utils/api-client.ts.
 *
 * A request refused because the session must enroll a second factor (it
 * became restricted mid-use) reloads the session and sends the user to the
 * setup page at once, with the page they were on as `redirect`; see
 * enrollmentRequiredHandler in utils/two-factor.ts.
 */
export function useApi(): ApiClient {
  if (client) return client
  const csrf = useCsrfToken()
  const user = useSessionUser()
  const reachable = useApiReachable()
  const router = useRouter()
  const reauthentication = useReauthentication()
  client = createApiClient({
    getCsrfToken: () => csrf.value,
    onReachable: (ok) => {
      reachable.value = ok
    },
    onUnauthorized: () => {
      if (sessionProbe.active > 0) return
      const route = router.currentRoute.value
      if (route.path === '/login') return
      const wasSignedIn = !!user.value
      user.value = null
      csrf.value = undefined
      void router.replace(loginLocation(route.fullPath, { expired: wasSignedIn }))
    },
    onReauthenticationRequired: error => reauthentication.ask(error),
    onEnrollmentRequired: enrollmentRequiredHandler({
      route: () => router.currentRoute.value,
      // As useSession's refresh, which can't be used here: it needs this client.
      refresh: async () => {
        const { data } = await client!.GET('/api/v1/auth/me')
        user.value = data ?? null
        csrf.value = data?.csrf_token ?? undefined
        return user.value
      },
      replace: to => router.replace(to),
    }),
  })
  return client
}
