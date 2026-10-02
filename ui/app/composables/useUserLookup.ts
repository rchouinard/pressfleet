import { userLabel } from '../utils/users'

/**
 * Users for select boxes and for naming the user behind an ID: the first 200
 * (the API's page maximum), far more than an agency has. Listing users is
 * Admin only, so use this on Admin pages. A failure leaves the lookup empty
 * rather than breaking the page; names then fall back to shortened IDs.
 */
export function useUserLookup() {
  const api = useApi()

  const { data } = useAsyncData('user-lookup', async () => {
    try {
      const { data } = await api.GET('/api/v1/users', { params: { query: { limit: 200 } } })
      return data?.items ?? []
    } catch {
      return []
    }
  })

  const users = computed(() => data.value ?? [])
  return {
    userItems: computed(() => users.value.map(u => ({ label: userLabel(u), value: u.id }))),
    userNames: computed<Record<string, string>>(() => Object.fromEntries(users.value.map(u => [u.id, userLabel(u)]))),
  }
}
