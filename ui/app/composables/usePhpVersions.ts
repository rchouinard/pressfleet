/**
 * The PHP versions with their support status and end-of-life dates, from
 * `GET /api/v1/catalog/php-versions`. A failure leaves the list empty rather
 * than breaking the page: statuses then show as unknown, and the selector
 * offers only the site's current version (the server still validates).
 */
export function usePhpVersions() {
  const api = useApi()
  const { data } = useAsyncData('php-versions', async () => {
    try {
      const { data } = await api.GET('/api/v1/catalog/php-versions')
      return data
    } catch {
      return undefined
    }
  })
  return computed(() => data.value?.items ?? [])
}
