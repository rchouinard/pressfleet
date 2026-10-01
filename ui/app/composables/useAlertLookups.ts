/**
 * Rules and client groups for the alert filters and the channel form's
 * "what it receives" limits: the first 200 of each (the API's page maximum),
 * which is far more than an agency has. A failure leaves the lists empty
 * rather than breaking the page; the filters then show IDs only.
 */
export function useAlertLookups() {
  const api = useApi()

  const { data } = useAsyncData('alert-lookups', async () => {
    const [rules, groups] = await Promise.allSettled([
      api.GET('/api/v1/alert-rules', { params: { query: { limit: 200 } } }),
      api.GET('/api/v1/client-groups', { params: { query: { limit: 200 } } }),
    ])
    return {
      rules: rules.status === 'fulfilled' ? (rules.value.data?.items ?? []) : [],
      groups: groups.status === 'fulfilled' ? (groups.value.data?.items ?? []) : [],
    }
  })

  const rules = computed(() => data.value?.rules ?? [])
  const groups = computed(() => data.value?.groups ?? [])
  return {
    ruleItems: computed(() => rules.value.map(r => ({ label: r.name, value: r.id }))),
    groupItems: computed(() => groups.value.map(g => ({ label: g.name, value: g.id }))),
    ruleNames: computed(() => Object.fromEntries(rules.value.map(r => [r.id, r.name]))),
    groupNames: computed(() => Object.fromEntries(groups.value.map(g => [g.id, g.name]))),
  }
}
