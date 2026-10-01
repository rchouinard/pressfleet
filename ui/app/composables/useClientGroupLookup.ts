/**
 * Client groups for select boxes and for naming a site's group: the first
 * 200 (the API's page maximum), far more than an agency has. A failure leaves
 * the lookup empty rather than breaking the page; names then fall back to
 * shortened IDs.
 */
export function useClientGroupLookup() {
  const api = useApi()

  const { data } = useAsyncData('client-group-lookup', async () => {
    try {
      const { data } = await api.GET('/api/v1/client-groups', { params: { query: { limit: 200 } } })
      return data?.items ?? []
    } catch {
      return []
    }
  })

  const groups = computed(() => data.value ?? [])
  return {
    groupItems: computed(() => groups.value.map(g => ({ label: g.name, value: g.id }))),
    groupNames: computed<Record<string, string>>(() => Object.fromEntries(groups.value.map(g => [g.id, g.name]))),
  }
}
