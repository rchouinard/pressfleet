/**
 * The current session: who is signed in, their role and CSRF token, and the
 * actions that change it. The route middleware (`middleware/auth.global.ts`)
 * loads it once per page load; components just read it.
 *
 * `user` is `undefined` before the first load, `null` when signed out.
 */
export function useSession() {
  const user = useSessionUser()
  const csrf = useCsrfToken()
  const loading = useState('session-loading', () => false)
  const api = useApi()

  function apply(current: CurrentUser | null) {
    user.value = current
    csrf.value = current?.csrf_token ?? undefined
  }

  /**
   * Asks the server who is signed in (GET /api/v1/auth/me). A 401 means
   * signed out and is not an error; anything else (network, 5xx) throws an
   * ApiError and leaves the previous state alone.
   */
  async function refresh(): Promise<CurrentUser | null> {
    loading.value = true
    sessionProbe.active++
    try {
      const { data } = await api.GET('/api/v1/auth/me')
      apply(data ?? null)
      return user.value ?? null
    } catch (e) {
      const error = toApiError(e)
      if (error.status === 401) {
        apply(null)
        return null
      }
      throw error
    } finally {
      sessionProbe.active--
      loading.value = false
    }
  }

  /** Signs in. Throws ApiError (401 for bad credentials, 422 with field errors, 429 throttled). */
  async function login(email: string, password: string): Promise<CurrentUser> {
    const { data } = await api.POST('/api/v1/auth/login', { body: { email, password } })
    if (!data) throw toApiError(new Error('The login response was empty.'))
    apply(data)
    return data
  }

  /** Ends the session and returns to the login page. A 401 means it was already over. */
  async function logout(): Promise<void> {
    try {
      await api.POST('/api/v1/auth/logout')
    } catch (e) {
      if (toApiError(e).status !== 401) throw e
    }
    apply(null)
    await navigateTo('/login', { replace: true })
  }

  return {
    user,
    role: computed(() => user.value?.role),
    csrfToken: csrf,
    loading,
    isAuthenticated: computed(() => !!user.value),
    loaded: computed(() => user.value !== undefined),
    refresh,
    login,
    logout,
  }
}
