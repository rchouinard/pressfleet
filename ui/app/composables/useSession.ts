import { factorChallenge, type FactorChallenge, type FactorProof } from '../utils/two-factor'

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

  /**
   * Finishes a login that answered "second factor required" (see
   * `secondFactorChallenge`). Throws ApiError: 422 for a wrong proof (the
   * login stays pending), 429 throttled, 401 when the pending login expired.
   */
  async function loginSecondFactor(proof: FactorProof): Promise<CurrentUser> {
    if (proof.method === 'password') throw new Error('A password is not a second factor.')
    const { data } = await api.POST('/api/v1/auth/login/second-factor', { body: proof })
    if (!data) throw toApiError(new Error('The login response was empty.'))
    apply(data)
    return data
  }

  /** The ways the signed-in user can reauthenticate, with a fresh WebAuthn challenge. */
  async function reauthenticationOptions(): Promise<FactorChallenge> {
    const { data } = await api.POST('/api/v1/auth/reauthenticate/options')
    return factorChallenge(data ?? {})
  }

  /** Proves again who the user is, for sensitive actions. Throws ApiError (422 wrong proof, 429 throttled). */
  async function reauthenticate(proof: FactorProof): Promise<void> {
    const { data } = await api.POST('/api/v1/auth/reauthenticate', { body: proof })
    if (data) apply(data)
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
    loginSecondFactor,
    reauthenticationOptions,
    reauthenticate,
    logout,
  }
}
