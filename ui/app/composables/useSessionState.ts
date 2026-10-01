import type { components } from '../api/schema.gen'

export type CurrentUser = components['schemas']['CurrentUser']

/**
 * The signed-in user: `undefined` until the session has been asked about
 * (once per page load, by the route middleware), `null` when nobody is
 * signed in. Read it through `useSession()`.
 */
export function useSessionUser() {
  return useState<CurrentUser | null | undefined>('session-user', () => undefined)
}

/** False while requests are failing at the network level (see the offline banner). */
export function useApiReachable() {
  return useState<boolean>('api-reachable', () => true)
}

/**
 * While above zero, 401s are expected (the session probe at startup) and the
 * API client's `onUnauthorized` redirect stays quiet; the caller handles it.
 */
export const sessionProbe = { active: 0 }
